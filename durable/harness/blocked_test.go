package harness

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi test/harness-blocked.test.ts. A crash is crashable's dead process,
// whose commits reach no record, so not even close's pending write-back
// lands; a close and reopen is JSONL storage over one directory, as in
// tasks recovery.

// restartLimit is a host setting tests change while the Harness runs.
func restartLimit(limit *atomic.Int64) func() HarnessSettings {
	return func() HarnessSettings {
		n := int(limit.Load())
		return HarnessSettings{RestartLimit: &n}
	}
}

func fixedLimit(n int64) func() HarnessSettings {
	var limit atomic.Int64
	limit.Store(n)
	return restartLimit(&limit)
}

// loopState drives crashLoop: while crashing, its phase hangs until its
// invocation ends, as one killed every time would.
type loopState struct {
	crashing atomic.Bool
	started  atomic.Int32
}

func crashLoop(t *testing.T, state *loopState, abort func(context.Context, *TaskRuntime) error) Task {
	if abort == nil {
		abort = func(context.Context, *TaskRuntime) error { return nil }
	}
	return oneStep(t, "test.crash-loop", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		state.started.Add(1)
		if state.crashing.Load() {
			<-ctx.Done()
			return context.Cause(ctx)
		}
		return rt.Commit(ctx, completeWith(nil))
	}, abort)
}

// firstProcess opens the first Harness of a crashable.
func firstProcess(t *testing.T, c *crashable, options openOptions) opened {
	t.Helper()
	c.current = &process{controlledStorage: c.controlledStorage}
	options.storage = c.current
	return openRoot(t, nil, options)
}

func registryWith(t *testing.T, name string, tasks ...Task) Registry {
	t.Helper()
	registry := NewRegistry()
	must(t, registry.Install(&Extension{Name: name, Tasks: tasks}))
	return registry
}

func blockedOf(t *testing.T, graph durable.DocumentState, id durable.TaskID) string {
	t.Helper()
	return decodeGraph(t, graph.Value()).Tasks[id].Blocked
}

func inspectedState(t *testing.T, h *Harness, id durable.TaskID) TaskInspection {
	t.Helper()
	inspection, err := h.Inspect(bg)
	must(t, err)
	for _, task := range inspection.Tasks {
		if task.Record.ID == id {
			return task
		}
	}
	t.Fatalf("task %d is not live", id)
	return TaskInspection{}
}

func TestBlockedKeepsTheHostsWakeActiveUntilOpensRecoveryPassHasLanded(t *testing.T) {
	held := oneStep(t, "test.held", func(context.Context, *stepTask, *TaskRuntime) error { return nil }, nil)
	registry := registryWith(t, "held", held)
	storage := newControlledStorage()
	first := openRoot(t, nil, openOptions{storage: storage, registry: registry})
	id := start(t, first.root, held, false)
	must(t, first.h.Close(bg))
	// A crash left the task holding its outcome: the next open's recovery
	// pass makes it terminal.
	storage = storage.reopen()
	record, err := storage.Task(bg, id)
	must(t, err)
	completing := record.Clone()
	completing.Memos = nil
	completing.State = durable.TaskState{Status: durable.TaskCompleting, Outcome: &durable.TaskOutcome{Status: durable.OutcomeCompleted, Result: json.RawMessage(`null`)}}
	_, err = storage.Commit(bg, []durable.StorageWrite{durable.TaskWrite{Value: completing}})
	must(t, err)

	// That pass's commit fails, as on a platform reset.
	storage.failNextCommit(errors.New("reset"))
	wakes := &wakeLog{}
	h, err := Open(bg, storage, Options{Registry: registry, OnWake: wakes.add})
	must(t, err)
	<-h.Closed()
	if reason := h.End().Reason; reason != durable.SessionFailedReason {
		t.Fatalf("end = %s", reason)
	}
	settleTimers()
	// The host was told work is underway, and never that it ended: its
	// recovery alarm stays.
	if all := wakes.all(); len(all) != 1 || !isWake(all[0], true, -1) {
		t.Fatalf("wakes = %+v", all)
	}
}

