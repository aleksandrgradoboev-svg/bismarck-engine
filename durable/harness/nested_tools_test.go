package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-nested-tools.test.ts, on
// JSONL in place of node's SQLite. The cases that read tool events of nested
// calls (pi.live.nestedTools as events, a late subscriber, a progress: false
// call aborted) and the event halves of three others come with slice 2c-2.

func textSchema() *ai.Schema {
	var schema ai.Schema
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"text":{"type":"string"}}}`), &schema); err != nil {
		panic(err)
	}
	return &schema
}

// nestedTool is a tool named name with an optional text argument.
func nestedTool(name string, execute toolExecute, change ...func(*ToolRegistration)) *ToolRegistration {
	tool := &ToolRegistration{Tool: ai.Tool{Name: name, Description: name, Parameters: textSchema()}, Execute: execute}
	for _, c := range change {
		c(tool)
	}
	return tool
}

// countingEcho returns "echo <text>" and counts its runs.
type countingEcho struct {
	registration *ToolRegistration
	runs         atomic.Int32
}

func newCountingEcho(change ...func(*ToolRegistration)) *countingEcho {
	e := &countingEcho{}
	e.registration = nestedTool("echo", func(_ context.Context, args map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		e.runs.Add(1)
		text, _ := args["text"].(string)
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "echo " + text}}}, nil
	}, change...)
	return e
}

// nestedText is the value a schema-less nested call gives programs, when it
// is a string.
func nestedText(result NestedToolExecutionResult) string {
	var text string
	if json.Unmarshal(result.StructuredOutput, &text) != nil {
		return ""
	}
	return text
}

// allToolTasks are the conversation's tool tasks in ID order.
func allToolTasks(t *testing.T, h *Harness, root *Conversation) []durable.TaskRecord {
	t.Helper()
	tasks := conversationTasks(t, h, root, toolKind)
	slices.SortFunc(tasks, func(a, b durable.TaskRecord) int { return int(a.ID - b.ID) })
	return tasks
}

func toolInput(t *testing.T, record durable.TaskRecord) ToolTaskInput {
	t.Helper()
	var input ToolTaskInput
	must(t, json.Unmarshal(record.Input, &input))
	return input
}

func toolOutcome(record durable.TaskRecord) (status, result string) {
	if record.State.Outcome == nil {
		return "", ""
	}
	return record.State.Outcome.Status, string(record.State.Outcome.Result)
}

// runNested submits once and waits for the run.
func runNested(t *testing.T, setup *chatSetup, responses ...providers.FauxResponseStep) (*Harness, *Conversation, string, []durable.EntryRecord) {
	t.Helper()
	setup.faux.SetResponses(responses)
	h, root := openChat(t, memory.New(), setup)
	settled := waitFor(t, submit(t, root, inputDraft("go")))
	return h, root, settled.Status, chatEntries(t, root)
}

func callStep(name string, args map[string]any) providers.FauxResponseStep {
	return fauxStep(toolCalls(call(name, args, "c1")))
}

func nestedDocument(t *testing.T, h *Harness, caller durable.TaskID, key string) map[string]any {
	t.Helper()
	value, err := h.Snapshot(bg, NestedCallDoc.Task(caller).Member(key, nil))
	must(t, err)
	return value
}

// docCreateRecorder records the kind and key of every document a commit
// creates.
type docCreateRecorder struct {
	durable.Storage
	mu      sync.Mutex
	created []string
}

func (s *docCreateRecorder) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	s.mu.Lock()
	for _, write := range writes {
		if create, ok := write.(durable.DocumentCreateWrite); ok && strings.HasPrefix(create.Record.Kind, "pi.tool.") {
			key := ""
			if create.Record.Key != nil {
				key = *create.Record.Key
			}
			s.created = append(s.created, create.Record.Kind+":"+key)
		}
	}
	s.mu.Unlock()
	return s.Storage.Commit(ctx, writes)
}

func TestNestedToolsStoreOneDocumentPerNestedCallKeyedByItsKeyWrittenOnlyByThatCall(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, newCountingEcho().registration)
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, key := range []string{"", "named"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = api.ExecuteTool(ctx, "echo", map[string]any{"text": key}, NestedCallOptions{Key: key})
			}()
		}
		wg.Wait()
		return ToolExecutionResult{Output: ai.ContentList{}}, errors.Join(errs...)
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	storage := &docCreateRecorder{Storage: memory.New()}
	_, root := openChat(t, storage, setup)
	waitFor(t, submit(t, root, inputDraft("go")))
	storage.mu.Lock()
	defer storage.mu.Unlock()
	created := slices.Clone(storage.created)
	slices.Sort(created)
	kind := NestedCallDoc.Definition().Kind
	expectStrings(t, "created", created, []string{kind + ":1", kind + ":named"})
}

func TestNestedToolsRejectANestedCallOfAnAbortedCallerAndLeaveNoDocumentForIt(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, newCountingEcho().registration)
	started, marked := make(chan struct{}), make(chan struct{})
	var rejected error
	var caller durable.TaskID
	installTool(t, setup.registry, nestedTool("late", func(_ context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		caller = api.TaskID()
		close(started)
		<-marked
		// Its context is cancelled too, so pass one that is not: the
		// admission itself must refuse.
		_, rejected = api.ExecuteTool(bg, "echo", map[string]any{"text": "a"}, NestedCallOptions{})
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("late", nil), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	submission := submit(t, root, inputDraft("go"))
	awaitSignal(t, started, "the caller")
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		record, err := tx.Task(caller)
		if err != nil {
			return err
		}
		marked := record.Clone()
		marked.AbortRequested = true
		return tx.SetTask(marked)
	}))
	close(marked)
	waitFor(t, submission)
	if rejected == nil || !strings.Contains(rejected.Error(), "abort mark") && !strings.Contains(rejected.Error(), "aborted") && !strings.Contains(rejected.Error(), "has settled") {
		t.Fatalf("rejected = %v", rejected)
	}
	if document := nestedDocument(t, h, caller, "1"); document != nil {
		t.Fatalf("document = %v", document)
	}
}

func TestNestedToolsRunANestedCallAsItsOwnToolTaskAndReturnItsResultToTheCallerNotTheTranscript(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, newCountingEcho().registration)
	var nested NestedToolExecutionResult
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		var err error
		nested, err = api.ExecuteTool(ctx, "echo", map[string]any{"text": "a"}, NestedCallOptions{})
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "got " + nestedText(nested)}}}, err
	}))
	h, root, status, entries := runNested(t, setup, callStep("batch", nil), fauxText("done"))
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	tasks := allToolTasks(t, h, root)
	parent, child := tasks[0], tasks[1]
	if nested.TaskID != child.ID || string(nested.StructuredOutput) != `"echo a"` || nested.IsError || len(nested.Diagnostics) != 0 || nested.DurationMs == nil || nested.Details != nil || nested.Usage != nil {
		t.Fatalf("nested = %s", jsonOf(t, nested))
	}
	// One result entry: the model-issued call's, which records nothing of
	// its nested calls.
	results := toolResults(entries)
	if len(results) != 1 || results[0].ToolCallID != "c1" || resultText(results[0]) != "got echo a" {
		t.Fatalf("results = %s", jsonOf(t, results))
	}
	if input := toolInput(t, parent); input.Kind != toolCallModel || input.CallID != "c1" {
		t.Fatalf("parent input = %s", parent.Input)
	}
	if child.Owner == nil || *child.Owner != parent.ID {
		t.Fatalf("child owner = %v", child.Owner)
	}
	want := fmt.Sprintf(`{"call":{"arguments":{"text":"a"},"id":"c1/1","name":"echo","type":"toolCall"},"key":"1","kind":"nested","parent":%d,"parentCallId":"c1"}`, parent.ID)
	if got := normalJSON(t, child.Input); got != want {
		t.Fatalf("child input = %s, want %s", got, want)
	}
	// The receipt stays small: the result lived in the caller's documents,
	// which retired with it.
	if status, result := toolOutcome(child); child.State.Status != durable.TaskTerminal || status != durable.OutcomeCompleted || result != `{"kind":"nested"}` {
		t.Fatalf("child state = %+v", child.State)
	}
	if document := nestedDocument(t, h, parent.ID, "1"); document != nil {
		t.Fatalf("document = %v", document)
	}
	expectIdleLive(t, h, root)
}

func TestNestedToolsGiveEachNestedCallItsOwnTaskAndCallID(t *testing.T) {
	setup := newChatSetup(t)
	var mu sync.Mutex
	var callIDs []string
	tasks := map[durable.TaskID]bool{}
	installTool(t, setup.registry, nestedTool("who", func(_ context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		mu.Lock()
		callIDs = append(callIDs, api.CallID())
		tasks[api.TaskID()] = true
		mu.Unlock()
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		var wg sync.WaitGroup
		for _, key := range []string{"a", "b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = api.ExecuteTool(ctx, "who", map[string]any{}, NestedCallOptions{Key: key})
			}()
		}
		wg.Wait()
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	runNested(t, setup, callStep("batch", nil), fauxText("done"))
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(callIDs)
	expectStrings(t, "call IDs", callIDs, []string{"c1/a", "c1/b"})
	if len(tasks) != 2 {
		t.Fatalf("tasks = %v", tasks)
	}
}

func TestNestedToolsRunTheToolHooksOnNestedCallsWhichSeeTheirParent(t *testing.T) {
	setup := newChatSetup(t)
	echo := newCountingEcho()
	installTool(t, setup.registry, echo.registration)
	var mu sync.Mutex
	var parents []*ToolCallParent
	installHooks(t, setup.registry, "hooks", ToolTask, &ToolHooks{
		BeforeTool: func(_ context.Context, call ToolHookCall, _ *TaskRuntime) (*BeforeToolDecision, error) {
			mu.Lock()
			parents = append(parents, call.Parent)
			mu.Unlock()
			if call.Parent != nil && call.Arguments["text"] == "secret" {
				block := "no secrets"
				return &BeforeToolDecision{Block: &block}, nil
			}
			return nil, nil
		},
		AfterTool: func(_ context.Context, call ToolHookCall, result ToolExecutionResult, _ *TaskRuntime) (*ToolExecutionResult, error) {
			if call.Parent == nil {
				return nil, nil
			}
			result.Output = ai.ContentList{ai.TextContent{Text: "rewritten"}}
			return &result, nil
		},
	})
	var nested []NestedToolExecutionResult
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		for _, text := range []string{"secret", "public"} {
			result, err := api.ExecuteTool(ctx, "echo", map[string]any{"text": text}, NestedCallOptions{})
			if err != nil {
				return ToolExecutionResult{}, err
			}
			nested = append(nested, result)
		}
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	h, root, _, _ := runNested(t, setup, callStep("batch", nil), fauxText("done"))
	parent := allToolTasks(t, h, root)[0]
	mu.Lock()
	defer mu.Unlock()
	want := fmt.Sprintf(`[null,{"TaskID":%d,"CallID":"c1"},{"TaskID":%d,"CallID":"c1"}]`, parent.ID, parent.ID)
	if got := jsonOf(t, parents); got != want {
		t.Fatalf("parents = %s, want %s", got, want)
	}
	if got := jsonOf(t, nested[0]); !nested[0].IsError || jsonOf(t, nested[0].Diagnostics) != `[{"severity":"error","message":"Tool call blocked: no secrets","code":"blocked"}]` {
		t.Fatalf("blocked = %s", got)
	}
	if nestedText(nested[1]) != "rewritten" {
		t.Fatalf("public = %s", jsonOf(t, nested[1]))
	}
	if runs := echo.runs.Load(); runs != 1 {
		t.Fatalf("echo runs = %d", runs)
	}
}

func TestNestedToolsReturnAnErrorResultForAnUnavailableToolAndInvalidArguments(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, newCountingEcho().registration)
	var nested []NestedToolExecutionResult
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		for _, c := range []struct {
			name string
			args map[string]any
		}{{"missing", map[string]any{}}, {"echo", map[string]any{"text": map[string]any{"not": "a string"}}}} {
			result, err := api.ExecuteTool(ctx, c.name, c.args, NestedCallOptions{})
			if err != nil {
				return ToolExecutionResult{}, err
			}
			nested = append(nested, result)
		}
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	runNested(t, setup, callStep("batch", nil), fauxText("done"))
	if len(nested) != 2 || !nested[0].IsError || firstCode(nested[0]) != "tool_unavailable" || !nested[1].IsError || firstCode(nested[1]) != "invalid_arguments" {
		t.Fatalf("nested = %s", jsonOf(t, nested))
	}
}

func TestNestedToolsReturnTheCallAKeyAlreadyNamesAndRejectReusingTheKeyForAnotherCall(t *testing.T) {
	setup := newChatSetup(t)
	echo := newCountingEcho()
	installTool(t, setup.registry, echo.registration)
	var reused NestedToolExecutionResult
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		if _, err := api.ExecuteTool(ctx, "echo", map[string]any{"text": "a"}, NestedCallOptions{Key: "k"}); err != nil {
			return ToolExecutionResult{}, err
		}
		var err error
		if reused, err = api.ExecuteTool(ctx, "echo", map[string]any{"text": "a"}, NestedCallOptions{Key: "k"}); err != nil {
			return ToolExecutionResult{}, err
		}
		_, err = api.ExecuteTool(ctx, "echo", map[string]any{"text": "b"}, NestedCallOptions{Key: "k"})
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	}))
	_, _, _, entries := runNested(t, setup, callStep("batch", nil), fauxText("done"))
	if runs := echo.runs.Load(); runs != 1 {
		t.Fatalf("echo runs = %d", runs)
	}
	if nestedText(reused) != "echo a" {
		t.Fatalf("reused = %s", jsonOf(t, reused))
	}
	if text := resultText(toolResults(entries)[0]); !strings.Contains(text, "Nested call c1/k was already made with another tool or other arguments") {
		t.Fatalf("result = %q", text)
	}
}

func TestNestedToolsMatchAReusedKeysArgumentsRegardlessOfKeyOrder(t *testing.T) {
	setup := newChatSetup(t)
	var runs atomic.Int32
	installTool(t, setup.registry, &ToolRegistration{
		Tool: ai.Tool{Name: "pair", Description: "pair", Parameters: schemaOf(t, `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a","b"]}`)},
		Execute: func(_ context.Context, args map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
			runs.Add(1)
			return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: fmt.Sprint(args["a"], args["b"])}}}, nil
		},
	})
	var nested []string
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		// Go maps have no key order; the arguments arrive as pi's two
		// orders would.
		for _, raw := range []string{`{"a":"x","b":"y"}`, `{"b":"y","a":"x"}`} {
			var args map[string]any
			must(t, json.Unmarshal([]byte(raw), &args))
			result, err := api.ExecuteTool(ctx, "pair", args, NestedCallOptions{Key: "k"})
			if err != nil {
				return ToolExecutionResult{}, err
			}
			nested = append(nested, nestedText(result))
		}
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	runNested(t, setup, callStep("batch", nil), fauxText("done"))
	if runs.Load() != 1 {
		t.Fatalf("runs = %d", runs.Load())
	}
	expectStrings(t, "results", nested, []string{"xy", "xy"})
}

