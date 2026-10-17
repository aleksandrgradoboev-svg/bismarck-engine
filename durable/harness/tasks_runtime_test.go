package harness

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi harness-tasks.test.ts, "task runtime" and "task scheduling".

func sessionNotes(kind string) *durable.Doc {
	return durable.MustDefineDoc(durable.DocDef{
		Kind:    kind,
		Version: 1,
		Scope:   durable.ScopeSession,
		Initial: func(any) map[string]any { return map[string]any{"text": ""} },
	})
}

func setText(t *testing.T, h *Harness, ref durable.DocRef, text string) {
	t.Helper()
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		doc, err := tx.Doc(ref)
		if err != nil {
			return err
		}
		return doc.Set("text", text)
	}))
}

func TestRuntimeFailsOperationsAfterTheInvocationEndsAndStopsItsWatches(t *testing.T) {
	notes := sessionNotes("test.task-notes")
	absentDoc := sessionNotes("test.task-absent")
	var captured *TaskRuntime
	var handlerCtx context.Context
	var watch, absent *durable.DocumentWatch
	var mu sync.Mutex
	var delivered []string
	watcher := oneStep(t, "test.watcher", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		captured, handlerCtx = rt, ctx
		var err error
		if absent, err = rt.WatchDoc(ctx, absentDoc.Session()); err != nil {
			return err
		}
		if watch, err = rt.WatchDoc(ctx, notes.Session()); err != nil {
			return err
		}
		if err := watch.Start(func(_ context.Context, value map[string]any, _ []delta.Op) error {
			mu.Lock()
			defer mu.Unlock()
			text, _ := value["text"].(string)
			delivered = append(delivered, text)
			return nil
		}); err != nil {
			return err
		}
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	o := openRoot(t, []Task{watcher}, openOptions{})
	setText(t, o.h, notes.Session(), "hello")
	id := start(t, o.root, watcher, false)
	must(t, o.h.Resume())
	waitDone(t, o.h, id)
	if absent != nil {
		t.Fatal("a watch of an absent document")
	}
	// The watch stops when the step after the phase ends the invocation; later
	// commits deliver nothing.
	<-watch.Closed()
	if end := watch.End(); end.Reason != durable.WatchStopped {
		t.Fatalf("watch end = %+v", end)
	}
	setText(t, o.h, notes.Session(), "after")
	flush()
	mu.Lock()
	if len(delivered) != 0 {
		t.Fatalf("delivered = %v", delivered)
	}
	mu.Unlock()
	rt := captured
	expectError(t, rt.Commit(bg, noChange), "invocation has ended")
	_, err := rt.Memo(bg, "x")
	expectError(t, err, "invocation has ended")
	_, err = rt.MemoOr(bg, "x", 1)
	expectError(t, err, "invocation has ended")
	expectError(t, rt.Sleep(bg, time.UnixMilli(0)), "invocation has ended")
	_, err = rt.WatchDoc(bg, notes.Session())
	expectError(t, err, "invocation has ended")
	// The handler's own context is cancelled by now; the ended invocation
	// still wins.
	_, err = rt.Agent(handlerCtx)
	expectError(t, err, "invocation has ended")
	_, err = rt.Now()
	expectError(t, err, "invocation has ended")
	expectError(t, rt.Report(errors.New("late")), "invocation has ended")
}

func TestRuntimeReadsCommittedDocumentsAndContextAndForwardsTheClockAndReports(t *testing.T) {
	notes := durable.MustDefineDoc(durable.DocDef{
		Kind:    "test.runtime-notes",
		Version: 1,
		Scope:   durable.ScopeConversation,
		History: "rewindable",
		Fork:    "asOf",
		Initial: func(any) map[string]any { return map[string]any{"text": ""} },
	})
	var captured *TaskRuntime
	var seen []any
	reader := oneStep(t, "test.reader", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		captured = rt
		view, err := rt.Context(ctx, rt.ConversationID, nil)
		if err != nil {
			return err
		}
		first, second := view.Entries[0].ID, view.Entries[1].ID
		value, err := rt.Snapshot(ctx, notes.Conversation(rt.ConversationID))
		if err != nil {
			return err
		}
		seen = append(seen, value["text"])
		value, err = rt.SnapshotAsOf(ctx, notes.Conversation(rt.ConversationID), first)
		if err != nil {
			return err
		}
		seen = append(seen, value["text"])
		atFirst, err := rt.Context(ctx, rt.ConversationID, &first)
		if err != nil {
			return err
		}
		atSecond, err := rt.Context(ctx, rt.ConversationID, &second)
		if err != nil {
			return err
		}
		now, err := rt.Now()
		if err != nil {
			return err
		}
		seen = append(seen, len(atFirst.Entries), len(atSecond.Messages), now.UnixMilli())
		if err := rt.Report(errors.New("reported")); err != nil {
			return err
		}
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	o := openRoot(t, []Task{reader}, openOptions{now: func() time.Time { return time.UnixMilli(1234) }})
	for _, text := range []string{"one", "two"} {
		must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
			doc, err := tx.Doc(notes.Conversation(o.root.ID))
			if err != nil {
				return err
			}
			if err := doc.Set("text", text); err != nil {
				return err
			}
			_, err = tx.AppendEntry(o.root.ID, durable.EntryDraft{Kind: "message", Model: []ai.Message{user(text)}})
			return err
		}))
	}
	id := start(t, o.root, reader, false)
	must(t, o.h.Resume())
	waitDone(t, o.h, id)
	if want := []any{"two", "one", 1, 2, int64(1234)}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen = %v, want %v", seen, want)
	}
	if got := names(o.reports.all(), func(err error) string { return err.Error() }); !reflect.DeepEqual(got, []string{"reported"}) {
		t.Fatalf("reports = %v", got)
	}
	// The step after the phase ends the invocation.
	var err error
	eventually(t, func() bool {
		_, err = captured.Snapshot(bg, notes.Conversation(o.root.ID))
		return err != nil
	})
	expectError(t, err, "invocation has ended")
	_, err = captured.Context(bg, o.root.ID, nil)
	expectError(t, err, "invocation has ended")
}

