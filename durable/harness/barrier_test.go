package harness

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// pi packages/durable/test/harness-cancellation-barrier.test.ts and
// harness-inspect.test.ts (its submissions case comes with the submissions
// stage).

// dormantSpec runs until aborted; its abort handler ends it aborted.
func dormantSpec(name string) TaskSpec[any, stepState, any] {
	return TaskSpec[any, stepState, any]{
		Name:    name,
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "work"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"work": func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
				<-ctx.Done()
				return context.Cause(ctx)
			},
		},
		Abort: func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error { return rt.Commit(ctx, abortWith("")) },
	}
}

var dormant = MustDefineTask(dormantSpec("test.dormant"))

// holdMarks is memory storage that holds every commit abort-marking target,
// while set, until released.
type holdMarks struct {
	*memory.Storage
	mu       sync.Mutex
	target   durable.TaskID
	held     chan struct{}
	heldOnce sync.Once
	release  chan struct{}
}

func (s *holdMarks) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	s.mu.Lock()
	target := s.target
	s.mu.Unlock()
	for _, write := range writes {
		if task, ok := write.(durable.TaskWrite); ok && task.Value.ID == target && task.Value.AbortRequested {
			s.heldOnce.Do(func() { close(s.held) })
			<-s.release
			break
		}
	}
	return s.Storage.Commit(ctx, writes)
}

func TestBarrierDoesNotStartTheNextPhaseWhileTheMarkingCascadeHasNotCommitted(t *testing.T) {
	storage := &holdMarks{Storage: memory.New(), held: make(chan struct{}), release: make(chan struct{})}
	entered, advance := make(chan struct{}), make(chan struct{})
	var ranNext sync.Map
	child := MustDefineTask(TaskSpec[any, stepState, any]{
		Name:    "test.child",
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "first"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"first": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				close(entered)
				<-advance
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return Next(stepState{Phase: "next"})
				})
			},
			"next": func(ctx context.Context, _ *stepTask, _ *TaskRuntime) error {
				ranNext.Store("ran", true)
				<-ctx.Done()
				return context.Cause(ctx)
			},
		},
		Abort: func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error { return rt.Commit(ctx, abortWith("")) },
	})
	o := openRoot(t, []Task{dormant, child}, openOptions{storage: storage})
	parent := start(t, o.root, dormant, false)
	var childID durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		childID, err = CreateTask[any](tx, child, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipTask, TaskID: parent}})
		return err
	}))
	storage.mu.Lock()
	storage.target = childID
	storage.mu.Unlock()
	must(t, o.h.Resume())
	<-entered
	aborting := async(func() (string, error) { return o.h.AbortTask(bg, parent) })
	<-storage.held
	// The parent's request is durable, the cascade's mark of the child is not:
	// its phase must not advance.
	close(advance)
	for i := 0; i < 5; i++ {
		flush()
	}
	if _, ran := ranNext.Load("ran"); ran {
		t.Fatal("the next phase ran before the cascade mark committed")
	}
	close(storage.release)
	if got := waitDone(t, o.h, childID); got.State.Outcome.Status != durable.OutcomeAborted {
		t.Fatalf("child = %s", outcomeJSON(t, got))
	}
	if _, err := await(t, aborting); err != nil {
		t.Fatal(err)
	}
	if _, ran := ranNext.Load("ran"); ran {
		t.Fatal("the next phase ran")
	}
}

// crashState creates tasks through a plain Session, then rewrites records
// directly, as a crash would have left them.
type crashState struct {
	storage *memory.Storage
	session *durable.Session
	conv    durable.ConversationID
}

func newCrashState(t *testing.T) crashState {
	t.Helper()
	storage := memory.New()
	session := durable.NewSession(&survivingStorage{storage}, durable.SessionOptions{})
	conv, err := durable.CommitValue(bg, session, func(tx *durable.Tx) (durable.ConversationID, error) {
		record, err := tx.CreateConversation(durable.Ownerless)
		return record.ID, err
	})
	must(t, err)
	return crashState{storage: storage, session: session, conv: conv}
}