func TestNestedToolsAbortTheNestedCallsACallerLeftRunningWhenItReturns(t *testing.T) {
	setup := newChatSetup(t)
	running := make(chan struct{})
	installTool(t, setup.registry, nestedTool("hang", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		if err := api.Output([]byte("partial\n"), nil); err != nil {
			return ToolExecutionResult{}, err
		}
		if err := api.Details(ctx, map[string]any{"step": 1}); err != nil {
			return ToolExecutionResult{}, err
		}
		close(running)
		<-ctx.Done()
		return ToolExecutionResult{}, context.Cause(ctx)
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		go func() { _, _ = api.ExecuteTool(ctx, "hang", map[string]any{}, NestedCallOptions{}) }()
		<-running
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "returned early"}}}, nil
	}))
	h, root, status, _ := runNested(t, setup, callStep("batch", nil), fauxText("done"))
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	must(t, h.WaitForIdle(bg))
	if text := resultText(toolResults(chatEntries(t, root))[0]); text != "returned early" {
		t.Fatalf("result = %q", text)
	}
	child := allToolTasks(t, h, root)[1]
	if status, result := toolOutcome(child); status != durable.OutcomeAborted || result != `{"kind":"nested"}` {
		t.Fatalf("child outcome = %+v", child.State.Outcome)
	}
	expectIdleLive(t, h, root)
}