// holdLine holds the Session line with a commit stuck in storage and returns
// the gate holding it, once the commit is in storage.
func holdLine(t *testing.T, h *Harness, storage *controlledStorage, conversation durable.ConversationID) *gate {
	t.Helper()
	g := storage.holdCommits()
	go func() {
		_ = h.Commit(bg, func(tx *durable.Tx) error {
			_, err := tx.AppendEntry(conversation, durable.EntryDraft{Kind: "blocker"})
			return err
		})
	}()
	g.Entered(t)
	return g
}

func TestRuntimeOrdersCommitsAgainstTheStepOneQueuedBeforeLandsOneAfterFails(t *testing.T) {
	storage := newControlledStorage()
	var h *Harness
	var before, after <-chan result[struct{}]
	var held *gate
	ready := make(chan struct{})
	commit := func(rt *TaskRuntime, value string) <-chan result[struct{}] {
		return async(func() (struct{}, error) { return struct{}{}, rt.Commit(bg, completeWith(value)) })
	}
	detached := oneStep(t, "test.detached", func(_ context.Context, _ *stepTask, rt *TaskRuntime) error {
		// Hold the line, queue a commit without waiting for it, and return;
		// the step queues behind that commit.
		held = holdLine(t, h, storage, rt.ConversationID)
		before = commit(rt, "before")
		parked(t, 1, onLine, "(*TaskRuntime).Commit(")
		// Queued after the handler returned, so after the step.
		go func() {
			parked(t, 1, onLine, "(*scheduler).step(")
			after = commit(rt, "after")
			close(ready)
		}()
		return nil
	}, nil)
	o := openRoot(t, []Task{detached}, openOptions{storage: storage})
	h = o.h
	id := start(t, o.root, detached, false)
	must(t, o.h.Resume())
	<-ready
	parked(t, 2, onLine, "(*TaskRuntime).Commit(")
	storage.releaseHold(held)
	if _, err := await(t, before); err != nil {
		t.Fatal(err)
	}
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"completed","result":"before"}` {
		t.Fatalf("outcome = %s", got)
	}
	_, err := await(t, after)
	if err == nil || !(contains(err, "invocation has ended") || contains(err, "is terminal")) {
		t.Fatalf("late commit = %v", err)
	}
}

func contains(err error, text string) bool { return err != nil && strings.Contains(err.Error(), text) }

func TestRuntimeStopsAWatchWhoseAcquisitionFinishesAfterTheInvocationEnded(t *testing.T) {
	notes := sessionNotes("test.late-watch")
	storage := newControlledStorage()
	var h *Harness
	var held *gate
	var watching <-chan result[*durable.DocumentWatch]
	ready := make(chan struct{})
	late := oneStep(t, "test.late-watch", func(_ context.Context, _ *stepTask, rt *TaskRuntime) error {
		held = holdLine(t, h, storage, rt.ConversationID)
		// Starts after the handler returned, so its line job queues behind the
		// step that ends the invocation.
		go func() {
			parked(t, 1, onLine, "(*scheduler).step(")
			watching = async(func() (*durable.DocumentWatch, error) { return rt.WatchDoc(bg, notes.Session()) })
			close(ready)
		}()
		return nil
	}, nil)
	o := openRoot(t, []Task{late}, openOptions{storage: storage})
	h = o.h
	setText(t, o.h, notes.Session(), "x")
	id := start(t, o.root, late, false)
	must(t, o.h.Resume())
	<-ready
	parked(t, 1, onLine, "(*TaskRuntime).WatchDoc(")
	storage.releaseHold(held)
	watch, err := await(t, watching)
	expectError(t, err, "invocation has ended")
	if watch != nil {
		t.Fatal("a watch returned after the invocation ended")
	}
	if got := waitDone(t, o.h, id); got.State.Outcome.Status != durable.OutcomeFaulted {
		t.Fatalf("outcome = %s", outcomeJSON(t, got))
	}
}

func TestRuntimeSleepsUntilTheHarnessClockReachesTheDeadline(t *testing.T) {
	var clock atomicMillis
	clock.set(1_000)
	woke := make(chan struct{})
	sleeper := oneStep(t, "test.sleeper", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		if err := rt.Sleep(ctx, time.UnixMilli(900)); err != nil {
			return err
		}
		if err := rt.Sleep(ctx, time.UnixMilli(1_005)); err != nil {
			return err
		}
		close(woke)
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	o := openRoot(t, []Task{sleeper}, openOptions{now: clock.now})
	id := start(t, o.root, sleeper, false)
	must(t, o.h.Resume())
	// The clock stands still, so real timers keep firing without waking the
	// task.
	if settledWithin(woke) {
		t.Fatal("woke before the clock reached the deadline")
	}
	clock.set(1_005)
	waitDone(t, o.h, id)
}

func TestRuntimeFailsASleepWhenTheInvocationIsSignalledOrItsOwnContextCancelled(t *testing.T) {
	var mu sync.Mutex
	var results []string
	note := func(s string) {
		mu.Lock()
		results = append(results, s)
		mu.Unlock()
	}
	sleeping := make(chan struct{})
	signalled := oneStep(t, "test.sleep-signalled", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		close(sleeping)
		err := rt.Sleep(ctx, time.Now().Add(time.Minute))
		note("signalled:" + err.Error())
		return err
	}, nil)
	cancelled := oneStep(t, "test.sleep-cancelled", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		sleepCtx, cancel := context.WithCancelCause(bg)
		done := async(func() (struct{}, error) { return struct{}{}, rt.Sleep(sleepCtx, time.Now().Add(time.Minute)) })
		cancel(errors.New("stop sleeping"))
		_, err := await(t, done)
		note("cancelled:" + err.Error())
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	o := openRoot(t, []Task{signalled, cancelled}, openOptions{})
	signalledID := start(t, o.root, signalled, false)
	cancelledID := start(t, o.root, cancelled, false)
	must(t, o.h.Resume())
	waitDone(t, o.h, cancelledID)
	<-sleeping
	if _, err := o.h.AbortTask(bg, signalledID); err != nil {
		t.Fatal(err)
	}
	if got := outcomeJSON(t, waitDone(t, o.h, signalledID)); got != `{"status":"aborted","reason":"test"}` {
		t.Fatalf("outcome = %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"cancelled:stop sleeping", "signalled:context canceled"}; !reflect.DeepEqual(results, want) {
		t.Fatalf("results = %v, want %v", results, want)
	}
}

func TestSchedulingFailsTheHarnessOnAFailedReservationCommitAndReopeningRunsTheTask(t *testing.T) {
	var runs sync.Map
	count := func() int {
		n, _ := runs.Load("n")
		v, _ := n.(int)
		return v
	}
	once := oneStep(t, "test.once", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		runs.Store("n", count()+1)
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	storage := newControlledStorage()
	o := openRoot(t, []Task{once}, openOptions{storage: storage})
	id := start(t, o.root, once, false)
	// Armed first: waiting asks for progress, which starts scheduling.
	failure := errors.New("disk gone")
	storage.failNextCommit(failure)
	waiting := async(func() (durable.TaskRecord, error) { return o.h.WaitForTask(bg, id) })
	must(t, o.h.Resume())
	<-o.h.Closed()
	if end := o.h.End(); end.Reason != durable.SessionFailedReason || end.Err != failure {
		t.Fatalf("end = %+v", end)
	}
	if got := o.reports.all(); len(got) != 1 || got[0] != failure {
		t.Fatalf("reports = %v", got)
	}
	// The pending wait and every later call fail with the storage error.
	var failed *durable.SessionFailedError
	if _, err := await(t, waiting); !errors.As(err, &failed) || failed.Cause != failure {
		t.Fatalf("wait = %v", err)
	}
	if _, err := o.h.GetTask(bg, id); !errors.As(err, &failed) || failed.Cause != failure {
		t.Fatalf("getTask = %v", err)
	}
	if count() != 0 {
		t.Fatalf("runs = %d", count())
	}

	reopened := openRoot(t, []Task{once}, openOptions{storage: storage.reopen()})
	must(t, reopened.h.Resume())
	if got := waitDone(t, reopened.h, id); got.State.Outcome.Status != durable.OutcomeCompleted || count() != 1 {
		t.Fatalf("outcome = %s, runs = %d", outcomeJSON(t, got), count())
	}
}

func TestSchedulingFailsTheHarnessInsteadOfRunningATaskAgainWhenItsFaultWriteFails(t *testing.T) {
	var mu sync.Mutex
	runs := 0
	storage := newControlledStorage()
	failure := errors.New("disk gone")
	fails := oneStep(t, "test.failed-fault", func(context.Context, *stepTask, *TaskRuntime) error {
		mu.Lock()
		runs++
		first := runs == 1
		mu.Unlock()
		// The next commit is the step's fault write.
		if first {
			storage.failNextCommit(failure)
		}
		return errors.New("boom")
	}, nil)
	o := openRoot(t, []Task{fails}, openOptions{storage: storage})
	id := start(t, o.root, fails, false)
	must(t, o.h.Resume())
	<-o.h.Closed()
	if end := o.h.End(); end.Reason != durable.SessionFailedReason || end.Err != failure || runs != 1 {
		t.Fatalf("end = %+v, runs = %d", end, runs)
	}
	if got := o.reports.all(); len(got) != 1 || got[0] != failure {
		t.Fatalf("reports = %v", got)
	}
	// Reopening recovers the task as after a crash: still running, so its
	// phase runs again.
	reopened := openRoot(t, []Task{fails}, openOptions{storage: storage.reopen()})
	must(t, reopened.h.Resume())
	if got := outcomeJSON(t, waitDone(t, reopened.h, id)); got != `{"status":"faulted","error":{"message":"boom"}}` || runs != 2 {
		t.Fatalf("outcome = %s, runs = %d", got, runs)
	}
}

func TestSchedulingWaitsForIdlenessCountingBlockedWorkAndIgnoringBackgroundTasks(t *testing.T) {
	var mu sync.Mutex
	gates := map[durable.TaskID]chan struct{}{}
	gatedRun := oneStep(t, "test.gated", func(ctx context.Context, task *stepTask, rt *TaskRuntime) error {
		release := make(chan struct{})
		mu.Lock()
		gates[task.Record.ID] = release
		mu.Unlock()
		<-release
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	open := func(id durable.TaskID) {
		mu.Lock()
		close(gates[id])
		mu.Unlock()
	}
	o := openRoot(t, []Task{gatedRun}, openOptions{})
	must(t, o.h.WaitForIdle(bg))
	other, err := o.h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	foreground := start(t, o.root, gatedRun, false)
	background := start(t, o.root, gatedRun, true)
	elsewhere := start(t, other, gatedRun, false)
	// Work that has not started yet is live; cancelling a wait only fails
	// that wait.
	waitCtx, cancel := context.WithCancelCause(bg)
	cancelledWait := async(func() (struct{}, error) { return struct{}{}, o.h.WaitForIdle(waitCtx) })
	cancel(errors.New("stop waiting"))
	_, err = await(t, cancelledWait)
	expectError(t, err, "stop waiting")
	already, cancelAlready := context.WithCancelCause(bg)
	cancelAlready(errors.New("already cancelled"))
	expectError(t, o.root.WaitForIdle(already), "already cancelled")
	must(t, o.h.Resume())
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(gates) == 3
	})
	rootIdle := async(func() (struct{}, error) { return struct{}{}, o.root.WaitForIdle(bg) })
	harnessIdle := async(func() (struct{}, error) { return struct{}{}, o.h.WaitForIdle(bg) })
	open(foreground)
	if _, err := await(t, rootIdle); err != nil {
		t.Fatal(err)
	}
	select {
	case <-harnessIdle:
		t.Fatal("the Harness was idle with work elsewhere")
	case <-time.After(30 * time.Millisecond):
	}
	open(elsewhere)
	if _, err := await(t, harnessIdle); err != nil {
		t.Fatal(err)
	}
	if status := getTask(t, o.h, background).State.Status; status != durable.TaskRunning {
		t.Fatalf("background = %s", status)
	}
	open(background)
	waitDone(t, o.h, background)
	must(t, o.h.Close(bg))
	expectError(t, o.h.WaitForIdle(bg), "closed")
	expectError(t, o.root.WaitForIdle(bg), "closed")
}

// pi #10546.
func TestSchedulingPagesTheNewestTasksFirst(t *testing.T) {
	idle := oneStep(t, "test.idle", func(context.Context, *stepTask, *TaskRuntime) error { return nil }, nil)
	o := openRoot(t, []Task{idle}, openOptions{})
	var ids []durable.TaskID
	for i := 0; i < 5; i++ {
		ids = append(ids, start(t, o.root, idle, false))
	}
	var first, second durable.Page[durable.TaskRecord]
	must(t, o.h.Commit(bg, func(tx *durable.Tx) error {
		var err error
		first, err = tx.ScanTasks(durable.TaskQuery{Order: durable.ScanOrder("descending")}, 2, nil)
		return err
	}))
	must(t, o.h.Commit(bg, func(tx *durable.Tx) error {
		var err error
		second, err = tx.ScanTasks(durable.TaskQuery{}, 2, first.Next)
		return err
	}))
	taskIDs := func(page durable.Page[durable.TaskRecord]) []durable.TaskID {
		return names(page.Items, func(r durable.TaskRecord) durable.TaskID { return r.ID })
	}
	if !reflect.DeepEqual(taskIDs(first), []durable.TaskID{ids[4], ids[3]}) || !reflect.DeepEqual(taskIDs(second), []durable.TaskID{ids[2], ids[1]}) {
		t.Fatalf("pages = %v, %v", taskIDs(first), taskIDs(second))
	}
}

func TestSchedulingRejectsUnknownTasksAndReportsTerminalOnes(t *testing.T) {
	done := oneStep(t, "test.quick", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	o := openRoot(t, []Task{done}, openOptions{})
	const unknown = durable.TaskID(999_999)
	if getTask(t, o.h, unknown) != nil {
		t.Fatal("an unknown task")
	}
	_, err := o.h.WaitForTask(bg, unknown)
	expectError(t, err, "does not exist")
	_, err = o.h.AbortTask(bg, unknown)
	expectError(t, err, "does not exist")
	id := start(t, o.root, done, false)
	must(t, o.h.Resume())
	waitDone(t, o.h, id)
	if got := waitDone(t, o.h, id); got.State.Status != durable.TaskTerminal {
		t.Fatalf("status = %s", got.State.Status)
	}
	if result, err := o.h.AbortTask(bg, id); err != nil || result != "terminal" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	must(t, o.h.Close(bg))
	expectError(t, o.h.Resume(), "closed")
}

func TestSchedulingFailsTaskWaitsCancelledOrClosedWhileQueuedAndPendingWaitsOnClose(t *testing.T) {
	release := make(chan struct{})
	blocking := gatedTask(t, "test.wait-close", release)
	storage := newControlledStorage()
	o := openRoot(t, []Task{blocking}, openOptions{storage: storage})
	id := start(t, o.root, blocking, false)

	// Hold the line with a commit, then queue waits behind it.
	held := holdLine(t, o.h, storage, o.root.ID)
	waitCtx, cancel := context.WithCancelCause(bg)
	cancelledWhileQueued := async(func() (durable.TaskRecord, error) { return o.h.WaitForTask(waitCtx, id) })
	parked(t, 1, onLine, "(*Harness).WaitForTask(")
	cancel(errors.New("wait cancelled"))
	storage.releaseHold(held)
	_, err := await(t, cancelledWhileQueued)
	expectError(t, err, "wait cancelled")

	pending := async(func() (durable.TaskRecord, error) { return o.h.WaitForTask(bg, id) })
	idle := async(func() (struct{}, error) { return struct{}{}, o.h.WaitForIdle(bg) })
	conversationIdle := async(func() (struct{}, error) { return struct{}{}, o.root.WaitForIdle(bg) })
	parked(t, 1, onWaiter, "(*Harness).WaitForTask(")
	parked(t, 1, onWaiter, "(*Harness).WaitForIdle(")
	parked(t, 1, onWaiter, "(*Conversation).WaitForIdle(")
	heldAgain := holdLine(t, o.h, storage, o.root.ID)
	closedWhileQueued := async(func() (durable.TaskRecord, error) { return o.h.WaitForTask(bg, id) })
	parked(t, 1, onLine, "(*Harness).WaitForTask(")
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	eventually(t, func() bool { return o.h.tasks.closing.Load() })
	storage.releaseHold(heldAgain)
	for _, ch := range []<-chan result[durable.TaskRecord]{closedWhileQueued, pending} {
		_, err := await(t, ch)
		expectError(t, err, "closed")
	}
	for _, ch := range []<-chan result[struct{}]{idle, conversationIdle} {
		_, err := await(t, ch)
		expectError(t, err, "closed")
	}
	close(release)
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
}