func TestBlockedParksATaskCutOffTheRestartLimitInARowShowsWhyAndRunsItAgainOnRetryTask(t *testing.T) {
	c := newCrashable()
	state := &loopState{}
	state.crashing.Store(true)
	loop := crashLoop(t, state, nil)
	options := openOptions{registry: registryWith(t, "loop", loop), settings: fixedLimit(3)}
	o := firstProcess(t, c, options)
	root := o.root.ID
	id := start(t, o.root, loop, false)
	// Each open runs the phase, and each crash cuts it off: three restarts.
	for run := int32(1); run <= 3; run++ {
		must(t, o.h.Resume())
		eventually(t, func() bool { return state.started.Load() == run })
		o = c.crashWith(t, o, nil, options)
	}
	if got := getTask(t, o.h, id).Restarts; got != 3 {
		t.Fatalf("restarts = %d", got)
	}
	infos := o.reports.allInfos()
	want := durable.ReportTask{ID: id, Kind: "test.crash-loop", ConversationID: root}
	if len(infos) != 1 || infos[0].Type != durable.ReportTaskParked || infos[0].Task == nil || *infos[0].Task != want || infos[0].Extension != "loop" {
		t.Fatalf("reports = %+v", infos)
	}
	if got := inspectedState(t, o.h, id); got.Kind != "blocked" || got.Reason != "restart_limit" {
		t.Fatalf("inspection = %+v", got)
	}
	graph, err := o.h.TaskGraph(bg)
	must(t, err)
	defer func() { graph.Dispose() }()
	if got := blockedOf(t, graph, id); got != "restart_limit" {
		t.Fatalf("graph blocked = %q", got)
	}
	// Parked work needs no process: nothing is active.
	eventually(t, func() bool { return !wakeOf(t, o.h).Active })
	if w := wakeOf(t, o.h); !isWake(w, false, -1) {
		t.Fatalf("wake = %+v", w)
	}
	must(t, o.h.Resume())
	// A scheduling pass runs within a few turns; it leaves the parked task
	// out.
	schedulerQuiet(t, o.h)
	settleTimers()
	if got := state.started.Load(); got != 3 {
		t.Fatalf("started %d times", got)
	}

	state.crashing.Store(false)
	must(t, o.h.RetryTask(bg, id))
	if got := waitDone(t, o.h, id).State.Outcome.Status; got != durable.OutcomeCompleted {
		t.Fatalf("outcome = %s", got)
	}
	if got := state.started.Load(); got != 4 {
		t.Fatalf("started %d times", got)
	}
}

func TestBlockedCountsNoRestartForATaskACloseStopped(t *testing.T) {
	directory := t.TempDir()
	state := &loopState{}
	state.crashing.Store(true)
	loop := crashLoop(t, state, nil)
	registry := registryWith(t, "loop", loop)
	open := func() opened {
		return openRoot(t, nil, openOptions{storage: openJSONL(t, directory), registry: registry, settings: fixedLimit(1)})
	}
	o := open()
	id := start(t, o.root, loop, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return state.started.Load() == 1 })
	must(t, o.h.Close(bg))
	o = open()
	if record := getTask(t, o.h, id); record.State.Status != durable.TaskPending || record.Restarts != 0 {
		t.Fatalf("record = %s", jsonOf(t, record))
	}
	state.crashing.Store(false)
	if got := waitDone(t, o.h, id).State.Outcome.Status; got != durable.OutcomeCompleted {
		t.Fatalf("outcome = %s", got)
	}
}