func TestNestedToolsRecordANestedCallsUsageUnderItsTool(t *testing.T) {
	setup := newChatSetup(t)
	usage := ai.Usage{Input: 1, Output: 2, TotalTokens: 3, Cost: ai.CostBreakdown{Input: 0.1, Output: 0.2, Total: 0.3}}
	installTool(t, setup.registry, nestedTool("paid", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Usage: &usage}, nil
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		_, err := api.ExecuteTool(ctx, "paid", map[string]any{}, NestedCallOptions{})
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	}))
	h, root, _, _ := runNested(t, setup, callStep("batch", nil), fauxText("done"))
	value, err := h.Snapshot(bg, UsageDoc.Conversation(root.ID))
	must(t, err)
	if got, want := jsonOf(t, value["tools"]), jsonOf(t, map[string]ai.Usage{"paid": usage}); normalJSON(t, []byte(got)) != normalJSON(t, []byte(want)) {
		t.Fatalf("tool usage = %s, want %s", got, want)
	}
}

func TestNestedToolsAbortNestedCallsWithTheirCaller(t *testing.T) {
	setup := newChatSetup(t)
	running := make(chan struct{})
	installTool(t, setup.registry, nestedTool("hang", func(ctx context.Context, _ map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		close(running)
		<-ctx.Done()
		return ToolExecutionResult{}, context.Cause(ctx)
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		_, err := api.ExecuteTool(ctx, "hang", map[string]any{}, NestedCallOptions{})
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	submission := submit(t, root, inputDraft("go"))
	awaitSignal(t, running, "the nested call")
	must(t, root.Abort(bg, false))
	if status := waitFor(t, submission).Status; status != durable.SubmissionUnanswered {
		t.Fatalf("status = %s", status)
	}
	tasks := allToolTasks(t, h, root)
	if status, result := toolOutcome(tasks[1]); status != durable.OutcomeAborted || result != `{"kind":"nested"}` {
		t.Fatalf("child outcome = %+v", tasks[1].State.Outcome)
	}
	if status, result := toolOutcome(tasks[0]); status != durable.OutcomeAborted || !strings.HasPrefix(result, `{"kind":"model"`) {
		t.Fatalf("parent outcome = %+v", tasks[0].State.Outcome)
	}
	expectIdleLive(t, h, root)
}

func TestNestedToolsAbortARunningNestedCallWhenItsCallerThrows(t *testing.T) {
	setup := newChatSetup(t)
	running := make(chan struct{})
	installTool(t, setup.registry, nestedTool("hang", func(ctx context.Context, _ map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		close(running)
		<-ctx.Done()
		return ToolExecutionResult{}, context.Cause(ctx)
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		go func() { _, _ = api.ExecuteTool(ctx, "hang", map[string]any{}, NestedCallOptions{}) }()
		<-running
		return ToolExecutionResult{}, errors.New("caller failed")
	}))
	h, root, status, entries := runNested(t, setup, callStep("batch", nil), fauxText("done"))
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	if text := resultText(toolResults(entries)[0]); !strings.Contains(text, "caller failed") {
		t.Fatalf("result = %q", text)
	}
	if status, _ := toolOutcome(allToolTasks(t, h, root)[1]); status != durable.OutcomeAborted {
		t.Fatalf("child outcome = %s", status)
	}
}

func TestNestedToolsReattachAReplaySafeCallerToTheNestedCallsItMadeBeforeARestart(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	echo := newCountingEcho()
	installTool(t, setup.registry, echo.registration)
	blocked := make(chan struct{})
	var runs atomic.Int32
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		run := runs.Add(1)
		first, err := api.ExecuteTool(ctx, "echo", map[string]any{"text": "a"}, NestedCallOptions{})
		if err != nil {
			return ToolExecutionResult{}, err
		}
		if run == 1 {
			close(blocked)
			<-ctx.Done()
			return ToolExecutionResult{}, context.Cause(ctx)
		}
		// A call new to the rerun: its slot, after the reattached one's, is
		// found and finishes.
		second, err := api.ExecuteTool(ctx, "echo", map[string]any{"text": "b"}, NestedCallOptions{})
		if err != nil {
			return ToolExecutionResult{}, err
		}
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: fmt.Sprintf("run %d: %s, %s", run, nestedText(first), nestedText(second))}}}, nil
	}, func(tool *ToolRegistration) { tool.Replay = ReplaySafe }))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, _ := openRecovering(t, dir, setup, nil)
	_, root := h, (*Conversation)(nil)
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, blocked, "the first run")
	must(t, h.Close(bg))

	h, root = openRecovering(t, dir, setup, nil)
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	if runs.Load() != 2 || echo.runs.Load() != 2 {
		t.Fatalf("runs = %d, echo runs = %d", runs.Load(), echo.runs.Load())
	}
	if text := resultText(toolResults(chatEntries(t, root))[0]); text != "run 2: echo a, echo b" {
		t.Fatalf("result = %q", text)
	}
	tasks := allToolTasks(t, h, root)
	nested := 0
	for _, task := range tasks {
		if toolInput(t, task).Kind == toolCallNested {
			nested++
		}
	}
	if _, result := toolOutcome(tasks[1]); nested != 2 || result != `{"kind":"nested"}` {
		t.Fatalf("nested = %d, second task result = %s", nested, result)
	}
}

