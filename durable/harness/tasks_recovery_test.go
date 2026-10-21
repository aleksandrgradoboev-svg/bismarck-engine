package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/jsonl"
)

// pi harness-tasks-recovery.test.ts.

var errCrashed = errors.New("the process crashed")

// process is one process's handle on a controlledStorage. After crash its
// commits and ID mints reach no record, a commit held at the moment of the
// crash included, and its Close leaves the storage open for the processes
// after it.
type process struct {
	*controlledStorage
	dead atomic.Bool
}

func (p *process) alive() bool { return !p.dead.Load() }

func (p *process) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	if !p.alive() {
		return 0, errCrashed
	}
	return p.controlledStorage.commit(ctx, writes, p.alive)
}

func (p *process) MintID(ctx context.Context) (durable.ID, error) {
	if !p.alive() {
		return 0, errCrashed
	}
	return p.controlledStorage.MintID(ctx)
}

func (p *process) Close(ctx context.Context) error {
	if !p.alive() {
		return nil
	}
	return p.controlledStorage.Close(ctx)
}

// crashable runs Harnesses over one controlledStorage as successive
// processes: pi's crash simulation, where the crashed Harness is abandoned
// without close, its held commit never lands, and its blocked handlers never
// return.
type crashable struct {
	*controlledStorage
	current *process
	// stop ends the handlers that never return in pi, once the test is over.
	stop     chan struct{}
	stopOnce sync.Once
}

func newCrashable() *crashable {
	return &crashable{controlledStorage: newControlledStorage(), stop: make(chan struct{})}
}

// open opens the Harness of a new process. It is not closed at cleanup:
// crash arranges that, after releasing what holds it.
func (c *crashable) open(t *testing.T, tasks []Task) opened {
	t.Helper()
	c.current = &process{controlledStorage: c.controlledStorage}
	registry := NewRegistry()
	if len(tasks) > 0 {
		must(t, registry.Install(&Extension{Name: "tasks", Tasks: tasks}))
	}
	reports := &reportLog{}
	h, err := Open(bg, c.current, Options{Registry: registry, OnReport: reports.add})
	must(t, err)
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	return opened{h: h, registry: registry, reports: reports, root: root}
}

// crash kills the current process and opens the Harness of a new one over
// the same records, with tasks. The dead Harness's held commit stays held
// until cleanup, which then releases it to fail without a write, ends the
// handlers that ignore their signal, and closes the dead Harness; the new
// one closes first.
func (c *crashable) crash(t *testing.T, dead opened, tasks []Task) opened {
	t.Helper()
	return c.crashWith(t, dead, tasks, openOptions{})
}

// crashWith is crash with the new Harness's options; their storage is the
// new process.
func (c *crashable) crashWith(t *testing.T, dead opened, tasks []Task, options openOptions) opened {
	t.Helper()
	c.mu.Lock()
	c.current.dead.Store(true)
	held := c.commitGate
	c.commitGate = nil
	c.mu.Unlock()
	t.Cleanup(func() {
		if held != nil {
			held.Release()
		}
		c.unblock()
		closeSoon(t, dead.h)
	})
	c.current = &process{controlledStorage: c.controlledStorage}
	options.storage = c.current
	recovered := openRoot(t, tasks, options)
	// A recovered handler may block on stop too; it must end before the new
	// Harness closes.
	t.Cleanup(c.unblock)
	return recovered
}

func (c *crashable) unblock() { c.stopOnce.Do(func() { close(c.stop) }) }

// ignoreSignal is a handler that ignores its signal until the test ends,
// pi's never-settling promise; then it ends with the signal.
func (c *crashable) ignoreSignal(ctx context.Context) error {
	<-c.stop
	<-ctx.Done()
	return context.Cause(ctx)
}

// openJSONL opens JSONL storage over directory, as a fresh process would.
func openJSONL(t *testing.T, directory string) durable.Storage {
	t.Helper()
	storage, err := jsonl.OpenDir(bg, directory, jsonl.Options{})
	must(t, err)
	return storage
}

// addTask installs a one-task extension named after the task; installing
// another task with the same name replaces it in place.
func addTask(t *testing.T, registry Registry, task Task) *Extension {
	t.Helper()
	extension := &Extension{Name: "task:" + task.Name(), Tasks: []Task{task}}
	must(t, registry.Install(extension))
	return extension
}

// signalled waits for the invocation's signal and ends with it: pi's
// aborted(runtime.signal).
func signalled(ctx context.Context) error {
	<-ctx.Done()
	return context.Cause(ctx)
}

// closedOnce is a channel closed once, by whichever caller is first.
type closedOnce struct {
	ch   chan struct{}
	once sync.Once
}

func newClosedOnce() *closedOnce { return &closedOnce{ch: make(chan struct{})} }

func (c *closedOnce) close() { c.once.Do(func() { close(c.ch) }) }

func reportTexts(reports *reportLog) []string {
	return names(reports.all(), func(err error) string { return err.Error() })
}

// transferService is a fake external service whose operations are idempotent
// by request key.
type transferService struct {
	mu      sync.Mutex
	applied map[string]int
	calls   int
}

func newTransferService() *transferService { return &transferService{applied: map[string]int{}} }

func (s *transferService) apply(key string, amount int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if _, ok := s.applied[key]; !ok {
		s.applied[key] = amount * 10
	}
	return s.applied[key]
}

func (s *transferService) counts() (calls, applied int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, len(s.applied)
}

type transferInput struct {
	Amount int `json:"amount"`
}

type transferState struct {
	Phase string `json:"phase"`
	Key   string `json:"key,omitempty"`
}

