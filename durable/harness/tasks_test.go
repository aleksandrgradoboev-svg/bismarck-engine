package harness

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// pi packages/durable/test/harness-tasks.test.ts.

// checkpointOf decodes a record's checkpoint.
func checkpointOf[S any](t *testing.T, record durable.TaskRecord) S {
	t.Helper()
	var s S
	must(t, json.Unmarshal(record.State.Checkpoint, &s))
	return s
}

func TestTaskContinuesOneInvocationThroughProgressAndCompletesWithATypedResult(t *testing.T) {
	type counter struct {
		Phase string `json:"phase"`
		N     int    `json:"n"`
	}
	var mu sync.Mutex
	var seen []int
	runtimes := map[*TaskRuntime]bool{}
	task := MustDefineTask(TaskSpec[struct{ To int }, counter, int]{
		Name:    "test.counter",
		Version: 1,
		Initial: func(struct{ To int }) counter { return counter{Phase: "count"} },
		Phases: map[string]func(context.Context, *Running[struct{ To int }, counter], *TaskRuntime) error{
			"count": func(ctx context.Context, task *Running[struct{ To int }, counter], rt *TaskRuntime) error {
				mu.Lock()
				seen = append(seen, task.Checkpoint.N)
				runtimes[rt] = true
				mu.Unlock()
				return rt.Commit(ctx, func(_ *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
					n := checkpointOf[counter](t, current).N + 1
					if n == task.Input.To {
						return Complete(n)
					}
					return Next(counter{Phase: "count", N: n})
				})
			},
		},
		Abort: func(context.Context, *Running[struct{ To int }, counter], *TaskRuntime) error { return nil },
	})
	o := openRoot(t, []Task{task}, openOptions{})
	var id durable.TaskID
	must(t, o.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		id, err = CreateTask(tx, task, struct{ To int }{3}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		return err
	}))
	must(t, o.h.Resume())
	receipt := waitDone(t, o.h, id)
	if got := outcomeJSON(t, receipt); got != `{"status":"completed","result":3}` {
		t.Fatalf("outcome = %s", got)
	}
	if !reflect.DeepEqual(seen, []int{0, 1, 2}) || len(runtimes) != 1 {
		t.Fatalf("seen = %v, runtimes = %d", seen, len(runtimes))
	}
}

func TestTaskFaultsAPhaseWithoutDurableProgressAndAFailingPhase(t *testing.T) {
	idle := oneStep(t, "test.idle", func(context.Context, *stepTask, *TaskRuntime) error { return nil }, nil)
	documentOnly := oneStep(t, "test.document-only", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		// A commit that returns no state is not progress.
		return rt.Commit(ctx, noChange)
	}, nil)
	fails := oneStep(t, "test.throws", func(context.Context, *stepTask, *TaskRuntime) error { return errors.New("boom") }, nil)
	panics := oneStep(t, "test.panics", func(context.Context, *stepTask, *TaskRuntime) error { panic("kaboom") }, nil)
	o := openRoot(t, []Task{idle, documentOnly, fails, panics}, openOptions{})
	ids := []durable.TaskID{start(t, o.root, idle, false), start(t, o.root, documentOnly, false), start(t, o.root, fails, false), start(t, o.root, panics, false)}
	must(t, o.h.Resume())
	want := []string{
		`{"status":"faulted","error":{"message":"Task test.idle phase run returned without durable progress"}}`,
		`{"status":"faulted","error":{"message":"Task test.document-only phase run returned without durable progress"}}`,
		`{"status":"faulted","error":{"message":"boom"}}`,
		`{"status":"faulted","error":{"message":"kaboom"}}`,
	}
	for i, id := range ids {
		if got := outcomeJSON(t, waitDone(t, o.h, id)); got != want[i] {
			t.Fatalf("outcome %d = %s, want %s", i, got, want[i])
		}
	}
}

