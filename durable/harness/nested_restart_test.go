package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Ported from pi packages/durable/test/harness-nested-tools-restart.test.ts,
// on JSONL in place of node's SQLite.

// openUnscheduled opens the JSONL Harness in dir without starting scheduling.
func openUnscheduled(t *testing.T, dir string, setup *chatSetup) (*Harness, *Conversation) {
	t.Helper()
	return openChat(t, openJSONL(t, dir), setup)
}

func nestedToolTasks(t *testing.T, h *Harness, root *Conversation) []durable.TaskRecord {
	t.Helper()
	var out []durable.TaskRecord
	for _, task := range allToolTasks(t, h, root) {
		if toolInput(t, task).Kind == toolCallNested {
			out = append(out, task)
		}
	}
	return out
}

func endResult(t *testing.T, events []AgentEvent, callID string) *NestedToolExecutionResult {
	t.Helper()
	end := endOf(events, callID)
	if end == nil {
		t.Fatalf("no end of %s", callID)
	}
	return end.Result
}

// recordingLog is an append-only list several goroutines write.
type recordingLog struct {
	mu    sync.Mutex
	items []string
}

func (l *recordingLog) add(item string) {
	l.mu.Lock()
	l.items = append(l.items, item)
	l.mu.Unlock()
}

func (l *recordingLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.items)
}

type workState struct {
	Phase string `json:"phase"`
}

// workerTask is a task whose one phase records "work", marks started and
// runs until cancelled; its abort records the abort reason (prefixed by
// abortPrefix) and ends it aborted.
func workerTask(t *testing.T, name string, phases *recordingLog, aborts *recordingLog, abortPrefix string, started chan struct{}) Task {
	t.Helper()
	var once sync.Once
	task, err := DefineTask(TaskSpec[any, workState, any]{
		Name:    name,
		Version: 1,
		Initial: func(any) workState { return workState{Phase: "work"} },
		Phases: map[string]func(context.Context, *Running[any, workState], *TaskRuntime) error{
			"work": func(ctx context.Context, _ *Running[any, workState], _ *TaskRuntime) error {
				if phases != nil {
					phases.add("work")
				}
				once.Do(func() { close(started) })
				<-ctx.Done()
				return context.Cause(ctx)
			},
		},
		Abort: func(ctx context.Context, task *Running[any, workState], rt *TaskRuntime) error {
			if aborts != nil {
				aborts.add(abortPrefix + task.Record.AbortReason)
			}
			return rt.Commit(ctx, abortWith(""))
		},
	})
	must(t, err)
	return task
}

// spawnTool creates worker owned by its call and waits for it; the created
// task's ID is stored in worker.
func spawnTool(worker Task, id *atomic.Int64) *ToolRegistration {
	return nestedTool("spawn", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		created, err := api.CreateTask(ctx, worker, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipTask, TaskID: api.TaskID()}})
		if err != nil {
			return ToolExecutionResult{}, err
		}
		id.Store(int64(created))
		_, err = api.WaitForTask(ctx, created)
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	})
}

// batchCalling is a tool that makes one nested call of name.
func batchCalling(name string) *ToolRegistration {
	return nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		_, err := api.ExecuteTool(ctx, name, map[string]any{}, NestedCallOptions{})
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	})
}

// admittedInOrder holds each attempt's next default-keyed call until the
// previous one took its key: goroutines take keys in no fixed order, pi's
// synchronous admissions in call order.
func admittedInOrder(t *testing.T) <-chan string {
	t.Helper()
	keys := make(chan string, 16)
	hook := func(key string) { keys <- key }
	beforeNestedAdmission.Store(&hook)
	t.Cleanup(func() { beforeNestedAdmission.Store(nil) })
	return keys
}

