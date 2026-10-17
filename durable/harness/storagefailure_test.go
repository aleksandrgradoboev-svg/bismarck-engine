package harness

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// pi packages/durable/test/harness-storage-failure.test.ts. n/a in Go: the
// cases of rejected promises, thenables and another realm's promise. The
// cases of event streams, views and a task's conversation handle come with
// those stages.

// waitingTask waits for its signal, records that it saw it, and ends aborted
// through its abort handler.
func waitingTask(t *testing.T, signals *phaseLog) Task {
	return MustDefineTask(TaskSpec[any, stepState, any]{
		Name:    "test.waiting",
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "work"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"work": func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
				<-ctx.Done()
				signals.add("aborted")
				return nil
			},
		},
		Abort: func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error { return rt.Commit(ctx, abortWith("")) },
	})
}

// failingCalls fails each Storage method named in failing once, with err. A
// scan of entries with a limit above 1 is a context read's scan of its range,
// off the Session line; heldRange holds it until released.
type failingCalls struct {
	*controlledStorage
	err          error
	mu           sync.Mutex
	failing      map[string]bool
	rangeEntered chan struct{}
	rangeRelease chan struct{}
}

func newFailingCalls() *failingCalls {
	return &failingCalls{controlledStorage: newControlledStorage(), err: errors.New("disk gone"), failing: map[string]bool{}}
}

func (s *failingCalls) fail(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing[method] {
		delete(s.failing, method)
		return s.err
	}
	return nil
}

func (s *failingCalls) arm(method string) {
	s.mu.Lock()
	s.failing[method] = true
	s.mu.Unlock()
}

func (s *failingCalls) Task(ctx context.Context, id durable.TaskID) (*durable.TaskRecord, error) {
	if err := s.fail("task"); err != nil {
		return nil, err
	}
	return s.controlledStorage.Task(ctx, id)
}

func (s *failingCalls) ScanEntries(ctx context.Context, query durable.EntryQuery, limit int, cursor durable.Cursor) (durable.Page[durable.EntryRecord], error) {
	if limit > 1 {
		s.mu.Lock()
		entered, release := s.rangeEntered, s.rangeRelease
		s.mu.Unlock()
		if entered != nil {
			close(entered)
			<-release
		}
		if err := s.fail("scanRange"); err != nil {
			return durable.Page[durable.EntryRecord]{}, err
		}
	}
	return s.controlledStorage.ScanEntries(ctx, query, limit, cursor)
}

func (s *failingCalls) MintID(ctx context.Context) (durable.ID, error) {
	if err := s.fail("mintId"); err != nil {
		return 0, err
	}
	return s.controlledStorage.MintID(ctx)
}

func appendNote(o opened) error {
	return o.root.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.AppendEntry(o.root.ID, durable.EntryDraft{Kind: "note"})
		return err
	})
}

func expectFailed(t *testing.T, err error, cause error) {
	t.Helper()
	var failed *durable.SessionFailedError
	if !errors.As(err, &failed) || failed.Cause != cause {
		t.Fatalf("err = %v, want SessionFailed caused by %v", err, cause)
	}
}

func expectEnd(t *testing.T, h *Harness, reason string, err error) {
	t.Helper()
	select {
	case <-h.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the Session did not end")
	}
	if end := h.End(); end.Reason != reason || end.Err != err {
		t.Fatalf("end = %+v, want %s %v", end, reason, err)
	}
}

func expectReports(t *testing.T, reports *reportLog, want ...error) {
	t.Helper()
	got := reports.all()
	if len(got) != len(want) {
		t.Fatalf("reports = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] && got[i].Error() != want[i].Error() {
			t.Fatalf("reports = %v, want %v", got, want)
		}
	}
}

