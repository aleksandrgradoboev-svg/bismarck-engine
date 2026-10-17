package durable

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
)

// The Session kernel (pi packages/durable/src/session/session.ts): one
// mutation line, the loaded document tracker cache, and committed
// publication.
//
// The line is a FIFO of jobs in admission order; a job runs on the goroutine
// that admitted it once every earlier job has finished. pi admits at call
// time and runs later; here a call admits and then waits for its turn, so
// what "admitted" means is the same: admission is checked and the place in
// line taken in one step.
//
// The cache is bounded by use: a document no watch or document state is
// attached to leaves once it was not read or written for the retention
// period, and the next access loads it from Storage. Eviction runs as a job
// on the line, so no transaction ever holds a tracker that leaves.
//
// Only committed state is observable. Every commit callback, preparation,
// Storage settlement, adoption and publication runs while the line is held;
// watch and state listeners run later, on goroutines of their own.
//
// Storage failures are final. Every Storage call goes through one guard: the
// first error a Storage method returns, other than for a cancelled read,
// fails the Session, as does a commit it cannot adopt. That call gets the
// error; every later call, and every Storage call still underway, gets a
// *SessionFailedError; close listeners run at once, and the Session then
// closes itself, Storage included. Nothing retries: transient errors are the
// Storage's to retry. Reopening recovers from what was committed.

// defaultDocumentRetention is how long an idle document stays cached: ten
// minutes, as for kept contexts.
var defaultDocumentRetention atomic.Int64 // a time.Duration

func init() { defaultDocumentRetention.Store(int64(10 * time.Minute)) }

// documentRetention is a retention as given when it is 0 or more, otherwise
// the default.
func documentRetention(retention func() time.Duration) time.Duration {
	if retention != nil {
		if d := retention(); d >= 0 {
			return d
		}
	}
	return time.Duration(defaultDocumentRetention.Load())
}

// maxSweepDelay is a retention treated as keeping documents for good.
const maxSweepDelay = 100 * 365 * 24 * time.Hour

// ErrSessionClosed is what a closing or closed Session rejects new work with.
var ErrSessionClosed = errors.New("Session is closed")

// SessionOptions configure a Session.
type SessionOptions struct {
	// Now is the wall clock for task times and cache retention; default
	// time.Now.
	Now func() time.Time
	// DocumentRetention is how long an idle document stays cached; nil or
	// negative is ten minutes. A very long one keeps documents for good.
	DocumentRetention func() time.Duration
	// Hooks are the harness's: pi's Harness subclasses the Session for them.
	Hooks SessionHooks
}

// SessionHooks are what the harness adds to a Session.
type SessionHooks struct {
	// Report receives the Session's failure and listener errors, which never
	// fail what ran them. A plain Session drops them.
	Report func(error)
	// ConversationCreated runs inside every transaction that creates or
	// forks a conversation, after the record is staged.
	ConversationCreated func(tx *Tx, record ConversationRecord) error
	// BeforeClose runs after close seals admission and before the line
	// closes Storage.
	BeforeClose func()
}

type commitListener func(publication CommitPublication, ctx context.Context)

// listenerSet is an insertion-ordered set of listeners.
type listenerSet[F any] struct {
	next  int
	order []int
	items map[int]F
}

func (l *listenerSet[F]) add(f F) int {
	if l.items == nil {
		l.items = map[int]F{}
	}
	l.next++
	l.items[l.next] = f
	l.order = append(l.order, l.next)
	return l.next
}

func (l *listenerSet[F]) remove(id int) {
	if _, ok := l.items[id]; !ok {
		return
	}
	delete(l.items, id)
	for i, o := range l.order {
		if o == id {
			l.order = append(l.order[:i:i], l.order[i+1:]...)
			break
		}
	}
}

func (l *listenerSet[F]) snapshot() []F {
	out := make([]F, 0, len(l.order))
	for _, id := range l.order {
		out = append(out, l.items[id])
	}
	return out
}

func (l *listenerSet[F]) clear() { l.order, l.items = nil, nil }

func (l *listenerSet[F]) len() int { return len(l.order) }

type idleDocument struct {
	addressID string
	since     time.Time
}

type documentSweep struct {
	at    time.Time
	timer *time.Timer
}

