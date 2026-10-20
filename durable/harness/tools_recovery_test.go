package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
)

// Ported from pi packages/durable/test/harness-tools-recovery.test.ts, on
// JSONL in place of node's SQLite. The real bash case comes with the tools
// on env (stage 5).

// openRecovering opens a Harness over the JSONL session in dir with
// scheduling resumed; change may adjust its options. It closes at cleanup,
// unless the test closed it first.
func openRecovering(t *testing.T, dir string, setup *chatSetup, change func(*Options)) (*Harness, *Conversation) {
	t.Helper()
	options := setup.options()
	if change != nil {
		change(&options)
	}
	h, err := Open(bg, openJSONL(t, dir), options)
	must(t, err)
	t.Cleanup(func() { _ = h.Close(bg) })
	root, err := h.Root(bg, &AgentChange{Model: Set(ModelRef{Provider: "faux", ModelID: "faux-1"})}, nil)
	must(t, err)
	must(t, h.Resume())
	return h, root
}

// blockingTool writes output and details, then blocks until its invocation
// is cancelled the first time it runs; started closes once the details are
// durable.
type blockingTool struct {
	registration *ToolRegistration
	started      chan struct{}
	once         sync.Once
	mu           sync.Mutex
	runs         int
}

func newBlockingTool(name, replay string) *blockingTool {
	b := &blockingTool{started: make(chan struct{})}
	b.registration = &ToolRegistration{
		Tool:   ai.Tool{Name: name, Description: name, Parameters: objectSchema()},
		Replay: replay,
		Execute: func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
			b.mu.Lock()
			b.runs++
			run := b.runs
			b.mu.Unlock()
			if err := api.Output([]byte(fmt.Sprintf("run %d\n", run)), nil); err != nil {
				return ToolExecutionResult{}, err
			}
			if err := api.Details(ctx, map[string]any{"run": run}); err != nil {
				return ToolExecutionResult{}, err
			}
			if run == 1 {
				b.once.Do(func() { close(b.started) })
				<-ctx.Done()
				return ToolExecutionResult{}, context.Cause(ctx)
			}
			return ToolExecutionResult{}, nil
		},
	}
	return b
}

func (b *blockingTool) runCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runs
}

func recoveryCall(name string) providers.FauxResponseStep {
	return fauxStep(toolCalls(call(name, nil, "c1")))
}

// toolTaskOf waits for the first slot's tool task.
func toolTaskOf(t *testing.T, h *Harness, root *Conversation) durable.TaskID {
	t.Helper()
	var id durable.TaskID
	eventually(t, func() bool {
		slots := liveSlots(t, h, root)
		if len(slots) == 0 || slots[0].TaskID == nil {
			return false
		}
		id = *slots[0].TaskID
		return true
	})
	return id
}

func TestToolRecoveryAnswersAnUnsafeToolInterruptedAfterIntentWithItsDurablePartialOutput(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	work := newBlockingTool("work", "")
	installTool(t, setup.registry, work.registration)
	setup.faux.SetResponses([]providers.FauxResponseStep{recoveryCall("work"), fauxText("done")})
	h, root := openRecovering(t, dir, setup, nil)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, work.started, "the tool")
	taskID := toolTaskOf(t, h, root)
	must(t, h.Close(bg))

	h, root = openRecovering(t, dir, setup, nil)
	record, err := h.GetTask(bg, taskID)
	must(t, err)
	if got := string(record.State.Checkpoint); got != `{"phase":"execute","replay":"unsafe"}` {
		t.Fatalf("checkpoint = %s", got)
	}
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	if runs := work.runCount(); runs != 1 {
		t.Fatalf("runs = %d", runs)
	}
	results := toolResults(chatEntries(t, root))
	if len(results) != 1 || !results[0].IsError || jsonOf(t, results[0].Details) != `{"run":1}` {
		t.Fatalf("results = %+v", results)
	}
	if got := resultText(results[0]); got != "run 1\n|<harness>\n[error] Tool work was interrupted and may have partially run\n</harness>" {
		t.Fatalf("text = %q", got)
	}
	expectIdleLive(t, h, root)
	// Beyond pi, whose case checks only the answer: the interrupted call
	// fails, which records cancellation intent for what it owns.
	record, err = h.GetTask(bg, taskID)
	must(t, err)
	if outcome := record.State.Outcome; outcome == nil || outcome.Status != durable.OutcomeFailed {
		t.Fatalf("outcome = %+v, want failed", outcome)
	}
}