func TestStorageFailureEndsEverythingWithTheFirstError(t *testing.T) {
	var signals phaseLog
	storage := newControlledStorage()
	task := waitingTask(t, &signals)
	o := openRoot(t, []Task{task}, openOptions{storage: storage})
	id := start(t, o.root, task, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return getTask(t, o.h, id).State.Status == durable.TaskRunning })
	waiting := async(func() (durable.TaskRecord, error) { return o.h.WaitForTask(bg, id) })
	idle := async(func() (struct{}, error) { return struct{}{}, o.h.WaitForIdle(bg) })
	watch, err := o.h.WatchDoc(bg, LiveDoc.Conversation(o.root.ID))
	must(t, err)
	parked(t, 1, onWaiter, "(*Harness).WaitForTask(")
	parked(t, 1, onWaiter, "(*Harness).WaitForIdle(")

	failure := errors.New("disk gone")
	storage.failNextCommit(failure)
	// The call that hit it gets the storage error itself.
	if err := appendNote(o); err != failure {
		t.Fatalf("commit = %v", err)
	}
	_, err = await(t, waiting)
	expectFailed(t, err, failure)
	_, err = await(t, idle)
	expectFailed(t, err, failure)
	<-watch.Closed()
	if end := watch.End(); end.Reason != durable.WatchSessionFailed || end.Err != failure {
		t.Fatalf("watch end = %+v", end)
	}
	// Every later call, and a wait that starts now.
	_, err = o.h.GetTask(bg, id)
	expectFailed(t, err, failure)
	expectFailed(t, o.h.WaitForIdle(bg), failure)
	expectEnd(t, o.h, durable.SessionFailedReason, failure)
	eventually(t, func() bool { return len(signals.all()) == 1 })
	expectReports(t, o.reports, failure)
}

func TestStorageFailureFailsItFromAReadOffTheSessionLine(t *testing.T) {
	storage := newFailingCalls()
	o := openRoot(t, nil, openOptions{storage: storage})
	must(t, appendNote(o))
	storage.arm("scanRange")
	if _, err := o.root.Context(bg, nil); err != storage.err {
		t.Fatalf("context = %v", err)
	}
	expectEnd(t, o.h, durable.SessionFailedReason, storage.err)
	expectReports(t, o.reports, storage.err)
}

func TestStorageFailureEndsCallsUnderwayWithSessionFailedAndClosesOnceTheySettled(t *testing.T) {
	storage := newFailingCalls()
	o := openRoot(t, nil, openOptions{storage: storage})
	must(t, appendNote(o))
	storage.mu.Lock()
	storage.rangeEntered, storage.rangeRelease = make(chan struct{}), make(chan struct{})
	entered, release := storage.rangeEntered, storage.rangeRelease
	storage.mu.Unlock()
	reading := async(func() (ContextView, error) { return o.root.Context(bg, nil) })
	<-entered
	// Another call fails the Session while the context read is underway.
	storage.arm("mintId")
	if err := appendNote(o); err != storage.err {
		t.Fatalf("commit = %v", err)
	}
	if settledWithin(o.h.Closed()) {
		t.Fatal("closed while a call is underway")
	}
	// The read succeeds at the backend, yet its caller gets the failure:
	// nothing it read is used.
	close(release)
	_, err := await(t, reading)
	expectFailed(t, err, storage.err)
	expectEnd(t, o.h, durable.SessionFailedReason, storage.err)
}

func TestStorageFailureClosesTheBackendOnlyAfterAPagedReadUnderway(t *testing.T) {
	var pages phaseLog
	firstPage, releaseFirst := make(chan struct{}), make(chan struct{})
	storage := &pagedStorage{controlledStorage: newControlledStorage(), pages: &pages, firstPage: firstPage, releaseFirst: releaseFirst}
	o := openRoot(t, nil, openOptions{storage: storage})
	// More entries than one page of the context read.
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		for n := 0; n < 300; n++ {
			if _, err := tx.AppendEntry(o.root.ID, durable.EntryDraft{Kind: "note"}); err != nil {
				return err
			}
		}
		return nil
	}))
	reading := async(func() (ContextView, error) { return o.root.Context(bg, nil) })
	<-firstPage
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	flush()
	close(releaseFirst)
	_, err := await(t, reading)
	expectError(t, err, "is closed")
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
	if got := pages.all(); !reflect.DeepEqual(got, []string{"first", "close"}) {
		t.Fatalf("pages = %v", got)
	}
	expectEnd(t, o.h, durable.SessionClosedReason, nil)
}