// transferTask is an intent/effect/outcome task: prepare commits the intent,
// apply performs the effect and commits the outcome. While interrupt is set,
// the first apply blocks after the effect until the invocation is signalled.
func transferTask(t *testing.T, service *transferService, interrupt *atomic.Bool) Task {
	t.Helper()
	task, err := DefineTask(TaskSpec[transferInput, transferState, any]{
		Name:    "test.transfer",
		Version: 1,
		Initial: func(transferInput) transferState { return transferState{Phase: "prepare"} },
		Phases: map[string]func(context.Context, *Running[transferInput, transferState], *TaskRuntime) error{
			"prepare": func(ctx context.Context, task *Running[transferInput, transferState], rt *TaskRuntime) error {
				if _, err := rt.MemoOr(ctx, "requested", task.Input.Amount); err != nil {
					return err
				}
				key := "transfer-" + jsonOf(t, task.Record.ID)
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return Next(transferState{Phase: "apply", Key: key})
				})
			},
			"apply": func(ctx context.Context, task *Running[transferInput, transferState], rt *TaskRuntime) error {
				receipt := service.apply(task.Checkpoint.Key, task.Input.Amount)
				if interrupt.CompareAndSwap(true, false) {
					return signalled(ctx)
				}
				return rt.Commit(ctx, completeWith(map[string]int{"receipt": receipt}))
			},
		},
		Abort: func(ctx context.Context, _ *Running[transferInput, transferState], rt *TaskRuntime) error {
			return rt.Commit(ctx, abortWith("test"))
		},
	})
	must(t, err)
	return task
}

// versioned is a one-phase task that completes with result, optionally
// migrating older records.
func versionedTask(t *testing.T, version int, result string, migrate func(input, checkpoint json.RawMessage, fromVersion int) (any, stepState, error)) Task {
	t.Helper()
	task, err := DefineTask(TaskSpec[any, stepState, any]{
		Name:    "test.versioned",
		Version: version,
		Initial: func(any) stepState { return stepState{Phase: "run"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"run": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				return rt.Commit(ctx, completeWith(result))
			},
		},
		Abort: func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
			return rt.Commit(ctx, abortWith(result))
		},
		Migrate: migrate,
	})
	must(t, err)
	return task
}

func stateJSON(t *testing.T, h *Harness, id durable.TaskID) string {
	t.Helper()
	return jsonOf(t, getTask(t, h, id).State)
}

func TestRecoveryResumesAnIntentEffectOutcomeTaskInterruptedAfterItsIntentAcrossCloseAndReopen(t *testing.T) {
	directory := t.TempDir()
	service := newTransferService()
	interrupt := &atomic.Bool{}
	interrupt.Store(true)
	transfer := transferTask(t, service, interrupt)

	first := openRoot(t, []Task{transfer}, openOptions{storage: openJSONL(t, directory)})
	var id durable.TaskID
	must(t, first.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		id, err = CreateTask(tx, transfer, transferInput{Amount: 7}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		return err
	}))
	must(t, first.h.Resume())
	eventually(t, func() bool { calls, _ := service.counts(); return calls == 1 })
	must(t, first.h.Close(bg))

	second := openRoot(t, []Task{transfer}, openOptions{storage: openJSONL(t, directory)})
	// Open reconciled running to pending and kept the checkpoint and memos;
	// nothing ran yet.
	reopened := getTask(t, second.h, id)
	key := "transfer-" + jsonOf(t, id)
	if reopened.AbortRequested || string(reopened.Memos["requested"]) != "7" || reopened.State.Status != durable.TaskPending ||
		jsonOf(t, reopened.State.Checkpoint) != jsonOf(t, transferState{Phase: "apply", Key: key}) {
		t.Fatalf("reopened = %s", jsonOf(t, reopened))
	}
	if calls, _ := service.counts(); calls != 1 {
		t.Fatalf("calls before resume = %d", calls)
	}
	must(t, second.h.Resume())
	receipt := waitDone(t, second.h, id)
	if got := outcomeJSON(t, receipt); got != `{"status":"completed","result":{"receipt":70}}` {
		t.Fatalf("outcome = %s", got)
	}
	if calls, applied := service.counts(); calls != 2 || applied != 1 {
		t.Fatalf("calls = %d, applied = %d", calls, applied)
	}
	must(t, second.h.Close(bg))

	third := openRoot(t, []Task{transfer}, openOptions{storage: openJSONL(t, directory)})
	if got, want := jsonOf(t, getTask(t, third.h, id)), jsonOf(t, receipt); got != want {
		t.Fatalf("third = %s, want %s", got, want)
	}
	must(t, third.h.Close(bg))
}

func TestRecoveryKeepsATasksStartedAtAcrossCloseAndReopenAndStampsEndedAtWhenItSettles(t *testing.T) {
	directory := t.TempDir()
	service := newTransferService()
	interrupt := &atomic.Bool{}
	interrupt.Store(true)
	transfer := transferTask(t, service, interrupt)
	var clock atomic.Int64
	clock.Store(2_000)
	now := func() time.Time { return time.UnixMilli(clock.Load()) }

	first := openRoot(t, []Task{transfer}, openOptions{storage: openJSONL(t, directory), now: now})
	var id durable.TaskID
	must(t, first.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		id, err = CreateTask(tx, transfer, transferInput{Amount: 7}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		return err
	}))
	must(t, first.h.Resume())
	eventually(t, func() bool { calls, _ := service.counts(); return calls == 1 })
	clock.Store(3_000)
	must(t, first.h.Close(bg))

	clock.Store(9_000)
	second := openRoot(t, []Task{transfer}, openOptions{storage: openJSONL(t, directory), now: now})
	reopened := getTask(t, second.h, id)
	if reopened.State.Status != durable.TaskPending || reopened.StartedAt == nil || *reopened.StartedAt != 2_000 || reopened.EndedAt != nil {
		t.Fatalf("reopened = %s", jsonOf(t, reopened))
	}
	must(t, second.h.Resume())
	receipt := waitDone(t, second.h, id)
	if receipt.StartedAt == nil || *receipt.StartedAt != 2_000 || receipt.EndedAt == nil || *receipt.EndedAt != 9_000 {
		t.Fatalf("receipt = %s", jsonOf(t, receipt))
	}
	must(t, second.h.Close(bg))
}