func TestToolRecoveryRerunsAToolOnlyWhenBothTheStoredAndTheCurrentReplayPolicyAreSafe(t *testing.T) {
	for _, c := range []struct {
		stored, current string
		reruns          bool
	}{
		{ReplaySafe, ReplaySafe, true},
		{ReplaySafe, ReplayUnsafe, false},
		{ReplayUnsafe, ReplaySafe, false},
	} {
		t.Run(c.stored+"/"+c.current, func(t *testing.T) {
			dir := t.TempDir()
			setup := newChatSetup(t)
			work := newBlockingTool("work", c.stored)
			installed := installTool(t, setup.registry, work.registration)
			setup.faux.SetResponses([]providers.FauxResponseStep{recoveryCall("work"), fauxText("done")})
			h, root := openRecovering(t, dir, setup, nil)
			id := submit(t, root, inputDraft("go")).ID
			awaitSignal(t, work.started, "the tool")
			must(t, h.Close(bg))

			setup.registry.Uninstall(installed)
			current := *work.registration
			current.Replay = c.current
			installTool(t, setup.registry, &current)
			h, root = openRecovering(t, dir, setup, nil)
			if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
				t.Fatalf("status = %s", status)
			}
			results := toolResults(chatEntries(t, root))
			want := 1
			if c.reruns {
				want = 2
			}
			if runs := work.runCount(); runs != want {
				t.Fatalf("runs = %d, want %d", runs, want)
			}
			if len(results) != 1 || results[0].IsError == c.reruns {
				t.Fatalf("results = %+v", results)
			}
			if c.reruns {
				if got := resultText(results[0]); got != "run 2\n" {
					t.Fatalf("text = %q", got)
				}
			}
		})
	}
}

func TestToolRecoveryRerunsASafeToolWithTheEnvironmentOfTheConversationsCwdAtRerunAndNotOnceItIsDeselected(t *testing.T) {
	for _, change := range []string{"cwd", "deselect"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			one, two := t.TempDir(), t.TempDir()
			setup := newChatSetup(t)
			var mu sync.Mutex
			var cwds []string
			started := make(chan struct{})
			work := &ToolRegistration{
				Tool:   ai.Tool{Name: "work", Description: "work", Parameters: objectSchema()},
				Replay: ReplaySafe,
				Execute: func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
					mu.Lock()
					cwds = append(cwds, api.Env().Cwd())
					first := len(cwds) == 1
					mu.Unlock()
					if first {
						close(started)
						<-ctx.Done()
						return ToolExecutionResult{}, context.Cause(ctx)
					}
					return ToolExecutionResult{}, nil
				},
			}
			installTool(t, setup.registry, work)
			setup.faux.SetResponses([]providers.FauxResponseStep{recoveryCall("work"), fauxText("done")})
			withEnv := func(options *Options) {
				options.Env = func(_ context.Context, target EnvTarget) (env.ExecutionEnv, error) {
					cwd := dir
					if target.Cwd != nil {
						cwd = *target.Cwd
					}
					return env.NewLocal(cwd), nil
				}
			}
			h, root := openRecovering(t, dir, setup, withEnv)
			must(t, root.Configure(bg, AgentChange{Cwd: Set(one)}))
			id := submit(t, root, inputDraft("go")).ID
			awaitSignal(t, started, "the tool")
			if change == "cwd" {
				must(t, root.Configure(bg, AgentChange{Cwd: Set(two)}))
			} else {
				must(t, root.Configure(bg, AgentChange{Extensions: Set(Exactly[*Extension]())}))
			}
			must(t, h.Close(bg))

			h, root = openRecovering(t, dir, setup, withEnv)
			if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
				t.Fatalf("status = %s", status)
			}
			results := toolResults(chatEntries(t, root))
			mu.Lock()
			defer mu.Unlock()
			if len(results) != 1 {
				t.Fatalf("results = %+v", results)
			}
			if change == "cwd" {
				if !slices.Equal(cwds, []string{one, two}) || results[0].IsError {
					t.Fatalf("cwds = %v, result = %+v", cwds, results[0])
				}
				return
			}
			// A tool that no longer resolves is treated as unsafe:
			// interrupted, not rerun.
			if !slices.Equal(cwds, []string{one}) || !strings.Contains(resultText(results[0]), "was interrupted") {
				t.Fatalf("cwds = %v, result = %q", cwds, resultText(results[0]))
			}
		})
	}
}