func TestTaskKeepsACommittedTerminalOutcomeWhenTheHandlerFailsAfterwards(t *testing.T) {
	late := make(chan error, 1)
	done := oneStep(t, "test.done", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		if err := rt.Commit(ctx, completeWith("ok")); err != nil {
			return err
		}
		late <- rt.Commit(ctx, noChange)
		return errors.New("after terminal")
	}, nil)
	o := openRoot(t, []Task{done}, openOptions{})
	id := start(t, o.root, done, false)
	must(t, o.h.Resume())
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"completed","result":"ok"}` {
		t.Fatalf("outcome = %s", got)
	}
	expectError(t, <-late, "is terminal")
}

func TestTaskComparesCheckpointsByValueIncludingArrays(t *testing.T) {
	type collect struct {
		Phase string   `json:"phase"`
		Items []string `json:"items"`
	}
	task := MustDefineTask(TaskSpec[any, collect, any]{
		Name:    "test.collect",
		Version: 1,
		Initial: func(any) collect { return collect{Phase: "collect", Items: []string{}} },
		Phases: map[string]func(context.Context, *Running[any, collect], *TaskRuntime) error{
			"collect": func(ctx context.Context, task *Running[any, collect], rt *TaskRuntime) error {
				items := task.Checkpoint.Items
				// Two rounds of progress, then an equal copy of the checkpoint,
				// which is no progress.
				next := append([]string{}, items...)
				if len(items) < 2 {
					next = append(next, "item"+string(rune('0'+len(items))))
				}
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return Next(collect{Phase: "collect", Items: next})
				})
			},
		},
		Abort: func(context.Context, *Running[any, collect], *TaskRuntime) error { return nil },
	})
	o := openRoot(t, []Task{task}, openOptions{})
	id := start(t, o.root, task, false)
	must(t, o.h.Resume())
	if got := outcomeJSON(t, waitDone(t, o.h, id)); got != `{"status":"faulted","error":{"message":"Task test.collect phase collect returned without durable progress"}}` {
		t.Fatalf("outcome = %s", got)
	}
}

func TestTaskCommitsResultsWithEntriesKeepsMemosUntilTerminalAndRetiresTaskDocuments(t *testing.T) {
	progress := durable.MustDefineDoc(durable.DocDef{
		Kind:    "test.task-progress",
		Version: 1,
		Scope:   durable.ScopeTask,
		Initial: func(any) map[string]any { return map[string]any{"lines": []any{}} },
	})
	child := oneStep(t, "test.child", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	var childID durable.TaskID
	var progressSeen any
	var failures []string
	var mu sync.Mutex
	fail := func(format string, args ...any) {
		mu.Lock()
		failures = append(failures, strings.TrimSpace(format))
		mu.Unlock()
		t.Errorf(format, args...)
	}
	writer := MustDefineTask(TaskSpec[any, stepState, any]{
		Name:    "test.writer",
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "write"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"write": func(ctx context.Context, task *stepTask, rt *TaskRuntime) error {
				if memo, err := rt.Memo(ctx, "choice"); err != nil || memo != nil {
					fail("initial memo = %s, %v", memo, err)
				}
				// Two writers race; both get the one durable winner.
				first := async(func() (json.RawMessage, error) { return rt.MemoOr(ctx, "choice", "a") })
				second := async(func() (json.RawMessage, error) { return rt.MemoOr(ctx, "choice", "b") })
				a, errA := await(t, first)
				b, errB := await(t, second)
				if errA != nil || errB != nil || string(a) != string(b) {
					fail("winners = %s, %s (%v, %v)", a, b, errA, errB)
				}
				if memo, _ := rt.Memo(ctx, "choice"); string(memo) != string(a) {
					fail("memo = %s, want %s", memo, a)
				}
				if own, err := rt.MemoOr(ctx, "toString", "own"); err != nil || string(own) != `"own"` {
					fail("toString memo = %s, %v", own, err)
				}
				return rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
					doc, err := tx.Doc(progress.Task(task.Record.ID))
					if err != nil {
						return nil, err
					}
					lines := doc.At("lines")
					if _, err := lines.Push("wrote"); err != nil {
						return nil, err
					}
					// Task creation defaults to the task's own conversation.
					if childID, err = CreateTask[any](tx, child, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}}); err != nil {
						return nil, err
					}
					return Next(stepState{Phase: "answer"})
				})
			},
			"answer": func(ctx context.Context, task *stepTask, rt *TaskRuntime) error {
				if len(task.Record.Memos) != 2 || task.Record.Memos["toString"] == nil {
					fail("memos = %v", task.Record.Memos)
				}
				if err := rt.Commit(ctx, func(tx *durable.Tx, _ durable.TaskRecord) (*durable.TaskState, error) {
					doc, err := tx.Doc(progress.Task(task.Record.ID))
					if err != nil {
						return nil, err
					}
					progressSeen, err = doc.Snapshot()
					return nil, err
				}); err != nil {
					return err
				}
				return rt.Commit(ctx, func(tx *durable.Tx, current durable.TaskRecord) (*durable.TaskState, error) {
					entry, err := tx.AppendEntry(current.ConversationID, durable.EntryDraft{Kind: "answer", Model: []ai.Message{user("done")}})
					if err != nil {
						return nil, err
					}
					return Complete(map[string]any{"entryId": entry.ID})
				})
			},
		},
		Abort: func(context.Context, *stepTask, *TaskRuntime) error { return nil },
	})
	o := openRoot(t, []Task{writer, child}, openOptions{})
	conversation, err := o.h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	id := start(t, conversation, writer, false)
	must(t, o.h.Resume())
	receipt := waitDone(t, o.h, id)
	if receipt.State.Checkpoint != nil || receipt.Memos != nil {
		t.Fatalf("terminal record keeps checkpoint %s or memos %v", receipt.State.Checkpoint, receipt.Memos)
	}
	var result struct {
		EntryID durable.EntryID `json:"entryId"`
	}
	must(t, json.Unmarshal(receipt.State.Outcome.Result, &result))
	var entry *durable.EntryRecord
	must(t, o.h.Commit(bg, func(tx *durable.Tx) error {
		var err error
		entry, err = tx.Entry(result.EntryID)
		return err
	}))
	if !durable.MustDefineEntry("answer").Is(entry) || entry.ConversationID != conversation.ID {
		t.Fatalf("entry = %+v", entry)
	}
	if got := jsonOf(t, progressSeen); got != `{"lines":["wrote"]}` {
		t.Fatalf("progress seen = %s", got)
	}
	if value := snapshotOf(t, o.h, progress.Task(id)); value != nil {
		t.Fatalf("task document not retired: %v", value)
	}
	if got := getTask(t, o.h, id); !reflect.DeepEqual(*got, receipt) {
		t.Fatalf("getTask = %+v, want %+v", got, receipt)
	}
	if got := waitDone(t, o.h, childID); got.ConversationID != conversation.ID {
		t.Fatalf("child conversation = %d", got.ConversationID)
	}
}

// waitPhases is the checkpoint of a task that waits, then resumes.
type waitPhases = stepState

func waiterTask(t *testing.T, name string, on *[]durable.TaskID, resume func(ctx context.Context, rt *TaskRuntime) error) Task {
	t.Helper()
	return MustDefineTask(TaskSpec[any, waitPhases, any]{
		Name:    name,
		Version: 1,
		Initial: func(any) waitPhases { return waitPhases{Phase: "wait"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"wait": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) {
					return Wait(*on, durable.JoinAllSettled, waitPhases{Phase: "resume"})
				})
			},
			"resume": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error { return resume(ctx, rt) },
		},
		Abort: func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
			return rt.Commit(ctx, abortWith("test"))
		},
	})
}

func TestTaskResumesAWaitingTaskOnceEveryTaskInOnIsTerminalWhateverTheOutcome(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	note := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	var on []durable.TaskID
	var outcomes []string
	waiter := waiterTask(t, "test.waiter", &on, func(ctx context.Context, rt *TaskRuntime) error {
		note("resume")
		got, err := rt.Outcomes(ctx, on)
		if err != nil {
			return err
		}
		for _, outcome := range got {
			outcomes = append(outcomes, outcome.Status)
		}
		return rt.Commit(ctx, completeWith(nil))
	})
	first := oneStep(t, "test.first", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		note("first")
		<-release
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	faulting := oneStep(t, "test.faulting", func(context.Context, *stepTask, *TaskRuntime) error {
		note("faulting")
		return errors.New("fails")
	}, nil)
	o := openRoot(t, []Task{first, faulting, waiter}, openOptions{})
	on = []durable.TaskID{start(t, o.root, first, false), start(t, o.root, faulting, false)}
	waiterID := start(t, o.root, waiter, false)
	must(t, o.h.Resume())
	eventually(t, func() bool {
		record := getTask(t, o.h, waiterID)
		mu.Lock()
		defer mu.Unlock()
		return len(order) >= 2 && record.State.Status == durable.TaskWaiting
	})
	if record := getTask(t, o.h, waiterID); !reflect.DeepEqual(record.State.On, on) {
		t.Fatalf("waiter state = %+v", record.State)
	}
	close(release)
	waitDone(t, o.h, waiterID)
	mu.Lock()
	last := order[len(order)-1]
	mu.Unlock()
	if last != "resume" || !reflect.DeepEqual(outcomes, []string{"completed", "faulted"}) {
		t.Fatalf("order = %v, outcomes = %v", order, outcomes)
	}
}

// pi #10549.
func TestTaskStampsStartedAtAtTheFirstRunAndEndedAtWhenTerminal(t *testing.T) {
	var clock atomicMillis
	clock.set(1_000)
	release := make(chan struct{})
	var on []durable.TaskID
	waiter := waiterTask(t, "test.timed-waiter", &on, func(ctx context.Context, rt *TaskRuntime) error {
		return rt.Commit(ctx, completeWith(nil))
	})
	held := gatedTask(t, "test.timed-held", release)
	o := openRoot(t, []Task{waiter, held}, openOptions{now: clock.now})
	heldID := start(t, o.root, held, false)
	on = []durable.TaskID{heldID}
	waiterID := start(t, o.root, waiter, false)
	times := func() [][3]int64 {
		inspection, err := o.h.Inspect(bg)
		must(t, err)
		var out [][3]int64
		for _, task := range inspection.Tasks {
			out = append(out, [3]int64{int64(task.Record.ID), deref(task.Record.StartedAt), deref(task.Record.EndedAt)})
		}
		return out
	}
	if got := times(); !reflect.DeepEqual(got, [][3]int64{{int64(heldID), -1, -1}, {int64(waiterID), -1, -1}}) {
		t.Fatalf("times = %v", got)
	}
	clock.set(2_000)
	must(t, o.h.Resume())
	eventually(t, func() bool { return getTask(t, o.h, waiterID).State.Status == durable.TaskWaiting })
	if got := times(); !reflect.DeepEqual(got, [][3]int64{{int64(heldID), 2_000, -1}, {int64(waiterID), 2_000, -1}}) {
		t.Fatalf("times = %v", got)
	}
	// The waiter runs again at 5000; its start stays the first run.
	clock.set(5_000)
	close(release)
	settledWaiter := waitDone(t, o.h, waiterID)
	settledHeld := getTask(t, o.h, heldID)
	if deref(settledHeld.StartedAt) != 2_000 || deref(settledHeld.EndedAt) != 5_000 ||
		deref(settledWaiter.StartedAt) != 2_000 || deref(settledWaiter.EndedAt) != 5_000 {
		t.Fatalf("held %v-%v, waiter %v-%v", settledHeld.StartedAt, settledHeld.EndedAt, settledWaiter.StartedAt, settledWaiter.EndedAt)
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// atomicMillis is a settable test clock in Unix milliseconds.
type atomicMillis struct {
	mu sync.Mutex
	ms int64
}

func (c *atomicMillis) set(ms int64) {
	c.mu.Lock()
	c.ms = ms
	c.mu.Unlock()
}

func (c *atomicMillis) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.UnixMilli(c.ms)
}

func TestTaskKeepsOneRegistrySnapshotPerPhaseAndRefreshesItAtTheBoundary(t *testing.T) {
	type seenPhase struct {
		phase string
		tools []string
		same  bool
	}
	var seen []seenPhase
	phaseGate, entered := make(chan struct{}), make(chan struct{})
	tools := func(snapshot *RegistrySnapshot) []string {
		return names(snapshot.Tools(), func(e ExtensionTool) string { return e.Tool.Name })
	}
	task := MustDefineTask(TaskSpec[any, stepState, any]{
		Name:    "test.snapshots",
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "a"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"a": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				before := rt.Registry()
				close(entered)
				<-phaseGate
				seen = append(seen, seenPhase{"a", tools(rt.Registry()), rt.Registry() == before})
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) { return Next(stepState{Phase: "b"}) })
			},
			"b": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				seen = append(seen, seenPhase{"b", tools(rt.Registry()), true})
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(context.Context, *stepTask, *TaskRuntime) error { return nil },
	})
	o := openRoot(t, []Task{task}, openOptions{})
	id := start(t, o.root, task, false)
	must(t, o.h.Resume())
	<-entered
	addTool(t, o.registry, testTool("late"))
	close(phaseGate)
	waitDone(t, o.h, id)
	want := []seenPhase{{"a", []string{}, true}, {"b", []string{"late"}, true}}
	if len(seen) == 2 && seen[0].tools == nil {
		seen[0].tools = []string{}
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen = %+v, want %+v", seen, want)
	}
}

// pingHooks are the hooks of the agent-phases test task.
type pingHooks struct{ Ping func() }

func TestTaskResolvesTheAgentAtFirstUseInAPhaseAndKeepsItForThePhase(t *testing.T) {
	var mu sync.Mutex
	var pings []string
	type seenPhase struct {
		phase    string
		pings    []string
		thinking string
		same     bool
	}
	var seen []seenPhase
	beforeUse, afterUse := make(chan struct{}), make(chan struct{})
	waitingBeforeUse, waitingAfterUse := make(chan struct{}), make(chan struct{})
	ping := func(ctx context.Context, rt *TaskRuntime) []string {
		mu.Lock()
		pings = nil
		mu.Unlock()
		must(t, EachHook(ctx, rt, func(h *pingHooks) error {
			if h.Ping != nil {
				h.Ping()
			}
			return nil
		}))
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), pings...)
	}
	push := func(s string) func() {
		return func() {
			mu.Lock()
			pings = append(pings, s)
			mu.Unlock()
		}
	}
	task := MustDefineTask(TaskSpec[any, stepState, any]{
		Name:    "test.agent-phases",
		Version: 1,
		Initial: func(any) stepState { return stepState{Phase: "a"} },
		Phases: map[string]func(context.Context, *stepTask, *TaskRuntime) error{
			"a": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				close(waitingBeforeUse)
				<-beforeUse
				first, err := rt.Agent(ctx)
				if err != nil {
					return err
				}
				seen = append(seen, seenPhase{phase: "a", pings: ping(ctx, rt), thinking: string(first.ThinkingLevel)})
				close(waitingAfterUse)
				<-afterUse
				second, err := rt.Agent(ctx)
				if err != nil {
					return err
				}
				seen = append(seen, seenPhase{"a", ping(ctx, rt), string(second.ThinkingLevel), second == first})
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) { return Next(stepState{Phase: "b"}) })
			},
			"b": func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
				agent, err := rt.Agent(ctx)
				if err != nil {
					return err
				}
				seen = append(seen, seenPhase{phase: "b", pings: ping(ctx, rt), thinking: string(agent.ThinkingLevel)})
				return rt.Commit(ctx, completeWith(nil))
			},
		},
		Abort: func(context.Context, *stepTask, *TaskRuntime) error { return nil },
	})
	o := openRoot(t, []Task{task}, openOptions{})
	must(t, o.registry.Install(&Extension{Name: "hooks:1", Hooks: []HookRegistration{Hook(task, &pingHooks{Ping: push("before")})}}))
	id := start(t, o.root, task, false)
	must(t, o.h.Resume())
	// Configured during the phase but before its first use: the lazy
	// resolution reads it.
	<-waitingBeforeUse
	must(t, o.root.Configure(bg, AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("low")}))
	close(beforeUse)
	// Configured and installed after the first use: not seen until the next
	// phase.
	<-waitingAfterUse
	must(t, o.registry.Install(&Extension{Name: "hooks:2", Hooks: []HookRegistration{Hook(task, &pingHooks{Ping: push("late")})}}))
	must(t, o.root.Configure(bg, AgentChange{ThinkingLevel: Set[ai.ModelThinkingLevel]("high")}))
	close(afterUse)
	waitDone(t, o.h, id)
	want := []seenPhase{
		{"a", []string{"before"}, "low", false},
		{"a", []string{"before"}, "low", true},
		{"b", []string{"before", "late"}, "high", false},
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen = %+v, want %+v", seen, want)
	}
}

func TestTaskFailsAnAgentWaitWhoseCallerIsCancelledWithoutAffectingTheResolution(t *testing.T) {
	var cancelled error
	var resolved string
	task := oneStep(t, "test.agent-cancel", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		caller, cancel := context.WithCancelCause(ctx)
		cancel(errors.New("caller gone"))
		_, cancelled = rt.Agent(caller)
		agent, err := rt.Agent(ctx)
		if err != nil {
			return err
		}
		resolved = string(agent.ThinkingLevel)
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	o := openRoot(t, []Task{task}, openOptions{})
	id := start(t, o.root, task, false)
	must(t, o.h.Resume())
	waitDone(t, o.h, id)
	expectError(t, cancelled, "caller gone")
	if resolved != "off" {
		t.Fatalf("resolved = %q", resolved)
	}
}

func TestTaskObservesAFailedAgentResolutionWhoseOnlyCallerStoppedWaiting(t *testing.T) {
	var fail sync.Map
	settings := func() HarnessSettings {
		if _, ok := fail.Load("on"); ok {
			panic(errors.New("settings broke"))
		}
		return HarnessSettings{}
	}
	var resolved error
	task := oneStep(t, "test.agent-failure", func(ctx context.Context, _ *stepTask, rt *TaskRuntime) error {
		caller, cancel := context.WithCancelCause(ctx)
		cancel(errors.New("caller gone"))
		fail.Store("on", true)
		_, _ = rt.Agent(caller)
		// Let an unobserved failure surface before the task ends.
		flush()
		_, resolved = rt.Agent(ctx)
		fail.Delete("on")
		return rt.Commit(ctx, completeWith(nil))
	}, nil)
	o := openRoot(t, []Task{task}, openOptions{settings: settings})
	id := start(t, o.root, task, false)
	must(t, o.h.Resume())
	waitDone(t, o.h, id)
	expectError(t, resolved, "settings broke")
}