func TestBlockedKeepsAnAbortMarkAdmittedBeforeCloseInTheRecordCloseWritesBack(t *testing.T) {
	directory := t.TempDir()
	state := &loopState{}
	state.crashing.Store(true)
	loop := crashLoop(t, state, nil)
	o := openRoot(t, nil, openOptions{storage: openJSONL(t, directory), registry: registryWith(t, "loop", loop)})
	id := start(t, o.root, loop, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return state.started.Load() == 1 })
	schedulerQuiet(t, o.h)
	// Hold the line; the abort mark is admitted behind it, then close begins.
	gate := make(chan struct{})
	entered := make(chan struct{})
	holding := async(func() (struct{}, error) {
		return struct{}{}, o.root.Commit(bg, func(*durable.Tx) error {
			close(entered)
			<-gate
			return nil
		})
	})
	<-entered
	aborting := async(func() (string, error) { return o.h.AbortTask(bg, id) })
	parked(t, 1, onLine, "(*scheduler).abort(")
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	eventually(t, func() bool { return o.h.tasks.closing.Load() })
	// The invocation ends while the line is held; its write-back waits behind
	// the abort mark.
	close(gate)
	if _, err := await(t, holding); err != nil {
		t.Fatal(err)
	}
	if result, err := await(t, aborting); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
	raw := openJSONL(t, directory)
	defer func() { _ = raw.Close(bg) }()
	record, err := raw.Task(bg, id)
	must(t, err)
	if record == nil || !record.AbortRequested || record.State.Status != durable.TaskPending {
		t.Fatalf("record = %s", jsonOf(t, record))
	}
}

func TestBlockedFaultsATaskWhoseAbortHandlerIsCutOffTheRestartLimitTimes(t *testing.T) {
	c := newCrashable()
	var aborts atomic.Int32
	stuck := oneStep(t, "test.stuck-abort", func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}, func(ctx context.Context, _ *TaskRuntime) error {
		aborts.Add(1)
		<-ctx.Done()
		return context.Cause(ctx)
	})
	options := openOptions{registry: registryWith(t, "stuck", stuck), settings: fixedLimit(1)}
	o := firstProcess(t, c, options)
	id := start(t, o.root, stuck, false)
	must(t, o.h.Resume())
	aborting := async(func() (string, error) { return o.h.AbortTask(bg, id) })
	eventually(t, func() bool { return aborts.Load() == 1 })
	if _, err := await(t, aborting); err != nil {
		t.Fatal(err)
	}
	// A crash in the abort handler.
	o = c.crashWith(t, o, nil, options)
	settled := waitDone(t, o.h, id)
	if settled.State.Outcome.Status != durable.OutcomeFaulted || settled.State.Outcome.Error == nil || string(settled.State.Outcome.Error.Detail) != `{"reason":"restart_limit"}` {
		t.Fatalf("outcome = %s", outcomeJSON(t, settled))
	}
	// The receipt keeps no restart count.
	if settled.Restarts != 0 {
		t.Fatalf("restarts = %d", settled.Restarts)
	}
	if got := aborts.Load(); got != 1 {
		t.Fatalf("abort ran %d times", got)
	}
	eventually(t, func() bool { return len(o.reports.allInfos()) > 0 })
	if infos := o.reports.allInfos(); infos[0].Type != durable.ReportTaskFaulted || infos[0].Task == nil || infos[0].Task.ID != id || infos[0].Reason != "restart_limit" {
		t.Fatalf("reports = %+v", infos)
	}
}

func TestBlockedRunsAParkedTaskOnceARaisedRestartLimitNoLongerParksIt(t *testing.T) {
	c := newCrashable()
	state := &loopState{}
	state.crashing.Store(true)
	loop := crashLoop(t, state, nil)
	var limit atomic.Int64
	limit.Store(1)
	options := openOptions{registry: registryWith(t, "loop", loop), settings: restartLimit(&limit)}
	o := firstProcess(t, c, options)
	id := start(t, o.root, loop, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return state.started.Load() == 1 })
	o = c.crashWith(t, o, nil, options)
	graph, err := o.h.TaskGraph(bg)
	must(t, err)
	defer func() { graph.Dispose() }()
	must(t, o.h.Resume())
	eventually(t, func() bool { return blockedOf(t, graph, id) == "restart_limit" })
	// Let a scheduling pass leave the parked task out.
	schedulerQuiet(t, o.h)
	state.crashing.Store(false)
	// The host's live settings change; the next pass runs the task.
	limit.Store(-1)
	must(t, o.h.Resume())
	if got := waitDone(t, o.h, id).State.Outcome.Status; got != durable.OutcomeCompleted {
		t.Fatalf("outcome = %s", got)
	}
	if got := state.started.Load(); got != 2 {
		t.Fatalf("started %d times", got)
	}
}