func TestNestedRestartAbandonsAReplayUnsafeCallersUnfinishedNestedCallsBeforeTheyRunAgain(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	var startedRuns, gatedRuns atomic.Int32
	running, gated := make(chan struct{}), make(chan struct{})
	var runningOnce, gatedOnce sync.Once
	// Replay-safe, so only the abandonment keeps it from rerunning.
	installTool(t, setup.registry, nestedTool("started", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		startedRuns.Add(1)
		if err := api.Output([]byte("partial\n"), nil); err != nil {
			return ToolExecutionResult{}, err
		}
		if err := api.Details(ctx, map[string]any{"step": 1}); err != nil {
			return ToolExecutionResult{}, err
		}
		runningOnce.Do(func() { close(running) })
		<-ctx.Done()
		return ToolExecutionResult{}, context.Cause(ctx)
	}, func(tool *ToolRegistration) { tool.Replay = ReplaySafe }))
	installTool(t, setup.registry, nestedTool("gated", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		gatedRuns.Add(1)
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	// Holds gated in its call phase, before intent, until the Harness
	// closes.
	installHooks(t, setup.registry, "hooks", ToolTask, &ToolHooks{BeforeTool: func(ctx context.Context, call ToolHookCall, _ *TaskRuntime) (*BeforeToolDecision, error) {
		if call.Name != "gated" {
			return nil, nil
		}
		gatedOnce.Do(func() { close(gated) })
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}})
	keys := admittedInOrder(t)
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		errs := make(chan error, 1)
		go func() {
			_, err := api.ExecuteTool(ctx, "started", map[string]any{}, NestedCallOptions{})
			errs <- err
		}()
		<-keys
		_, err := api.ExecuteTool(ctx, "gated", map[string]any{}, NestedCallOptions{})
		if first := <-errs; first != nil {
			return ToolExecutionResult{}, first
		}
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openUnscheduled(t, dir, setup)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, running, "started")
	awaitSignal(t, gated, "gated")
	must(t, h.Close(bg))

	h, root = openUnscheduled(t, dir, setup)
	_, log := listen(t, root)
	must(t, h.Resume())
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	must(t, h.WaitForIdle(bg))
	log.quiet(t)
	if startedRuns.Load() != 1 || gatedRuns.Load() != 0 {
		t.Fatalf("runs = started %d, gated %d", startedRuns.Load(), gatedRuns.Load())
	}
	nested := nestedToolTasks(t, h, root)
	if len(nested) != 2 {
		t.Fatalf("nested tasks = %d", len(nested))
	}
	for _, task := range nested {
		if status, _ := toolOutcome(task); !task.AbandonOnRestart || !task.AbortRequested || task.AbortReason != durable.AbortReasonRestart || task.State.Status != durable.TaskTerminal || status != durable.OutcomeAborted {
			t.Fatalf("nested task = %+v", task)
		}
	}
	all := log.events()
	started := endResult(t, all, "c1/1")
	if started == nil || !started.IsError || normalJSON(t, started.Details) != `{"step":1}` || started.StructuredOutput != nil {
		t.Fatalf("started end = %s", jsonOf(t, started))
	}
	expectStrings(t, "started codes", codes(started.Diagnostics), []string{"interrupted"})
	if gate := endResult(t, all, "c1/2"); gate == nil {
		t.Fatal("gated end has no result")
	} else {
		expectStrings(t, "gated codes", codes(gate.Diagnostics), []string{"abandoned"})
	}
	// The caller reports the interruption.
	result := toolResults(chatEntries(t, root))[0]
	if result.ToolCallID != "c1" || !result.IsError {
		t.Fatalf("result = %s", jsonOf(t, result))
	}
	expectIdleLive(t, h, root)
}

func TestNestedRestartAbandonsTheWorkAnAbandonedNestedCallOwnsWhichNeverRunsAPhaseAgain(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	phases, aborts := &recordingLog{}, &recordingLog{}
	started := make(chan struct{})
	worker := workerTask(t, "test.worker", phases, aborts, "", started)
	must(t, setup.registry.Install(&Extension{Name: "worker", Tasks: []Task{worker}}))
	var workerID atomic.Int64
	installTool(t, setup.registry, spawnTool(worker, &workerID))
	installTool(t, setup.registry, batchCalling("spawn"))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openUnscheduled(t, dir, setup)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, started, "the worker")
	must(t, h.Close(bg))

	h, _ = openRecovering(t, dir, setup, nil)
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	must(t, h.WaitForIdle(bg))
	expectStrings(t, "phases", phases.all(), []string{"work"})
	// The cascade passed the restart reason on.
	expectStrings(t, "aborts", aborts.all(), []string{durable.AbortReasonRestart})
	record, err := h.GetTask(bg, durable.TaskID(workerID.Load()))
	must(t, err)
	if status, _ := toolOutcome(*record); record.State.Status != durable.TaskTerminal || status != durable.OutcomeAborted {
		t.Fatalf("worker = %+v", record.State)
	}
}