func TestRecoveryResumesAbortWorkAfterCloseAtEveryDirectTaskAbortStage(t *testing.T) {
	directory := t.TempDir()
	var log phaseLog
	var blockAbort atomic.Bool
	blockAbort.Store(true)
	abortReached := newClosedOnce()
	runRelease := make(chan struct{})
	abortable := oneStep(t, "test.abortable", func(context.Context, *stepTask, *TaskRuntime) error {
		log.add("run")
		// Ignores the abort signal until released, so the mark is durable
		// while the run is active.
		<-runRelease
		return nil
	}, func(ctx context.Context, rt *TaskRuntime) error {
		log.add("abort")
		if blockAbort.Load() {
			abortReached.close()
			return signalled(ctx)
		}
		return rt.Commit(ctx, abortWith("stop"))
	})

	// Stage 1: the mark is committed while the run invocation is still
	// active.
	o := openRoot(t, []Task{abortable}, openOptions{storage: openJSONL(t, directory)})
	id := start(t, o.root, abortable, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return len(log.all()) == 1 })
	aborting := markDurably(t, o.h, id)
	closing := async(func() (struct{}, error) { return struct{}{}, o.h.Close(bg) })
	// pi's close marks the Harness closing before it returns its promise; wait
	// for that before the run may end, or the abort invocation starts first.
	eventually(t, o.h.tasks.closing.Load)
	close(runRelease)
	if _, err := await(t, closing); err != nil {
		t.Fatal(err)
	}
	if result, err := await(t, aborting); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if got := log.all(); !reflect.DeepEqual(got, []string{"run"}) {
		t.Fatalf("stage 1 log = %v", got)
	}

	// Stage 2: reopen dispatches the abort invocation, never the run; close
	// while it is active.
	o = openRoot(t, []Task{abortable}, openOptions{storage: openJSONL(t, directory)})
	if record := getTask(t, o.h, id); !record.AbortRequested || record.State.Status != durable.TaskPending {
		t.Fatalf("reopened = %s", jsonOf(t, record))
	}
	must(t, o.h.Resume())
	<-abortReached.ch
	must(t, o.h.Close(bg))
	if got := log.all(); !reflect.DeepEqual(got, []string{"run", "abort"}) {
		t.Fatalf("stage 2 log = %v", got)
	}

	// Stage 3: a fresh abort invocation settles the task.
	blockAbort.Store(false)
	o = openRoot(t, []Task{abortable}, openOptions{storage: openJSONL(t, directory)})
	must(t, o.h.Resume())
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"aborted","reason":"stop"}` {
		t.Fatalf("outcome = %s", got)
	}
	must(t, o.h.Close(bg))
	if got := log.all(); !reflect.DeepEqual(got, []string{"run", "abort", "abort"}) {
		t.Fatalf("stage 3 log = %v", got)
	}

	// Stage 4: the terminal receipt survives reopen and nothing runs again.
	o = openRoot(t, []Task{abortable}, openOptions{storage: openJSONL(t, directory)})
	must(t, o.h.Resume())
	schedulerQuiet(t, o.h)
	if result, err := o.h.AbortTask(bg, id); err != nil || result != "terminal" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	must(t, o.h.Close(bg))
	if got := log.all(); !reflect.DeepEqual(got, []string{"run", "abort", "abort"}) {
		t.Fatalf("stage 4 log = %v", got)
	}
}

// crashTask's run ignores its signal, and its abort handler blocks while
// blockAbort is set; otherwise the abort settles with "recovered".
func crashTask(t *testing.T, c *crashable, log *phaseLog, blockAbort bool) Task {
	return oneStep(t, "test.crash", func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
		log.add("run")
		return c.ignoreSignal(ctx)
	}, func(ctx context.Context, rt *TaskRuntime) error {
		log.add("abort")
		if blockAbort {
			return c.ignoreSignal(ctx)
		}
		return rt.Commit(ctx, abortWith("recovered"))
	})
}

// crashedRun leaves a crash task running in a Harness that will crash.
func crashedRun(t *testing.T, log *phaseLog) (*crashable, opened, durable.TaskID) {
	t.Helper()
	c := newCrashable()
	o := c.open(t, []Task{crashTask(t, c, log, true)})
	id := start(t, o.root, crashTask(t, c, log, true), false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return len(log.all()) == 1 })
	return c, o, id
}

func TestCrashWhileTheMarkCommitIsInStorageResumesTheRun(t *testing.T) {
	var log phaseLog
	c, dead, id := crashedRun(t, &log)
	held := c.holdCommits()
	go func() { _, _ = dead.h.AbortTask(bg, id) }()
	held.Entered(t)
	recovered := c.crash(t, dead, []Task{crashTask(t, c, &log, false)})
	if record := getTask(t, recovered.h, id); record.AbortRequested || record.State.Status != durable.TaskPending {
		t.Fatalf("recovered = %s", jsonOf(t, record))
	}
	must(t, recovered.h.Resume())
	eventually(t, func() bool { return len(log.all()) == 2 })
	if got := log.all(); !reflect.DeepEqual(got, []string{"run", "run"}) {
		t.Fatalf("log = %v", got)
	}
}

func TestCrashAfterTheMarkBeforeTheRunJoinsRunsOnlyTheAbortHandler(t *testing.T) {
	var log phaseLog
	c, dead, id := crashedRun(t, &log)
	// The run ignores its signal, so AbortTask never finishes joining it.
	aborting := markDurably(t, dead.h, id)
	select {
	case r := <-aborting:
		t.Fatalf("abort joined a run that ignores its signal: %s, %v", r.value, r.err)
	case <-time.After(30 * time.Millisecond):
	}
	recovered := c.crash(t, dead, []Task{crashTask(t, c, &log, false)})
	if record := getTask(t, recovered.h, id); !record.AbortRequested || record.State.Status != durable.TaskPending {
		t.Fatalf("recovered = %s", jsonOf(t, record))
	}
	must(t, recovered.h.Resume())
	if got := outcomeJSON(t, waitDone(t, recovered.h, id)); got != `{"status":"aborted","reason":"recovered"}` {
		t.Fatalf("outcome = %s", got)
	}
	if got := log.all(); !reflect.DeepEqual(got, []string{"run", "abort"}) {
		t.Fatalf("log = %v", got)
	}
	must(t, recovered.h.Close(bg))
}

func TestCrashWhileTheAbortHandlerRunsSettlesTheTaskThroughAFreshAbortInvocation(t *testing.T) {
	var log phaseLog
	c := newCrashable()
	crash := crashTask(t, c, &log, true)
	dead := c.open(t, []Task{crash})
	id := start(t, dead.root, crash, false)
	if result, err := dead.h.AbortTask(bg, id); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	must(t, dead.h.Resume())
	eventually(t, func() bool { return len(log.all()) == 1 })
	if got := log.all(); !reflect.DeepEqual(got, []string{"abort"}) {
		t.Fatalf("log = %v", got)
	}
	if status := getTask(t, dead.h, id).State.Status; status != durable.TaskRunning {
		t.Fatalf("status = %s", status)
	}
	recovered := c.crash(t, dead, []Task{crashTask(t, c, &log, false)})
	must(t, recovered.h.Resume())
	if got := outcomeJSON(t, waitDone(t, recovered.h, id)); got != `{"status":"aborted","reason":"recovered"}` {
		t.Fatalf("outcome = %s", got)
	}
	if got := log.all(); !reflect.DeepEqual(got, []string{"abort", "abort"}) {
		t.Fatalf("log = %v", got)
	}
	must(t, recovered.h.Close(bg))
}

func TestCrashWhileTheAbortOutcomeIsInStorageRunsTheAbortHandlerAgain(t *testing.T) {
	var log phaseLog
	c := newCrashable()
	reached, proceed := make(chan struct{}), make(chan struct{})
	crash := oneStep(t, "test.crash", func(context.Context, *stepTask, *TaskRuntime) error { return nil }, func(ctx context.Context, rt *TaskRuntime) error {
		log.add("abort")
		close(reached)
		<-proceed
		return rt.Commit(ctx, abortWith("lost"))
	})
	dead := c.open(t, []Task{crash})
	id := start(t, dead.root, crash, false)
	if result, err := dead.h.AbortTask(bg, id); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	must(t, dead.h.Resume())
	<-reached
	held := c.holdCommits()
	close(proceed)
	held.Entered(t)
	recovered := c.crash(t, dead, []Task{crashTask(t, c, &log, false)})
	must(t, recovered.h.Resume())
	if got := outcomeJSON(t, waitDone(t, recovered.h, id)); got != `{"status":"aborted","reason":"recovered"}` {
		t.Fatalf("outcome = %s", got)
	}
	if got := log.all(); !reflect.DeepEqual(got, []string{"abort", "abort"}) {
		t.Fatalf("log = %v", got)
	}
	must(t, recovered.h.Close(bg))
}

func TestCrashAfterTheTerminalOutcomeRunsNothingAgain(t *testing.T) {
	var log phaseLog
	c := newCrashable()
	crash := crashTask(t, c, &log, false)
	dead := c.open(t, []Task{crash})
	id := start(t, dead.root, crash, false)
	if result, err := dead.h.AbortTask(bg, id); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	must(t, dead.h.Resume())
	waitDone(t, dead.h, id)
	recovered := c.crash(t, dead, []Task{crashTask(t, c, &log, false)})
	must(t, recovered.h.Resume())
	schedulerQuiet(t, recovered.h)
	if got := log.all(); !reflect.DeepEqual(got, []string{"abort"}) {
		t.Fatalf("log = %v", got)
	}
	if got := stateJSON(t, recovered.h, id); got != `{"status":"terminal","outcome":{"status":"aborted","reason":"recovered"}}` {
		t.Fatalf("state = %s", got)
	}
	must(t, recovered.h.Close(bg))
}

func TestCrashWhileTheReservationCommitIsInStorageLeavesTheTaskPending(t *testing.T) {
	var log phaseLog
	c := newCrashable()
	crash := crashTask(t, c, &log, false)
	dead := c.open(t, []Task{crash})
	id := start(t, dead.root, crash, false)
	held := c.holdCommits()
	must(t, dead.h.Resume())
	held.Entered(t)
	recovered := c.crash(t, dead, []Task{crashTask(t, c, &log, false)})
	if status := getTask(t, recovered.h, id).State.Status; status != durable.TaskPending {
		t.Fatalf("status = %s", status)
	}
	if got := log.all(); len(got) != 0 {
		t.Fatalf("log = %v", got)
	}
	if _, err := recovered.h.AbortTask(bg, id); err != nil {
		t.Fatal(err)
	}
	must(t, recovered.h.Resume())
	waitDone(t, recovered.h, id)
	if got := log.all(); !reflect.DeepEqual(got, []string{"abort"}) {
		t.Fatalf("log = %v", got)
	}
	must(t, recovered.h.Close(bg))
}

func TestBlockedKeepsATaskWithAMissingDefinitionPendingAndLiveForIdleWaitsUntilRegistration(t *testing.T) {
	v1 := versionedTask(t, 1, "v1", nil)
	o := openRoot(t, nil, openOptions{})
	id := start(t, o.root, v1, false)
	must(t, o.h.Resume())
	schedulerQuiet(t, o.h)
	if status := getTask(t, o.h, id).State.Status; status != durable.TaskPending {
		t.Fatalf("status = %s", status)
	}
	idle := async(func() (struct{}, error) { return struct{}{}, o.h.WaitForIdle(bg) })
	parked(t, 1, onWaiter, "harness.(*Harness).WaitForIdle(")
	addTask(t, o.registry, v1)
	if _, err := await(t, idle); err != nil {
		t.Fatal(err)
	}
	if got := stateJSON(t, o.h, id); got != `{"status":"terminal","outcome":{"status":"completed","result":"v1"}}` {
		t.Fatalf("state = %s", got)
	}
	must(t, o.h.Close(bg))
}

func TestBlockedKeepsATaskStoredByANewerVersionPendingUntilAFittingDefinitionIsRegistered(t *testing.T) {
	registry := NewRegistry()
	addTask(t, registry, versionedTask(t, 1, "old", nil))
	o := openRoot(t, nil, openOptions{registry: registry})
	id := start(t, o.root, versionedTask(t, 2, "new", nil), false)
	must(t, o.h.Resume())
	schedulerQuiet(t, o.h)
	if status := getTask(t, o.h, id).State.Status; status != durable.TaskPending {
		t.Fatalf("status = %s", status)
	}
	// The same extension name replaces the old one in place.
	// Too old is a block, not a failure: it reports nothing.
	if got := reportTexts(o.reports); len(got) != 0 {
		t.Fatalf("reports = %v", got)
	}
	addTask(t, registry, versionedTask(t, 2, "new", nil))
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"completed","result":"new"}` {
		t.Fatalf("outcome = %s", got)
	}
	must(t, o.h.Close(bg))
}