// Session owns one mutation line, its records and its tracked documents.
type Session struct {
	storage   *guardedStorage
	hooks     SessionHooks
	nowFn     func() time.Time
	retention func() time.Duration

	// lineMu orders admission; it is taken before mu.
	lineMu      sync.Mutex
	tail        chan struct{}
	lineWaiting int

	mu        sync.Mutex
	documents map[string]*loadedDocument
	// idle are the cached documents with no observer attached, oldest use
	// first, with the time of that use.
	idle      *list.List
	idleIndex map[string]*list.Element
	// attached counts the observers per incarnation; a cached incarnation
	// with any is never evicted.
	attached  map[DocumentID]int
	sweep     *documentSweep
	sweepJobs int
	// lineBusy is set while a job runs; a sweep due meanwhile runs at the
	// job's end, still on the line (dueSweep), as pi's microtask queues it
	// before anything that awaits the job.
	lineBusy          bool
	dueSweep          *documentSweep
	commitListeners   listenerSet[commitListener]
	internalListeners listenerSet[commitListener]
	closeListeners    listenerSet[func()]
	// taskGraph is the task graph view, subscribed at first use.
	taskGraphOnce sync.Once
	taskGraph     *taskGraphView
	closing       bool
	closeDone     chan struct{}
	closeErr      error
	failure       error
	hasFailure    bool
	failed        chan struct{}
	closed        chan struct{}
	end           SessionEnd
}

// NewSession opens a Session kernel over one storage backend.
func NewSession(storage Storage, options SessionOptions) *Session {
	s := &Session{
		hooks:     options.Hooks,
		nowFn:     options.Now,
		retention: options.DocumentRetention,
		tail:      make(chan struct{}),
		documents: map[string]*loadedDocument{},
		idle:      list.New(),
		idleIndex: map[string]*list.Element{},
		attached:  map[DocumentID]int{},
		failed:    make(chan struct{}),
		closed:    make(chan struct{}),
	}
	close(s.tail)
	if s.nowFn == nil {
		s.nowFn = time.Now
	}
	s.storage = &guardedStorage{backend: storage, session: s}
	return s
}

// now is the wall clock for task times.
func (s *Session) now() time.Time { return s.nowFn() }

// clock is the cache's clock. Cache bookkeeping must never change the
// outcome of the operation it follows: a clock that panics counts as
// time.Now.
func (s *Session) clock() (now time.Time) {
	defer func() {
		if recover() != nil {
			now = time.Now()
		}
	}()
	return s.nowFn()
}

// documentRetention is the configured retention; one that panics counts as
// the default.
func (s *Session) documentRetention() (retention time.Duration) {
	defer func() {
		if recover() != nil {
			retention = time.Duration(defaultDocumentRetention.Load())
		}
	}()
	return documentRetention(s.retention)
}

// Storage is the Storage behind the failure guard; every component reads and
// writes through it.
func (s *Session) Storage() Storage { return s.storage }

// Closed is closed once the Session has closed, Storage included: after
// Close, or after the first failed Storage call, which closes the Session
// itself.
func (s *Session) Closed() <-chan struct{} { return s.closed }

// End is why the Session ended; the zero SessionEnd until Closed is closed.
func (s *Session) End() SessionEnd {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.end
}

// Failure is the error that failed the Session, if one did.
func (s *Session) Failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

// Failed is closed the moment the Session fails, before work underway has
// ended.
func (s *Session) Failed() <-chan struct{} { return s.failed }

func (s *Session) report(err error) {
	if s.hooks.Report != nil {
		contained(func() { s.hooks.Report(err) }, func(error) {})
	}
}

// Fail fails the Session with err; the first wins and is reported once.
// Admission ends and close listeners run now. The Storage guard calls it;
// the harness also calls it for a failure of its own bookkeeping.
func (s *Session) Fail(err error) {
	s.mu.Lock()
	if s.hasFailure {
		s.mu.Unlock()
		return
	}
	s.hasFailure, s.failure = true, err
	close(s.failed)
	s.mu.Unlock()
	s.beginClose(context.Background())
	s.report(err)
}

func (s *Session) healthy() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthyLocked()
}

func (s *Session) healthyLocked() error {
	if s.hasFailure {
		return &SessionFailedError{Cause: s.failure}
	}
	return nil
}

func (s *Session) usableLocked() error {
	if err := s.healthyLocked(); err != nil {
		return err
	}
	if s.closing {
		return ErrSessionClosed
	}
	return nil
}

// enqueue runs job on the line. admit checks that the Session is usable in
// the same step as taking the place in line; internal jobs skip it.
func (s *Session) enqueue(admit bool, job func() error) error {
	s.lineMu.Lock()
	if admit {
		s.mu.Lock()
		err := s.usableLocked()
		s.mu.Unlock()
		if err != nil {
			s.lineMu.Unlock()
			return err
		}
	}
	prev := s.tail
	done := make(chan struct{})
	s.tail = done
	s.lineWaiting++
	s.lineMu.Unlock()
	<-prev
	s.lineMu.Lock()
	s.lineWaiting--
	s.lineMu.Unlock()
	defer close(done)
	s.mu.Lock()
	s.lineBusy = true
	s.mu.Unlock()
	defer s.endJob()
	return job()
}

// LineWaiting is how many admitted jobs wait for their turn on the line, not
// counting the one running. A Go call blocks where pi's returns a promise, so
// a caller that must know another call has queued — tests above all — waits
// for this count instead of for time to pass.
func (s *Session) LineWaiting() int {
	s.lineMu.Lock()
	defer s.lineMu.Unlock()
	return s.lineWaiting
}