type pagedStorage struct {
	*controlledStorage
	pages        *phaseLog
	once         sync.Once
	firstPage    chan struct{}
	releaseFirst chan struct{}
}

func (s *pagedStorage) ScanEntries(ctx context.Context, query durable.EntryQuery, limit int, cursor durable.Cursor) (durable.Page[durable.EntryRecord], error) {
	if limit > 1 {
		if cursor == nil {
			s.pages.add("first")
			s.once.Do(func() { close(s.firstPage) })
			<-s.releaseFirst
		} else {
			s.pages.add("next")
		}
	}
	return s.controlledStorage.ScanEntries(ctx, query, limit, cursor)
}

func (s *pagedStorage) Close(ctx context.Context) error {
	s.pages.add("close")
	return s.controlledStorage.Close(ctx)
}

func TestStorageFailureFailsItWhenStorageCannotCloseAndAnEarlierFailureStaysTheCause(t *testing.T) {
	closeError := errors.New("close failed")
	healthyStorage := newControlledStorage()
	healthyStorage.closeError = closeError
	healthy := openRoot(t, nil, openOptions{storage: healthyStorage})
	if err := healthy.h.Close(bg); err != closeError {
		t.Fatalf("close = %v", err)
	}
	expectEnd(t, healthy.h, durable.SessionFailedReason, closeError)
	expectReports(t, healthy.reports, closeError)

	storage := newControlledStorage()
	storage.closeError = closeError
	failed := openRoot(t, nil, openOptions{storage: storage})
	failure := errors.New("disk gone")
	storage.failNextCommit(failure)
	if err := appendNote(failed); err != failure {
		t.Fatalf("commit = %v", err)
	}
	expectEnd(t, failed.h, durable.SessionFailedReason, failure)
	expectReports(t, failed.reports, failure)
}

func TestStorageFailureFailsTheHarnessesOwnCallsWithSessionFailed(t *testing.T) {
	storage := newControlledStorage()
	o := openRoot(t, nil, openOptions{storage: storage})
	failure := errors.New("disk gone")
	storage.failNextCommit(failure)
	if err := appendNote(o); err != failure {
		t.Fatalf("commit = %v", err)
	}
	expectFailed(t, o.h.Resume(), failure)
	_, err := o.h.Root(bg, nil, nil)
	expectFailed(t, err, failure)
	_, err = o.h.Conversation(bg, o.root.ID)
	expectFailed(t, err, failure)
	_, err = o.h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.Ownerless})
	expectFailed(t, err, failure)
}

func TestStorageFailureFailsItFromAFailedIDMint(t *testing.T) {
	storage := newFailingCalls()
	o := openRoot(t, nil, openOptions{storage: storage})
	storage.arm("mintId")
	if err := appendNote(o); err != storage.err {
		t.Fatalf("commit = %v", err)
	}
	expectEnd(t, o.h, durable.SessionFailedReason, storage.err)
}

func TestStorageFailureDoesNotCommitWhatACallbackDidAfterItCaughtAFailedRead(t *testing.T) {
	storage := newFailingCalls()
	o := openRoot(t, nil, openOptions{storage: storage})
	written := storage.commitCount()
	storage.arm("task")
	err := o.root.Commit(bg, func(tx *durable.Tx) error {
		_, _ = tx.Task(1)
		_, err := tx.AppendEntry(o.root.ID, durable.EntryDraft{Kind: "note"})
		return err
	})
	expectFailed(t, err, storage.err)
	if storage.commitCount() != written {
		t.Fatal("a commit landed after a failed read")
	}
}