func TestBlockedMigratesAtReservationLeavesTheRecordWhenMigrationFailsAndRetriesOnlyForANewDefinition(t *testing.T) {
	directory := t.TempDir()
	o := openRoot(t, nil, openOptions{storage: openJSONL(t, directory)})
	id := start(t, o.root, versionedTask(t, 1, "v1", nil), false)
	must(t, o.h.Close(bg))

	registry := NewRegistry()
	var failures atomic.Int32
	addTask(t, registry, versionedTask(t, 2, "v2", func(json.RawMessage, json.RawMessage, int) (any, stepState, error) {
		failures.Add(1)
		return nil, stepState{}, errors.New("cannot migrate")
	}))
	o = openRoot(t, nil, openOptions{storage: openJSONL(t, directory), registry: registry})
	must(t, o.h.Resume())
	eventually(t, func() bool { return len(o.reports.all()) == 1 })
	// An unrelated registry change wakes the scheduler without retrying the
	// same failed definition.
	addTool(t, registry, testTool("unrelated"))
	schedulerQuiet(t, o.h)
	if got := reportTexts(o.reports); failures.Load() != 1 || len(got) != 1 || !strings.Contains(got[0], "cannot migrate") {
		t.Fatalf("failures = %d, reports = %v", failures.Load(), got)
	}
	if record := getTask(t, o.h, id); record.Version != 1 || record.State.Status != durable.TaskPending || string(record.State.Checkpoint) != `{"phase":"run"}` {
		t.Fatalf("record = %s", jsonOf(t, record))
	}

	var mu sync.Mutex
	var migrations []int
	// The same extension name replaces the old one in place.
	addTask(t, registry, versionedTask(t, 2, "v2", func(_, checkpoint json.RawMessage, fromVersion int) (any, stepState, error) {
		mu.Lock()
		migrations = append(migrations, fromVersion)
		mu.Unlock()
		var state stepState
		return nil, state, json.Unmarshal(checkpoint, &state)
	}))
	receipt := waitDone(t, o.h, id)
	if receipt.Version != 2 || outcomeJSON(t, receipt) != `{"status":"completed","result":"v2"}` {
		t.Fatalf("receipt = %s", jsonOf(t, receipt))
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(migrations, []int{1}) {
		t.Fatalf("migrations = %v", migrations)
	}
	must(t, o.h.Close(bg))
}

// An abort reservation migrates an older record like a run reservation does,
// and the abort handler gets the migrated record it committed.
func TestRecoveryGivesAnAbortHandlerTheRecordItsReservationMigrated(t *testing.T) {
	directory := t.TempDir()
	o := openRoot(t, nil, openOptions{storage: openJSONL(t, directory)})
	id := start(t, o.root, versionedTask(t, 1, "v1", nil), false)
	must(t, o.h.Close(bg))

	var mu sync.Mutex
	var got []string
	v2, err := DefineTask(TaskSpec[any, stepState, any]{
		Name:    "test.versioned",
		Version: 2,
		Initial: func(any) stepState { return stepState{Phase: "run"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"migrated": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				return rt.Commit(ctx, completeWith("v2"))
			},
		},
		Abort: func(ctx context.Context, task *stepTask, rt *TaskRuntime) error {
			mu.Lock()
			got = append(got, fmt.Sprintf("version %d, %s, phase %s", task.Record.Version, task.Record.State.Status, task.Checkpoint.Phase))
			mu.Unlock()
			return rt.Commit(ctx, abortWith("v2"))
		},
		Migrate: func(json.RawMessage, json.RawMessage, int) (any, stepState, error) {
			return nil, stepState{Phase: "migrated"}, nil
		},
	})
	must(t, err)
	o = openRoot(t, []Task{v2}, openOptions{storage: openJSONL(t, directory)})
	if marked, err := o.h.AbortTask(bg, id); err != nil || marked != "marked" {
		t.Fatalf("abort = %q, %v", marked, err)
	}
	must(t, o.h.Resume())
	receipt := waitDone(t, o.h, id)
	if receipt.Version != 2 || receipt.State.Outcome.Status != durable.OutcomeAborted {
		t.Fatalf("receipt = %s", jsonOf(t, receipt))
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"version 2, running, phase migrated"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("abort handler got %v, want %v", got, want)
	}
	must(t, o.h.Close(bg))
}

func TestBlockedBlocksAnOlderRecordWhoseNewerDefinitionHasNoMigration(t *testing.T) {
	directory := t.TempDir()
	o := openRoot(t, nil, openOptions{storage: openJSONL(t, directory)})
	id := start(t, o.root, versionedTask(t, 1, "v1", nil), false)
	must(t, o.h.Close(bg))
	o = openRoot(t, []Task{versionedTask(t, 2, "v2", nil)}, openOptions{storage: openJSONL(t, directory)})
	must(t, o.h.Resume())
	eventually(t, func() bool { return len(o.reports.all()) == 1 })
	if got := reportTexts(o.reports); !strings.Contains(got[0], "has no migration from 1") {
		t.Fatalf("reports = %v", got)
	}
	if record := getTask(t, o.h, id); record.Version != 1 || record.State.Status != durable.TaskPending {
		t.Fatalf("record = %s", jsonOf(t, record))
	}
	must(t, o.h.Close(bg))
}

func TestBlockedSettlesAnAbortedBlockedTaskAsOrphanedAndRetiresItsDocuments(t *testing.T) {
	scratch := durable.MustDefineDoc(durable.DocDef{
		CheckpointWhen: durable.NeverCheckpoint,
		Kind:           "test.orphan-scratch",
		Version:        1,
		Scope:          durable.ScopeTask,
		Initial:        func(any) map[string]any { return map[string]any{"n": 0} },
	})
	o := openRoot(t, nil, openOptions{})
	var id durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		id, err = CreateTask[any](tx, versionedTask(t, 1, "x", nil), nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		if err != nil {
			return err
		}
		draft, err := tx.Doc(scratch.Task(id))
		if err != nil {
			return err
		}
		return draft.Set("n", 1)
	}))
	// Before resume: the marking commit settles the blocked task directly.
	if result, err := o.h.AbortTask(bg, id); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if got := stateJSON(t, o.h, id); got != `{"status":"terminal","outcome":{"status":"orphaned","reason":"missing_task"}}` {
		t.Fatalf("state = %s", got)
	}
	if value := snapshotOf(t, o.h, scratch.Task(id)); value != nil {
		t.Fatalf("scratch = %v", value)
	}
	must(t, o.h.Close(bg))
}