// endJob ends the running job and runs a sweep that fell due during it.
func (s *Session) endJob() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lineBusy = false
	sweep := s.dueSweep
	s.dueSweep = nil
	if sweep == nil {
		return
	}
	if s.sweep == sweep {
		s.sweep = nil
	}
	if !s.closing {
		s.sweepLocked()
	}
}

// Commit runs one atomic transaction on the mutation line. Inside the
// callback read only through tx: the Session's own reads queue on the line
// the callback holds, and never return.
func (s *Session) Commit(ctx context.Context, change func(tx *Tx) error) error {
	return s.CommitWith(ctx, TransactionScope{}, change)
}

// CommitValue is Session.Commit for a callback with a result.
func CommitValue[T any](ctx context.Context, s *Session, change func(tx *Tx) (T, error)) (T, error) {
	var result T
	err := s.Commit(ctx, func(tx *Tx) error {
		var err error
		result, err = change(tx)
		return err
	})
	return result, err
}

// CommitWith commits bound to scope: the default Tx.CreateTask conversation
// and the task attributed to appended entries.
func (s *Session) CommitWith(ctx context.Context, scope TransactionScope, change func(tx *Tx) error) error {
	return s.enqueue(true, func() error { return s.runCommit(ctx, scope, change) })
}

// ReadOnLine runs a read-only job on the line, so multi-read derivations
// observe one committed state.
func (s *Session) ReadOnLine(job func() error) error {
	return s.enqueue(true, func() error {
		if err := s.healthy(); err != nil {
			return err
		}
		return job()
	})
}

// ConversationDocumentOnLine is a conversation document's current
// incarnation, version and value, for a job already running on the line
// (see ReadOnLine). An absent document is nil.
func (s *Session) ConversationDocumentOnLine(ctx context.Context, ref DocRef) (*StoredDocument, error) {
	def, address, id, err := ref.resolve()
	if err != nil {
		return nil, err
	}
	loaded, err := s.loadDocument(ctx, def, id, address)
	if err != nil || loaded == nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLoadedLocked(def, loaded); err != nil {
		return nil, err
	}
	return &StoredDocument{Record: loaded.record, Version: loaded.valueVersion, Value: loaded.tracker.Value()}, nil
}

func (s *Session) checkLoadedLocked(def DocDef, loaded *loadedDocument) error {
	if err := checkRecordScope(def, loaded.record.Create()); err != nil {
		return err
	}
	return checkRecordVersion(def, loaded.record.Create(), loaded.storedVersion)
}

// Snapshot is a document's committed value, shared and immutable; nil when
// absent. It never creates a document. A cached value is read without
// waiting for the line.
func (s *Session) Snapshot(ctx context.Context, ref DocRef) (map[string]any, error) {
	def, address, id, err := ref.resolve()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if err := s.usableLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if cached := s.documents[id]; cached != nil && cached.valueVersion == def.Version {
		s.touchLocked(id)
		defer s.mu.Unlock()
		if err := s.checkLoadedLocked(def, cached); err != nil {
			return nil, err
		}
		return cached.tracker.Value(), nil
	}
	s.mu.Unlock()
	var value map[string]any
	err = s.enqueue(true, func() error {
		if err := s.healthy(); err != nil {
			return err
		}
		loaded, err := s.loadDocument(ctx, def, id, address)
		if err != nil || loaded == nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.checkLoadedLocked(def, loaded); err != nil {
			return err
		}
		value = loaded.tracker.Value()
		return nil
	})
	return value, err
}

// DocumentState attaches a read-only chord state to a document's current
// incarnation; nil when absent. Dispose it when done.
func (s *Session) DocumentState(ctx context.Context, ref DocRef) (DocumentState, error) {
	def, address, id, err := ref.resolve()
	if err != nil {
		return nil, err
	}
	var state DocumentState
	err = s.enqueue(true, func() error {
		if err := s.healthy(); err != nil {
			return err
		}
		loaded, err := s.loadDocument(ctx, def, id, address)
		if err != nil || loaded == nil {
			return err
		}
		var source *committedStateSource
		detach, err := s.attachDocument(def, loaded, true, func(value map[string]any, detach func()) Observer {
			source = newCommittedStateSource(value, detach)
			return source
		})
		if err != nil {
			return err
		}
		defer func() {
			if r := recover(); r != nil {
				detach()
				panic(r)
			}
		}()
		state = chord.AttachReplicatedState[map[string]any](source, chord.ReplicatedStateSourceOptions{OnError: s.report})
		return nil
	})
	return state, err
}