func TestNestedToolsStopOnlyTheWaitWhenACallerCancelsItTheCallRunsOnAndItsKeyReturnsItsResult(t *testing.T) {
	setup := newChatSetup(t)
	release := make(chan struct{})
	var cancel context.CancelCauseFunc
	var attempts []string
	var retry NestedToolExecutionResult
	installTool(t, setup.registry, nestedTool("slow", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		cancel(errors.New("cancelled"))
		<-release
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "slow done"}}}, nil
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		cancelled, cancelFunc := context.WithCancelCause(ctx)
		cancel = cancelFunc
		if _, err := api.ExecuteTool(cancelled, "slow", map[string]any{}, NestedCallOptions{Key: "same"}); err != nil {
			attempts = append(attempts, "first cancelled")
		} else {
			attempts = append(attempts, "first done")
		}
		close(release)
		var err error
		retry, err = api.ExecuteTool(ctx, "slow", map[string]any{}, NestedCallOptions{Key: "same"})
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	}))
	h, root, _, _ := runNested(t, setup, callStep("batch", nil), fauxText("done"))
	expectStrings(t, "attempts", attempts, []string{"first cancelled"})
	if retry.IsError || nestedText(retry) != "slow done" {
		t.Fatalf("retry = %s", jsonOf(t, retry))
	}
	if tasks := allToolTasks(t, h, root); len(tasks) != 2 {
		t.Fatalf("tool tasks = %d", len(tasks))
	}
}

func TestNestedToolsAdmitOneNestedCallForAKeyUsedByConcurrentCalls(t *testing.T) {
	setup := newChatSetup(t)
	echo := newCountingEcho()
	installTool(t, setup.registry, echo.registration)
	both := make([]NestedToolExecutionResult, 2)
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range both {
			wg.Add(1)
			go func() {
				defer wg.Done()
				both[i], errs[i] = api.ExecuteTool(ctx, "echo", map[string]any{"text": "a"}, NestedCallOptions{Key: "k"})
			}()
		}
		wg.Wait()
		return ToolExecutionResult{Output: ai.ContentList{}}, errors.Join(errs...)
	}))
	h, root, _, _ := runNested(t, setup, callStep("batch", nil), fauxText("done"))
	if echo.runs.Load() != 1 || nestedText(both[0]) != "echo a" || nestedText(both[1]) != "echo a" {
		t.Fatalf("echo runs = %d, both = %s", echo.runs.Load(), jsonOf(t, both))
	}
	if tasks := allToolTasks(t, h, root); len(tasks) != 2 {
		t.Fatalf("tool tasks = %d", len(tasks))
	}
}