func TestBlockedOrphansAMarkedTaskWhoseDefinitionDisappearedWhileItsRunWasActive(t *testing.T) {
	reached := make(chan struct{})
	running := oneStep(t, "test.vanishing", func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
		close(reached)
		return signalled(ctx)
	}, func(context.Context, *TaskRuntime) error {
		t.Error("the abort handler must not run")
		return errors.New("must not run")
	})
	registry := NewRegistry()
	registration := addTask(t, registry, running)
	o := openRoot(t, nil, openOptions{registry: registry})
	id := start(t, o.root, running, false)
	must(t, o.h.Resume())
	<-reached
	registry.Uninstall(registration)
	// An active run means the mark does not orphan directly; the scheduler
	// orphans once the run has ended.
	if result, err := o.h.AbortTask(bg, id); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"orphaned","reason":"missing_task"}` {
		t.Fatalf("outcome = %s", got)
	}
	must(t, o.h.Close(bg))
}

func TestBlockedOrphansAReopenedAbortMarkedTaskWithoutADefinitionOnceSchedulingResumes(t *testing.T) {
	directory := t.TempDir()
	x := versionedTask(t, 1, "x", nil)
	o := openRoot(t, []Task{x}, openOptions{storage: openJSONL(t, directory)})
	id := start(t, o.root, x, false)
	if result, err := o.h.AbortTask(bg, id); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	must(t, o.h.Close(bg))

	o = openRoot(t, nil, openOptions{storage: openJSONL(t, directory)})
	if record := getTask(t, o.h, id); !record.AbortRequested || record.State.Status != durable.TaskPending {
		t.Fatalf("reopened = %s", jsonOf(t, record))
	}
	// WaitForIdle resumes scheduling itself.
	must(t, o.h.WaitForIdle(bg))
	if got := stateJSON(t, o.h, id); got != `{"status":"terminal","outcome":{"status":"orphaned","reason":"missing_task"}}` {
		t.Fatalf("state = %s", got)
	}
	must(t, o.h.Close(bg))
}

type handoverState struct {
	Phase string `json:"phase"`
}

type handoverTask = Running[any, handoverState]

// handoverGates hold phases a and b until closed, when set; onEnd runs as an
// a or b invocation ends.
type handoverGates struct {
	a, b  <-chan struct{}
	onEnd func()
}

func newHandoverTask(t *testing.T, label string, version int, log *phaseLog, gates handoverGates, migrate func(json.RawMessage, json.RawMessage, int) (any, handoverState, error)) Task {
	t.Helper()
	advance := func(phase string, gate <-chan struct{}, next string) func(context.Context, *handoverTask, *TaskRuntime) error {
		return func(ctx context.Context, _ *handoverTask, rt *TaskRuntime) error {
			log.add(label + ":" + phase + " start")
			if gate != nil {
				<-gate
			}
			if err := rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
				return Next(handoverState{Phase: next})
			}); err != nil {
				return err
			}
			// Leave room for a wrongly dispatched successor before this
			// invocation ends: a check that nothing else starts, so a pause.
			flush()
			if gates.onEnd != nil {
				gates.onEnd()
			}
			log.add(label + ":" + phase + " end")
			return nil
		}
	}
	task, err := DefineTask(TaskSpec[any, handoverState, any]{
		Name:    "test.handover",
		Version: version,
		Initial: func(any) handoverState { return handoverState{Phase: "a"} },
		Phases: map[string]func(context.Context, *handoverTask, *TaskRuntime) error{
			"a": advance("a", gates.a, "b"),
			"b": advance("b", gates.b, "c"),
			"c": func(ctx context.Context, _ *handoverTask, rt *TaskRuntime) error {
				log.add(label + ":c")
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(ctx context.Context, _ *handoverTask, rt *TaskRuntime) error {
			log.add(label + ":abort")
			return rt.Commit(ctx, abortWith(label))
		},
		Migrate: migrate,
	})
	must(t, err)
	return task
}

type handover struct {
	opened
	old *Extension
	id  durable.TaskID
}

// startHandover runs the old definition until its first phase has started.
func startHandover(t *testing.T, log *phaseLog, gates handoverGates, storage durable.Storage) handover {
	t.Helper()
	registry := NewRegistry()
	old := addTask(t, registry, newHandoverTask(t, "old", 1, log, gates, nil))
	o := openRoot(t, nil, openOptions{storage: storage, registry: registry})
	id := start(t, o.root, newHandoverTask(t, "old", 1, log, handoverGates{}, nil), false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return len(log.all()) == 1 })
	return handover{opened: o, old: old, id: id}
}

func TestHandoverHandsOverAtTheNextPhaseBoundaryToASameVersionReplacementWithoutOverlap(t *testing.T) {
	var log phaseLog
	gate := make(chan struct{})
	o := startHandover(t, &log, handoverGates{a: gate}, nil)
	// The same extension name replaces the old one in place.
	addTask(t, o.registry, newHandoverTask(t, "new", 1, &log, handoverGates{}, nil))
	close(gate)
	waitDone(t, o.h, o.id)
	if got := log.all(); !reflect.DeepEqual(got, []string{"old:a start", "old:a end", "new:b start", "new:b end", "new:c"}) {
		t.Fatalf("log = %v", got)
	}
	must(t, o.h.Close(bg))
}

func TestHandoverKeepsTheMemosAcrossAHandover(t *testing.T) {
	var log phaseLog
	gate := make(chan struct{})
	reached := newClosedOnce()
	memoTask := func(label string) Task {
		return MustDefineTask(TaskSpec[any, handoverState, any]{
			Name:    "test.handover-memo",
			Version: 1,
			Initial: func(any) handoverState { return handoverState{Phase: "a"} },
			Phases: map[string]func(context.Context, *handoverTask, *TaskRuntime) error{
				"a": func(ctx context.Context, _ *handoverTask, rt *TaskRuntime) error {
					if _, err := rt.MemoOr(ctx, "picked", label); err != nil {
						return err
					}
					reached.close()
					<-gate
					return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
						return Next(handoverState{Phase: "b"})
					})
				},
				"b": func(ctx context.Context, _ *handoverTask, rt *TaskRuntime) error {
					// The candidate loses to the memo the old definition stored.
					raw, err := rt.MemoOr(ctx, "picked", label)
					if err != nil {
						return err
					}
					var picked string
					if err := json.Unmarshal(raw, &picked); err != nil {
						return err
					}
					log.add(label + ":b " + picked)
					return rt.Commit(ctx, completeWith(picked))
				},
			},
			Abort: func(context.Context, *handoverTask, *TaskRuntime) error { return nil },
		})
	}
	registry := NewRegistry()
	addTask(t, registry, memoTask("old"))
	o := openRoot(t, nil, openOptions{registry: registry})
	id := start(t, o.root, memoTask("old"), false)
	must(t, o.h.Resume())
	<-reached.ch
	addTask(t, registry, memoTask("new"))
	close(gate)
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"completed","result":"old"}` {
		t.Fatalf("outcome = %s", got)
	}
	if got := log.all(); !reflect.DeepEqual(got, []string{"new:b old"}) {
		t.Fatalf("log = %v", got)
	}
	must(t, o.h.Close(bg))
}