func TestToolRecoveryRerunsBeforeToolWhenInterruptedBeforeIntentAndExecutesOnce(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	var mu sync.Mutex
	runs, asked := 0, 0
	var decisions []string
	installTool(t, setup.registry, echoTool("work", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	reached := make(chan struct{})
	installHooks(t, setup.registry, "hooks", ToolTask, &ToolHooks{BeforeTool: func(ctx context.Context, _ ToolHookCall, rt *TaskRuntime) (*BeforeToolDecision, error) {
		mu.Lock()
		asked++
		attempt := asked
		mu.Unlock()
		// A durable first-writer-wins decision survives the rerun.
		raw, err := rt.MemoOr(ctx, "test:decision", fmt.Sprintf("attempt %d", attempt))
		if err != nil {
			return nil, err
		}
		var decision string
		must(t, json.Unmarshal(raw, &decision))
		mu.Lock()
		decisions = append(decisions, decision)
		mu.Unlock()
		if attempt == 1 {
			close(reached)
			<-ctx.Done()
			return nil, context.Cause(ctx)
		}
		return nil, nil
	}})
	setup.faux.SetResponses([]providers.FauxResponseStep{recoveryCall("work"), fauxText("done")})
	h, root := openRecovering(t, dir, setup, nil)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, reached, "beforeTool")
	must(t, h.Close(bg))

	h, _ = openRecovering(t, dir, setup, nil)
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked != 2 || runs != 1 || !slices.Equal(decisions, []string{"attempt 1", "attempt 1"}) {
		t.Fatalf("asked = %d, runs = %d, decisions = %v", asked, runs, decisions)
	}
}

func TestToolRecoveryRerunsTheGenerationToolsPhaseInterruptedBeforeItsCommit(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("work", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	reached := make(chan struct{})
	var mu sync.Mutex
	observed := 0
	installHooks(t, setup.registry, "hooks", GenerationTask, &GenerationHooks{AfterTools: func(ctx context.Context, _ durable.EntryID, _ []durable.EntryID, _ *TaskRuntime) error {
		mu.Lock()
		observed++
		first := observed == 1
		mu.Unlock()
		if first {
			close(reached)
			<-ctx.Done()
			return context.Cause(ctx)
		}
		return nil
	}})
	setup.faux.SetResponses([]providers.FauxResponseStep{recoveryCall("work"), fauxText("done")})
	h, root := openRecovering(t, dir, setup, nil)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, reached, "afterTools")
	must(t, h.Close(bg))

	h, root = openRecovering(t, dir, setup, nil)
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if observed != 2 {
		t.Fatalf("observed = %d", observed)
	}
	var kinds []string
	for _, entry := range chatEntries(t, root) {
		kinds = append(kinds, entry.Kind)
	}
	if want := []string{"pi.user", "pi.system", "pi.assistant", "pi.tool-result", "pi.assistant"}; !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v", kinds)
	}
}

func TestToolRecoveryAnswersAnAbortedToolWithItsPartialOutputAndContinuesTheRun(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	work := newBlockingTool("work", "")
	installTool(t, setup.registry, work.registration)
	setup.faux.SetResponses([]providers.FauxResponseStep{recoveryCall("work"), fauxText("done")})
	h, root := openRecovering(t, dir, setup, nil)
	submission := submit(t, root, inputDraft("go"))
	awaitSignal(t, work.started, "the tool")
	taskID := toolTaskOf(t, h, root)
	marked, err := h.AbortTask(bg, taskID)
	must(t, err)
	if marked != "marked" {
		t.Fatalf("abort = %s", marked)
	}
	record, err := h.WaitForTask(bg, taskID)
	must(t, err)
	if outcome := record.State.Outcome; outcome.Status != durable.OutcomeAborted || !strings.Contains(string(outcome.Result), `"entryId":`) {
		t.Fatalf("outcome = %+v", outcome)
	}
	if status := waitFor(t, submission).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	results := toolResults(chatEntries(t, root))
	if len(results) != 1 || resultText(results[0]) != "run 1\n|<harness>\n[error] Tool work was aborted\n</harness>" {
		t.Fatalf("results = %+v", results)
	}
}