func TestBlockedStillRunsTheAbortHandlerOfAParkedTask(t *testing.T) {
	c := newCrashable()
	state := &loopState{}
	state.crashing.Store(true)
	var aborted atomic.Int32
	loop := crashLoop(t, state, func(ctx context.Context, rt *TaskRuntime) error {
		aborted.Add(1)
		return rt.Commit(ctx, abortWith(""))
	})
	options := openOptions{registry: registryWith(t, "loop", loop), settings: fixedLimit(1)}
	o := firstProcess(t, c, options)
	id := start(t, o.root, loop, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return state.started.Load() == 1 })
	o = c.crashWith(t, o, nil, options)
	if _, err := o.h.AbortTask(bg, id); err != nil {
		t.Fatal(err)
	}
	if got := waitDone(t, o.h, id).State.Outcome.Status; got != durable.OutcomeAborted {
		t.Fatalf("outcome = %s", got)
	}
	if aborted.Load() != 1 || state.started.Load() != 1 {
		t.Fatalf("aborted %d, started %d", aborted.Load(), state.started.Load())
	}
}

func TestBlockedDoesNotCountTheSameCheckpointCommittedAgainAsProgress(t *testing.T) {
	c := newCrashable()
	var started atomic.Int32
	intent := oneStep(t, "test.intent", func(ctx context.Context, task *stepTask, rt *TaskRuntime) error {
		started.Add(1)
		// Intent before an effect, then the effect never returns: the
		// checkpoint is unchanged.
		if err := rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) { return Next(task.Checkpoint) }); err != nil {
			return err
		}
		<-ctx.Done()
		return context.Cause(ctx)
	}, func(context.Context, *TaskRuntime) error { return nil })
	options := openOptions{registry: registryWith(t, "intent", intent), settings: fixedLimit(2)}
	o := firstProcess(t, c, options)
	id := start(t, o.root, intent, false)
	for run := int32(1); run <= 2; run++ {
		must(t, o.h.Resume())
		eventually(t, func() bool { return started.Load() == run })
		eventually(t, func() bool { return getTask(t, o.h, id).State.Status == durable.TaskRunning })
		o = c.crashWith(t, o, nil, options)
	}
	if got := getTask(t, o.h, id).Restarts; got != 2 {
		t.Fatalf("restarts = %d", got)
	}
	if got := inspectedState(t, o.h, id); got.Kind != "blocked" || got.Reason != "restart_limit" {
		t.Fatalf("inspection = %+v", got)
	}
}

func TestBlockedStartsTheCountOverWhenTheTaskMakesProgress(t *testing.T) {
	c := newCrashable()
	var crashing atomic.Bool
	crashing.Store(true)
	var started atomic.Int32
	steps, err := DefineTask(TaskSpec[any, stepState, any]{
		Name:    "test.steps",
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "one"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"one": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				started.Add(1)
				if crashing.Load() {
					<-ctx.Done()
					return context.Cause(ctx)
				}
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return Next(stepState{Phase: "two"})
				})
			},
			"two": func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
				started.Add(1)
				<-ctx.Done()
				return context.Cause(ctx)
			},
		},
		Abort: func(context.Context, *stepTask, *TaskRuntime) error { return nil },
	})
	must(t, err)
	options := openOptions{registry: registryWith(t, "steps", steps), settings: fixedLimit(2)}
	o := firstProcess(t, c, options)
	id := start(t, o.root, steps, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return started.Load() == 1 })
	o = c.crashWith(t, o, nil, options)
	if got := getTask(t, o.h, id).Restarts; got != 1 {
		t.Fatalf("restarts = %d", got)
	}
	// Progress: phase one commits phase two, which resets the count.
	crashing.Store(false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return started.Load() == 3 })
	if got := getTask(t, o.h, id).Restarts; got != 0 {
		t.Fatalf("restarts = %d", got)
	}
}

