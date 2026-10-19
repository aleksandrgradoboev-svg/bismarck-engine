package harness

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// pi harness-tasks.test.ts, "task abort" and "task close".

// markDurably starts AbortTask and returns once its mark is durable, before
// it has joined the run.
func markDurably(t *testing.T, h *Harness, id durable.TaskID) <-chan result[string] {
	t.Helper()
	aborting := async(func() (string, error) { return h.AbortTask(bg, id) })
	eventually(t, func() bool { return getTask(t, h, id).AbortRequested })
	return aborting
}

func twoPhases(t *testing.T, name string, one, two func(ctx context.Context, rt *TaskRuntime) error, abort func(ctx context.Context, rt *TaskRuntime) error) Task {
	t.Helper()
	return MustDefineTask(TaskSpec[any, stepState, any]{
		Name:    name,
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "one"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"one": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error { return one(ctx, rt) },
			"two": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error { return two(ctx, rt) },
		},
		Abort: func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error { return abort(ctx, rt) },
	})
}

func toPhaseTwo(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
	return Next(stepState{Phase: "two"})
}

// phaseLog is a goroutine-safe log of phase names.
type phaseLog struct {
	mu  sync.Mutex
	log []string
}

func (l *phaseLog) add(s string) {
	l.mu.Lock()
	l.log = append(l.log, s)
	l.mu.Unlock()
}

func (l *phaseLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.log...)
}

func TestAbortFailsTheRunsCommitsAfterTheMarkAndSettlesThroughAFreshAbortInvocation(t *testing.T) {
	reached := make(chan struct{})
	var errs phaseLog
	var memoRead string
	var abortRuntimes []*TaskRuntime
	var runRuntime *TaskRuntime
	marked := oneStep(t, "test.marked", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		runRuntime = rt
		if _, err := rt.MemoOr(ctx, "kept", 1); err != nil {
			return err
		}
		close(reached)
		// Keep working after the signal: every later write of this run must
		// fail.
		<-ctx.Done()
		memo, _ := rt.Memo(bg, "kept")
		memoRead = string(memo)
		if _, err := rt.MemoOr(bg, "late", 2); err != nil {
			errs.add(err.Error())
		}
		if err := rt.Commit(bg, completeWith(nil)); err != nil {
			errs.add(err.Error())
		}
		return nil
	}, func(ctx context.Context, rt *TaskRuntime) error {
		abortRuntimes = append(abortRuntimes, rt)
		return rt.Commit(ctx, func(_ *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
			if !current.AbortRequested || len(current.Memos) != 1 || string(current.Memos["kept"]) != "1" {
				t.Errorf("abort sees %+v", current)
			}
			return Abort("mark"), nil
		})
	})
	o := openRoot(t, []Task{marked}, openOptions{})
	id := start(t, o.root, marked, false)
	must(t, o.h.Resume())
	<-reached
	if result, err := o.h.AbortTask(bg, id); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"aborted","reason":"mark"}` {
		t.Fatalf("outcome = %s", got)
	}
	want := fmt.Sprintf("Task %d has a durable abort mark", id)
	if memoRead != "1" || !reflect.DeepEqual(errs.all(), []string{want, want}) {
		t.Fatalf("memo = %s, errors = %v", memoRead, errs.all())
	}
	if len(abortRuntimes) != 1 || abortRuntimes[0] == runRuntime {
		t.Fatalf("abort runtimes = %v", abortRuntimes)
	}
}

func TestAbortStartsNoFurtherPhaseAfterAMarkThatLandsDuringAPhaseWithProgress(t *testing.T) {
	reached, proceed := make(chan struct{}), make(chan struct{})
	var phases phaseLog
	two := twoPhases(t, "test.mark-boundary", func(ctx context.Context, rt *TaskRuntime) error {
		phases.add("one")
		if err := rt.Commit(ctx, toPhaseTwo); err != nil {
			return err
		}
		close(reached)
		// Ignores the signal and returns normally after the mark.
		<-proceed
		return nil
	}, func(context.Context, *TaskRuntime) error {
		phases.add("two")
		return nil
	}, func(ctx context.Context, rt *TaskRuntime) error {
		phases.add("abort")
		return rt.Commit(ctx, abortWith("boundary"))
	})
	o := openRoot(t, []Task{two}, openOptions{})
	id := start(t, o.root, two, false)
	must(t, o.h.Resume())
	<-reached
	aborting := markDurably(t, o.h, id)
	close(proceed)
	if result, err := await(t, aborting); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"aborted","reason":"boundary"}` {
		t.Fatalf("outcome = %s", got)
	}
	if got := phases.all(); !reflect.DeepEqual(got, []string{"one", "abort"}) {
		t.Fatalf("phases = %v", got)
	}
}