// WatchDoc acquires a watch of a document's current incarnation; nil when
// absent. Cancelling ctx cancels acquisition, and afterwards the watch.
func (s *Session) WatchDoc(ctx context.Context, ref DocRef) (*DocumentWatch, error) {
	def, address, id, err := ref.resolve()
	if err != nil {
		return nil, err
	}
	var watch *DocumentWatch
	err = s.enqueue(true, func() error {
		if err := s.healthy(); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		loaded, err := s.loadDocument(ctx, def, id, address)
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err != nil || loaded == nil {
			return err
		}
		_, err = s.attachDocument(def, loaded, false, func(value map[string]any, detach func()) Observer {
			watch = newDocumentWatch(value, detach, s.report)
			return watch
		})
		return err
	})
	if err != nil || watch == nil {
		return nil, err
	}
	if ctx.Err() != nil {
		watch.Cancel()
		return nil, context.Cause(ctx)
	}
	if ctx.Done() != nil {
		watch.ObserveCancellation(ctx)
	}
	return watch, nil
}

// SnapshotAsOf is a rewindable conversation document's value as of one
// entry visible from the conversation; nil when absent then.
func (s *Session) SnapshotAsOf(ctx context.Context, ref DocRef, at EntryID) (map[string]any, error) {
	def, address, _, err := ref.resolve()
	if err != nil {
		return nil, err
	}
	if address.Scope.Kind != ScopeConversation {
		return nil, fmt.Errorf("Session.SnapshotAsOf() requires a conversation document")
	}
	var value map[string]any
	err = s.enqueue(true, func() error {
		if err := s.healthy(); err != nil {
			return err
		}
		stored, err := s.storage.VisibleEntry(ctx, address.Scope.ConversationID, at)
		if err != nil {
			return err
		}
		if stored == nil {
			return fmt.Errorf("Entry %d is not visible from conversation %d", at, address.Scope.ConversationID)
		}
		historical := address
		historical.Scope = ConversationScope(stored.Entry.ConversationID)
		record, err := s.storage.FindDocument(ctx, historical, stored.CommitSeq)
		if err != nil || record == nil {
			return err
		}
		document, err := s.storage.Document(ctx, record.ID, stored.CommitSeq)
		if err != nil {
			return err
		}
		if document == nil {
			return fmt.Errorf("Historical document %d (%s) cannot be read", record.ID, record.Kind)
		}
		value, err = materializeDocument(def, document)
		return err
	})
	return value, err
}

// Close seals admission, settles admitted commits, then closes Storage. A
// failed Session closes as well; Close then returns what failed the backend's
// close, if anything did.
func (s *Session) Close(ctx context.Context) error {
	done := s.beginClose(ctx)
	select {
	case <-done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closeErr
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// beginClose starts closing once and returns when closing is done.
func (s *Session) beginClose(ctx context.Context) <-chan struct{} {
	s.lineMu.Lock()
	s.mu.Lock()
	if s.closeDone != nil {
		done := s.closeDone
		s.mu.Unlock()
		s.lineMu.Unlock()
		return done
	}
	s.closing = true
	s.closeDone = make(chan struct{})
	done := s.closeDone
	if s.sweep != nil && s.sweep.timer != nil {
		s.sweep.timer.Stop()
	}
	s.sweep = nil
	listeners := s.closeListeners.snapshot()
	s.closeListeners.clear()
	s.mu.Unlock()
	s.lineMu.Unlock()
	// Close listeners run at once, each on its own.
	for _, listener := range listeners {
		contained(listener, s.report)
	}
	cleanup := context.WithoutCancel(ctx)
	go func() {
		if s.hooks.BeforeClose != nil {
			contained(s.hooks.BeforeClose, s.report)
		}
		err := s.enqueue(false, func() error {
			s.mu.Lock()
			s.commitListeners.clear()
			s.internalListeners.clear()
			s.documents = map[string]*loadedDocument{}
			s.idle.Init()
			s.idleIndex = map[string]*list.Element{}
			s.attached = map[DocumentID]int{}
			s.mu.Unlock()
			if err := s.storage.Close(cleanup); err != nil {
				// A Storage that cannot close is a failed one; an earlier
				// failure stays the cause.
				s.mu.Lock()
				first := !s.hasFailure
				if first {
					s.hasFailure, s.failure = true, err
					close(s.failed)
				}
				s.mu.Unlock()
				if first {
					s.report(err)
				}
				return err
			}
			return nil
		})
		s.mu.Lock()
		s.closeErr = err
		if s.hasFailure {
			s.end = SessionEnd{Reason: SessionFailedReason, Err: s.failure}
		} else {
			s.end = SessionEnd{Reason: SessionClosedReason}
		}
		s.mu.Unlock()
		close(s.closed)
		close(done)
	}()
	return done
}

// SubscribeCommits registers a synchronous post-adoption listener; the
// returned function unsubscribes. It must not block or call the Session; a
// panic is reported.
func (s *Session) SubscribeCommits(listener func(publication CommitPublication, ctx context.Context)) (func(), error) {
	return s.subscribe(&s.commitListeners, listener)
}

// ObserveCommits is SubscribeCommits for the Session's own components,
// whose state follows each commit: a panic fails the Session. They run
// before host listeners.
func (s *Session) ObserveCommits(listener func(publication CommitPublication, ctx context.Context)) (func(), error) {
	return s.subscribe(&s.internalListeners, listener)
}

func (s *Session) subscribe(set *listenerSet[commitListener], listener commitListener) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(); err != nil {
		return nil, err
	}
	id := set.add(listener)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		set.remove(id)
	}, nil
}