func (c crashState) create(t *testing.T, task Task, owner durable.TaskID, abandon bool) durable.TaskID {
	t.Helper()
	options := durable.TaskOptions{ConversationID: c.conv, Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}, AbandonOnRestart: abandon}
	if owner != 0 {
		options = durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipTask, TaskID: owner}, AbandonOnRestart: abandon}
	}
	id, err := durable.CommitValue(bg, c.session, func(tx *durable.Tx) (durable.TaskID, error) {
		return CreateTask[any](tx, task, nil, options)
	})
	must(t, err)
	return id
}

func (c crashState) task(t *testing.T, id durable.TaskID) durable.TaskRecord {
	t.Helper()
	record, err := c.storage.Task(bg, id)
	must(t, err)
	return *record
}

func (c crashState) rewrite(t *testing.T, records ...durable.TaskRecord) {
	t.Helper()
	writes := make([]durable.StorageWrite, len(records))
	for i, record := range records {
		writes[i] = durable.TaskWrite{Value: record}
	}
	_, err := c.storage.Commit(bg, writes)
	must(t, err)
}

// reopen closes the plain Session and opens a Harness over the storage.
func (c crashState) reopen(t *testing.T, tasks ...Task) *Harness {
	t.Helper()
	must(t, c.session.Close(bg))
	registry := NewRegistry()
	if len(tasks) > 0 {
		must(t, registry.Install(&Extension{Name: "tasks", Tasks: tasks}))
	}
	h, err := Open(bg, &survivingStorage{c.storage}, Options{Registry: registry})
	must(t, err)
	t.Cleanup(func() { closeSoon(t, h) })
	return h
}

// survivingStorage keeps memory storage open across a Session's close.
type survivingStorage struct{ *memory.Storage }

func (survivingStorage) Close(context.Context) error { return nil }

func TestBarrierLetsAFailFastRequestInTheSamePassOverrideARestartMark(t *testing.T) {
	c := newCrashState(t)
	parent := c.create(t, dormant, 0, false)
	failed := c.create(t, dormant, parent, false)
	child := c.create(t, dormant, parent, false)
	// A restart-abandoned owner that waits failFast on a failed child and a
	// live one.
	owner := c.task(t, parent)
	owner.AbortRequested, owner.AbortReason = true, durable.AbortReasonRestart
	owner.State = durable.TaskState{Status: durable.TaskWaiting, Checkpoint: json.RawMessage(`{"phase":"work"}`), On: []durable.TaskID{failed, child}, Policy: durable.JoinFailFast}
	failedRecord := c.task(t, failed)
	failedRecord.State = durable.TaskState{Status: durable.TaskTerminal, Outcome: &durable.TaskOutcome{Status: durable.OutcomeFailed, Error: &durable.TaskOutcomeError{Message: "failed"}}}
	c.rewrite(t, owner, failedRecord)
	// The child's definition is missing: a restart mark would keep it
	// waiting, a request orphans it.
	h := c.reopen(t)
	must(t, h.Resume())
	eventually(t, func() bool { return getTask(t, h, child).State.Status == durable.TaskTerminal })
	if got := outcomeJSON(t, *getTask(t, h, child)); got != `{"status":"orphaned","reason":"missing_task"}` {
		t.Fatalf("child = %s", got)
	}
}

func TestBarrierUpgradesRestartMarksDownTheTreeWhenAnAbortIsRequestedAbove(t *testing.T) {
	c := newCrashState(t)
	top := c.create(t, dormant, 0, false)
	middle := c.create(t, dormant, top, true)
	missing := MustDefineTask(dormantSpec("test.missing"))
	leaf := c.create(t, missing, middle, false)
	// Only dormant is registered: the leaf waits for its definition under the
	// restart cascade.
	h := c.reopen(t, dormant)
	must(t, h.Resume())
	eventually(t, func() bool { return getTask(t, h, leaf).AbortReason == durable.AbortReasonRestart })
	eventually(t, func() bool {
		inspection, err := h.Inspect(bg)
		must(t, err)
		for _, task := range inspection.Tasks {
			if task.Record.ID == leaf {
				return task.Kind == "blocked"
			}
		}
		return false
	})
	if status := getTask(t, h, leaf).State.Status; status == durable.TaskTerminal {
		t.Fatal("the leaf ended under a restart mark")
	}
	// A request on the top task reaches the leaf two levels down, which is
	// then orphaned.
	if _, err := h.AbortTask(bg, top); err != nil {
		t.Fatal(err)
	}
	for _, each := range []struct {
		id     durable.TaskID
		status string
	}{{leaf, durable.OutcomeOrphaned}, {middle, durable.OutcomeAborted}, {top, durable.OutcomeAborted}} {
		if got := waitDone(t, h, each.id); got.State.Outcome.Status != each.status {
			t.Fatalf("task %d = %s, want %s", each.id, outcomeJSON(t, got), each.status)
		}
	}
}

