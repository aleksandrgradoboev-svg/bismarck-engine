package server

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/protocol"
)

// These tests reach inside the session manager on purpose. What they pin —
// which of a live session's two attachment halves is visible to a concurrent
// reader — has no wire representation until it has already gone wrong, and by
// then the session is stuck for the life of the process.

// stubRuntime is the smallest SessionRuntime these tests can drive. The
// servertest fake cannot be used here: it imports this package.
type stubRuntime struct {
	id string

	// emitOnSubscribe, when set, reports a terminal failure from inside
	// Subscribe and then holds Subscribe open for subscribeDelay, which is how
	// a runtime reaches the manager before the session has been published.
	emitOnSubscribe *Error
	subscribeDelay  time.Duration

	mu           sync.Mutex
	listener     func(RuntimeEvent)
	unsubscribes int
	disposes     int
}

func (r *stubRuntime) Snapshot(context.Context) (*protocol.SessionSnapshot, error) {
	return &protocol.SessionSnapshot{
		ID:            r.id,
		Cwd:           "/tmp/pi-server-internal",
		CreatedAt:     1,
		UpdatedAt:     1,
		Phase:         protocol.PhaseIdle,
		Model:         protocol.ModelRef{Provider: "test", ID: "small"},
		ThinkingLevel: protocol.ThinkingOff,
	}, nil
}

func (r *stubRuntime) Phase() protocol.SessionPhase              { return protocol.PhaseIdle }
func (r *stubRuntime) Prompt(context.Context, PromptInput) error { return nil }
func (r *stubRuntime) Steer(context.Context, SteerInput) error   { return nil }
func (r *stubRuntime) Abort(context.Context) error               { return nil }
func (r *stubRuntime) SetModel(context.Context, protocol.ModelRef) error {
	return nil
}
func (r *stubRuntime) SetThinking(context.Context, protocol.ThinkingLevel) error { return nil }

func (r *stubRuntime) Subscribe(listener func(RuntimeEvent)) Unsubscribe {
	r.mu.Lock()
	r.listener = listener
	r.mu.Unlock()
	if r.emitOnSubscribe != nil {
		listener(RuntimeEvent{Type: RuntimeErrorEvent, Err: r.emitOnSubscribe})
		time.Sleep(r.subscribeDelay)
	}
	return func() {
		r.mu.Lock()
		r.listener = nil
		r.unsubscribes++
		r.mu.Unlock()
	}
}

func (r *stubRuntime) Dispose(context.Context) error {
	r.mu.Lock()
	r.disposes++
	r.mu.Unlock()
	return nil
}

func (r *stubRuntime) unsubscribeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unsubscribes
}

// stubService hands out one stubRuntime per acquisition.
type stubService struct {
	emitOnSubscribe *Error
	subscribeDelay  time.Duration

	mu       sync.Mutex
	runtimes []*stubRuntime
}

func (b *stubService) ListSessions(context.Context) ([]protocol.SessionMetadata, error) {
	return nil, nil
}
func (b *stubService) ListModels(context.Context) ([]protocol.ModelMetadata, error) { return nil, nil }

func (b *stubService) CreateSession(ctx context.Context, options CreateSessionOptions) (SessionRuntime, error) {
	return b.OpenSession(ctx, options.ID)
}

func (b *stubService) OpenSession(_ context.Context, sessionID string) (SessionRuntime, error) {
	r := &stubRuntime{id: sessionID, emitOnSubscribe: b.emitOnSubscribe, subscribeDelay: b.subscribeDelay}
	b.mu.Lock()
	b.runtimes = append(b.runtimes, r)
	b.mu.Unlock()
	return r, nil
}

func (b *stubService) latest() *stubRuntime {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.runtimes) == 0 {
		return nil
	}
	return b.runtimes[len(b.runtimes)-1]
}

// hookConn is a ByteConn whose Closed answer runs a one-shot hook, which is the
// only point inside attach a test can get a foot in the door.
type hookConn struct {
	mu       sync.Mutex
	closed   bool
	onClosed func()
}

func (c *hookConn) Closed() bool {
	c.mu.Lock()
	hook := c.onClosed
	c.onClosed = nil
	closed := c.closed
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return closed
}

func (c *hookConn) Send([]byte) error { return nil }