// SubscribeClose registers a listener called synchronously when close
// begins, also when a failure closes the Session (Failure is then set). It
// must not block or call the Session; a panic is reported.
func (s *Session) SubscribeClose(listener func()) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(); err != nil {
		return nil, err
	}
	id := s.closeListeners.add(listener)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closeListeners.remove(id)
	}, nil
}

// UnloadDocuments drops every loaded tracker on the line; later access loads
// from Storage.
func (s *Session) UnloadDocuments() error {
	return s.enqueue(false, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.documents = map[string]*loadedDocument{}
		s.idle.Init()
		s.idleIndex = map[string]*list.Element{}
		return nil
	})
}

// DocumentCacheSizes are how many documents the cache holds, how many are
// idle, how many incarnations are pinned, and how many sweep jobs were
// queued so far; for tests.
func (s *Session) DocumentCacheSizes() (cached, idle, attached, sweeps int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.documents), s.idle.Len(), len(s.attached), s.sweepJobs
}

// ─── Document cache ─────────────────────────────────────────────────────

// touchLocked notes a use of a cached document: an unattached one becomes
// the newest idle one, and an eviction is scheduled.
func (s *Session) touchLocked(addressID string) {
	cached := s.documents[addressID]
	if cached == nil || s.attached[cached.record.ID] > 0 {
		return
	}
	now := s.clock()
	retention := s.documentRetention()
	// A hot document moves to the back at most once per granule, a tenth of
	// the retention up to 1 s: it may then leave up to a granule early,
	// which costs a reload, while a commit-heavy workload skips the
	// reordering.
	if e := s.idleIndex[addressID]; e != nil && now.Sub(e.Value.(*idleDocument).since) < min(time.Second, retention/10) {
		return
	}
	s.removeIdleLocked(addressID)
	s.idleIndex[addressID] = s.idle.PushBack(&idleDocument{addressID: addressID, since: now})
	// A pending sweep for the oldest document comes first unless the
	// retention was shortened since; then it moves up.
	if s.sweep == nil || s.sweep.at.After(now.Add(retention)) {
		s.scheduleSweepLocked()
	}
}

func (s *Session) removeIdleLocked(addressID string) {
	if e := s.idleIndex[addressID]; e != nil {
		s.idle.Remove(e)
		delete(s.idleIndex, addressID)
	}
}

func (s *Session) dropLocked(addressID string) {
	delete(s.documents, addressID)
	s.removeIdleLocked(addressID)
}

func (s *Session) installLocked(document *loadedDocument) {
	s.documents[document.addressID] = document
	s.touchLocked(document.addressID)
}

func (s *Session) evictLocked(addressID string, recordID DocumentID) {
	if cached := s.documents[addressID]; cached != nil && cached.record.ID == recordID {
		s.dropLocked(addressID)
	}
}

// cachedDocument is the cached current incarnation, without loading.
func (s *Session) cachedDocument(addressID string) *loadedDocument {
	s.mu.Lock()
	defer s.mu.Unlock()
	cached := s.documents[addressID]
	if cached != nil {
		s.touchLocked(addressID)
	}
	return cached
}

func (s *Session) storedVersion(loaded *loadedDocument) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return loaded.storedVersion
}

// scheduleSweepLocked schedules the eviction of the oldest idle document:
// on the line at once when it has expired, otherwise when it expires.
func (s *Session) scheduleSweepLocked() {
	if s.closing {
		return
	}
	front := s.idle.Front()
	if front == nil {
		return
	}
	retention := s.documentRetention()
	if retention >= maxSweepDelay {
		return
	}
	at := front.Value.(*idleDocument).since.Add(retention)
	if s.sweep != nil && !s.sweep.at.After(at) {
		return
	}
	if s.sweep != nil && s.sweep.timer != nil {
		s.sweep.timer.Stop()
	}
	// The pending sweep stays set until its job runs, so touches while the
	// line is busy queue no more.
	sweep := &documentSweep{at: at}
	run := func() {
		s.mu.Lock()
		s.sweepJobs++
		s.mu.Unlock()
		_ = s.enqueue(false, func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.sweep == sweep {
				s.sweep = nil
			}
			if !s.closing {
				s.sweepLocked()
			}
			return nil
		})
	}
	s.sweep = sweep
	delay := at.Sub(s.clock())
	if delay <= 0 {
		if s.lineBusy {
			s.sweepJobs++
			s.dueSweep = sweep
			return
		}
		go run()
		return
	}
	sweep.timer = time.AfterFunc(delay, run)
}