func TestHandoverHandsOverToANewerVersionWithAMigration(t *testing.T) {
	var log phaseLog
	gate := make(chan struct{})
	o := startHandover(t, &log, handoverGates{a: gate}, nil)
	// The same extension name replaces the old one in place.
	addTask(t, o.registry, newHandoverTask(t, "v2", 2, &log, handoverGates{}, func(json.RawMessage, json.RawMessage, int) (any, handoverState, error) {
		return nil, handoverState{Phase: "c"}, nil
	}))
	close(gate)
	if receipt := waitDone(t, o.h, o.id); receipt.Version != 2 {
		t.Fatalf("version = %d", receipt.Version)
	}
	if got := log.all(); !reflect.DeepEqual(got, []string{"old:a start", "old:a end", "v2:c"}) {
		t.Fatalf("log = %v", got)
	}
	must(t, o.h.Close(bg))
}

func TestHandoverToANewerVersionWhoseMigrationFailsLeavesTheTaskBlocked(t *testing.T) {
	var log phaseLog
	gate := make(chan struct{})
	o := startHandover(t, &log, handoverGates{a: gate}, nil)
	// The same extension name replaces the old one in place.
	addTask(t, o.registry, newHandoverTask(t, "broken", 2, &log, handoverGates{}, func(json.RawMessage, json.RawMessage, int) (any, handoverState, error) {
		return nil, handoverState{}, errors.New("broken migration")
	}))
	close(gate)
	eventually(t, func() bool { return len(o.reports.all()) == 1 })
	if info := o.reports.allInfos()[0]; info.Type != durable.ReportMigrationFailed || info.Task == nil || info.Task.ID != o.id {
		t.Fatalf("report = %+v", info)
	}
	schedulerQuiet(t, o.h)
	if record := getTask(t, o.h, o.id); record.Version != 1 || record.State.Status != durable.TaskPending || string(record.State.Checkpoint) != `{"phase":"b"}` {
		t.Fatalf("record = %s", jsonOf(t, record))
	}
	if got := log.all(); !reflect.DeepEqual(got, []string{"old:a start", "old:a end"}) {
		t.Fatalf("log = %v", got)
	}
	must(t, o.h.Close(bg))
}