func (c *hookConn) Close([]byte) error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func newInternalServer(t *testing.T, service Service) *Server {
	t.Helper()
	srv, err := New(service, Options{Listeners: []Listener{}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	return srv
}

// readyConnection adopts conn and promotes it past the handshake, which is
// what attach requires and what no test transport is involved in here.
func readyConnection(t *testing.T, srv *Server, conn ByteConn) *connState {
	t.Helper()
	srv.Accept(conn)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.connections) != 1 {
		t.Fatalf("connections = %d, want 1", len(srv.connections))
	}
	for state := range srv.connections {
		state.mu.Lock()
		state.stage = stageReady
		state.mu.Unlock()
		return state
	}
	return nil
}

func acquireLive(t *testing.T, m *sessionManager, service Service, id string) *liveSession {
	t.Helper()
	live, err := m.acquire(context.Background(), id, func(ctx context.Context) (SessionRuntime, error) {
		return service.OpenSession(ctx, id)
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return live
}

// An attachment must never be half visible. A reader holding the manager's lock
// the moment the connection starts reporting the session as attached must
// already see the connection registered on the session — otherwise a disconnect
// arriving there clears the connection's half, finds nothing to remove from the
// session's, and leaves a dead connection holding the runtime forever.
func TestAttachPublishesBothHalvesAtOnce(t *testing.T) {
	t.Parallel()
	service := &stubService{}
	srv := newInternalServer(t, service)
	conn := &hookConn{}
	state := readyConnection(t, srv, conn)

	m := srv.sessions
	live := acquireLive(t, m, service, "session-1")

	holding := make(chan struct{})
	torn := make(chan bool, 1)
	conn.onClosed = func() {
		go func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			close(holding)
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) {
				if state.attached("session-1") {
					_, registered := live.connections[state]
					torn <- !registered
					return
				}
				runtime.Gosched()
			}
			torn <- false
		}()
		<-holding
	}

	if err := m.attach(context.Background(), state, live); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if <-torn {
		t.Fatal("attach published the session on the connection before registering the connection on the session; " +
			"a disconnect landing in that window leaves the runtime acquired forever")
	}
}

// requireAttached and the operation itself are two steps, and a disposal can
// begin between them. The operation must be refused rather than run against a
// runtime that is already being released.
func TestRunOperationRefusesASessionThatStartedDisposing(t *testing.T) {
	t.Parallel()
	service := &stubService{}
	srv := newInternalServer(t, service)
	conn := &hookConn{}
	state := readyConnection(t, srv, conn)

	m := srv.sessions
	live := acquireLive(t, m, service, "session-1")
	if err := m.attach(context.Background(), state, live); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := m.requireAttached(state, "session-1"); err != nil {
		t.Fatalf("requireAttached: %v", err)
	}

	disposing := make(chan struct{})
	m.mu.Lock()
	live.disposing = disposing
	m.mu.Unlock()
	defer close(disposing)

	ran := false
	_, err := m.runOperation(context.Background(), state, live, func(SessionRuntime) error {
		ran = true
		return nil
	})
	if ran {
		t.Fatal("the operation ran against a session that had already started disposing")
	}
	var serverError *Error
	if !errors.As(err, &serverError) || serverError.Code != protocol.ErrorNotFound {
		t.Fatalf("error = %v, want a not_found server error", err)
	}
}

// A runtime is free to report a terminal failure from inside Subscribe, before
// the manager has had a chance to record the canceller Subscribe is about to
// return. The teardown must still cancel the real subscription.
func TestTerminalErrorRaisedDuringSubscribeStillUnsubscribes(t *testing.T) {
	t.Parallel()
	service := &stubService{
		emitOnSubscribe: NewLockedError("lock ownership lost", nil),
		subscribeDelay:  50 * time.Millisecond,
	}
	srv := newInternalServer(t, service)

	m := srv.sessions
	live := acquireLive(t, m, service, "session-1")
	live.queue.Wait()

	m.mu.Lock()
	terminal := live.terminal
	m.mu.Unlock()
	if !terminal {
		t.Fatal("the terminal error must have marked the session")
	}
	if got := service.latest().unsubscribeCount(); got != 1 {
		t.Fatalf("unsubscribes = %d, want 1: the subscription the runtime handed back was never cancelled", got)
	}
}

// TestOverlayLiveMetadataWinsOverStored pins pi's
// `{ ...item, ...toMetadata(snapshot) }` (upstream 6189e53b3): every field the
// live projection produces wins, INCLUDING when it produces nothing, and
// parentSessionId -- the one field toMetadata never sets -- survives from the
// stored record.
func TestOverlayLiveMetadataWinsOverStored(t *testing.T) {
	parent := "parent-1"
	storedUpdated := int64(10)
	storedName := "stored name"
	storedCwd := "/stored"
	stored := protocol.SessionMetadata{
		ID:              "s1",
		CreatedAt:       1,
		UpdatedAt:       &storedUpdated,
		ParentSessionID: &parent,
		SessionName:     &storedName,
		Cwd:             &storedCwd,
	}

	liveUpdated := int64(99)
	liveCwd := "/live"
	live := protocol.SessionMetadata{
		ID:        "s1",
		CreatedAt: 2,
		UpdatedAt: &liveUpdated,
		Cwd:       &liveCwd,
		// SessionName deliberately absent: a live session with no name.
	}

	merged := overlayLiveMetadata(stored, live)

	if merged.CreatedAt != 2 || merged.UpdatedAt == nil || *merged.UpdatedAt != 99 {
		t.Fatalf("live timestamps did not win: %+v", merged)
	}
	if merged.Cwd == nil || *merged.Cwd != "/live" {
		t.Fatalf("live cwd did not win: %+v", merged.Cwd)
	}
	if merged.SessionName != nil {
		t.Fatalf("an absent live sessionName must clear the stored one, got %q", *merged.SessionName)
	}
	if merged.ParentSessionID == nil || *merged.ParentSessionID != parent {
		t.Fatalf("parentSessionId must survive from storage, got %+v", merged.ParentSessionID)
	}
}