// sweepLocked evicts the idle documents whose retention passed, oldest
// first, then schedules the next eviction. It runs on the line.
func (s *Session) sweepLocked() {
	retention := s.documentRetention()
	if retention >= maxSweepDelay {
		// pi's Infinity: a Duration ends after 292 years, which a clock can
		// pass, so a retention this long never expires.
		return
	}
	expired := s.clock().Add(-retention)
	for e := s.idle.Front(); e != nil; e = s.idle.Front() {
		idle := e.Value.(*idleDocument)
		if idle.since.After(expired) {
			break
		}
		s.dropLocked(idle.addressID)
	}
	s.scheduleSweepLocked()
}

func (s *Session) sweepDocuments() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
}

// pinLocked pins a cached incarnation for one observer; the returned
// release unpins it once, and it is idle again after the last.
func (s *Session) pinLocked(addressID string, recordID DocumentID) func() {
	s.attached[recordID]++
	s.removeIdleLocked(addressID)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if count := s.attached[recordID] - 1; count > 0 {
				s.attached[recordID] = count
				return
			}
			delete(s.attached, recordID)
			if cached := s.documents[addressID]; cached != nil && cached.record.ID == recordID {
				s.touchLocked(addressID)
			}
		})
	}
}

func (s *Session) loadDocument(ctx context.Context, def DocDef, addressID string, address DocumentAddress) (*loadedDocument, error) {
	s.mu.Lock()
	cached := s.documents[addressID]
	// A tracker serves only definitions of the version its value was
	// materialized for; others reload from Storage.
	if cached != nil && cached.valueVersion == def.Version {
		s.touchLocked(addressID)
		s.mu.Unlock()
		return cached, nil
	}
	if cached != nil {
		s.dropLocked(addressID)
	}
	s.mu.Unlock()
	record, err := s.storage.FindDocument(ctx, address, Current)
	if err != nil || record == nil {
		return nil, err
	}
	stored, err := s.storage.Document(ctx, record.ID, Current)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("Current document %d (%s) cannot be read", record.ID, record.Kind)
	}
	value, err := materializeDocument(def, stored)
	if err != nil {
		return nil, err
	}
	loaded := &loadedDocument{
		addressID:       addressID,
		record:          stored.Record,
		storedVersion:   stored.Version,
		valueVersion:    def.Version,
		deltasSinceBase: stored.DeltasSinceBase,
		tracker:         delta.Track(value),
	}
	s.mu.Lock()
	s.documents[addressID] = loaded
	s.touchLocked(addressID)
	s.mu.Unlock()
	return loaded, nil
}

// ─── Commits and observers ──────────────────────────────────────────────