func TestAbortSignalsAndJoinsTheRunBeforeReturningThenRunsTheAbortHandler(t *testing.T) {
	reached, again := make(chan struct{}), make(chan struct{})
	var runEnded bool
	signalled := oneStep(t, "test.signalled", func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
		defer func() { runEnded = true }()
		close(reached)
		<-ctx.Done()
		return context.Cause(ctx)
	}, func(ctx context.Context, rt *TaskRuntime) error {
		// Held until the second abort: in pi that call runs before the abort
		// invocation the first one scheduled.
		<-again
		return rt.Commit(ctx, abortWith("test"))
	})
	o := openRoot(t, []Task{signalled}, openOptions{})
	id := start(t, o.root, signalled, false)
	must(t, o.h.Resume())
	<-reached
	if result, err := o.h.AbortTask(bg, id); err != nil || result != "marked" || !runEnded {
		t.Fatalf("abort = %s, %v, run ended %v", result, err, runEnded)
	}
	if result, err := o.h.AbortTask(bg, id); err != nil || result != "marked" {
		t.Fatalf("second abort = %s, %v", result, err)
	}
	close(again)
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"aborted","reason":"test"}` {
		t.Fatalf("outcome = %s", got)
	}
}

func TestAbortAbortsWaitingWorkAndFaultsAbortHandlersThatFailOrSettleNothing(t *testing.T) {
	release := make(chan struct{})
	first := gatedTask(t, "test.dependency", release)
	var firstID durable.TaskID
	wait := func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
			return Wait([]durable.TaskID{firstID}, durable.JoinAllSettled, stepState{Phase: "run"})
		})
	}
	lazy := oneStep(t, "test.lazy-abort", wait, func(context.Context, *TaskRuntime) error { return nil })
	failing := oneStep(t, "test.throwing-abort", wait, func(context.Context, *TaskRuntime) error { return errors.New("abort failed") })
	o := openRoot(t, []Task{first, lazy, failing}, openOptions{})
	firstID = start(t, o.root, first, false)
	lazyID := start(t, o.root, lazy, false)
	failingID := start(t, o.root, failing, false)
	must(t, o.h.Resume())
	for _, id := range []durable.TaskID{failingID, lazyID} {
		eventually(t, func() bool { return getTask(t, o.h, id).State.Status == durable.TaskWaiting })
	}
	for _, id := range []durable.TaskID{lazyID, failingID} {
		if _, err := o.h.AbortTask(bg, id); err != nil {
			t.Fatal(err)
		}
	}
	if got := outcomeJSON(t, waitDone(t, o.h, lazyID)); got != fmt.Sprintf(`{"status":"faulted","error":{"message":"Abort handler of task %d returned without a terminal outcome"}}`, lazyID) {
		t.Fatalf("lazy outcome = %s", got)
	}
	if got := outcomeJSON(t, waitDone(t, o.h, failingID)); got != `{"status":"faulted","error":{"message":"abort failed"}}` {
		t.Fatalf("failing outcome = %s", got)
	}
	if status := getTask(t, o.h, firstID).State.Status; status != durable.TaskRunning {
		t.Fatalf("first = %s", status)
	}
	close(release)
	waitDone(t, o.h, firstID)
}

func TestAbortDoesNotSignalARunningAbortHandlerAndACancelledCallerLeavesTheMarkDurable(t *testing.T) {
	reached, proceed, runRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	entered := make(chan struct{})
	var abortSignalled bool
	var aborts int
	run := oneStep(t, "test.abort-again", func(context.Context, *stepTask, *TaskRuntime) error {
		close(entered)
		// Ignores the signal, so the first caller is still joining when it
		// gives up.
		<-runRelease
		return nil
	}, func(ctx context.Context, rt *TaskRuntime) error {
		aborts++
		close(reached)
		<-proceed
		abortSignalled = ctx.Err() != nil
		return rt.Commit(ctx, abortWith("once"))
	})
	o := openRoot(t, []Task{run}, openOptions{})
	id := start(t, o.root, run, false)
	must(t, o.h.Resume())
	// The handler itself, not the running record: the reservation writes that,
	// and an abort mark that lands before the run's first step ends the run
	// there, so the caller would join a run that already ended.
	<-entered
	// This caller gives up while joining; the mark and the abort invocation
	// are unaffected.
	callerCtx, giveUp := context.WithCancelCause(bg)
	cancelled := async(func() (string, error) { return o.h.AbortTask(callerCtx, id) })
	eventually(t, func() bool { return getTask(t, o.h, id).AbortRequested })
	giveUp(errors.New("caller gave up"))
	_, err := await(t, cancelled)
	expectError(t, err, "caller gave up")
	close(runRelease)
	<-reached
	if result, err := o.h.AbortTask(bg, id); err != nil || result != "marked" {
		t.Fatalf("abort again = %s, %v", result, err)
	}
	close(proceed)
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"aborted","reason":"once"}` || abortSignalled || aborts != 1 {
		t.Fatalf("outcome = %s, signalled %v, aborts %d", got, abortSignalled, aborts)
	}
}