func TestBarrierMarksAFlaggedTaskThatHoldsItsOutcomeAbortingItsWorkAndKeepingTheOutcome(t *testing.T) {
	c := newCrashState(t)
	holder := c.create(t, dormant, 0, true)
	child := c.create(t, dormant, holder, false)
	record := c.task(t, holder)
	record.State = durable.TaskState{Status: durable.TaskCompleting, Outcome: &durable.TaskOutcome{Status: durable.OutcomeCompleted, Result: json.RawMessage("null")}}
	c.rewrite(t, record)
	h := c.reopen(t, dormant)
	must(t, h.Resume())
	if got := waitDone(t, h, child); got.State.Outcome.Status != durable.OutcomeAborted {
		t.Fatalf("child = %s", outcomeJSON(t, got))
	}
	settled := waitDone(t, h, holder)
	if settled.AbortReason != durable.AbortReasonRestart || settled.State.Outcome.Status != durable.OutcomeCompleted {
		t.Fatalf("holder = %+v", settled)
	}
}

// versioned is a one-phase task that completes once release is closed (nil
// does not wait); migrate, when set, runs when an older stored version
// migrates.
func versioned(t *testing.T, name string, version int, release <-chan struct{}, migrate func() error) Task {
	t.Helper()
	spec := TaskSpec[any, stepState, any]{
		Name:    name,
		Version: version,
		Initial: func(any) stepState { return stepState{Phase: "run"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"run": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				if release != nil {
					<-release
				}
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(context.Context, *stepTask, *TaskRuntime) error { return nil },
	}
	if migrate != nil {
		spec.Migrate = func(json.RawMessage, json.RawMessage, int) (any, stepState, error) {
			return nil, stepState{Phase: "run"}, migrate()
		}
	}
	return MustDefineTask(spec)
}

func inspectionOf(inspection HarnessInspection, id durable.TaskID) *TaskInspection {
	for i := range inspection.Tasks {
		if inspection.Tasks[i].Record.ID == id {
			return &inspection.Tasks[i]
		}
	}
	return nil
}

func expectInspection(t *testing.T, got *TaskInspection, kind string, migrates bool, reason, errText string, on ...durable.TaskID) {
	t.Helper()
	if got == nil {
		t.Fatalf("no inspection, want %s", kind)
	}
	gotErr := ""
	if got.Error != nil {
		gotErr = got.Error.Error()
	}
	if got.Kind != kind || got.Migrates != migrates || got.Reason != reason || gotErr != errText || !reflect.DeepEqual(got.On, on) && len(on)+len(got.On) > 0 {
		t.Fatalf("inspection = %+v, want %s %v %s %q %v", got, kind, migrates, reason, errText, on)
	}
}

func TestInspectDerivesEveryLiveTasksStateWithoutRunningTaskCode(t *testing.T) {
	release := make(chan struct{})
	gateTask := versioned(t, "test.gate", 1, release, nil)
	var gateID durable.TaskID
	dependent := MustDefineTask(TaskSpec[any, stepState, any]{
		Name:    "test.dependent",
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "wait"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"wait": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return Wait([]durable.TaskID{gateID}, durable.JoinAllSettled, stepState{Phase: "done"})
				})
			},
			"done": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(context.Context, *stepTask, *TaskRuntime) error { return nil },
	})
	var mu sync.Mutex
	migrations := 0
	registered := []Task{
		gateTask,
		dependent,
		versioned(t, "test.migrating", 2, nil, func() error {
			mu.Lock()
			migrations++
			mu.Unlock()
			return nil
		}),
		versioned(t, "test.no-migration", 2, nil, nil),
		versioned(t, "test.failing", 2, nil, func() error { return errors.New("cannot migrate") }),
		versioned(t, "test.too-old", 1, nil, nil),
	}
	o := openRoot(t, registered, openOptions{})
	type ids struct{ gate, dependent, migrating, noMigration, failing, tooOld, missing durable.TaskID }
	var created ids
	createOwned := func(tx *durable.Tx, task Task) (durable.TaskID, error) {
		return CreateTask[any](tx, task, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
	}
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		steps := []struct {
			into *durable.TaskID
			task Task
		}{
			{&created.gate, gateTask},
			{&created.dependent, dependent},
			// Stored by definitions other than the registered ones.
			{&created.migrating, versioned(t, "test.migrating", 1, nil, nil)},
			{&created.noMigration, versioned(t, "test.no-migration", 1, nil, nil)},
			{&created.failing, versioned(t, "test.failing", 1, nil, nil)},
			{&created.tooOld, versioned(t, "test.too-old", 2, nil, nil)},
			{&created.missing, versioned(t, "test.missing", 1, nil, nil)},
		}
		for _, step := range steps {
			if *step.into, err = createOwned(tx, step.task); err != nil {
				return err
			}
		}
		return nil
	}))
	gateID = created.gate

	paused, err := o.h.Inspect(bg)
	must(t, err)
	if paused.Scheduling != "paused" {
		t.Fatalf("scheduling = %s", paused.Scheduling)
	}
	all := []durable.TaskID{created.gate, created.dependent, created.migrating, created.noMigration, created.failing, created.tooOld, created.missing}
	if got := names(paused.Tasks, func(i TaskInspection) durable.TaskID { return i.Record.ID }); !reflect.DeepEqual(got, all) {
		t.Fatalf("tasks = %v", got)
	}
	expectInspection(t, inspectionOf(paused, created.gate), "ready", false, "", "")
	expectInspection(t, inspectionOf(paused, created.dependent), "ready", false, "", "")
	expectInspection(t, inspectionOf(paused, created.migrating), "ready", true, "", "")
	// A migration that was never tried is not run to find out.
	expectInspection(t, inspectionOf(paused, created.failing), "ready", true, "", "")
	expectInspection(t, inspectionOf(paused, created.noMigration), "blocked", false, blockedMigrationFailed, "Task test.no-migration version 2 has no migration from 1")
	expectInspection(t, inspectionOf(paused, created.tooOld), "blocked", false, blockedTaskTooOld, "")
	expectInspection(t, inspectionOf(paused, created.missing), "blocked", false, blockedMissingTask, "")
	if migrations != 0 {
		t.Fatalf("migrations = %d", migrations)
	}

	must(t, o.h.Resume())
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return migrations == 1
	})
	waitDone(t, o.h, created.migrating)
	var running HarnessInspection
	eventually(t, func() bool {
		running, err = o.h.Inspect(bg)
		must(t, err)
		g, d := inspectionOf(running, created.gate), inspectionOf(running, created.dependent)
		return g != nil && g.Kind == "running" && d != nil && d.Kind == "waiting"
	})
	expectInspection(t, inspectionOf(running, created.dependent), "waiting", false, "", "", created.gate)
	if running.Scheduling != "running" || inspectionOf(running, created.migrating) != nil {
		t.Fatalf("running = %+v", running)
	}
	expectInspection(t, inspectionOf(running, created.failing), "blocked", false, blockedMigrationFailed, "cannot migrate")

	close(release)
	waitDone(t, o.h, created.dependent)
	settled, err := o.h.Inspect(bg)
	must(t, err)
	if got := names(settled.Tasks, func(i TaskInspection) durable.TaskID { return i.Record.ID }); !reflect.DeepEqual(got, []durable.TaskID{created.noMigration, created.failing, created.tooOld, created.missing}) {
		t.Fatalf("settled tasks = %v", got)
	}
}