func TestStorageFailureFailsItForAStorageRequestErrorFromACommit(t *testing.T) {
	storage := newControlledStorage()
	o := openRoot(t, nil, openOptions{storage: storage})
	rejection := durable.NewStorageRequestError("bad batch")
	storage.failNextCommit(rejection)
	if err := appendNote(o); err != rejection {
		t.Fatalf("commit = %v", err)
	}
	expectEnd(t, o.h, durable.SessionFailedReason, rejection)
}

func TestStorageFailureFailsOnlyTheCallForAnInvalidRequest(t *testing.T) {
	o := openRoot(t, nil, openOptions{storage: memory.New()})
	var entry durable.EntryRecord
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		entry, err = tx.AppendEntry(o.root.ID, durable.EntryDraft{Kind: "note"})
		return err
	}))
	requests := []func() error{
		func() error {
			_, err := o.root.Entries(bg, durable.EntryQuery{}, 10, durable.Cursor{"after": "x"})
			return err
		},
		func() error {
			page, err := o.root.Entries(bg, durable.EntryQuery{Order: "ascending"}, 1, nil)
			if err != nil {
				return err
			}
			next := page.Next
			if next == nil {
				next = durable.Cursor{"after": 1, "order": "ascending"}
			}
			_, err = o.root.Entries(bg, durable.EntryQuery{Order: "descending"}, 1, next)
			return err
		},
		// pi.live keeps no history.
		func() error {
			_, err := o.h.SnapshotAsOf(bg, LiveDoc.Conversation(o.root.ID), entry.ID)
			return err
		},
	}
	for i, request := range requests {
		if err := request(); !durable.IsStorageRequestError(err) {
			t.Fatalf("request %d = %v, want a request error", i, err)
		}
	}
	must(t, appendNote(o))
	if settledWithin(o.h.Closed()) {
		t.Fatal("an invalid request ended the Session")
	}
	expectReports(t, o.reports)
}

func TestStorageFailureFailsItForAPanicInItsOwnCommitObservers(t *testing.T) {
	storage := newControlledStorage()
	o := openRoot(t, nil, openOptions{storage: storage})
	bug := errors.New("listener bug")
	_, err := o.h.ObserveCommits(func(durable.CommitPublication, context.Context) { panic(bug) })
	must(t, err)
	// The commit itself is durable and returns; the Session fails behind it.
	must(t, appendNote(o))
	expectEnd(t, o.h, durable.SessionFailedReason, bug)
	expectReports(t, o.reports, bug)
}

func TestStorageFailureFailsItForAStorageErrorDuringAHostCloseBeforeTheBackendCloses(t *testing.T) {
	storage := newControlledStorage()
	o := openRoot(t, nil, openOptions{storage: storage})
	held := storage.holdCommits()
	failure := errors.New("disk gone")
	storage.failNextCommit(failure)
	committing := async(func() (struct{}, error) { return struct{}{}, appendNote(o) })
	held.Entered(t)
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	eventually(t, func() bool { return o.h.tasks.closing.Load() })
	storage.releaseHold(held)
	if _, err := await(t, committing); err != failure {
		t.Fatalf("commit = %v", err)
	}
	_, _ = await(t, closing)
	expectEnd(t, o.h, durable.SessionFailedReason, failure)
	expectReports(t, o.reports, failure)
}