func TestHandoverKeepsRunningUnderTheOldDefinitionWhenTheReplacementIsMissingOrCannotTakeTheTask(t *testing.T) {
	var log phaseLog
	gateA, gateB := make(chan struct{}), make(chan struct{})
	o := startHandover(t, &log, handoverGates{a: gateA, b: gateB}, nil)
	o.registry.Uninstall(o.old)
	close(gateA)
	eventually(t, func() bool {
		for _, line := range log.all() {
			if line == "old:b start" {
				return true
			}
		}
		return false
	})
	// A newer definition without a migration cannot take the task either.
	addTask(t, o.registry, newHandoverTask(t, "incompatible", 2, &log, handoverGates{}, nil))
	close(gateB)
	waitDone(t, o.h, o.id)
	if got := log.all(); !reflect.DeepEqual(got, []string{"old:a start", "old:a end", "old:b start", "old:b end", "old:c"}) {
		t.Fatalf("log = %v", got)
	}
	infos := o.reports.allInfos()
	if len(infos) != 2 {
		t.Fatalf("reports = %v", reportTexts(o.reports))
	}
	for i, reason := range []string{"missing_task", "incompatible_task"} {
		if info := infos[i]; info.Type != durable.ReportTaskOutdated || info.Task == nil || info.Task.ID != o.id || info.Reason != reason {
			t.Fatalf("report %d = %+v, want task_outdated of task %d with reason %s", i, info, o.id, reason)
		}
	}
	must(t, o.h.Close(bg))
}