func TestNestedToolsAbortLeftRunningNestedCallsOfACallerInterruptedWhileItCleanedThemUp(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	running, cleaning, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var hangRuns atomic.Int32
	installTool(t, setup.registry, nestedTool("hang", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		hangRuns.Add(1)
		if err := api.Output([]byte("partial\n"), nil); err != nil {
			return ToolExecutionResult{}, err
		}
		if err := api.Details(ctx, map[string]any{"step": 1}); err != nil {
			return ToolExecutionResult{}, err
		}
		close(running)
		<-ctx.Done()
		// The caller's cleanup waits for this abort; hold it there until the
		// Harness closes.
		close(cleaning)
		<-release
		return ToolExecutionResult{}, errors.New("aborted")
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		go func() { _, _ = api.ExecuteTool(ctx, "hang", map[string]any{}, NestedCallOptions{}) }()
		<-running
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "returned early"}}}, nil
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openRecovering(t, dir, setup, nil)
	id := submit(t, root, inputDraft("go")).ID
	awaitSignal(t, cleaning, "the cleanup")
	closed := make(chan error, 1)
	go func() { closed <- h.Close(bg) }()
	close(release)
	must(t, <-closed)

	h, root = openRecovering(t, dir, setup, nil)
	if status := waitSubmission(t, h, id).Status; status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	must(t, h.WaitForIdle(bg))
	// The caller is replay-unsafe: it reports the interruption, and its
	// cleanup aborts the nested call.
	result := toolResults(chatEntries(t, root))[0]
	if result.ToolCallID != "c1" || !result.IsError || !strings.Contains(resultText(result), "Tool batch was interrupted") {
		t.Fatalf("result = %s", jsonOf(t, result))
	}
	if status, _ := toolOutcome(allToolTasks(t, h, root)[1]); status != durable.OutcomeAborted {
		t.Fatalf("child outcome = %s", status)
	}
	if hangRuns.Load() != 1 {
		t.Fatalf("hang runs = %d", hangRuns.Load())
	}
	expectIdleLive(t, h, root)
}

func TestNestedToolsAbortANestedCallWhoseAdmissionWasStillCommittingWhenItsCallerReturned(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, newCountingEcho().registration)
	installTool(t, setup.registry, nestedTool("hang", func(ctx context.Context, _ map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		<-ctx.Done()
		return ToolExecutionResult{}, context.Cause(ctx)
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		if _, err := api.ExecuteTool(ctx, "echo", map[string]any{"text": "warm"}, NestedCallOptions{Key: "warm"}); err != nil {
			return ToolExecutionResult{}, err
		}
		started := make(chan struct{})
		go func() {
			close(started)
			_, _ = api.ExecuteTool(ctx, "hang", map[string]any{}, NestedCallOptions{Key: "late"})
		}()
		<-started
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	h, root, status, _ := runNested(t, setup, callStep("batch", nil), fauxText("done"))
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	var late *durable.TaskRecord
	for _, task := range allToolTasks(t, h, root) {
		if input := toolInput(t, task); input.Kind == toolCallNested && input.Call.Name == "hang" {
			late = &task
		}
	}
	// The admission may lose the race with the caller's end: then the call
	// is refused and never made.
	if late != nil {
		if status, _ := toolOutcome(*late); status != durable.OutcomeAborted {
			t.Fatalf("late outcome = %s", status)
		}
	}
	expectIdleLive(t, h, root)
}

func TestNestedToolsAbortEveryLeftRunningNestedCallBeforeWaitingForAny(t *testing.T) {
	setup := newChatSetup(t)
	bAborted := make(chan struct{})
	startedA, startedB := make(chan struct{}), make(chan struct{})
	// A only stops once B was told to stop.
	installTool(t, setup.registry, nestedTool("a", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		close(startedA)
		<-bAborted
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	installTool(t, setup.registry, nestedTool("b", func(ctx context.Context, _ map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		close(startedB)
		<-ctx.Done()
		close(bAborted)
		return ToolExecutionResult{}, errors.New("aborted")
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		go func() { _, _ = api.ExecuteTool(ctx, "a", map[string]any{}, NestedCallOptions{Key: "a"}) }()
		go func() { _, _ = api.ExecuteTool(ctx, "b", map[string]any{}, NestedCallOptions{Key: "b"}) }()
		<-startedA
		<-startedB
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	h, root, status, _ := runNested(t, setup, callStep("batch", nil), fauxText("done"))
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	for _, task := range allToolTasks(t, h, root)[1:] {
		if task.State.Status != durable.TaskTerminal {
			t.Fatalf("nested task %d = %s", task.ID, task.State.Status)
		}
	}
}

func TestNestedToolsRejectExplicitKeysWithASlashThatAreProtoOrArePositiveIntegers(t *testing.T) {
	// Go has no undefined key: an empty Key takes the call's position, so
	// pi's empty-key case has no counterpart.
	setup := newChatSetup(t)
	installTool(t, setup.registry, newCountingEcho().registration)
	var errs []string
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		for _, key := range []string{"a/b", "__proto__", "7"} {
			if _, err := api.ExecuteTool(ctx, "echo", map[string]any{}, NestedCallOptions{Key: key}); err != nil {
				errs = append(errs, err.Error())
			}
		}
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	runNested(t, setup, callStep("batch", nil), fauxText("done"))
	rule := `must be non-empty, without "/", not "__proto__", and not a positive integer`
	expectStrings(t, "errors", errs, []string{
		`Nested call key "a/b" ` + rule,
		`Nested call key "__proto__" ` + rule,
		`Nested call key "7" ` + rule,
	})
}

func TestNestedToolsRunNestedCallsOfNestedCalls(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, newCountingEcho().registration)
	installTool(t, setup.registry, nestedTool("inner", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		result, err := api.ExecuteTool(ctx, "echo", map[string]any{"text": "deep"}, NestedCallOptions{Key: "x"})
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: nestedText(result)}}}, err
	}))
	installTool(t, setup.registry, nestedTool("outer", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		result, err := api.ExecuteTool(ctx, "inner", map[string]any{}, NestedCallOptions{})
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: nestedText(result)}}}, err
	}))
	h, root, _, entries := runNested(t, setup, callStep("outer", nil), fauxText("done"))
	must(t, h.WaitForIdle(bg))
	if text := resultText(toolResults(entries)[0]); text != "echo deep" {
		t.Fatalf("result = %q", text)
	}
	tasks := allToolTasks(t, h, root)
	outer, inner, deep := tasks[0], tasks[1], tasks[2]
	if inner.Owner == nil || *inner.Owner != outer.ID || deep.Owner == nil || *deep.Owner != inner.ID {
		t.Fatalf("owners = %v, %v", inner.Owner, deep.Owner)
	}
	if call := toolInput(t, deep).Call; call == nil || call.ID != "c1/1/x" {
		t.Fatalf("deep call = %+v", call)
	}
	expectIdleLive(t, h, root)
}