func TestStorageFailureEndsAnAbortJoiningARunThatIgnoresItsSignal(t *testing.T) {
	release, entered := make(chan struct{}), make(chan struct{})
	stubborn := oneStep(t, "test.stubborn", func(context.Context, *stepTask, *TaskRuntime) error {
		close(entered)
		<-release
		return nil
	}, func(context.Context, *TaskRuntime) error { return nil })
	storage := newControlledStorage()
	registry := NewRegistry()
	must(t, registry.Install(&Extension{Name: "tasks", Tasks: []Task{stubborn}}))
	var h *Harness
	var reentered <-chan result[struct{}]
	var once sync.Once
	h, err := Open(bg, storage, Options{Registry: registry, OnReport: func(error) {
		once.Do(func() {
			reentered = async(func() (struct{}, error) { return struct{}{}, h.WaitForIdle(bg) })
		})
	}})
	must(t, err)
	t.Cleanup(func() { closeSoon(t, h) })
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	id := start(t, root, stubborn, false)
	must(t, h.Resume())
	<-entered
	aborting := async(func() (string, error) { return h.AbortTask(bg, id) })
	// The abort mark commits and the abort joins the run, which ignores its
	// signal; then the Session fails.
	eventually(t, func() bool { return getTask(t, h, id).AbortRequested })
	parked(t, 1, "(*scheduler).abort(", "(*Harness).AbortTask(")
	failure := errors.New("disk gone")
	storage.failNextCommit(failure)
	if err := root.Commit(bg, func(tx *durable.Tx) error {
		_, err := tx.AppendEntry(root.ID, durable.EntryDraft{Kind: "note"})
		return err
	}); err != failure {
		t.Fatalf("commit = %v", err)
	}
	_, err = await(t, aborting)
	expectFailed(t, err, failure)
	eventually(t, func() bool { return reentered != nil })
	_, err = await(t, reentered)
	expectFailed(t, err, failure)
	close(release)
}

func TestStorageFailureDoesNotFailItForAReadItsCallerCancelled(t *testing.T) {
	cancelled := errors.New("caller gave up")
	ctx, cancel := context.WithCancelCause(bg)
	storage := &cancelledRead{Storage: memory.New(), ctx: ctx, cancel: cancel, err: cancelled}
	o := openRoot(t, nil, openOptions{storage: storage})
	if _, err := o.root.Context(ctx, nil); err != cancelled {
		t.Fatalf("context = %v", err)
	}
	must(t, appendNote(o))
}

type cancelledRead struct {
	*memory.Storage
	ctx    context.Context
	cancel context.CancelCauseFunc
	err    error
}

func (s *cancelledRead) ScanEntries(ctx context.Context, query durable.EntryQuery, limit int, cursor durable.Cursor) (durable.Page[durable.EntryRecord], error) {
	if ctx == s.ctx {
		s.cancel(s.err)
		return durable.Page[durable.EntryRecord]{}, s.err
	}
	return s.Storage.ScanEntries(ctx, query, limit, cursor)
}

func (*cancelledRead) Close(context.Context) error { return nil }

func TestListenersReportAPanickingCommitOrCloseListenerAndRunTheOthers(t *testing.T) {
	o := openRoot(t, nil, openOptions{storage: memory.New()})
	var seen phaseLog
	for _, listener := range []func(durable.CommitPublication, context.Context){
		func(durable.CommitPublication, context.Context) { panic(errors.New("commit listener")) },
		func(durable.CommitPublication, context.Context) { seen.add("commit") },
	} {
		_, err := o.h.SubscribeCommits(listener)
		must(t, err)
	}
	for _, listener := range []func(){
		func() { panic(errors.New("close listener")) },
		func() { seen.add("close") },
	} {
		_, err := o.h.SubscribeClose(listener)
		must(t, err)
	}
	must(t, appendNote(o))
	must(t, o.h.Close(bg))
	if got := seen.all(); !reflect.DeepEqual(got, []string{"commit", "close"}) {
		t.Fatalf("seen = %v", got)
	}
	expectReports(t, o.reports, errors.New("commit listener"), errors.New("close listener"))
}