func TestNestedRestartKeepsAnAbandonedTaskWhoseDefinitionIsMissingWaitingUntilItIsInstalledThenRunsItsAbort(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	aborts := &recordingLog{}
	started := make(chan struct{})
	worker := workerTask(t, "test.late", nil, aborts, "", started)
	extension := &Extension{Name: "late", Tasks: []Task{worker}}
	must(t, setup.registry.Install(extension))
	var workerID atomic.Int64
	installTool(t, setup.registry, spawnTool(worker, &workerID))
	installTool(t, setup.registry, batchCalling("spawn"))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openUnscheduled(t, dir, setup)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, started, "the worker")
	must(t, h.Close(bg))

	setup.registry.Uninstall(extension)
	h, _ = openRecovering(t, dir, setup, nil)
	eventually(t, func() bool {
		inspection, err := h.Inspect(bg)
		must(t, err)
		for _, task := range inspection.Tasks {
			if task.Record.ID == durable.TaskID(workerID.Load()) {
				return task.Kind == "blocked"
			}
		}
		return false
	})
	record, err := h.GetTask(bg, durable.TaskID(workerID.Load()))
	must(t, err)
	if record.State.Status == durable.TaskTerminal || len(aborts.all()) != 0 {
		t.Fatalf("worker = %+v, aborts = %v", record.State, aborts.all())
	}
	must(t, setup.registry.Install(extension))
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	expectStrings(t, "aborts", aborts.all(), []string{durable.AbortReasonRestart})
	record, err = h.GetTask(bg, durable.TaskID(workerID.Load()))
	must(t, err)
	if status, _ := toolOutcome(*record); record.State.Status != durable.TaskTerminal || status != durable.OutcomeAborted {
		t.Fatalf("worker = %+v", record.State)
	}
}

func TestNestedRestartOrphansAnAbandonedTaskWhoseDefinitionIsMissingOnceAnAbortIsRequested(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	started := make(chan struct{})
	worker := workerTask(t, "test.gone", nil, nil, "", started)
	extension := &Extension{Name: "gone", Tasks: []Task{worker}}
	must(t, setup.registry.Install(extension))
	var workerID atomic.Int64
	installTool(t, setup.registry, spawnTool(worker, &workerID))
	installTool(t, setup.registry, batchCalling("spawn"))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openUnscheduled(t, dir, setup)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, started, "the worker")
	must(t, h.Close(bg))

	setup.registry.Uninstall(extension)
	h, root = openRecovering(t, dir, setup, nil)
	eventually(t, func() bool {
		record, err := h.GetTask(bg, durable.TaskID(workerID.Load()))
		must(t, err)
		return record != nil && record.AbortReason == durable.AbortReasonRestart
	})
	must(t, root.Abort(bg, false))
	record, err := h.GetTask(bg, durable.TaskID(workerID.Load()))
	must(t, err)
	if outcome := record.State.Outcome; record.State.Status != durable.TaskTerminal || outcome == nil || outcome.Status != durable.OutcomeOrphaned || outcome.Reason != "missing_task" {
		t.Fatalf("worker = %+v", record.State)
	}
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionUnanswered {
		t.Fatalf("status = %s", status)
	}
}

func TestNestedRestartMarksNothingWhenTheReopenedHarnessNeverStartsScheduling(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	running := make(chan struct{})
	installTool(t, setup.registry, nestedTool("hang", func(ctx context.Context, _ map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		close(running)
		<-ctx.Done()
		return ToolExecutionResult{}, context.Cause(ctx)
	}))
	installTool(t, setup.registry, batchCalling("hang"))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openUnscheduled(t, dir, setup)
	submit(t, root, inputDraft("go"))
	awaitSignal(t, running, "the nested call")
	must(t, h.Close(bg))

	// An inspection-only open: no resume, no submission.
	h, _ = openUnscheduled(t, dir, setup)
	inspection, err := h.Inspect(bg)
	must(t, err)
	if inspection.Scheduling != "paused" {
		t.Fatalf("scheduling = %s", inspection.Scheduling)
	}
	must(t, h.Close(bg))

	h, root = openUnscheduled(t, dir, setup)
	nested := nestedToolTasks(t, h, root)
	if len(nested) != 1 || !nested[0].AbandonOnRestart || nested[0].AbortRequested || nested[0].State.Status != durable.TaskPending {
		t.Fatalf("nested = %+v", nested)
	}
}