func liveNested(t *testing.T, h *Harness, root *Conversation) []map[string]any {
	t.Helper()
	slots, _ := liveOf(t, h, root)["nestedTools"].([]any)
	out := make([]map[string]any, len(slots))
	for i, slot := range slots {
		out[i], _ = slot.(map[string]any)
	}
	return out
}

func TestNestedToolsCommitOnlyTheStatusOfANestedCallMadeWithoutProgressAndKeepItsOutputInTheResult(t *testing.T) {
	setup := newChatSetup(t)
	running, release := make(chan struct{}), make(chan struct{})
	installTool(t, setup.registry, nestedTool("chatty", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		if err := api.Output([]byte("line\n"), nil); err != nil {
			return ToolExecutionResult{}, err
		}
		if err := api.Diagnostic(ToolDiagnostic{Severity: "info", Message: "note"}); err != nil {
			return ToolExecutionResult{}, err
		}
		if err := api.Details(ctx, map[string]any{"step": 1}); err != nil {
			return ToolExecutionResult{}, err
		}
		close(running)
		<-release
		return ToolExecutionResult{}, nil
	}))
	var result NestedToolExecutionResult
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		var err error
		result, err = api.ExecuteTool(ctx, "chatty", map[string]any{}, NestedCallOptions{NoProgress: true})
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	submission := submit(t, root, inputDraft("go"))
	// Details returned before this, so a progress commit would have landed
	// already.
	awaitSignal(t, running, "the nested call")
	slots := liveNested(t, h, root)
	if len(slots) != 1 {
		t.Fatalf("slots = %s", jsonOf(t, slots))
	}
	want := fmt.Sprintf(`{"arguments":{},"callId":"c1/1","name":"chatty","parentCallId":"c1","parentTaskId":%v,"status":"running","taskId":%v}`, slots[0]["parentTaskId"], slots[0]["taskId"])
	if got := jsonOf(t, slots[0]); got != want {
		t.Fatalf("slot = %s, want %s", got, want)
	}
	close(release)
	waitFor(t, submission)
	if string(result.StructuredOutput) != `"line\n"` || normalJSON(t, result.Details) != `{"step":1}` || jsonOf(t, result.Diagnostics) != `[{"severity":"info","message":"note"}]` {
		t.Fatalf("result = %s", jsonOf(t, result))
	}
}

func TestNestedToolsShowANestedCallsArgumentsInItsSlotAsMadeThenUnboundedAsItRunsWithThem(t *testing.T) {
	setup := newChatSetup(t)
	hookEntered, hookRelease := make(chan struct{}), make(chan struct{})
	running, release := make(chan struct{}), make(chan struct{})
	large := strings.Repeat("x", 64*1024)
	installTool(t, setup.registry, nestedTool("slow", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		close(running)
		<-release
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	installHooks(t, setup.registry, "hooks", ToolTask, &ToolHooks{BeforeTool: func(_ context.Context, call ToolHookCall, _ *TaskRuntime) (*BeforeToolDecision, error) {
		if call.Parent == nil {
			return nil, nil
		}
		close(hookEntered)
		<-hookRelease
		return &BeforeToolDecision{Arguments: map[string]any{"text": large}}, nil
	}})
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		_, err := api.ExecuteTool(ctx, "slow", map[string]any{"text": "asked"}, NestedCallOptions{})
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	submission := submit(t, root, inputDraft("go"))
	awaitSignal(t, hookEntered, "beforeTool")
	if slot := liveNested(t, h, root)[0]; slot["status"] != slotPending || jsonOf(t, slot["arguments"]) != `{"text":"asked"}` {
		t.Fatalf("pending slot = %s", jsonOf(t, slot))
	}
	close(hookRelease)
	awaitSignal(t, running, "the nested call")
	if slot := liveNested(t, h, root)[0]; slot["status"] != slotRunning || jsonOf(t, slot["arguments"]) != jsonOf(t, map[string]any{"text": large}) {
		t.Fatalf("running slot status = %v", slot["status"])
	}
	close(release)
	waitFor(t, submission)
	// The task input keeps the arguments the call was made with.
	for _, task := range allToolTasks(t, h, root) {
		if input := toolInput(t, task); input.Kind == toolCallNested && jsonOf(t, input.Call.Arguments) != `{"text":"asked"}` {
			t.Fatalf("nested input = %s", task.Input)
		}
	}
}

func TestNestedToolsRunANestedCallWhoseRepairedArgumentsDropAnOptionalProperty(t *testing.T) {
	// pi's repair sets the property to undefined, which strict JSON drops;
	// Go has no undefined, so the repair drops it.
	setup := newChatSetup(t)
	var seen []map[string]any
	installTool(t, setup.registry, nestedTool("repaired", func(_ context.Context, args map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		seen = append(seen, args)
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}, func(tool *ToolRegistration) {
		tool.PrepareArguments = func(any) (any, error) { return map[string]any{}, nil }
	}))
	var result NestedToolExecutionResult
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		var err error
		result, err = api.ExecuteTool(ctx, "repaired", map[string]any{"text": "x"}, NestedCallOptions{})
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	}))
	runNested(t, setup, callStep("batch", nil), fauxText("done"))
	if result.IsError || jsonOf(t, seen) != `[{}]` {
		t.Fatalf("result = %s, seen = %s", jsonOf(t, result), jsonOf(t, seen))
	}
}