func TestToolRecoveryLetsContextDerivationAnswerAFaultedToolAndContinuesTheRun(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	// A result that is not JSON makes the result commit fail, so the
	// scheduler faults the task.
	installTool(t, setup.registry, echoTool("bad", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{}, Details: map[string]any{"fn": func() int { return 1 }}}, nil
	}))
	var mu sync.Mutex
	var requests []string
	setup.faux.SetResponses([]providers.FauxResponseStep{
		recoveryCall("bad"),
		func(_ context.Context, request ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			seen := "none"
			for _, message := range request.Messages {
				if result, ok := message.(ai.ToolResultMessage); ok {
					seen = resultText(result)
					break
				}
			}
			mu.Lock()
			requests = append(requests, seen)
			mu.Unlock()
			return fauxAnswer("done")
		},
	})
	h, root := openRecovering(t, dir, setup, nil)
	if status := waitFor(t, submit(t, root, inputDraft("go"))).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	if results := toolResults(chatEntries(t, root)); len(results) != 0 {
		t.Fatalf("results = %+v", results)
	}
	mu.Lock()
	if !slices.Equal(requests, []string{missingResultText}) {
		t.Fatalf("requests = %q", requests)
	}
	mu.Unlock()
	tool := conversationTasks(t, h, root, toolKind)
	if len(tool) != 1 || tool[0].State.Status != durable.TaskTerminal || tool[0].State.Outcome.Status != durable.OutcomeFaulted {
		t.Fatalf("tool tasks = %+v", tool)
	}
}

func TestToolRecoveryClearsTheInterruptedAttemptsProgressBeforeASafeRerun(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	started := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var mu sync.Mutex
	runs := 0
	var rerunArgs map[string]any
	installTool(t, setup.registry, &ToolRegistration{
		Tool:   ai.Tool{Name: "work", Description: "work", Parameters: objectSchema()},
		Replay: ReplaySafe,
		Execute: func(ctx context.Context, args map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
			mu.Lock()
			run := runs
			runs++
			if run == 1 {
				rerunArgs = args
			}
			mu.Unlock()
			if run == 0 {
				must(t, api.Diagnostic(ToolDiagnostic{Severity: "info", Message: "first a"}))
				must(t, api.Diagnostic(ToolDiagnostic{Severity: "info", Message: "first b"}))
				must(t, api.Details(ctx, map[string]any{"run": 1, "extra": true}))
			} else {
				must(t, api.Diagnostic(ToolDiagnostic{Severity: "info", Message: "second"}))
				must(t, api.Details(ctx, map[string]any{"run": 2}))
			}
			close(started[run])
			<-ctx.Done()
			return ToolExecutionResult{}, context.Cause(ctx)
		},
	})
	setup.faux.SetResponses([]providers.FauxResponseStep{recoveryCall("work"), fauxText("done")})
	h, root := openRecovering(t, dir, setup, nil)
	submit(t, root, inputDraft("go"))
	awaitSignal(t, started[0], "the first run")
	must(t, h.Close(bg))

	h, root = openRecovering(t, dir, setup, nil)
	awaitSignal(t, started[1], "the rerun")
	slots := liveSlots(t, h, root)
	if len(slots) != 1 {
		t.Fatalf("slots = %+v", slots)
	}
	if want := []ToolDiagnostic{{Severity: "info", Message: "second"}}; !reflect.DeepEqual(slots[0].Diagnostics, want) {
		t.Fatalf("diagnostics = %+v", slots[0].Diagnostics)
	}
	if got := jsonOf(t, slots[0].Details); got != `{"run":2}` {
		t.Fatalf("details = %s", got)
	}
	// Beyond pi, where the intent always holds its arguments: empty ones,
	// omitted from the stored intent, rerun as {}, not nil.
	mu.Lock()
	defer mu.Unlock()
	if rerunArgs == nil {
		t.Fatal("the rerun got nil arguments, want {}")
	}
}