func TestAbortKeepsATerminalOutcomeCommittedByAnAbortHandlerThatFailsAfterwards(t *testing.T) {
	run := oneStep(t, "test.abort-then-throw", func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}, func(ctx context.Context, rt *TaskRuntime) error {
		if err := rt.Commit(ctx, abortWith("done")); err != nil {
			return err
		}
		return errors.New("after terminal")
	})
	o := openRoot(t, []Task{run}, openOptions{})
	id := start(t, o.root, run, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return getTask(t, o.h, id).State.Status == durable.TaskRunning })
	if _, err := o.h.AbortTask(bg, id); err != nil {
		t.Fatal(err)
	}
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"aborted","reason":"done"}` {
		t.Fatalf("outcome = %s", got)
	}
}

func TestAbortNeverStartsPhaseOneWhenTheAbortLandsWhileTheReservationSettles(t *testing.T) {
	var ran bool
	reserved := oneStep(t, "test.reserved", func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
		ran = true
		<-ctx.Done()
		return context.Cause(ctx)
	}, nil)
	storage := newControlledStorage()
	o := openRoot(t, []Task{reserved}, openOptions{storage: storage})
	id := start(t, o.root, reserved, false)
	held := storage.holdCommits()
	must(t, o.h.Resume())
	held.Entered(t)
	// The reservation commit is in storage; the abort mark commit queues
	// behind it.
	aborting := async(func() (string, error) { return o.h.AbortTask(bg, id) })
	parked(t, 1, onLine, "(*Harness).AbortTask(")
	storage.releaseHold(held)
	if result, err := await(t, aborting); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"aborted","reason":"test"}` || ran {
		t.Fatalf("outcome = %s, ran %v", got, ran)
	}
}