func (s *Session) runCommit(ctx context.Context, scope TransactionScope, change func(tx *Tx) error) error {
	if err := s.healthy(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	tx := newTx(s, ctx, scope)
	err := change(tx)
	if err == nil {
		// A callback that swallowed a failed read must not commit, nor
		// succeed as if it had.
		err = s.healthy()
	}
	if err != nil {
		tx.settleFailure()
		s.sweepDocuments()
		return err
	}
	writes, err := tx.settleSuccess()
	if err != nil {
		// Settlement aborted every change already.
		s.sweepDocuments()
		return err
	}
	if len(writes) == 0 {
		tx.discard()
		s.sweepDocuments()
		return nil
	}
	// Once admitted, caller cancellation does not interrupt Storage
	// settlement.
	seq, err := s.storage.Commit(context.WithoutCancel(ctx), writes)
	if err != nil {
		// The guard has failed the Session.
		tx.discard()
		return err
	}
	documents, err := s.adopt(tx, seq)
	if err != nil {
		// Storage already committed; a failed adoption leaves memory behind
		// durable state.
		s.Fail(err)
		return err
	}
	s.publish(ctx, seq, writes, documents)
	s.sweepDocuments()
	return nil
}

func (s *Session) adopt(tx *Tx, seq Seq) (documents []CommitChange, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	documents = tx.adopt(seq)
	for _, d := range documents {
		switch d := d.(type) {
		case DocumentChange:
			s.touchLocked(addressID(d.Record.Address()))
		case DocumentCopyChange:
			s.touchLocked(addressID(d.Record.Address()))
		}
	}
	return documents, nil
}

func (s *Session) publish(ctx context.Context, seq Seq, writes []StorageWrite, documents []CommitChange) {
	s.mu.Lock()
	if s.commitListeners.len() == 0 && s.internalListeners.len() == 0 {
		s.mu.Unlock()
		return
	}
	internal := s.internalListeners.snapshot()
	host := s.commitListeners.snapshot()
	s.mu.Unlock()
	var changes []CommitChange
	for _, w := range writes {
		switch w := w.(type) {
		case ConversationWrite:
			changes = append(changes, w)
		case EntryWrite:
			changes = append(changes, w)
		case TaskWrite:
			changes = append(changes, w)
		case SubmissionWrite:
			changes = append(changes, w)
		}
	}
	changes = append(changes, documents...)
	publication := CommitPublication{Seq: seq, Changes: changes}
	// The commit is durable: a failing listener neither fails the commit nor
	// skips the others.
	for _, listener := range internal {
		contained(func() { listener(publication, ctx) }, s.Fail)
	}
	for _, listener := range host {
		contained(func() { listener(publication, ctx) }, s.report)
	}
}

// attachDocument attaches an observer to one committed incarnation: check
// the definition, then forward this incarnation's committed changes and
// close. The returned detach removes both subscriptions. It runs on the line.
func (s *Session) attachDocument(def DocDef, loaded *loadedDocument, stateSource bool, create func(value map[string]any, detach func()) Observer) (func(), error) {
	s.mu.Lock()
	if err := s.checkLoadedLocked(def, loaded); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	// Attached, the incarnation stays cached; once its last observer
	// detaches, or it retires, it is idle again.
	unpin := s.pinLocked(loaded.addressID, loaded.record.ID)
	value := loaded.tracker.Value()
	observedVersion := loaded.valueVersion
	s.mu.Unlock()

	var mu sync.Mutex
	detached := false
	var unsubscribers []func()
	detach := func() {
		mu.Lock()
		detached = true
		subs := unsubscribers
		unsubscribers = nil
		mu.Unlock()
		for _, unsubscribe := range subs {
			unsubscribe()
		}
		unpin()
	}
	keep := func(unsubscribe func()) {
		mu.Lock()
		if detached {
			mu.Unlock()
			unsubscribe()
			return
		}
		unsubscribers = append(unsubscribers, unsubscribe)
		mu.Unlock()
	}
	observer := create(value, detach)
	unsubscribe, err := s.ObserveCommits(func(publication CommitPublication, ctx context.Context) {
		for _, c := range publication.Changes {
			change, ok := c.(DocumentChange)
			if !ok || change.Record.ID != loaded.record.ID {
				continue
			}
			// A document state's frames carry no caller cancellation; a
			// watch observes its own.
			frameCtx := ctx
			if stateSource {
				frameCtx = context.WithoutCancel(ctx)
			}
			ops := observedOperations(&observedVersion, change)
			// A migration-only base changes nothing for an observer of the
			// new version.
			if len(ops) == 0 {
				continue
			}
			// A retired incarnation never changes again: its observers keep
			// their last value but pin nothing.
			if change.Value == nil {
				unpin()
			}
			observer.Advance(frameCtx, change.Value, ops)
		}
	})
	if err != nil {
		unpin()
		return nil, err
	}
	keep(unsubscribe)
	unsubscribeClose, err := s.SubscribeClose(func() { observer.CloseSession(s.Failure()) })
	if err != nil {
		detach()
		return nil, err
	}
	keep(unsubscribeClose)
	return detach, nil
}

// observedOperations are the operations an observer applies for one
// committed change. An observer hydrated under another definition version
// holds a differently shaped value, so it receives the new value as a root
// replacement.
func observedOperations(observed *int, change DocumentChange) []delta.Op {
	if change.Value == nil {
		return retirementOps
	}
	if change.Version == *observed {
		return change.Ops
	}
	*observed = change.Version
	return []delta.Op{delta.Replace{Value: change.Value}}
}

// contained runs a callback that must not fail its caller: a panic goes to
// onError.
func contained(callback func(), onError func(error)) {
	defer func() {
		if r := recover(); r != nil {
			onError(panicError(r))
		}
	}()
	callback()
}

// ─── Storage guard ──────────────────────────────────────────────────────

// guardedStorage is the Session's Storage behind its failure guard. Before a
// call, a failed Session gets *SessionFailedError without reaching the
// backend. A call that fails fails the Session and returns the backend's
// error, unless it is a *StorageRequestError or a read whose context was
// cancelled: neither says the Storage is broken. Close always reaches the
// backend, so a failed Session still closes it.
type guardedStorage struct {
	backend Storage
	session *Session

	mu       sync.Mutex
	underway sync.WaitGroup
	closing  bool
}

// call runs one backend call under the guard. read is a read's context;
// Commit and MintID pass none, so nothing exempts their errors: once a batch
// is admitted, whether it committed is unknown. A call still underway when
// another fails the Session ends with *SessionFailedError too.
func (g *guardedStorage) call(read context.Context, run func() error) (err error) {
	if err := g.session.healthy(); err != nil {
		return err
	}
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return ErrSessionClosed
	}
	g.underway.Add(1)
	g.mu.Unlock()
	defer g.underway.Done()
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = panicError(r)
			}
		}()
		err = run()
	}()
	if err != nil {
		if failed := g.session.healthy(); failed != nil {
			return failed
		}
		exempt := read != nil && (IsStorageRequestError(err) || read.Err() != nil)
		if !exempt {
			g.session.Fail(err)
		}
		return err
	}
	return g.session.healthy()
}

