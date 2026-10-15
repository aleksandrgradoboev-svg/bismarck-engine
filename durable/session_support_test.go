package durable_test

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Session test support (pi packages/durable/test/session-support.ts).

var bg = context.Background()

// gate holds calls until released and reports when the first held call
// arrives.
type gate struct {
	entered     chan struct{}
	enteredOnce sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func newGate() *gate { return &gate{entered: make(chan struct{}), release: make(chan struct{})} }

func (g *gate) enter() {
	g.enteredOnce.Do(func() { close(g.entered) })
	<-g.release
}

func (g *gate) Release() { g.releaseOnce.Do(func() { close(g.release) }) }

// Entered waits for the first held call.
func (g *gate) Entered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no call reached the gate")
	}
}

// controlledStorage is memory storage with observable commits, held calls and
// injected commit failures.
type controlledStorage struct {
	*memory.Storage

	mu sync.Mutex
	// admitted are the exact batches the Session admitted; commits are
	// detached copies for value assertions.
	admitted          [][]durable.StorageWrite
	commits           [][]durable.StorageWrite
	mintCount         int
	documentReadCount int
	commitGate        *gate
	findGate          *gate
	commitFailure     error
}

func newControlledStorage() *controlledStorage { return &controlledStorage{Storage: memory.New()} }

func (s *controlledStorage) holdCommits() *gate {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitGate = newGate()
	return s.commitGate
}

func (s *controlledStorage) holdFindDocument() *gate {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.findGate = newGate()
	return s.findGate
}

func (s *controlledStorage) failNextCommit(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitFailure = err
}

func (s *controlledStorage) commitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.commits)
}

func (s *controlledStorage) lastCommit() []durable.StorageWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commits[len(s.commits)-1]
}

func (s *controlledStorage) lastAdmitted() []durable.StorageWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.admitted[len(s.admitted)-1]
}

func (s *controlledStorage) mints() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mintCount
}

func (s *controlledStorage) documentReads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.documentReadCount
}

func (s *controlledStorage) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	detached := make([]durable.StorageWrite, len(writes))
	for i, w := range writes {
		c, err := durable.CloneWrite(w)
		if err != nil {
			return 0, err
		}
		detached[i] = c
	}
	s.mu.Lock()
	s.admitted = append(s.admitted, writes)
	s.commits = append(s.commits, detached)
	held := s.commitGate
	s.mu.Unlock()
	if held != nil {
		held.enter()
		s.mu.Lock()
		if s.commitGate == held {
			s.commitGate = nil
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	failure := s.commitFailure
	s.commitFailure = nil
	s.mu.Unlock()
	if failure != nil {
		return 0, failure
	}
	return s.Storage.Commit(ctx, writes)
}

func (s *controlledStorage) MintID(ctx context.Context) (durable.ID, error) {
	s.mu.Lock()
	s.mintCount++
	s.mu.Unlock()
	return s.Storage.MintID(ctx)
}

func (s *controlledStorage) Document(ctx context.Context, id durable.DocumentID, at durable.DocumentPoint) (*durable.StoredDocument, error) {
	s.mu.Lock()
	s.documentReadCount++
	s.mu.Unlock()
	return s.Storage.Document(ctx, id, at)
}

func (s *controlledStorage) FindDocument(ctx context.Context, address durable.DocumentAddress, at durable.DocumentPoint) (*durable.DocumentRecord, error) {
	s.mu.Lock()
	held := s.findGate
	s.mu.Unlock()
	if held != nil {
		held.enter()
		s.mu.Lock()
		if s.findGate == held {
			s.findGate = nil
		}
		s.mu.Unlock()
	}
	return s.Storage.FindDocument(ctx, address, at)
}

// testSession is a Session kernel, its controlled storage and every
// committed publication.
type testSession struct {
	storage *controlledStorage
	session *durable.Session

	mu           sync.Mutex
	publications []durable.CommitPublication
}

func openTestSession(t *testing.T) *testSession {
	t.Helper()
	ts := &testSession{storage: newControlledStorage()}
	// These tests pin the cache's identities and loads, so documents stay as
	// long as the default keeps them.
	ts.session = durable.NewSession(ts.storage, durable.SessionOptions{
		DocumentRetention: func() time.Duration { return 10 * time.Minute },
	})
	if _, err := ts.session.SubscribeCommits(func(p durable.CommitPublication, _ context.Context) {
		ts.mu.Lock()
		ts.publications = append(ts.publications, p)
		ts.mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ts.session.Close(bg) })
	return ts
}

func (ts *testSession) publicationCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.publications)
}

func (ts *testSession) lastPublication() durable.CommitPublication {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.publications[len(ts.publications)-1]
}

func documentChanges(p durable.CommitPublication) []durable.DocumentChange {
	var out []durable.DocumentChange
	for _, c := range p.Changes {
		if d, ok := c.(durable.DocumentChange); ok {
			out = append(out, d)
		}
	}
	return out
}

func createConversation(t *testing.T, s *durable.Session) durable.ConversationID {
	t.Helper()
	id, err := durable.CommitValue(bg, s, func(tx *durable.Tx) (durable.ConversationID, error) {
		c, err := tx.CreateConversation(durable.Ownerless)
		return c.ID, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func commit(t *testing.T, s *durable.Session, change func(tx *durable.Tx) error) {
	t.Helper()
	if err := s.Commit(bg, change); err != nil {
		t.Fatal(err)
	}
}

func mustDoc(tx *durable.Tx, ref durable.DocRef) *delta.Draft {
	d, err := tx.Doc(ref)
	if err != nil {
		panic(err)
	}
	return d
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func snapshot(t *testing.T, s *durable.Session, ref durable.DocRef) map[string]any {
	t.Helper()
	v, err := s.Snapshot(bg, ref)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// same reports whether two maps or slices are the same container: pi's toBe.
func same(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if !va.IsValid() || !vb.IsValid() {
		return !va.IsValid() && !vb.IsValid()
	}
	if va.Kind() != vb.Kind() || (va.Kind() != reflect.Map && va.Kind() != reflect.Slice) {
		return false
	}
	if va.IsNil() || vb.IsNil() {
		return va.IsNil() && vb.IsNil()
	}
	return va.UnsafePointer() == vb.UnsafePointer() && va.Len() == vb.Len()
}

func errorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want one containing %q", err, want)
	}
}

// eventually waits for cond, as pi's flush() waits for deliveries.
func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// settle lets background deliveries run before asserting that nothing more
// arrived.
func settle() { time.Sleep(20 * time.Millisecond) }

// async runs f on a goroutine and returns its error channel.
func async(f func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	return ch
}

func wait(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("call did not return")
		return nil
	}
}

func get(m map[string]any, path ...string) any {
	var v any = m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[p]
	}
	return v
}