func TestNestedRestartLetsAReplaySafeCallersUnfinishedNestedCallsRecoverAndReattachesToThemByKey(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	var safeRuns, unsafeRuns, callerRuns atomic.Int32
	runningSafe, runningUnsafe := make(chan struct{}), make(chan struct{})
	installTool(t, setup.registry, nestedTool("safe", func(ctx context.Context, _ map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		run := safeRuns.Add(1)
		if run == 1 {
			close(runningSafe)
			<-ctx.Done()
			return ToolExecutionResult{}, context.Cause(ctx)
		}
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: fmt.Sprintf("safe run %d", run)}}}, nil
	}, func(tool *ToolRegistration) { tool.Replay = ReplaySafe }))
	installTool(t, setup.registry, nestedTool("unsafe", func(ctx context.Context, _ map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		if unsafeRuns.Add(1) == 1 {
			close(runningUnsafe)
		}
		<-ctx.Done()
		return ToolExecutionResult{}, context.Cause(ctx)
	}))
	keys := admittedInOrder(t)
	var mu sync.Mutex
	var results []NestedToolExecutionResult
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		callerRuns.Add(1)
		got := make([]NestedToolExecutionResult, 2)
		errs := make(chan error, 1)
		go func() {
			var err error
			got[0], err = api.ExecuteTool(ctx, "safe", map[string]any{}, NestedCallOptions{})
			errs <- err
		}()
		<-keys
		var err error
		got[1], err = api.ExecuteTool(ctx, "unsafe", map[string]any{}, NestedCallOptions{})
		<-keys
		if first := <-errs; first != nil {
			return ToolExecutionResult{}, first
		}
		mu.Lock()
		results = got
		mu.Unlock()
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	}, func(tool *ToolRegistration) { tool.Replay = ReplaySafe }))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openUnscheduled(t, dir, setup)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, runningSafe, "safe")
	awaitSignal(t, runningUnsafe, "unsafe")
	must(t, h.Close(bg))

	h, root = openRecovering(t, dir, setup, nil)
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	if safeRuns.Load() != 2 || unsafeRuns.Load() != 1 || callerRuns.Load() != 2 {
		t.Fatalf("runs = safe %d, unsafe %d, caller %d", safeRuns.Load(), unsafeRuns.Load(), callerRuns.Load())
	}
	// Default keys follow call order, so the rerun found both calls.
	nested := nestedToolTasks(t, h, root)
	if len(nested) != 2 {
		t.Fatalf("nested tasks = %d", len(nested))
	}
	for _, task := range nested {
		if task.AbandonOnRestart || task.AbortReason != "" {
			t.Fatalf("nested task = %+v", task)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if nestedText(results[0]) != "safe run 2" {
		t.Fatalf("safe = %s", jsonOf(t, results[0]))
	}
	expectStrings(t, "unsafe codes", codes(results[1].Diagnostics), []string{"interrupted"})
}

func TestNestedRestartClearsTheSlotsOfNestedCallsAnEarlierAttemptFinishedWhenTheRerunMakesNone(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	finished := make(chan struct{})
	var attempts atomic.Int32
	installTool(t, setup.registry, returning("quick", ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "ok"}}}))
	installTool(t, setup.registry, nestedTool("caller", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		if attempts.Add(1) == 1 {
			if _, err := api.ExecuteTool(ctx, "quick", map[string]any{}, NestedCallOptions{}); err != nil {
				return ToolExecutionResult{}, err
			}
			close(finished)
			<-ctx.Done()
			return ToolExecutionResult{}, context.Cause(ctx)
		}
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "done"}}}, nil
	}, func(tool *ToolRegistration) { tool.Replay = ReplaySafe }))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("caller", nil), fauxText("done")})
	h, root := openUnscheduled(t, dir, setup)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, finished, "the nested call")
	// The finished nested call's slot is still listed, below its running
	// caller.
	if slots := liveNested(t, h, root); len(slots) != 1 {
		t.Fatalf("slots = %s", jsonOf(t, slots))
	}
	must(t, h.Close(bg))

	h, _ = openUnscheduled(t, dir, setup)
	var mu sync.Mutex
	slotsAfterCaller := -1
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			write, ok := change.(durable.TaskWrite)
			if !ok || write.Value.Kind != toolKind || write.Value.State.Status != durable.TaskTerminal {
				continue
			}
			var input ToolTaskInput
			if json.Unmarshal(write.Value.Input, &input) != nil || input.Kind != toolCallModel {
				continue
			}
			count := 0
			for _, other := range publication.Changes {
				if document, ok := other.(durable.DocumentChange); ok && document.Record.Kind == LiveDoc.Definition().Kind && document.Value != nil {
					list, _ := document.Value["nestedTools"].([]any)
					count = len(list)
				}
			}
			mu.Lock()
			slotsAfterCaller = count
			mu.Unlock()
		}
	})
	must(t, err)
	defer unsubscribe()
	must(t, h.Resume())
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
	// The caller's terminal commit removed the slot, though the rerun made
	// no nested call.
	mu.Lock()
	defer mu.Unlock()
	if slotsAfterCaller != 0 {
		t.Fatalf("slots after the caller = %d", slotsAfterCaller)
	}
}