func TestAbortLetsAnAbortMarkWinOverAFaultThatRacesIt(t *testing.T) {
	storage := newControlledStorage()
	var h *Harness
	var marking <-chan result[string]
	var held *gate
	ready := make(chan struct{})
	racing := oneStep(t, "test.racing-fault", func(_ context.Context, task *stepTask, _ *TaskRuntime) error {
		held = storage.holdCommits()
		marking = async(func() (string, error) { return h.AbortTask(bg, task.Record.ID) })
		held.Entered(t)
		close(ready)
		// The mark is in storage but not committed when the handler fails, so
		// the fault commit queues behind it.
		return errors.New("would fault")
	}, nil)
	o := openRoot(t, []Task{racing}, openOptions{storage: storage})
	h = o.h
	id := start(t, o.root, racing, false)
	must(t, o.h.Resume())
	<-ready
	parked(t, 1, onLine, "(*scheduler).step(")
	storage.releaseHold(held)
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"aborted","reason":"test"}` {
		t.Fatalf("outcome = %s", got)
	}
	if result, err := await(t, marking); err != nil || result != "marked" {
		t.Fatalf("marking = %s, %v", result, err)
	}
}

func TestCloseStopsWithoutOutcomesAndStartsNoFreshPhaseOrAbortInvocation(t *testing.T) {
	reached, proceed := make(chan struct{}), make(chan struct{})
	var phases phaseLog
	var abortRan bool
	two := twoPhases(t, "test.two", func(ctx context.Context, rt *TaskRuntime) error {
		phases.add("one")
		if err := rt.Commit(ctx, toPhaseTwo); err != nil {
			return err
		}
		close(reached)
		// Ignores signals and returns normally once released; the closing rule
		// wins over the next phase.
		<-proceed
		return nil
	}, func(context.Context, *TaskRuntime) error {
		phases.add("two")
		return nil
	}, func(context.Context, *TaskRuntime) error {
		abortRan = true
		return nil
	})
	storage := newControlledStorage()
	o := openRoot(t, []Task{two}, openOptions{storage: storage})
	id := start(t, o.root, two, false)
	must(t, o.h.Resume())
	<-reached
	aborting := markDurably(t, o.h, id)
	record := getTask(t, o.h, id)
	commits := storage.commitCount()
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	eventually(t, func() bool { return o.h.tasks.closing.Load() })
	close(proceed)
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
	if result, err := await(t, aborting); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	// No outcome: close writes only the stopped task back as pending, so a
	// reopen counts no restart.
	if storage.commitCount() != commits+1 || !reflect.DeepEqual(phases.all(), []string{"one"}) || abortRan {
		t.Fatalf("commits %d -> %d, phases %v, abort ran %v", commits, storage.commitCount(), phases.all(), abortRan)
	}
	if last := storage.lastTaskWrites(); len(last) != 1 || last[0].ID != id || !last[0].AbortRequested || last[0].State.Status != durable.TaskPending || string(last[0].State.Checkpoint) != `{"phase":"two"}` {
		t.Fatalf("last commit = %+v", last)
	}
	if !record.AbortRequested || record.State.Status != durable.TaskRunning || string(record.State.Checkpoint) != `{"phase":"two"}` {
		t.Fatalf("record = %+v", record)
	}
	expectError(t, o.h.Commit(bg, func(*durable.Tx) error { return nil }), "closed")
}

func TestCloseSealsAdmissionBeforeSignallingHandlersAndStopsWatchesBeforeJoining(t *testing.T) {
	notes := sessionNotes("test.close-notes")
	reached := make(chan struct{})
	var h *Harness
	fromListener := make(chan error, 1)
	var watchEnd durable.WatchEnd
	stubborn := oneStep(t, "test.stubborn", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		// Uses a context close does not cancel, then waits for the watch to
		// close.
		watch, err := rt.WatchDoc(bg, notes.Session())
		if err != nil {
			return err
		}
		go func() {
			<-ctx.Done()
			fromListener <- h.Commit(bg, func(*durable.Tx) error { return nil })
		}()
		close(reached)
		<-watch.Closed()
		watchEnd = watch.End()
		return nil
	}, nil)
	o := openRoot(t, []Task{stubborn}, openOptions{})
	h = o.h
	setText(t, o.h, notes.Session(), "x")
	start(t, o.root, stubborn, false)
	must(t, o.h.Resume())
	<-reached
	must(t, o.h.Close(bg))
	expectError(t, <-fromListener, "closed")
	if watchEnd.Reason != durable.WatchSessionClosed {
		t.Fatalf("watch end = %+v", watchEnd)
	}
}

func TestCloseWritesNoFaultWhenItSealsWhileTheStepAfterAFailedPhaseIsQueued(t *testing.T) {
	storage := newControlledStorage()
	var h *Harness
	var held *gate
	ready := make(chan struct{})
	fails := oneStep(t, "test.close-fault", func(_ context.Context, _ *stepTask, rt *TaskRuntime) error {
		held = holdLine(t, h, storage, rt.ConversationID)
		close(ready)
		return errors.New("would fault")
	}, nil)
	o := openRoot(t, []Task{fails}, openOptions{storage: storage})
	h = o.h
	id := start(t, o.root, fails, false)
	must(t, o.h.Resume())
	<-ready
	parked(t, 1, onLine, "(*scheduler).step(")
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	eventually(t, func() bool { return o.h.tasks.closing.Load() })
	storage.releaseHold(held)
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
	// Created, reserved, and written back as pending by close: no fault.
	if got := names(storage.taskWrites(id), func(r durable.TaskRecord) string { return r.State.Status }); !reflect.DeepEqual(got, []string{"pending", "running", "pending"}) {
		t.Fatalf("task writes = %v", got)
	}
}

func TestCloseStartsNoAbortHandlerWhoseReservationSettlesWhileClosing(t *testing.T) {
	var ran bool
	marked := oneStep(t, "test.close-abort-reservation", func(context.Context, *stepTask, *TaskRuntime) error { return nil },
		func(context.Context, *TaskRuntime) error {
			ran = true
			return nil
		})
	storage := newControlledStorage()
	o := openRoot(t, []Task{marked}, openOptions{storage: storage})
	id := start(t, o.root, marked, false)
	if result, err := o.h.AbortTask(bg, id); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	held := storage.holdCommits()
	must(t, o.h.Resume())
	held.Entered(t)
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	eventually(t, func() bool { return o.h.tasks.closing.Load() })
	storage.releaseHold(held)
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
	// The reservation landed; close wrote the task back as pending.
	reserved, last := storage.taskWritesBack(2), storage.lastTaskWrites()
	if ran || len(reserved) != 1 || reserved[0].ID != id || !reserved[0].AbortRequested || reserved[0].State.Status != durable.TaskRunning {
		t.Fatalf("ran %v, reservation %+v", ran, reserved)
	}
	if len(last) != 1 || last[0].ID != id || !last[0].AbortRequested || last[0].State.Status != durable.TaskPending {
		t.Fatalf("last commit = %+v", last)
	}
}

func TestCloseFailsARuntimeCommitThatWasQueuedOnTheLineWhenCloseSealedIt(t *testing.T) {
	storage := newControlledStorage()
	var h *Harness
	var held *gate
	var queued <-chan result[struct{}]
	ready, proceed := make(chan struct{}), make(chan struct{})
	q := oneStep(t, "test.close-queued-commit", func(_ context.Context, _ *stepTask, rt *TaskRuntime) error {
		held = holdLine(t, h, storage, rt.ConversationID)
		queued = async(func() (struct{}, error) { return struct{}{}, rt.Commit(bg, completeWith(nil)) })
		parked(t, 1, onLine, "(*TaskRuntime).Commit(")
		close(ready)
		// Ignores the close signal, so the invocation is still alive when the
		// queued commit reaches the line.
		<-proceed
		return nil
	}, nil)
	o := openRoot(t, []Task{q}, openOptions{storage: storage})
	h = o.h
	id := start(t, o.root, q, false)
	must(t, o.h.Resume())
	<-ready
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	eventually(t, func() bool { return o.h.tasks.closing.Load() })
	storage.releaseHold(held)
	_, err := await(t, queued)
	expectError(t, err, "Harness is closed")
	close(proceed)
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
	// Created, reserved, and written back as pending by close; the queued
	// outcome never landed.
	if got := names(storage.taskWrites(id), func(r durable.TaskRecord) string { return r.State.Status }); !reflect.DeepEqual(got, []string{"pending", "running", "pending"}) {
		t.Fatalf("task writes = %v", got)
	}
}

// closingReader is a registry reader whose snapshot can close the Harness, as
// a step refreshes the snapshot inside its line callback after its closing
// check.
type closingReader struct {
	Registry
	mu      sync.Mutex
	h       *Harness
	armed   bool
	closing chan error
}

func (r *closingReader) Snapshot() *RegistrySnapshot {
	r.mu.Lock()
	armed := r.armed
	r.armed = false
	r.mu.Unlock()
	if armed {
		// pi's close seals synchronously inside the snapshot; Go's close waits
		// for the line this runs on, so it runs on its own and the snapshot
		// waits for the seal.
		go func() { r.closing <- r.h.Close(bg) }()
		for !r.h.tasks.closing.Load() {
			time.Sleep(time.Millisecond)
		}
	}
	return r.Registry.Snapshot()
}

func TestCloseStartsNoNextPhaseWhenItSealsDuringAStepThatDecidedToContinue(t *testing.T) {
	var phases phaseLog
	reader := &closingReader{Registry: NewRegistry(), closing: make(chan error, 1)}
	two := twoPhases(t, "test.close-in-step", func(ctx context.Context, rt *TaskRuntime) error {
		phases.add("one")
		if err := rt.Commit(ctx, toPhaseTwo); err != nil {
			return err
		}
		reader.mu.Lock()
		reader.armed = true
		reader.mu.Unlock()
		return nil
	}, func(context.Context, *TaskRuntime) error {
		phases.add("two")
		return nil
	}, func(context.Context, *TaskRuntime) error { return nil })
	must(t, reader.Install(&Extension{Name: "task:test.close-in-step", Tasks: []Task{two}}))
	h, err := Open(bg, memory.New(), Options{Registry: reader})
	must(t, err)
	reader.h = h
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	start(t, root, two, false)
	must(t, h.Resume())
	select {
	case err := <-reader.closing:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("close did not finish")
	}
	if got := phases.all(); !reflect.DeepEqual(got, []string{"one"}) {
		t.Fatalf("phases = %v", got)
	}
}

func TestCloseJoinsAReservationThatSettlesWhileClosingWithoutStartingItsHandler(t *testing.T) {
	var ran bool
	never := oneStep(t, "test.close-reservation", func(context.Context, *stepTask, *TaskRuntime) error {
		ran = true
		return nil
	}, nil)
	storage := newControlledStorage()
	o := openRoot(t, []Task{never}, openOptions{storage: storage})
	id := start(t, o.root, never, false)
	held := storage.holdCommits()
	must(t, o.h.Resume())
	held.Entered(t)
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	eventually(t, func() bool { return o.h.tasks.closing.Load() })
	storage.releaseHold(held)
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
	// The reservation landed; close wrote the task back as pending.
	reserved, last := storage.taskWritesBack(2), storage.lastTaskWrites()
	if ran || len(reserved) != 1 || reserved[0].ID != id || reserved[0].State.Status != durable.TaskRunning {
		t.Fatalf("ran %v, reservation %+v", ran, reserved)
	}
	if len(last) != 1 || last[0].ID != id || last[0].State.Status != durable.TaskPending {
		t.Fatalf("last commit = %+v", last)
	}
}