func TestNestedToolsFindEachNestedCallsSlotAfterSlotsInTheMiddleWereRemoved(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, newCountingEcho().registration)
	installTool(t, setup.registry, nestedTool("inner", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		for _, text := range []string{"m1", "m2"} {
			if _, err := api.ExecuteTool(ctx, "echo", map[string]any{"text": text}, NestedCallOptions{}); err != nil {
				return ToolExecutionResult{}, err
			}
		}
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	before, checked := make(chan struct{}), make(chan struct{})
	var slots []string
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		// n2 makes m1 and m2 and settles, which removes them from between n2
		// and n3.
		for _, c := range []struct{ name, text string }{{"echo", "n1"}, {"inner", ""}, {"echo", "n3"}, {"echo", "n4"}} {
			if _, err := api.ExecuteTool(ctx, c.name, map[string]any{"text": c.text}, NestedCallOptions{}); err != nil {
				return ToolExecutionResult{}, err
			}
		}
		live, err := api.Snapshot(ctx, LiveDoc.Conversation(api.ConversationID()))
		if err != nil {
			return ToolExecutionResult{}, err
		}
		list, _ := live["nestedTools"].([]any)
		for _, item := range list {
			slot := item.(map[string]any)
			summary, _ := slot["summary"].(map[string]any)
			_, hasDuration := summary["durationMs"]
			slots = append(slots, fmt.Sprintf("%v %v %v %v", slot["callId"], slot["status"], summary["isError"], hasDuration))
		}
		close(before)
		<-checked
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	_, root := openChat(t, memory.New(), setup)
	submission := submit(t, root, inputDraft("go"))
	awaitSignal(t, before, "the snapshot")
	expectStrings(t, "slots", slots, []string{"c1/1 done false true", "c1/2 done false true", "c1/3 done false true", "c1/4 done false true"})
	close(checked)
	waitFor(t, submission)
}

func TestNestedToolsGiveCallersAResultWithItsTaskIDAndOnlyDiagnosticsForANestedCallTheSchedulerFaulted(t *testing.T) {
	setup := newChatSetup(t)
	// Details that are not JSON make the result commit fail, so the task
	// faults and stores no result.
	installTool(t, setup.registry, nestedTool("echo", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Details: map[string]any{"count": func() {}}}, nil
	}))
	var result NestedToolExecutionResult
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		var err error
		result, err = api.ExecuteTool(ctx, "echo", map[string]any{"text": "a"}, NestedCallOptions{})
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	}))
	h, root, _, _ := runNested(t, setup, callStep("batch", nil), fauxText("done"))
	child := allToolTasks(t, h, root)[1]
	if status, _ := toolOutcome(child); status != durable.OutcomeFaulted {
		t.Fatalf("child outcome = %s", status)
	}
	if result.TaskID != child.ID || !result.IsError || result.StructuredOutput != nil || result.Details != nil || len(result.Diagnostics) != 1 ||
		result.Diagnostics[0].Severity != "error" || result.Diagnostics[0].Code != "faulted" || !strings.Contains(result.Diagnostics[0].Message, "Tool echo failed") {
		t.Fatalf("result = %s", jsonOf(t, result))
	}
}

func TestNestedToolsGiveCallersWhatASchemaLessToolStreamedBeforeItThrew(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, nestedTool("fails", func(_ context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		if err := api.Output([]byte("half way\n"), nil); err != nil {
			return ToolExecutionResult{}, err
		}
		return ToolExecutionResult{}, errors.New("broke")
	}))
	var result NestedToolExecutionResult
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		var err error
		result, err = api.ExecuteTool(ctx, "fails", map[string]any{}, NestedCallOptions{})
		return ToolExecutionResult{Output: ai.ContentList{}}, err
	}))
	runNested(t, setup, callStep("batch", nil), fauxText("done"))
	if !result.IsError || string(result.StructuredOutput) != `"half way\n"` || firstCode(result) != "tool_error" || result.Diagnostics[0].Message != "broke" {
		t.Fatalf("result = %s", jsonOf(t, result))
	}
}