func TestBlockedTellsTheGraphAndTheHostWhenAMigrationFails(t *testing.T) {
	directory := t.TempDir()
	noop := func(context.Context, *stepTask, *TaskRuntime) error { return nil }
	define := func(version int, migrate func(json.RawMessage, json.RawMessage, int) (any, stepState, error)) Task {
		task, err := DefineTask(TaskSpec[any, stepState, any]{
			Name:    "test.migrating",
			Version: version,
			Initial: func(any) stepState { return stepState{Phase: "run"} },
			Phases:  map[string]func(context.Context, *stepTask, *TaskRuntime) error{"run": noop},
			Abort:   func(context.Context, *stepTask, *TaskRuntime) error { return nil },
			Migrate: migrate,
		})
		must(t, err)
		return task
	}
	v1 := define(1, nil)
	v2 := define(2, func(json.RawMessage, json.RawMessage, int) (any, stepState, error) {
		return nil, stepState{}, errors.New("cannot migrate")
	})
	first := openRoot(t, nil, openOptions{storage: openJSONL(t, directory), registry: registryWith(t, "v1", v1)})
	// Created, never run: scheduling stays off.
	id := start(t, first.root, v1, false)
	must(t, first.h.Close(bg))
	wakes := &wakeLog{}
	o := openRoot(t, nil, openOptions{storage: openJSONL(t, directory), registry: registryWith(t, "v2", v2), onWake: wakes.add})
	graph, err := o.h.TaskGraph(bg)
	must(t, err)
	defer func() { graph.Dispose() }()
	must(t, o.h.Resume())
	eventually(t, func() bool { return blockedOf(t, graph, id) == "migration_failed" })
	eventually(t, func() bool { return !wakeOf(t, o.h).Active })
	if w := wakeOf(t, o.h); !isWake(w, false, -1) {
		t.Fatalf("wake = %+v", w)
	}
	eventually(t, func() bool { w, ok := wakes.last(); return ok && !w.Active })
}

func TestBlockedShowsAnAbandonedTaskWaitingForItsDefinitionAsBlocked(t *testing.T) {
	directory := t.TempDir()
	abandoned := oneStep(t, "test.abandoned", func(context.Context, *stepTask, *TaskRuntime) error { return nil }, nil)
	first := openRoot(t, nil, openOptions{storage: openJSONL(t, directory), registry: registryWith(t, "abandoned", abandoned)})
	var id durable.TaskID
	must(t, first.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		id, err = CreateTask[any](tx, abandoned, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}, AbandonOnRestart: true})
		return err
	}))
	must(t, first.h.Close(bg))
	// The next open marks it restart; without its definition, it waits for
	// one to clean up.
	o := openRoot(t, nil, openOptions{storage: openJSONL(t, directory)})
	graph, err := o.h.TaskGraph(bg)
	must(t, err)
	defer func() { graph.Dispose() }()
	must(t, o.h.Resume())
	eventually(t, func() bool { return getTask(t, o.h, id).AbortReason == durable.AbortReasonRestart })
	eventually(t, func() bool { return blockedOf(t, graph, id) == "missing_task" })
}

func TestBlockedShowsATaskWithoutADefinitionAsBlockedAndWakesTheHostOnceARegistryChangeUnblocksIt(t *testing.T) {
	missing := oneStep(t, "test.missing", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	wakes := &wakeLog{}
	o := openRoot(t, nil, openOptions{onWake: wakes.add})
	graph, err := o.h.TaskGraph(bg)
	must(t, err)
	defer func() { graph.Dispose() }()
	id := start(t, o.root, missing, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return blockedOf(t, graph, id) == "missing_task" })
	eventually(t, func() bool { return !wakeOf(t, o.h).Active })
	must(t, o.registry.Install(&Extension{Name: "late", Tasks: []Task{missing}}))
	// The graph follows the registry; the host hears it can run work now.
	eventually(t, func() bool { return blockedOf(t, graph, id) == "" })
	eventually(t, func() bool {
		for _, w := range wakes.all() {
			if w.Active {
				return true
			}
		}
		return false
	})
	if got := waitDone(t, o.h, id).State.Outcome.Status; got != durable.OutcomeCompleted {
		t.Fatalf("outcome = %s", got)
	}
}