func TestNestedRestartAbortsANestedCallAnEarlierAttemptOfAReplaySafeCallerLeftRunningAndTheRerunNeverMadeAgain(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	running := make(chan struct{})
	var runningOnce sync.Once
	var attempts atomic.Int32
	installTool(t, setup.registry, nestedTool("slow", func(ctx context.Context, _ map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		runningOnce.Do(func() { close(running) })
		<-ctx.Done()
		return ToolExecutionResult{}, context.Cause(ctx)
	}, func(tool *ToolRegistration) { tool.Replay = ReplaySafe }))
	installTool(t, setup.registry, nestedTool("caller", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		// Only the first attempt calls slow; the rerun after the restart
		// returns at once.
		if attempts.Add(1) == 1 {
			if _, err := api.ExecuteTool(ctx, "slow", map[string]any{}, NestedCallOptions{Key: "slow"}); err != nil {
				return ToolExecutionResult{}, err
			}
		}
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "done"}}}, nil
	}, func(tool *ToolRegistration) { tool.Replay = ReplaySafe }))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("caller", nil), fauxText("done")})
	h, root := openUnscheduled(t, dir, setup)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, running, "slow")
	must(t, h.Close(bg))

	h, root = openRecovering(t, dir, setup, nil)
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
	// The rerun's settlement found the leftover call among the tasks it owns
	// and aborted it.
	leftover := nestedToolTasks(t, h, root)[0]
	if status, _ := toolOutcome(leftover); leftover.State.Status != durable.TaskTerminal || status != durable.OutcomeAborted {
		t.Fatalf("leftover = %+v", leftover.State)
	}
}