func TestNestedToolsShowAFinishedNestedCallAsADoneSlotWithItsSummaryUntilItsCallerSettles(t *testing.T) {
	setup := newChatSetup(t)
	release, checked := make(chan struct{}), make(chan struct{})
	// Diagnostics win over output for the error text, which is cut to 500
	// characters.
	installTool(t, setup.registry, returning("verbose", ToolExecutionResult{
		Output:      ai.ContentList{ai.TextContent{Text: "ignored"}},
		IsError:     true,
		Diagnostics: []ToolDiagnostic{{Severity: "error", Message: strings.Repeat("e", 600)}},
	}))
	usage := ai.Usage{Input: 1, Output: 2, TotalTokens: 3, Cost: ai.CostBreakdown{Input: 0.1, Output: 0.2, Total: 0.3}}
	installTool(t, setup.registry, newCountingEcho().registration)
	installTool(t, setup.registry, returning("paid", ToolExecutionResult{Usage: &usage}))
	// An error with mixed content: programs get the list, the summary its
	// text.
	installTool(t, setup.registry, returning("broken", ToolExecutionResult{
		Output:  ai.ContentList{ai.TextContent{Text: "bad image"}, ai.ImageContent{Data: "AAAA", MimeType: "image/png"}},
		IsError: true,
	}))
	installTool(t, setup.registry, nestedTool("batch", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		for _, c := range []struct {
			name string
			args map[string]any
		}{{"echo", map[string]any{"text": "a"}}, {"missing", map[string]any{}}, {"paid", map[string]any{}}, {"broken", map[string]any{}}, {"verbose", map[string]any{}}} {
			if _, err := api.ExecuteTool(ctx, c.name, c.args, NestedCallOptions{}); err != nil {
				return ToolExecutionResult{}, err
			}
		}
		close(checked)
		<-release
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{callStep("batch", nil), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	submission := submit(t, root, inputDraft("go"))
	awaitSignal(t, checked, "the calls")
	var got []string
	for _, slot := range liveNested(t, h, root) {
		summary, _ := slot["summary"].(map[string]any)
		_, hasDuration := summary["durationMs"]
		delete(summary, "durationMs")
		got = append(got, fmt.Sprintf("%v %v %v %s", slot["callId"], slot["status"], hasDuration, normalJSON(t, []byte(jsonOf(t, summary)))))
	}
	usageJSON := normalJSON(t, []byte(jsonOf(t, usage)))
	expectStrings(t, "slots", got, []string{
		`c1/1 done true {"isError":false}`,
		`c1/2 done false {"error":"Tool missing is not available","isError":true}`,
		`c1/3 done true {"isError":false,"usage":` + usageJSON + `}`,
		`c1/4 done true {"error":"bad image","isError":true}`,
		`c1/5 done true {"error":"` + strings.Repeat("e", 500) + `","isError":true}`,
	})
	close(release)
	waitFor(t, submission)
	expectIdleLive(t, h, root)
}

func TestNestedToolsMigrateVersion1ToolTaskInputToAModelIssuedCall(t *testing.T) {
	input, checkpoint, err := ToolTask.Migrate([]byte(`{"assistant":3,"callId":"c1"}`), []byte(`{"phase":"call"}`), 1)
	must(t, err)
	if normalJSON(t, input) != `{"assistant":3,"callId":"c1","kind":"model"}` || normalJSON(t, checkpoint) != `{"phase":"call"}` {
		t.Fatalf("migrated = %s, %s", input, checkpoint)
	}
}

// storeVersion1 stores a live tool task for call c1 of an assistant entry,
// closes, and rewrites its record as version 1 stored it: {assistant,
// callId} input at version 1, with checkpoint.
func storeVersion1(t *testing.T, dir string, setup *chatSetup, checkpoint string) durable.TaskID {
	t.Helper()
	// No submission, so the Harness never starts scheduling and the task
	// stays pending.
	h, root := openChat(t, openJSONL(t, dir), setup)
	var id durable.TaskID
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		message := toolCalls(call("echo", map[string]any{"text": "old"}, "c1"))
		entry, err := tx.AppendEntry(root.ID, durable.EntryDraft{Kind: durable.AssistantEntry.Kind, Model: []ai.Message{*message}})
		if err != nil {
			return err
		}
		assistant := entry.ID
		id, err = CreateTask(tx, ToolTask, ToolTaskInput{Kind: toolCallModel, Assistant: &assistant, CallID: "c1"}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		return err
	}))
	must(t, h.Close(bg))
	storage := openJSONL(t, dir)
	record, err := storage.Task(bg, id)
	must(t, err)
	var input map[string]any
	must(t, json.Unmarshal(record.Input, &input))
	delete(input, "kind")
	old := record.Clone()
	old.Version = 1
	old.Input, err = json.Marshal(input)
	must(t, err)
	old.State.Checkpoint = json.RawMessage(checkpoint)
	_, err = storage.Commit(bg, []durable.StorageWrite{durable.TaskWrite{Value: old}})
	must(t, err)
	must(t, storage.Close(bg))
	return id
}

func TestNestedToolsMigrateAPendingVersion1TaskAndRunItAsAModelIssuedCall(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	echo := newCountingEcho()
	installTool(t, setup.registry, echo.registration)
	id := storeVersion1(t, dir, setup, `{"phase":"call"}`)
	h, root := openChat(t, openJSONL(t, dir), setup)
	record, err := h.GetTask(bg, id)
	must(t, err)
	if record.Version != 1 {
		t.Fatalf("stored version = %d", record.Version)
	}
	must(t, h.Resume())
	settled, err := h.WaitForTask(bg, id)
	must(t, err)
	if input := toolInput(t, settled); settled.Version != 2 || input.Kind != toolCallModel || input.CallID != "c1" {
		t.Fatalf("settled = version %d, input %s", settled.Version, settled.Input)
	}
	if status, result := toolOutcome(settled); status != durable.OutcomeCompleted || !strings.HasPrefix(result, `{"kind":"model"`) {
		t.Fatalf("outcome = %+v", settled.State.Outcome)
	}
	if echo.runs.Load() != 1 {
		t.Fatalf("echo runs = %d", echo.runs.Load())
	}
	result := toolResults(chatEntries(t, root))[0]
	if result.ToolCallID != "c1" || resultText(result) != "echo old" {
		t.Fatalf("result = %s", jsonOf(t, result))
	}
}

func TestNestedToolsMigrateAVersion1TaskInterruptedAfterIntentAndSettleItInterrupted(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	echo := newCountingEcho()
	installTool(t, setup.registry, echo.registration)
	id := storeVersion1(t, dir, setup, `{"phase":"execute","arguments":{"text":"old"},"replay":"unsafe"}`)
	h, root := openChat(t, openJSONL(t, dir), setup)
	must(t, h.Resume())
	settled, err := h.WaitForTask(bg, id)
	must(t, err)
	if status, result := toolOutcome(settled); settled.Version != 2 || status != durable.OutcomeFailed || !strings.HasPrefix(result, `{"kind":"model"`) {
		t.Fatalf("settled = version %d, outcome %+v", settled.Version, settled.State.Outcome)
	}
	if echo.runs.Load() != 0 {
		t.Fatalf("echo runs = %d", echo.runs.Load())
	}
	result := toolResults(chatEntries(t, root))[0]
	if result.ToolCallID != "c1" || !result.IsError || !strings.Contains(resultText(result), "Tool echo was interrupted") {
		t.Fatalf("result = %s", jsonOf(t, result))
	}
}