func TestHandoverRejectsARuntimeCommitOfTheOldInvocationQueuedBehindItsHandoverCommit(t *testing.T) {
	var log phaseLog
	phaseGate := make(chan struct{})
	storage := newControlledStorage()
	var held atomic.Pointer[gate]
	var oldRuntime atomic.Pointer[TaskRuntime]
	var harnessRef atomic.Pointer[Harness]
	old := MustDefineTask(TaskSpec[any, handoverState, any]{
		Name:    "test.handover",
		Version: 1,
		Initial: func(any) handoverState { return handoverState{Phase: "a"} },
		Phases: map[string]func(context.Context, *handoverTask, *TaskRuntime) error{
			"a": func(ctx context.Context, task *handoverTask, rt *TaskRuntime) error {
				oldRuntime.Store(rt)
				<-phaseGate
				if err := rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return Next(handoverState{Phase: "b"})
				}); err != nil {
					return err
				}
				// Hold the line with an unrelated commit so the handover
				// commit queues behind it.
				held.Store(storage.holdCommits())
				conversation := task.Record.ConversationID
				go func() {
					_ = harnessRef.Load().Commit(bg, func(tx *durable.Tx) error {
						_, err := tx.AppendEntry(conversation, durable.EntryDraft{Kind: "blocker"})
						return err
					})
				}()
				// pi's commit joins the line before the phase returns; here the
				// blocker's goroutine must reach the held storage first.
				<-held.Load().entered
				return nil
			},
			"b": func(context.Context, *handoverTask, *TaskRuntime) error { return nil },
			"c": func(context.Context, *handoverTask, *TaskRuntime) error { return nil },
		},
		Abort: func(context.Context, *handoverTask, *TaskRuntime) error { return nil },
	})
	registry := NewRegistry()
	addTask(t, registry, old)
	o := openRoot(t, nil, openOptions{storage: storage, registry: registry})
	harnessRef.Store(o.h)
	id := start(t, o.root, old, false)
	must(t, o.h.Resume())
	eventually(t, func() bool { return oldRuntime.Load() != nil })
	// The same extension name replaces the old one in place.
	addTask(t, registry, newHandoverTask(t, "new", 1, &log, handoverGates{}, nil))
	close(phaseGate)
	eventually(t, func() bool { return held.Load() != nil })
	hold := held.Load()
	hold.Entered(t)
	parked(t, 1, onLine, "harness.(*scheduler).step(")
	// Queued behind the handover commit while the invocation has not ended
	// yet.
	late := async(func() (struct{}, error) { return struct{}{}, oldRuntime.Load().Commit(bg, completeWith(nil)) })
	parked(t, 1, onLine, "harness.(*TaskRuntime).gated(")
	storage.releaseHold(hold)
	if _, err := await(t, late); err == nil || !strings.Contains(err.Error(), "invocation has ended") {
		t.Fatalf("late commit = %v", err)
	}
	waitDone(t, o.h, id)
	if got := log.all(); !reflect.DeepEqual(got, []string{"new:b start", "new:b end", "new:c"}) {
		t.Fatalf("log = %v", got)
	}
	must(t, o.h.Close(bg))
}

func TestHandoverPreservesAnAbortMarkThatRacesTheHandoverCommitAndTheNewDefinitionAborts(t *testing.T) {
	var log phaseLog
	phaseGate := make(chan struct{})
	storage := newControlledStorage()
	var held atomic.Pointer[gate]
	// The progress commit landed; hold the next commit, the handover, and
	// queue the abort mark behind it.
	gates := handoverGates{a: phaseGate, onEnd: func() {
		if held.Load() == nil {
			held.Store(storage.holdCommits())
		}
	}}
	o := startHandover(t, &log, gates, storage)
	// The same extension name replaces the old one in place.
	addTask(t, o.registry, newHandoverTask(t, "new", 1, &log, handoverGates{}, nil))
	close(phaseGate)
	eventually(t, func() bool { return held.Load() != nil })
	hold := held.Load()
	hold.Entered(t)
	aborting := async(func() (string, error) { return o.h.AbortTask(bg, o.id) })
	parked(t, 1, onLine, "harness.(*scheduler).abort(")
	storage.releaseHold(hold)
	if result, err := await(t, aborting); err != nil || result != "marked" {
		t.Fatalf("abort = %s, %v", result, err)
	}
	if got := outcomeJSON(t, waitDone(t, o.h, o.id)); got != `{"status":"aborted","reason":"new"}` {
		t.Fatalf("outcome = %s", got)
	}
	var states []string
	for _, record := range storage.taskWrites(o.id) {
		state := record.State.Status
		if record.AbortRequested {
			state += "+mark"
		}
		states = append(states, state)
	}
	// created, reserved, progress, handover, mark, abort reservation, aborted
	want := []string{"pending", "running", "running", "pending", "pending+mark", "running+mark", "terminal+mark"}
	if !reflect.DeepEqual(states, want) {
		t.Fatalf("states = %v", states)
	}
	if got := log.all(); !reflect.DeepEqual(got, []string{"old:a start", "old:a end", "new:abort"}) {
		t.Fatalf("log = %v", got)
	}
	must(t, o.h.Close(bg))
}

func TestOpenReleasesItsRegistrySubscriptionAndClosesTheSessionWhenOpenFails(t *testing.T) {
	storage := newControlledStorage()
	// Leave a running task behind so open has a reconciliation commit to
	// fail.
	seedRunningTask(t, storage)
	reader := &countingReader{Registry: NewRegistry()}
	storage.failNextCommit(errors.New("disk full"))
	_, err := Open(bg, storage, Options{Registry: reader})
	expectError(t, err, "disk full")
	if reader.subscriptions() != 0 || storage.open() {
		t.Fatalf("subscriptions %d, storage open %v", reader.subscriptions(), storage.open())
	}
}