// Beyond pi, whose retry case has scheduling running already: RetryTask
// enables it, as a host's retry button on a paused Harness expects.
func TestBlockedRetryTaskEnablesSchedulingOnAPausedHarness(t *testing.T) {
	c := newCrashable()
	state := &loopState{}
	state.crashing.Store(true)
	loop := crashLoop(t, state, nil)
	options := openOptions{registry: registryWith(t, "loop", loop), settings: fixedLimit(1)}
	o := firstProcess(t, c, options)
	id := start(t, o.root, loop, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return state.started.Load() == 1 })
	o = c.crashWith(t, o, nil, options)
	state.crashing.Store(false)
	must(t, o.h.RetryTask(bg, id))
	// Polled: WaitForTask would enable scheduling itself.
	eventually(t, func() bool { return getTask(t, o.h, id).State.Status == durable.TaskTerminal })
}

// Beyond pi, whose case runs the task once its definition arrives, and the
// run's commit updates the graph: a paused Harness commits nothing, so only
// the registry change can.
func TestBlockedFollowsARegistryChangeInTheGraphWithoutACommit(t *testing.T) {
	missing := oneStep(t, "test.missing", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	o := openRoot(t, nil, openOptions{})
	graph, err := o.h.TaskGraph(bg)
	must(t, err)
	defer func() { graph.Dispose() }()
	id := start(t, o.root, missing, false)
	eventually(t, func() bool { return blockedOf(t, graph, id) == "missing_task" })
	must(t, o.registry.Install(&Extension{Name: "late", Tasks: []Task{missing}}))
	eventually(t, func() bool { return blockedOf(t, graph, id) == "" })
	if got := getTask(t, o.h, id).State.Status; got != durable.TaskPending {
		t.Fatalf("status = %s", got)
	}
}

// Beyond pi: a lowered restart limit parks a task at the next pass without a
// commit, and the graph hears it.
func TestBlockedShowsATaskALoweredRestartLimitParksInTheGraph(t *testing.T) {
	c := newCrashable()
	state := &loopState{}
	state.crashing.Store(true)
	loop := crashLoop(t, state, nil)
	var limit atomic.Int64
	limit.Store(5)
	options := openOptions{registry: registryWith(t, "loop", loop), settings: restartLimit(&limit)}
	o := firstProcess(t, c, options)
	id := start(t, o.root, loop, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return state.started.Load() == 1 })
	o = c.crashWith(t, o, nil, options)
	graph, err := o.h.TaskGraph(bg)
	must(t, err)
	defer func() { graph.Dispose() }()
	if got := blockedOf(t, graph, id); got != "" {
		t.Fatalf("graph blocked = %q before the limit falls", got)
	}
	limit.Store(1)
	must(t, o.h.Resume())
	eventually(t, func() bool { return blockedOf(t, graph, id) == "restart_limit" })
	if got := state.started.Load(); got != 1 {
		t.Fatalf("started %d times", got)
	}
}

// Beyond pi: an abort request on a task without its definition orphans it
// once its owned work drains; meanwhile it is not blocked, as nothing waits
// for a definition.
func TestBlockedShowsAnAbortRequestedTaskWithoutADefinitionAsNotBlocked(t *testing.T) {
	abortGate := make(chan struct{})
	child := oneStep(t, "test.child", func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}, func(ctx context.Context, rt *TaskRuntime) error {
		<-abortGate
		return rt.Commit(ctx, abortWith("test"))
	})
	parent := oneStep(t, "test.parent-missing", func(context.Context, *stepTask, *TaskRuntime) error { return nil }, nil)
	o := openRoot(t, []Task{child}, openOptions{})
	graph, err := o.h.TaskGraph(bg)
	must(t, err)
	defer func() { graph.Dispose() }()
	var parentID durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		if parentID, err = CreateTask[any](tx, parent, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}}); err != nil {
			return err
		}
		_, err = CreateTask[any](tx, child, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipTask, TaskID: parentID}})
		return err
	}))
	must(t, o.h.Resume())
	eventually(t, func() bool { return blockedOf(t, graph, parentID) == "missing_task" })
	if result, err := o.h.AbortTask(bg, parentID); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	eventually(t, func() bool { return getTask(t, o.h, parentID).AbortRequested })
	eventually(t, func() bool { return blockedOf(t, graph, parentID) == "" })
	close(abortGate)
	if got := waitDone(t, o.h, parentID).State.Outcome.Status; got != durable.OutcomeOrphaned {
		t.Fatalf("outcome = %s", got)
	}
}
