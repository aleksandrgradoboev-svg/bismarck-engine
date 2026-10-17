package harness

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Beyond pi: cases for scheduler gates pi has no test for, found by sabotage.
// A runtime commit needs a running task; work below an owner whose
// cancellation intent has committed does not run on before the cascade
// marks it.

// twice is a one-phase task whose run commits each of steps, then commits
// again in the same phase and records what that last commit returned.
func twice(t *testing.T, steps []func(tx *durable.Tx, rt *TaskRuntime) (*durable.TaskState, error), second *atomic.Pointer[error]) Task {
	t.Helper()
	return MustDefineTask(TaskSpec[any, stepState, any]{
		Name:    "test.twice",
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "run"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"run": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				for _, step := range steps {
					if err := rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
						return step(tx, rt)
					}); err != nil {
						return err
					}
				}
				err := rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return Next(stepState{Phase: "done"})
				})
				second.Store(&err)
				return nil
			},
			"done": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				return rt.Commit(ctx, completeWith("done"))
			},
		},
		Abort: func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error { return rt.Commit(ctx, abortWith("")) },
	})
}

func secondCommit(t *testing.T, second *atomic.Pointer[error]) error {
	t.Helper()
	eventually(t, func() bool { return second.Load() != nil })
	return *second.Load()
}

func TestGateRejectsACommitOfAPhaseAfterItCommittedAWait(t *testing.T) {
	f := newOwnership(t)
	var target durable.TaskID
	var second atomic.Pointer[error]
	waiting := twice(t, []func(*durable.Tx, *TaskRuntime) (*durable.TaskState, error){func(*durable.Tx, *TaskRuntime) (*durable.TaskState, error) {
		return Wait([]durable.TaskID{target}, durable.JoinAllSettled, stepState{Phase: "done"})
	}}, &second)
	o := openRoot(t, []Task{f.hold, waiting}, openOptions{})
	target = f.create(t, o.root, "target")
	id := start(t, o.root, waiting, false)
	must(t, o.h.Resume())
	expectError(t, secondCommit(t, &second), "is waiting")
	// The wait stands: the second commit changed nothing.
	if state := getTask(t, o.h, id).State; state.Status != durable.TaskWaiting || jsonOf(t, state.On) != jsonOf(t, []durable.TaskID{target}) {
		t.Fatalf("state = %s", jsonOf(t, state))
	}
	f.open("target", durable.OutcomeCompleted)
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"completed","result":"done"}` {
		t.Fatalf("outcome = %s", got)
	}
}

func TestGateRejectsACommitOfAPhaseAfterItsOutcomeIsHeldForOwnedWork(t *testing.T) {
	f := newOwnership(t)
	var second atomic.Pointer[error]
	// A task cannot create a child in the commit that ends it: spawn first.
	held := twice(t, []func(*durable.Tx, *TaskRuntime) (*durable.TaskState, error){
		func(tx *durable.Tx, rt *TaskRuntime) (*durable.TaskState, error) {
			_, err := CreateTask(tx, f.hold, holdInput{Name: "child"}, taskOwned(rt.TaskID))
			return nil, err
		},
		func(*durable.Tx, *TaskRuntime) (*durable.TaskState, error) { return Complete("held") },
	}, &second)
	o := openRoot(t, []Task{f.hold, held}, openOptions{})
	id := start(t, o.root, held, false)
	must(t, o.h.Resume())
	expectError(t, secondCommit(t, &second), "is completing")
	// The held outcome stands.
	if got := jsonOf(t, getTask(t, o.h, id).State); got != `{"status":"completing","outcome":{"status":"completed","result":"held"}}` {
		t.Fatalf("state = %s", got)
	}
	f.open("child", durable.OutcomeCompleted)
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"completed","result":"held"}` {
		t.Fatalf("outcome = %s", got)
	}
}

// phasedChild is a two-phase task: "first" commits its progress, reports it
// and returns once returned is closed; "next" records that it ran.
func phasedChild(t *testing.T, committed, returned chan struct{}, ranNext *atomic.Bool) Task {
	t.Helper()
	return MustDefineTask(TaskSpec[any, stepState, any]{
		Name:    "test.phased",
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "first"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"first": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				if err := rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return Next(stepState{Phase: "next"})
				}); err != nil {
					return err
				}
				close(committed)
				<-returned
				return nil
			},
			"next": func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
				ranNext.Store(true)
				return signalled(ctx)
			},
		},
		Abort: func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error { return rt.Commit(ctx, abortWith("")) },
	})
}

func TestGateEndsAPhaseWhoseStepRunsBetweenItsOwnersAbortMarkAndTheCascade(t *testing.T) {
	f := newOwnership(t)
	committed, returned := make(chan struct{}), make(chan struct{})
	var ranNext atomic.Bool
	child := phasedChild(t, committed, returned, &ranNext)
	storage := newControlledStorage()
	o := openRoot(t, []Task{f.hold, child}, openOptions{storage: storage})
	owner := f.create(t, o.root, "owner")
	var childID durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		childID, err = CreateTask[any](tx, child, nil, taskOwned(owner))
		return err
	}))
	must(t, o.h.Resume())
	<-committed
	// Hold the owner's abort mark in storage and queue the child's step
	// behind it: once the mark lands, the step runs before the cascade,
	// which the mark schedules only then.
	held := storage.holdCommits()
	aborting := async(func() (string, error) { return o.h.AbortTask(bg, owner) })
	held.Entered(t)
	close(returned)
	parked(t, 1, onLine, "harness.(*scheduler).step(")
	storage.releaseHold(held)
	if got := ended(t, o.h, childID); got != durable.OutcomeAborted {
		t.Fatalf("child = %s", got)
	}
	if result, err := await(t, aborting); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if ranNext.Load() {
		t.Fatal("the next phase ran below an owner whose abort mark had committed")
	}
}

func TestGateReservesNoWorkBetweenItsOwnersAbortMarkAndTheCascade(t *testing.T) {
	f := newOwnership(t)
	var ran atomic.Bool
	// Not registered yet: the child waits blocked until a registry change
	// wakes a reservation pass.
	late := oneStep(t, "test.late", func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
		ran.Store(true)
		return signalled(ctx)
	}, nil)
	storage := newControlledStorage()
	o := f.openOwned(t, storage)
	owner := f.create(t, o.root, "owner")
	var child durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		child, err = CreateTask[any](tx, late, nil, taskOwned(owner))
		return err
	}))
	eventually(t, func() bool { return f.ran("owner") == 1 })
	// Nothing of the scheduler's own is about to join the line.
	schedulerQuiet(t, o.h)
	// Hold the owner's abort mark in storage and queue a reservation pass
	// behind it: once the mark lands, the pass runs before the cascade,
	// which the mark schedules only then.
	held := storage.holdCommits()
	aborting := async(func() (string, error) { return o.h.AbortTask(bg, owner) })
	held.Entered(t)
	addTask(t, o.registry, late)
	parked(t, 1, onLine, "harness.(*scheduler).reserve(")
	storage.releaseHold(held)
	if got := ended(t, o.h, child); got != durable.OutcomeAborted {
		t.Fatalf("child = %s", got)
	}
	if result, err := await(t, aborting); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	// Reserved, it would have been written running before its mark.
	for _, record := range storage.taskWrites(child) {
		if record.State.Status == durable.TaskRunning && !record.AbortRequested {
			t.Fatal("the child was reserved below an owner whose abort mark had committed")
		}
	}
	if ran.Load() {
		t.Fatal("the child ran below an owner whose abort mark had committed")
	}
}