func TestNestedRestartReattachesAReplaySafeCallerByDefaultAndExplicitKeysAndRejectsARerunThatCallsDifferently(t *testing.T) {
	for _, order := range []string{"same", "swapped"} {
		t.Run(order, func(t *testing.T) {
			dir := t.TempDir()
			setup := newChatSetup(t)
			runs := &recordingLog{}
			installTool(t, setup.registry, nestedTool("echo", func(_ context.Context, args map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
				text, _ := args["text"].(string)
				runs.add(text)
				return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "echo " + text}}}, nil
			}))
			blocked := make(chan struct{})
			var callerRuns atomic.Int32
			var seen []string
			installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
				run := callerRuns.Add(1)
				first := "a"
				if run == 2 && order == "swapped" {
					first = "b"
				}
				one, err := api.ExecuteTool(ctx, "echo", map[string]any{"text": first}, NestedCallOptions{})
				if err != nil {
					return ToolExecutionResult{}, err
				}
				named, err := api.ExecuteTool(ctx, "echo", map[string]any{"text": "named"}, NestedCallOptions{Key: "named"})
				if err != nil {
					return ToolExecutionResult{}, err
				}
				if run == 1 {
					close(blocked)
					<-ctx.Done()
					return ToolExecutionResult{}, context.Cause(ctx)
				}
				seen = []string{nestedText(one), nestedText(named)}
				return ToolExecutionResult{Output: ai.ContentList{}}, nil
			}, func(tool *ToolRegistration) { tool.Replay = ReplaySafe }))
			setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
			h, root := openUnscheduled(t, dir, setup)
			id := submit(t, root, inputDraft("go")).ID
			awaitSignal(t, blocked, "the first run")
			must(t, h.Close(bg))

			h, root = openRecovering(t, dir, setup, nil)
			if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
				t.Fatalf("status = %s", status)
			}
			result := toolResults(chatEntries(t, root))[0]
			expectStrings(t, "runs", runs.all(), []string{"a", "named"})
			if order == "same" {
				expectStrings(t, "seen", seen, []string{"echo a", "echo named"})
				if result.IsError {
					t.Fatalf("result = %s", jsonOf(t, result))
				}
				return
			}
			// Key 1 was made with other arguments: the rerun throws instead
			// of reusing a different call's result.
			if !result.IsError || !strings.Contains(jsonOf(t, result.Content), "Nested call c1/1 was already made with another tool") {
				t.Fatalf("result = %s", jsonOf(t, result))
			}
		})
	}
}

func TestNestedRestartAbandonsTheChildTasksAReplayUnsafeToolCreatedAndAwaitedInMemory(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	phases := &recordingLog{}
	started := make(chan struct{})
	worker := workerTask(t, "test.direct-worker", phases, phases, "abort ", started)
	must(t, setup.registry.Install(&Extension{Name: "direct-worker", Tasks: []Task{worker}}))
	var workerID atomic.Int64
	work := spawnTool(worker, &workerID)
	work.Name = "work"
	installTool(t, setup.registry, work)
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("work", nil), fauxText("done")})
	h, root := openUnscheduled(t, dir, setup)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, started, "the worker")
	must(t, h.Close(bg))

	h, _ = openRecovering(t, dir, setup, nil)
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	expectStrings(t, "phases", phases.all(), []string{"work", "abort " + durable.AbortReasonRestart})
}

func TestNestedRestartLeavesBackgroundWorkUnderAnAbandonedNestedCallRunningAndSettlesWithoutIt(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	started := make(chan struct{})
	background := workerTask(t, "test.background", nil, nil, "", started)
	must(t, setup.registry.Install(&Extension{Name: "background", Tasks: []Task{background}}))
	var backgroundID atomic.Int64
	installTool(t, setup.registry, nestedTool("spawn", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		// A conversation the call owns, with background work in it, as a
		// persistent subagent has.
		err := api.Commit(ctx, func(tx *durable.Tx) error {
			child, err := tx.CreateConversation(durable.ConversationOwnership{Kind: durable.OwnershipTask, TaskID: api.TaskID()})
			if err != nil {
				return err
			}
			id, err := CreateTask[any](tx, background, nil, durable.TaskOptions{
				ConversationID: child.ID,
				Ownership:      durable.TaskOwnership{Kind: durable.OwnershipConversation},
				Background:     true,
			})
			backgroundID.Store(int64(id))
			return err
		})
		if err != nil {
			return ToolExecutionResult{}, err
		}
		<-ctx.Done()
		return ToolExecutionResult{}, context.Cause(ctx)
	}))
	installTool(t, setup.registry, batchCalling("spawn"))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openUnscheduled(t, dir, setup)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, started, "the background work")
	must(t, h.Close(bg))

	h, root = openRecovering(t, dir, setup, nil)
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	spawn := nestedToolTasks(t, h, root)[0]
	if spawn.AbortReason != durable.AbortReasonRestart || spawn.State.Status != durable.TaskTerminal {
		t.Fatalf("spawn = %+v", spawn)
	}
	record, err := h.GetTask(bg, durable.TaskID(backgroundID.Load()))
	must(t, err)
	if record.AbortRequested || record.State.Status != durable.TaskRunning {
		t.Fatalf("background = %+v", record)
	}
	_, err = h.AbortTask(bg, durable.TaskID(backgroundID.Load()))
	must(t, err)
}