func (g *guardedStorage) Commit(ctx context.Context, writes []StorageWrite) (seq Seq, err error) {
	err = g.call(nil, func() (err error) { seq, err = g.backend.Commit(ctx, writes); return })
	return
}

func (g *guardedStorage) MintID(ctx context.Context) (id ID, err error) {
	err = g.call(nil, func() (err error) { id, err = g.backend.MintID(ctx); return })
	return
}

func (g *guardedStorage) Conversation(ctx context.Context, id ConversationID) (r *ConversationRecord, err error) {
	err = g.call(ctx, func() (err error) { r, err = g.backend.Conversation(ctx, id); return })
	return
}

func (g *guardedStorage) ScanConversations(ctx context.Context, query ConversationQuery, limit int, cursor Cursor) (p Page[ConversationRecord], err error) {
	err = g.call(ctx, func() (err error) { p, err = g.backend.ScanConversations(ctx, query, limit, cursor); return })
	return
}

func (g *guardedStorage) Entry(ctx context.Context, id EntryID) (e *StoredEntry, err error) {
	err = g.call(ctx, func() (err error) { e, err = g.backend.Entry(ctx, id); return })
	return
}

func (g *guardedStorage) VisibleEntry(ctx context.Context, conversationID ConversationID, id EntryID) (e *StoredEntry, err error) {
	err = g.call(ctx, func() (err error) { e, err = g.backend.VisibleEntry(ctx, conversationID, id); return })
	return
}

func (g *guardedStorage) FindLatestHeadMarker(ctx context.Context, conversationID ConversationID, atOrBefore *EntryID) (e *EntryRecord, err error) {
	err = g.call(ctx, func() (err error) { e, err = g.backend.FindLatestHeadMarker(ctx, conversationID, atOrBefore); return })
	return
}

func (g *guardedStorage) ScanEntries(ctx context.Context, query EntryQuery, limit int, cursor Cursor) (p Page[EntryRecord], err error) {
	err = g.call(ctx, func() (err error) { p, err = g.backend.ScanEntries(ctx, query, limit, cursor); return })
	return
}

func (g *guardedStorage) Task(ctx context.Context, id TaskID) (t *TaskRecord, err error) {
	err = g.call(ctx, func() (err error) { t, err = g.backend.Task(ctx, id); return })
	return
}

func (g *guardedStorage) ScanTasks(ctx context.Context, query TaskQuery, limit int, cursor Cursor) (p Page[TaskRecord], err error) {
	err = g.call(ctx, func() (err error) { p, err = g.backend.ScanTasks(ctx, query, limit, cursor); return })
	return
}

func (g *guardedStorage) Submission(ctx context.Context, id SubmissionID) (r *SubmissionRecord, err error) {
	err = g.call(ctx, func() (err error) { r, err = g.backend.Submission(ctx, id); return })
	return
}

func (g *guardedStorage) ScanSubmissions(ctx context.Context, query SubmissionQuery, limit int, cursor Cursor) (p Page[SubmissionRecord], err error) {
	err = g.call(ctx, func() (err error) { p, err = g.backend.ScanSubmissions(ctx, query, limit, cursor); return })
	return
}

func (g *guardedStorage) SubmissionByRequest(ctx context.Context, conversationID ConversationID, requestID string) (r *SubmissionRecord, err error) {
	err = g.call(ctx, func() (err error) { r, err = g.backend.SubmissionByRequest(ctx, conversationID, requestID); return })
	return
}

func (g *guardedStorage) FindDocument(ctx context.Context, address DocumentAddress, at DocumentPoint) (r *DocumentRecord, err error) {
	err = g.call(ctx, func() (err error) { r, err = g.backend.FindDocument(ctx, address, at); return })
	return
}

func (g *guardedStorage) Document(ctx context.Context, id DocumentID, at DocumentPoint) (d *StoredDocument, err error) {
	err = g.call(ctx, func() (err error) { d, err = g.backend.Document(ctx, id, at); return })
	return
}

func (g *guardedStorage) ScanDocuments(ctx context.Context, query DocumentQuery, limit int, cursor Cursor) (p Page[DocumentRecord], err error) {
	err = g.call(ctx, func() (err error) { p, err = g.backend.ScanDocuments(ctx, query, limit, cursor); return })
	return
}

// Close closes the backend once every call underway has settled. New calls
// are refused from here on: a paged read must not start its next page while
// the backend closes.
func (g *guardedStorage) Close(ctx context.Context) error {
	g.mu.Lock()
	g.closing = true
	g.mu.Unlock()
	g.underway.Wait()
	return g.backend.Close(ctx)
}