func setCompactions(o opened) error {
	return o.root.Commit(bg, func(tx *durable.Tx) error {
		live, err := tx.Doc(LiveDoc.Conversation(o.root.ID))
		if err != nil {
			return err
		}
		return live.Set("compactions", []any{})
	})
}

func TestListenersReportAFailingWatchListenerWhichEndsThatWatch(t *testing.T) {
	o := openRoot(t, nil, openOptions{storage: memory.New()})
	watch, err := o.h.WatchDoc(bg, LiveDoc.Conversation(o.root.ID))
	must(t, err)
	failure := errors.New("listener broke")
	must(t, watch.Start(func(context.Context, map[string]any, []delta.Op) error { return failure }))
	must(t, setCompactions(o))
	<-watch.Closed()
	if end := watch.End(); end.Reason != durable.WatchListenerError || end.Err != failure {
		t.Fatalf("watch end = %+v", end)
	}
	expectReports(t, o.reports, failure)
}

func TestListenersFaultOnlyTheTaskAndEndOnlyTheWatchForAnOddPanicValue(t *testing.T) {
	odd := struct{ x []int }{}
	fails := oneStep(t, "test.throws-unformattable", func(context.Context, *stepTask, *TaskRuntime) error { panic(odd) }, nil)
	o := openRoot(t, []Task{fails}, openOptions{storage: memory.New()})
	watch, err := o.h.WatchDoc(bg, LiveDoc.Conversation(o.root.ID))
	must(t, err)
	must(t, watch.Start(func(context.Context, map[string]any, []delta.Op) error { panic(odd) }))
	id := start(t, o.root, fails, false)
	must(t, o.h.Resume())
	if got := waitDone(t, o.h, id); got.State.Outcome.Status != durable.OutcomeFaulted || got.State.Outcome.Error.Message != "{[]}" {
		t.Fatalf("outcome = %s", outcomeJSON(t, got))
	}
	must(t, setCompactions(o))
	<-watch.Closed()
	if end := watch.End(); end.Reason != durable.WatchListenerError {
		t.Fatalf("watch end = %+v", end)
	}
	must(t, appendNote(o))
}

func TestListenersReportAPanickingClockOnceAlsoWhenTheReportReadsTheClockAgain(t *testing.T) {
	var reports []error
	var clock func() time.Time
	clock = safeNow(func() time.Time { panic(errors.New("clock broke")) }, func(err error) {
		reports = append(reports, err)
		// Reentry, as a handler that commits does: the clock is read again
		// before this report returns.
		if len(reports) < 5 {
			clock()
		}
	})
	if clock().IsZero() {
		t.Fatal("no fallback time")
	}
	clock()
	if got := names(reports, func(err error) string { return err.Error() }); !reflect.DeepEqual(got, []string{"clock broke"}) {
		t.Fatalf("reports = %v", got)
	}
}

func TestListenersKeepWorkingWhenOnReportPanicsAndFallBackWhenTheClockPanics(t *testing.T) {
	var signals phaseLog
	task := waitingTask(t, &signals)
	registry := NewRegistry()
	must(t, registry.Install(&Extension{Name: "tasks", Tasks: []Task{task}}))
	h, err := Open(bg, memory.New(), Options{
		Registry: registry,
		Now:      func() time.Time { panic(errors.New("clock broke")) },
		OnReport: func(error) { panic(errors.New("report broke")) },
	})
	must(t, err)
	t.Cleanup(func() { closeSoon(t, h) })
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	id := start(t, root, task, false)
	must(t, h.Resume())
	eventually(t, func() bool { return getTask(t, h, id).State.Status == durable.TaskRunning })
	if getTask(t, h, id).StartedAt == nil {
		t.Fatal("no start time")
	}
	if _, err := h.AbortTask(bg, id); err != nil {
		t.Fatal(err)
	}
	if got := waitDone(t, h, id); got.State.Outcome.Status != durable.OutcomeAborted {
		t.Fatalf("outcome = %s", outcomeJSON(t, got))
	}
}
