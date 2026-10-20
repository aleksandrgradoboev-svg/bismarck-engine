package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/output"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-tools.test.ts. The two
// "coding tools" cases are in durable/tools, beside the tools they run.

func echoSchema() *ai.Schema {
	var schema ai.Schema
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"text":{"type":"string"}}}`), &schema); err != nil {
		panic(err)
	}
	return &schema
}

type toolExecute = func(ctx context.Context, args map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error)

// echoTool is a tool named name with optional text arguments.
func echoTool(name string, execute toolExecute, extra ...func(*ToolRegistration)) *ToolRegistration {
	tool := &ToolRegistration{Tool: ai.Tool{Name: name, Description: "The " + name + " tool", Parameters: echoSchema()}, Execute: execute}
	for _, change := range extra {
		change(tool)
	}
	return tool
}

// installTool installs a one-tool extension and returns it, so the test can
// uninstall it.
func installTool(t *testing.T, registry Registry, tool *ToolRegistration) *Extension {
	t.Helper()
	extension := &Extension{Name: "tool:" + tool.Name, Tools: []*ToolRegistration{tool}}
	must(t, registry.Install(extension))
	return extension
}

// installHooks installs a one-hook extension for task.
func installHooks(t *testing.T, registry Registry, name string, task Task, handlers any) *Extension {
	t.Helper()
	extension := &Extension{Name: name, Hooks: []HookRegistration{Hook(task, handlers)}}
	must(t, registry.Install(extension))
	return extension
}

// toolCalls is a tool-calling answer with one call per [name, args, id].
func toolCalls(calls ...[3]any) *ai.AssistantMessage {
	content := ai.ContentList{}
	for _, c := range calls {
		args, _ := c[1].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		content = append(content, providers.FauxToolCall(c[0].(string), args, c[2].(string)))
	}
	return providers.FauxAssistantMessage(content, ai.StopToolUse)
}

func call(name string, args map[string]any, id string) [3]any { return [3]any{name, args, id} }

// runChat opens a chat with responses, runs prepare, submits "go" and waits.
func runChat(t *testing.T, setup *chatSetup, responses []providers.FauxResponseStep, prepare func(h *Harness, root *Conversation)) (*Harness, *Conversation, []durable.EntryRecord, string) {
	t.Helper()
	setup.faux.SetResponses(responses)
	h, root := openChat(t, memory.New(), setup)
	if prepare != nil {
		prepare(h, root)
	}
	settled := waitFor(t, submit(t, root, inputDraft("go")))
	return h, root, chatEntries(t, root), settled.Status
}

func toolResults(entries []durable.EntryRecord) []ai.ToolResultMessage {
	var out []ai.ToolResultMessage
	for _, entry := range entries {
		if entry.Kind == durable.ToolResultEntry.Kind {
			if message, ok := entry.Model[0].(ai.ToolResultMessage); ok {
				out = append(out, message)
			}
		}
	}
	return out
}

func resultText(message ai.ToolResultMessage) string {
	parts := make([]string, len(message.Content))
	for i, item := range message.Content {
		switch c := item.(type) {
		case ai.TextContent:
			parts[i] = c.Text
		case ai.ImageContent:
			parts[i] = "[image]"
		default:
			parts[i] = "[other]"
		}
	}
	return strings.Join(parts, "|")
}

func noOutput(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
	return ToolExecutionResult{Output: ai.ContentList{}}, nil
}

func lastSystem(t *testing.T, entries []durable.EntryRecord) ai.SystemMessage {
	t.Helper()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == durable.SystemEntry.Kind {
			return entries[i].Model[0].(ai.SystemMessage)
		}
	}
	t.Fatal("no pi.system entry")
	return ai.SystemMessage{}
}

func conversationTasks(t *testing.T, h *Harness, conversation *Conversation, kind string) []durable.TaskRecord {
	t.Helper()
	var page durable.Page[durable.TaskRecord]
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		var err error
		page, err = tx.ScanTasks(durable.TaskQuery{ConversationID: &conversation.ID, Kind: kind}, 50, nil)
		return err
	}))
	return page.Items
}

func TestToolRoundRunsInputToolCallToolResultAndAnswerAndSettlesTheInput(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("echo", func(_ context.Context, args map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "echo " + args["text"].(string)}}}, nil
	}))
	h, root, entries, status := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("echo", map[string]any{"text": "hi"}, "c1"))), fauxText("done")}, nil)
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	if got := entryKinds(entries); !slices.Equal(got, []string{"pi.user", "pi.system", "pi.assistant", "pi.tool-result", "pi.assistant"}) {
		t.Fatalf("kinds = %v", got)
	}
	system := entries[1].Model[0].(ai.SystemMessage)
	if len(system.ToolsAdded) != 1 || system.ToolsAdded[0].Name != "echo" || system.ToolsAdded[0].Description != "The echo tool" || system.ToolsAdded[0].Parameters == nil {
		t.Fatalf("toolsAdded = %+v", system.ToolsAdded)
	}
	result := toolResults(entries)[0]
	if result.ToolCallID != "c1" || result.ToolName != "echo" || result.IsError || resultText(result) != "echo hi" {
		t.Fatalf("result = %+v %q", result, resultText(result))
	}
	if string(entries[3].Data) != `{"diagnostics":[]}` || entries[3].ByTaskID == nil {
		t.Fatalf("result entry = %s by %v", entries[3].Data, entries[3].ByTaskID)
	}
	// The second request sees the tool result right after its call.
	if calls := setup.faux.State.CallCount; calls != 2 {
		t.Fatalf("model calls = %d", calls)
	}
	expectIdleLive(t, h, root)
}

// pi#10395.
func TestToolRoundGivesToolsAndHooksTheHarnessModels(t *testing.T) {
	setup := newChatSetup(t)
	var mu sync.Mutex
	var seen []ai.Models
	installTool(t, setup.registry, echoTool("echo", func(_ context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		mu.Lock()
		seen = append(seen, api.Models())
		mu.Unlock()
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	installHooks(t, setup.registry, "hooks", ToolTask, &ToolHooks{BeforeTool: func(_ context.Context, _ ToolHookCall, rt *TaskRuntime) (*BeforeToolDecision, error) {
		mu.Lock()
		seen = append(seen, rt.Models())
		mu.Unlock()
		return nil, nil
	}})
	_, _, _, status := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("echo", nil, "c1"))), fauxText("done")}, nil)
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != setup.models || seen[1] != setup.models {
		t.Fatalf("models seen = %v, want the Harness's twice", seen)
	}
}

func TestToolRoundAnswersCallsToToolsTheRequestDidNotOfferWithoutATask(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("echo", noOutput))
	h, root, entries, status := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("ghost", nil, "c1"), call("echo", nil, "c2"))), fauxText("done")}, nil)
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	results := toolResults(entries)
	if results[0].ToolCallID != "c1" || !results[0].IsError || resultText(results[0]) != "<harness>\n[error] Tool ghost is not available\n</harness>" {
		t.Fatalf("ghost = %+v", results[0])
	}
	if results[1].ToolCallID != "c2" || results[1].IsError {
		t.Fatalf("echo = %+v", results[1])
	}
	for _, entry := range entries {
		if entry.Kind == durable.ToolResultEntry.Kind && entry.Model[0].(ai.ToolResultMessage).ToolCallID == "c1" {
			if string(entry.Data) != `{"diagnostics":[{"severity":"error","message":"Tool ghost is not available","code":"tool_unavailable"}]}` {
				t.Fatalf("ghost data = %s", entry.Data)
			}
		}
	}
	if tools := conversationTasks(t, h, root, "pi.tool"); len(tools) != 1 {
		t.Fatalf("tool tasks = %d, want 1", len(tools))
	}
}

func TestToolRoundAnswersACallToAToolDeactivatedAfterPreparationWithToolUnavailable(t *testing.T) {
	setup := newChatSetup(t)
	var ran bool
	installTool(t, setup.registry, echoTool("echo", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		ran = true
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	var root *Conversation
	deactivate := func(context.Context, ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
		must(t, root.Configure(bg, AgentChange{Tools: Set(Exactly[*ToolRegistration]())}))
		return toolCalls(call("echo", nil, "c1"))
	}
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{deactivate, fauxText("done")}, func(_ *Harness, conversation *Conversation) { root = conversation })
	if ran {
		t.Fatal("the deactivated tool ran")
	}
	if got := resultText(toolResults(entries)[0]); got != "<harness>\n[error] Tool echo is not available\n</harness>" {
		t.Fatalf("result = %q", got)
	}
	// The next preparation removes it.
	if removed := lastSystem(t, entries).ToolsRemoved; len(removed) != 1 || removed[0].Name != "echo" {
		t.Fatalf("toolsRemoved = %+v", removed)
	}
}

func TestToolRoundRemovesUnregisteredActiveToolsFromTheOfferAndAddsThemBackAfterReRegistration(t *testing.T) {
	setup := newChatSetup(t)
	echo := echoTool("echo", noOutput)
	registration := installTool(t, setup.registry, echo)
	_, root, _, _ := runChat(t, setup, []providers.FauxResponseStep{fauxText("done")}, nil)
	setup.registry.Uninstall(registration)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("echo", nil, "c1"))), fauxText("done")})
	if settled := waitFor(t, submit(t, root, inputDraft("again"))); settled.Status != durable.SubmissionDone {
		t.Fatalf("second = %+v", settled)
	}
	entries := chatEntries(t, root)
	if removed := lastSystem(t, entries).ToolsRemoved; len(removed) != 1 || removed[0].Name != "echo" {
		t.Fatalf("toolsRemoved = %+v", removed)
	}
	if !toolResults(entries)[0].IsError {
		t.Fatal("the call of the unregistered tool succeeded")
	}
	// The stored agent is not rewritten; the tool is only not resolved.
	agent, err := root.Agent(bg)
	must(t, err)
	if len(agent.Tools) != 0 {
		t.Fatalf("agent tools = %v", toolNames(agent.Tools))
	}
	installTool(t, setup.registry, echo)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("done")})
	waitFor(t, submit(t, root, inputDraft("back")))
	added := lastSystem(t, chatEntries(t, root)).ToolsAdded
	if len(added) != 1 || added[0].Name != "echo" {
		t.Fatalf("toolsAdded = %+v", added)
	}
}

func TestToolRoundProducesToolUnavailableWhenTheImplementationIsUnregisteredBeforeItsTaskRuns(t *testing.T) {
	setup := newChatSetup(t)
	var second *Extension
	installTool(t, setup.registry, echoTool("first", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		setup.registry.Uninstall(second)
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}, func(tool *ToolRegistration) { tool.ExecutionMode = ToolExecutionSequential }))
	second = installTool(t, setup.registry, echoTool("second", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "ran"}}}, nil
	}))
	_, _, entries, status := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("first", nil, "c1"), call("second", nil, "c2"))), fauxText("done")}, nil)
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	if got := resultText(toolResults(entries)[1]); got != "<harness>\n[error] Tool second is not available\n</harness>" {
		t.Fatalf("late = %q", got)
	}
}

// eventTrace is a goroutine-safe list of events.
type eventTrace struct {
	mu     sync.Mutex
	events []string
}

func (e *eventTrace) add(event string) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

func (e *eventTrace) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.events)
}

func TestToolRoundReadsTheExecutionModeWhenARoundStartsAndKeepsItForTheRound(t *testing.T) {
	setup := newChatSetup(t)
	trace := &eventTrace{}
	slow := func(name string) toolExecute {
		return func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
			trace.add("start " + name)
			// Changed after the round started: not seen by this round.
			setup.setSettings(func(s *HarnessSettings) { s.ToolExecution = ToolExecutionParallel })
			time.Sleep(20 * time.Millisecond)
			trace.add("end " + name)
			return ToolExecutionResult{Output: ai.ContentList{}}, nil
		}
	}
	installTool(t, setup.registry, echoTool("a", slow("a")))
	installTool(t, setup.registry, echoTool("b", slow("b")))
	// Changed while the model request runs: the round that follows uses it.
	request := func(context.Context, ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
		setup.setSettings(func(s *HarnessSettings) { s.ToolExecution = ToolExecutionSequential })
		return toolCalls(call("a", nil, "c1"), call("b", nil, "c2"))
	}
	runChat(t, setup, []providers.FauxResponseStep{request, fauxText("done")}, nil)
	if got := trace.all(); !slices.Equal(got, []string{"start a", "end a", "start b", "end b"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestToolRoundRunsInParallelByDefaultAndSequentiallyWhenConfiguredOrRequiredByATool(t *testing.T) {
	traceRound := func(configure func(*chatSetup)) []string {
		setup := newChatSetup(t)
		if configure != nil {
			configure(setup)
		}
		trace := &eventTrace{}
		slow := func(name string) toolExecute {
			return func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
				trace.add("start " + name)
				time.Sleep(20 * time.Millisecond)
				trace.add("end " + name)
				return ToolExecutionResult{Output: ai.ContentList{}}, nil
			}
		}
		installTool(t, setup.registry, echoTool("a", slow("a")))
		installTool(t, setup.registry, echoTool("b", slow("b")))
		runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("a", nil, "c1"), call("b", nil, "c2"))), fauxText("done")}, nil)
		return trace.all()
	}
	// Both start before either ends; goroutines start in any order, where
	// pi's single thread starts them in creation order.
	if got := slices.Sorted(slices.Values(traceRound(nil)[:2])); !slices.Equal(got, []string{"start a", "start b"}) {
		t.Fatalf("default = %v", got)
	}
	if got := traceRound(func(s *chatSetup) {
		s.setSettings(func(settings *HarnessSettings) { settings.ToolExecution = ToolExecutionSequential })
	}); !slices.Equal(got, []string{"start a", "end a", "start b", "end b"}) {
		t.Fatalf("configured = %v", got)
	}
	setup := newChatSetup(t)
	trace := &eventTrace{}
	installTool(t, setup.registry, echoTool("a", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		time.Sleep(20 * time.Millisecond)
		trace.add("a")
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}, func(tool *ToolRegistration) { tool.ExecutionMode = ToolExecutionSequential }))
	installTool(t, setup.registry, echoTool("b", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		trace.add("b")
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	h, root, _, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("a", nil, "c1"), call("b", nil, "c2"))), fauxText("done")}, nil)
	tools := conversationTasks(t, h, root, "pi.tool")
	generations := conversationTasks(t, h, root, "pi.generation")
	// The generation owns both tools and creates the second only after the
	// first ended.
	if len(tools) != 2 || tools[0].Owner == nil || tools[1].Owner == nil || *tools[0].Owner != generations[0].ID || *tools[1].Owner != generations[0].ID {
		t.Fatalf("tool owners = %+v, generation %d", tools, generations[0].ID)
	}
	if got := trace.all(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("per-tool = %v", got)
	}
}

func toolResultByID(entries []durable.EntryRecord) map[string]ai.ToolResultMessage {
	out := map[string]ai.ToolResultMessage{}
	for _, result := range toolResults(entries) {
		out[result.ToolCallID] = result
	}
	return out
}

func diagnosticCodes(t *testing.T, entry durable.EntryRecord) []string {
	t.Helper()
	var data struct {
		Diagnostics []ToolDiagnostic `json:"diagnostics"`
	}
	must(t, json.Unmarshal(entry.Data, &data))
	return names(data.Diagnostics, func(d ToolDiagnostic) string { return d.Code })
}

func TestToolResultsUseRetainedOutputAndTheLastDetailsWhenTheResultOmitsThemWithDiagnosticsInOrder(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("log", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		must(t, api.Output([]byte("line 1\n"), nil))
		must(t, api.Output([]byte("line 2\nline 3\n"), nil))
		must(t, api.Diagnostic(ToolDiagnostic{Severity: "info", Message: "from api"}))
		must(t, api.Details(ctx, map[string]any{"step": 1}))
		must(t, api.Details(ctx, map[string]any{"step": 2}))
		return ToolExecutionResult{Diagnostics: []ToolDiagnostic{{Severity: "warn", Message: "from result"}}}, nil
	}, func(tool *ToolRegistration) { tool.OutputLimits = &OutputLimits{MaxLines: 2} }))
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("log", nil, "c1"))), fauxText("done")}, nil)
	result := toolResults(entries)[0]
	if jsonOf(t, result.Details) != `{"step":2}` {
		t.Fatalf("details = %s", jsonOf(t, result.Details))
	}
	if got := resultText(result); got != "line 1\nline 2\n|<harness>\n[info] from api\n[warn] from result\n[warn] Output truncated to its beginning: 1 lines, 7 bytes dropped\n</harness>" {
		t.Fatalf("result = %q", got)
	}
	for _, entry := range entries {
		if entry.Kind == durable.ToolResultEntry.Kind {
			if codes := diagnosticCodes(t, entry); !slices.Equal(codes, []string{"", "", "truncated"}) {
				t.Fatalf("codes = %q", codes)
			}
		}
	}
}

func TestToolResultsOfferTheTailWindowWithTheConfiguredPaceNotAHeadWindowAndAcceptSkippedOutput(t *testing.T) {
	setup := newChatSetup(t)
	setup.setSettings(func(s *HarnessSettings) { s.Progress.OutputInterval = ptr(250 * time.Millisecond) })
	var mu sync.Mutex
	windows := map[string]*env.OutputWindow{}
	installTool(t, setup.registry, echoTool("tailed", func(_ context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		mu.Lock()
		windows["tailed"] = api.OutputWindow()
		mu.Unlock()
		must(t, api.Output([]byte("dropped\n"), nil))
		must(t, api.Output([]byte("x\ny\n"), &output.Skip{Bytes: 8, Newlines: 1, EndsWithNewline: true}))
		return ToolExecutionResult{}, nil
	}, func(tool *ToolRegistration) { tool.OutputLimits = &OutputLimits{MaxLines: 1, Retain: "tail"} }))
	installTool(t, setup.registry, echoTool("headed", func(_ context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		mu.Lock()
		windows["headed"] = api.OutputWindow()
		mu.Unlock()
		return ToolExecutionResult{}, nil
	}, func(tool *ToolRegistration) { tool.OutputLimits = &OutputLimits{MaxLines: 1} }))
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("tailed", nil, "c1"), call("headed", nil, "c2"))), fauxText("done")}, nil)
	mu.Lock()
	defer mu.Unlock()
	if want := (env.OutputWindow{MaxBytes: 50 * 1024, MaxLines: 1, MinInterval: 250 * time.Millisecond, BytesPerSecond: 100 * 1024}); windows["tailed"] == nil || *windows["tailed"] != want {
		t.Fatalf("tail window = %+v, want %+v", windows["tailed"], want)
	}
	if windows["headed"] != nil {
		t.Fatalf("head window = %+v, want none", windows["headed"])
	}
	// 8 bytes written, 8 skipped, then "x\n" dropped by the window: 3 lines,
	// 18 bytes in all.
	if got := resultText(toolResultByID(entries)["c1"]); got != "y\n|<harness>\n[warn] Output truncated to its end: 3 lines, 18 bytes dropped\n</harness>" {
		t.Fatalf("result = %q", got)
	}
}

func TestToolResultsBoundExplicitTextContentAndKeepOtherContent(t *testing.T) {
	setup := newChatSetup(t)
	image := ai.ImageContent{Data: "AAAA", MimeType: "image/png"}
	installTool(t, setup.registry, echoTool("big", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "a\nb\n"}, image, ai.TextContent{Text: "c\nd\n"}}}, nil
	}, func(tool *ToolRegistration) { tool.OutputLimits = &OutputLimits{MaxLines: 2, Retain: "tail"} }))
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("big", nil, "c1"))), fauxText("done")}, nil)
	if got := resultText(toolResults(entries)[0]); got != "[image]|c\nd\n|<harness>\n[warn] Output truncated to its end: 2 lines, 4 bytes dropped\n</harness>" {
		t.Fatalf("result = %q", got)
	}
}

func TestToolResultsTurnAThrowIntoAToolErrorResultWithThePartialOutput(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("fail", func(_ context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		must(t, api.Output([]byte("partial\n"), nil))
		return ToolExecutionResult{}, errors.New("boom")
	}))
	_, _, entries, status := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("fail", nil, "c1"))), fauxText("done")}, nil)
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	result := toolResults(entries)[0]
	if !result.IsError || resultText(result) != "partial\n|<harness>\n[error] boom\n</harness>" {
		t.Fatalf("result = %v %q", result.IsError, resultText(result))
	}
}

// pi#10549.
func TestToolResultsRecordHowLongExecuteTookExcludingHooksAndNothingForCallsThatDidNotRun(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("slow", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		time.Sleep(30 * time.Millisecond)
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	installTool(t, setup.registry, echoTool("thrower", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		time.Sleep(30 * time.Millisecond)
		return ToolExecutionResult{}, errors.New("boom")
	}))
	installHooks(t, setup.registry, "hooks", ToolTask, &ToolHooks{BeforeTool: func(_ context.Context, call ToolHookCall, _ *TaskRuntime) (*BeforeToolDecision, error) {
		time.Sleep(100 * time.Millisecond)
		if call.ID == "blocked" {
			return &BeforeToolDecision{Block: ptr("no")}, nil
		}
		return nil, nil
	}})
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("slow", nil, "slow"), call("thrower", nil, "thrower"), call("slow", nil, "blocked"))), fauxText("done")}, nil)
	byID := toolResultByID(entries)
	for _, id := range []string{"slow", "thrower"} {
		if d := byID[id].DurationMs; d == nil || *d < 25 || *d >= 100 {
			t.Fatalf("%s duration = %v", id, d)
		}
	}
	if !byID["thrower"].IsError {
		t.Fatal("the thrower's result is not an error")
	}
	if byID["blocked"].DurationMs != nil {
		t.Fatalf("blocked duration = %d", *byID["blocked"].DurationMs)
	}
}

func TestToolResultsValidateArgumentsBeforeAndAfterBeforeToolAndApplyBlocksAndReplacements(t *testing.T) {
	setup := newChatSetup(t)
	var mu sync.Mutex
	var seen []string
	installTool(t, setup.registry, echoTool("echo", func(_ context.Context, args map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		mu.Lock()
		seen = append(seen, jsonOf(t, args))
		mu.Unlock()
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	installHooks(t, setup.registry, "hooks", ToolTask, &ToolHooks{BeforeTool: func(_ context.Context, call ToolHookCall, _ *TaskRuntime) (*BeforeToolDecision, error) {
		switch call.ID {
		case "block":
			return &BeforeToolDecision{Block: ptr("not today")}, nil
		case "throw":
			return nil, errors.New("hook failed")
		case "bad":
			return &BeforeToolDecision{Arguments: map[string]any{"text": map[string]any{"not": "a string"}}}, nil
		}
		return &BeforeToolDecision{Arguments: map[string]any{"text": call.Arguments["text"].(string) + "!"}}, nil
	}})
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(
		call("echo", map[string]any{"text": float64(1)}, "coerced"),
		call("echo", map[string]any{"text": map[string]any{"not": "a string"}}, "invalid"),
		call("echo", nil, "block"),
		call("echo", nil, "throw"),
		call("echo", map[string]any{"text": "x"}, "bad"),
		call("echo", map[string]any{"text": "x"}, "ok"),
	)), fauxText("done")}, nil)
	// Parallel tools append their results in completion order.
	byID := toolResultByID(entries)
	expect := func(id string, isError bool, text string) {
		t.Helper()
		if byID[id].IsError != isError || resultText(byID[id]) != text {
			t.Fatalf("%s = %v %q, want %v %q", id, byID[id].IsError, resultText(byID[id]), isError, text)
		}
	}
	expect("block", true, "<harness>\n[error] Tool call blocked: not today\n</harness>")
	expect("throw", true, "<harness>\n[error] Tool call blocked: hook failed\n</harness>")
	expect("ok", false, "")
	// The ai package coerces a number to a string before the first
	// validation.
	expect("coerced", false, "")
	for _, id := range []string{"bad", "invalid"} {
		if !byID[id].IsError || !strings.Contains(resultText(byID[id]), "Validation failed") {
			t.Fatalf("%s = %v %q, want a validation failure", id, byID[id].IsError, resultText(byID[id]))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Sort(seen); !slices.Equal(seen, []string{`{"text":"1!"}`, `{"text":"x!"}`}) {
		t.Fatalf("executed with %v", seen)
	}
}

func TestToolResultsRepairArgumentsWithPrepareArgumentsBeforeValidationAndAFailingRepairIsInvalid(t *testing.T) {
	setup := newChatSetup(t)
	var mu sync.Mutex
	var seen []string
	installTool(t, setup.registry, echoTool("echo", func(_ context.Context, args map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
		mu.Lock()
		seen = append(seen, jsonOf(t, args))
		mu.Unlock()
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}, func(tool *ToolRegistration) {
		tool.PrepareArguments = func(args any) (any, error) {
			text := args.(map[string]any)["text"]
			if text == "throw" {
				return nil, errors.New("cannot repair")
			}
			if number, ok := text.(float64); ok {
				return map[string]any{"text": fmt.Sprintf("#%v", number)}, nil
			}
			return args, nil
		}
	}))
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("echo", map[string]any{"text": float64(7)}, "fixed"), call("echo", map[string]any{"text": "throw"}, "broken"))), fauxText("done")}, nil)
	byID := toolResultByID(entries)
	if resultText(byID["fixed"]) != "" || resultText(byID["broken"]) != "<harness>\n[error] cannot repair\n</harness>" {
		t.Fatalf("fixed %q, broken %q", resultText(byID["fixed"]), resultText(byID["broken"]))
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []string{`{"text":"#7"}`}) {
		t.Fatalf("executed with %v", seen)
	}
	// The stored call keeps what the model sent.
	stored, _ := toolCallOf(entryAssistant(t, entries[2]).Content[0])
	if jsonOf(t, stored.Arguments) != `{"text":7}` {
		t.Fatalf("stored arguments = %s", jsonOf(t, stored.Arguments))
	}
}

func TestToolResultsLetTheFirstBeforeToolBlockWinAndSkipLaterHandlers(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("echo", noOutput))
	trace := &eventTrace{}
	installHooks(t, setup.registry, "first", ToolTask, &ToolHooks{BeforeTool: func(context.Context, ToolHookCall, *TaskRuntime) (*BeforeToolDecision, error) {
		trace.add("first")
		return &BeforeToolDecision{Block: ptr("first says no")}, nil
	}})
	installHooks(t, setup.registry, "second", ToolTask, &ToolHooks{BeforeTool: func(context.Context, ToolHookCall, *TaskRuntime) (*BeforeToolDecision, error) {
		trace.add("second")
		return &BeforeToolDecision{Block: ptr("second says no")}, nil
	}})
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("echo", nil, "c1"))), fauxText("done")}, nil)
	if got := trace.all(); !slices.Equal(got, []string{"first"}) {
		t.Fatalf("asked = %v", got)
	}
	if got := resultText(toolResults(entries)[0]); got != "<harness>\n[error] Tool call blocked: first says no\n</harness>" {
		t.Fatalf("result = %q", got)
	}
}

func TestToolResultsChainAfterToolReplacementsAndObserveTheRoundWithAfterTools(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("echo", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "raw"}}}, nil
	}))
	installHooks(t, setup.registry, "first", ToolTask, &ToolHooks{AfterTool: func(_ context.Context, _ ToolHookCall, result ToolExecutionResult, _ *TaskRuntime) (*ToolExecutionResult, error) {
		result.Output = ai.ContentList{ai.TextContent{Text: "first"}}
		return &result, nil
	}})
	installHooks(t, setup.registry, "second", ToolTask, &ToolHooks{AfterTool: func(_ context.Context, _ ToolHookCall, result ToolExecutionResult, _ *TaskRuntime) (*ToolExecutionResult, error) {
		var texts []string
		for _, item := range result.Output {
			if text, ok := item.(ai.TextContent); ok {
				texts = append(texts, text.Text)
			}
		}
		result.Details = map[string]any{"replaced": strings.Join(texts, "|")}
		return &result, nil
	}})
	var mu sync.Mutex
	var observed []any
	installHooks(t, setup.registry, "observer", GenerationTask, &GenerationHooks{AfterTools: func(_ context.Context, assistant durable.EntryID, results []durable.EntryID, _ *TaskRuntime) error {
		mu.Lock()
		observed = append(observed, assistant, results)
		mu.Unlock()
		return nil
	}})
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("echo", nil, "c1"))), fauxText("done")}, nil)
	result := toolResults(entries)[0]
	if resultText(result) != "first" || jsonOf(t, result.Details) != `{"replaced":"first"}` {
		t.Fatalf("result = %q %s", resultText(result), jsonOf(t, result.Details))
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []any{entries[2].ID, []durable.EntryID{entries[3].ID}}; !reflect.DeepEqual(observed, want) {
		t.Fatalf("afterTools saw %v, want %v", observed, want)
	}
}

func TestToolResultsRunTheHooksOfTheSelectedExtensionsAndATaskOwnedChildCopiesItsOwnersSelection(t *testing.T) {
	setup := newChatSetup(t)
	var mu sync.Mutex
	var calledIn []durable.ConversationID
	echoExtension := &Extension{Name: "echo", Tools: []*ToolRegistration{echoTool("echo", noOutput)}}
	audit := &Extension{Name: "audit", Hooks: []HookRegistration{Hook(ToolTask, &ToolHooks{BeforeTool: func(_ context.Context, _ ToolHookCall, rt *TaskRuntime) (*BeforeToolDecision, error) {
		mu.Lock()
		calledIn = append(calledIn, rt.ConversationID)
		mu.Unlock()
		return nil, nil
	}})}}
	must(t, setup.registry.Install(echoExtension))
	must(t, setup.registry.Install(audit))
	// Audit is installed but not in the default selection.
	setup.setSettings(func(s *HarnessSettings) { s.Extensions = []*Extension{echoExtension} })
	h, root, _, _ := runChat(t, setup, []providers.FauxResponseStep{fauxText("done")}, nil)
	must(t, root.Configure(bg, AgentChange{Extensions: Set(Selection[*Extension]{Add: []*Extension{audit}})}))
	// An owner task no registered definition takes stays live and pending.
	owner := oneStep(t, "test.owner", func(context.Context, *stepTask, *TaskRuntime) error { return nil }, nil)
	var childID durable.ConversationID
	must(t, h.Commit(bg, func(tx *durable.Tx) error {
		taskID, err := CreateTask(tx, owner, map[string]any{}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}, ConversationID: root.ID})
		if err != nil {
			return err
		}
		record, err := tx.CreateConversation(durable.ConversationOwnership{Kind: durable.OwnershipTask, TaskID: taskID})
		childID = record.ID
		return err
	}))
	child, err := h.Conversation(bg, childID)
	must(t, err)
	other, err := h.CreateConversation(bg, ConversationCreateOptions{Ownership: durable.ConversationOwnership{Kind: durable.OwnershipOwnerless}, Agent: &AgentChange{Model: Set(ModelRef{Provider: "faux", ModelID: "faux-1"})}})
	must(t, err)
	for _, conversation := range []*Conversation{root, child, other} {
		setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("echo", nil, "c1"))), fauxText("done")})
		waitFor(t, submit(t, conversation, inputDraft("go")))
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calledIn, []durable.ConversationID{root.ID, child.ID}) {
		t.Fatalf("hook ran in %v, want %d and %d", calledIn, root.ID, child.ID)
	}
}

func TestToolResultsApplyAddToolsAndTerminateOnlyWhenEveryResultOfTheRoundAsksTo(t *testing.T) {
	setup := newChatSetup(t)
	stop := echoTool("stop", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{}, Control: &ToolControl{Terminate: true}}, nil
	})
	grow := echoTool("grow", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{}, Control: &ToolControl{AddTools: []string{"extra", "stop"}}}, nil
	})
	installTool(t, setup.registry, stop)
	installTool(t, setup.registry, grow)
	installTool(t, setup.registry, echoTool("extra", noOutput))
	h, root, entries, status := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("stop", nil, "c1")))}, func(_ *Harness, root *Conversation) {
		must(t, root.Configure(bg, AgentChange{Tools: Set(Exactly(stop, grow))}))
	})
	if status != durable.SubmissionDone || entries[len(entries)-1].Kind != durable.ToolResultEntry.Kind {
		t.Fatalf("status %s, last entry %s", status, entries[len(entries)-1].Kind)
	}
	for _, task := range conversationTasks(t, h, root, "") {
		if task.State.Status != durable.TaskTerminal {
			t.Fatalf("task %d (%s) is %s", task.ID, task.Kind, task.State.Status)
		}
	}
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("stop", nil, "c1"), call("grow", nil, "c2"))), fauxText("done")})
	if settled := waitFor(t, submit(t, root, inputDraft("again"))); settled.Status != durable.SubmissionDone {
		t.Fatalf("second = %+v", settled)
	}
	if entries := chatEntries(t, root); entries[len(entries)-1].Kind != durable.AssistantEntry.Kind {
		t.Fatalf("last entry = %s", entries[len(entries)-1].Kind)
	}
	// addTools appends to the stored tool array, skipping names it already
	// holds.
	agent, err := h.Snapshot(bg, AgentDoc.Conversation(root.ID))
	must(t, err)
	if got := jsonOf(t, agent["tools"]); got != `["stop","grow","extra"]` {
		t.Fatalf("stored tools = %s", got)
	}
}

// pi#10858.
func TestToolResultsDoNotTerminateOnTerminateFalse(t *testing.T) {
	setup := newChatSetup(t)
	keep := echoTool("keep", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{}, Control: &ToolControl{Terminate: false}}, nil
	})
	installTool(t, setup.registry, keep)
	_, _, entries, status := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("keep", nil, "c1"))), fauxText("done")}, func(_ *Harness, root *Conversation) {
		must(t, root.Configure(bg, AgentChange{Tools: Set(Exactly(keep))}))
	})
	if status != durable.SubmissionDone || entries[len(entries)-1].Kind != durable.AssistantEntry.Kind {
		t.Fatalf("status %s, last entry %s", status, entries[len(entries)-1].Kind)
	}
}

func TestGenerationHooksReplaceRequestMessagesObserveResponsesAndContinueOnYield(t *testing.T) {
	setup := newChatSetup(t)
	log := &requestLog{}
	var mu sync.Mutex
	var responses []string
	yields := 0
	record := func(n int) providers.FauxResponseStep { return log.step(fauxAnswer(fmt.Sprintf("answer %d", n))) }
	installHooks(t, setup.registry, "request", GenerationTask, &GenerationHooks{BeforeRequest: func(_ context.Context, messages []ai.Message, _ *TaskRuntime) ([]ai.Message, error) {
		return append(slices.Clone(messages), ai.NewUserText("injected", 0)), nil
	}})
	installHooks(t, setup.registry, "response", GenerationTask, &GenerationHooks{AfterResponse: func(_ context.Context, message ai.AssistantMessage, _ *TaskRuntime) error {
		mu.Lock()
		responses = append(responses, partialText(message))
		mu.Unlock()
		return nil
	}})
	installHooks(t, setup.registry, "yield", GenerationTask, &GenerationHooks{OnYield: func(context.Context, ai.AssistantMessage, *TaskRuntime) (*UserInput, error) {
		mu.Lock()
		defer mu.Unlock()
		yields++
		if yields == 1 {
			input := ContentInput(ai.ContentList{ai.TextContent{Text: "keep going"}})
			return &input, nil
		}
		return nil, nil
	}})
	_, _, entries, status := runChat(t, setup, []providers.FauxResponseStep{record(1), record(2)}, nil)
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	first := log.all()[0]
	if got := first[len(first)-2:]; !slices.Equal(got, []string{"user:go", "user:injected"}) {
		t.Fatalf("first request ends %q", got)
	}
	mu.Lock()
	if !slices.Equal(responses, []string{"answer 1", "answer 2"}) {
		t.Fatalf("responses = %v", responses)
	}
	mu.Unlock()
	if got := entryKinds(entries); !slices.Equal(got, []string{"pi.user", "pi.assistant", "pi.user", "pi.assistant"}) {
		t.Fatalf("kinds = %v", got)
	}
	// The injected message was used for the request only.
	if slices.ContainsFunc(entries, func(entry durable.EntryRecord) bool { return textOf(entry) == "injected" }) {
		t.Fatal("the injected message was stored")
	}
}

func TestGenerationHooksKeepTheRunsInputOpenAcrossAnOnYieldContinuationAndAnswerItWithTheFinalAnswer(t *testing.T) {
	setup := newChatSetup(t)
	yields := 0
	installHooks(t, setup.registry, "yield", GenerationTask, &GenerationHooks{OnYield: func(context.Context, ai.AssistantMessage, *TaskRuntime) (*UserInput, error) {
		yields++
		if yields == 1 {
			input := TextInput("again")
			return &input, nil
		}
		return nil, nil
	}})
	var h *Harness
	var root *Conversation
	var input durable.SubmissionID
	var statusAtSecond string
	second := func(context.Context, ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
		input = decodeLive(t, liveOf(t, h, root)).Run.Inputs[0]
		submission, err := h.Submission(bg, input)
		must(t, err)
		statusAtSecond = submissionStatus(t, submission).Status
		return fauxAnswer("second")
	}
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxText("first"), second}, func(opened *Harness, conversation *Conversation) { h, root = opened, conversation })
	if statusAtSecond != durable.SubmissionPlaced {
		t.Fatalf("status at the second request = %s", statusAtSecond)
	}
	var answers []durable.EntryRecord
	for _, entry := range entries {
		if entry.Kind == durable.AssistantEntry.Kind {
			answers = append(answers, entry)
		}
	}
	submission, err := h.Submission(bg, input)
	must(t, err)
	if final := submissionStatus(t, submission); final.Status != durable.SubmissionDone || final.Answer == nil || *final.Answer != answers[1].ID {
		t.Fatalf("final = %+v, want done with the second answer", final)
	}
}

func TestGenerationHooksObserveResponsesThatArriveByPollingADeferredRequest(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{Deferred: &providers.FauxDeferredOptions{PendingFetches: 1, PollAfterMs: 1}})
	var mu sync.Mutex
	var observed []string
	installHooks(t, setup.registry, "observer", GenerationTask, &GenerationHooks{AfterResponse: func(_ context.Context, message ai.AssistantMessage, _ *TaskRuntime) error {
		mu.Lock()
		observed = append(observed, string(message.StopReason)+":"+partialText(message))
		mu.Unlock()
		return nil
	}})
	setup.setSettings(func(s *HarnessSettings) { s.Stream.Deferred = &StreamDeferral{} })
	_, _, _, status := runChat(t, setup, []providers.FauxResponseStep{fauxText("late")}, nil)
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	// The still deferred results are not terminal.
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(observed, []string{"stop:late"}) {
		t.Fatalf("observed = %v", observed)
	}
}

func TestGenerationHooksLetTheFirstOnYieldContinuationWinAndReportThrowsWithoutStoppingLaterHandlers(t *testing.T) {
	setup := newChatSetup(t)
	trace := &eventTrace{}
	installHooks(t, setup.registry, "throwing", GenerationTask, &GenerationHooks{AfterResponse: func(context.Context, ai.AssistantMessage, *TaskRuntime) error {
		trace.add("throwing observer")
		return errors.New("observer failed")
	}})
	installHooks(t, setup.registry, "next", GenerationTask, &GenerationHooks{AfterResponse: func(context.Context, ai.AssistantMessage, *TaskRuntime) error {
		trace.add("next observer")
		return nil
	}})
	yields := 0
	installHooks(t, setup.registry, "first-yield", GenerationTask, &GenerationHooks{OnYield: func(context.Context, ai.AssistantMessage, *TaskRuntime) (*UserInput, error) {
		yields++
		if yields == 1 {
			input := TextInput("first")
			return &input, nil
		}
		return nil, nil
	}})
	installHooks(t, setup.registry, "second-yield", GenerationTask, &GenerationHooks{OnYield: func(context.Context, ai.AssistantMessage, *TaskRuntime) (*UserInput, error) {
		trace.add("second onYield")
		if count(trace.all(), "second onYield") == 1 {
			input := TextInput("second")
			return &input, nil
		}
		return nil, nil
	}})
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxText("a"), fauxText("b"), fauxText("c")}, nil)
	// The first continuation skips the second handler; on the next answer
	// the second handler's continuation wins.
	var users []string
	for _, entry := range entries {
		if entry.Kind == durable.UserEntry.Kind {
			users = append(users, textOf(entry))
		}
	}
	if !slices.Equal(users, []string{"go", "first", "second"}) {
		t.Fatalf("users = %v", users)
	}
	if got := count(trace.all(), "second onYield"); got != 2 {
		t.Fatalf("second onYield ran %d times", got)
	}
	if got := count(trace.all(), "next observer"); got != 3 {
		t.Fatalf("next observer ran %d times", got)
	}
	if _, ok := setup.reports.infoOf("observer failed"); !ok {
		t.Fatalf("reports = %v", setup.reports.all())
	}
}

func count(items []string, item string) int {
	n := 0
	for _, each := range items {
		if each == item {
			n++
		}
	}
	return n
}

func TestGenerationHooksKeepDurableHookDecisionsInTaskMemos(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("echo", noOutput))
	asked := 0
	installHooks(t, setup.registry, "approval", ToolTask, &ToolHooks{BeforeTool: func(ctx context.Context, _ ToolHookCall, rt *TaskRuntime) (*BeforeToolDecision, error) {
		asked++
		decision, err := rt.MemoOr(ctx, "approval:decision", "approved")
		if err != nil {
			return nil, err
		}
		again, err := rt.MemoOr(ctx, "approval:decision", "denied")
		if err != nil {
			return nil, err
		}
		if string(again) != string(decision) {
			t.Errorf("memo changed from %s to %s", decision, again)
		}
		return nil, nil
	}})
	runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("echo", nil, "c1"))), fauxText("done")}, nil)
	if asked != 1 {
		t.Fatalf("asked = %d", asked)
	}
}

// openChatWith opens a chat like openChat, with options changed by change.
func openChatWith(t *testing.T, setup *chatSetup, change func(*Options)) (*Harness, *Conversation) {
	t.Helper()
	options := setup.options()
	change(&options)
	h, err := Open(bg, memory.New(), options)
	must(t, err)
	t.Cleanup(func() { _ = h.Close(bg) })
	root, err := h.Root(bg, &AgentChange{Model: Set(ModelRef{Provider: "faux", ModelID: "faux-1"})}, nil)
	must(t, err)
	return h, root
}

func TestToolExecutionAPIBuildsTheEnvironmentPerCallFromTheConversationsCwdAndRunsCommitsMemosAndChildTasks(t *testing.T) {
	setup := newChatSetup(t)
	first, second := t.TempDir(), t.TempDir()
	child := MustDefineTask(TaskSpec[map[string]any, stepState, float64]{
		Name:    "test.child",
		Version: 1,
		Initial: func(map[string]any) stepState { return stepState{Phase: "run"} },
		Phases: map[string]func(context.Context, *Running[map[string]any, stepState], *TaskRuntime) error{
			"run": func(ctx context.Context, task *Running[map[string]any, stepState], rt *TaskRuntime) error {
				n := task.Input["n"].(float64)
				return rt.Commit(ctx, func(*durable.Tx, durable.TaskRecord) (*durable.TaskState, error) { return Complete(n * 2) })
			},
		},
		Abort: func(context.Context, *Running[map[string]any, stepState], *TaskRuntime) error { return nil },
	})
	addTask(t, setup.registry, child)
	var mu sync.Mutex
	var seen []any
	installTool(t, setup.registry, echoTool("probe", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		var local []any
		local = append(local, api.Env().Cwd())
		agent, err := api.Agent(ctx)
		must(t, err)
		local = append(local, toolNames(agent.Tools), api.Registry().Extension("tool:probe") != nil)
		var entry durable.EntryRecord
		must(t, api.Commit(ctx, func(tx *durable.Tx) error {
			// The next call runs in the new directory: its environment is
			// built when it executes.
			if err := Configure(tx, api.ConversationID(), AgentChange{Cwd: Set(second)}); err != nil {
				return err
			}
			var err error
			entry, err = tx.AppendEntry(api.ConversationID(), durable.EntryDraft{Kind: "test.note", Data: json.RawMessage(jsonOf(t, api.CallID()))})
			return err
		}))
		local = append(local, entry.ByTaskID != nil && *entry.ByTaskID == api.TaskID())
		memo1, err := api.MemoOr(ctx, "m", 1)
		must(t, err)
		memo2, err := api.MemoOr(ctx, "m", 2)
		must(t, err)
		local = append(local, string(memo1), string(memo2))
		id, err := api.CreateTask(ctx, child, map[string]any{"n": 21}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		must(t, err)
		done, err := api.WaitForTask(ctx, id)
		must(t, err)
		local = append(local, done.State.Outcome.Status+":"+string(done.State.Outcome.Result))
		mu.Lock()
		seen = append(seen, local...)
		mu.Unlock()
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("probe", nil, "c1"), call("probe", nil, "c2"))), fauxText("done")})
	setup.setSettings(func(s *HarnessSettings) { s.ToolExecution = ToolExecutionSequential })
	var targets []string
	h, root := openChatWith(t, setup, func(options *Options) {
		options.Env = func(_ context.Context, target EnvTarget) (env.ExecutionEnv, error) {
			cwd := first
			if target.Cwd != nil {
				cwd = *target.Cwd
			}
			mu.Lock()
			targets = append(targets, fmt.Sprintf("%d:%s", target.ConversationID, cwd))
			mu.Unlock()
			return env.NewLocal(cwd), nil
		}
	})
	_ = h
	must(t, root.Configure(bg, AgentChange{Cwd: Set(first)}))
	waitFor(t, submit(t, root, inputDraft("go")))
	perCall := func(cwd string) []any { return []any{cwd, []string{"probe"}, true, true, "1", "1", "completed:42"} }
	mu.Lock()
	defer mu.Unlock()
	if want := append(perCall(first), perCall(second)...); !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen = %v, want %v", seen, want)
	}
	for _, cwd := range []string{first, second} {
		if !slices.Contains(targets, fmt.Sprintf("%d:%s", root.ID, cwd)) {
			t.Fatalf("env targets = %v, want one in %s", targets, cwd)
		}
	}
}

func TestToolExecutionAPIAnswersAThrowingEnvironmentWithAToolErrorResultAndReportsItOnceWhilePreparing(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("probe", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "ran"}}}, nil
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("probe", nil, "c1"))), fauxText("done")})
	_, root := openChatWith(t, setup, func(options *Options) {
		options.Env = func(context.Context, EnvTarget) (env.ExecutionEnv, error) { return nil, errors.New("no sandbox") }
	})
	waitFor(t, submit(t, root, inputDraft("go")))
	result := toolResults(chatEntries(t, root))[0]
	if !result.IsError || resultText(result) != "<harness>\n[error] no sandbox\n</harness>" {
		t.Fatalf("result = %v %q", result.IsError, resultText(result))
	}
	// Each preparation reports the failure and renders without an
	// environment.
	n := 0
	for _, err := range setup.reports.all() {
		if err.Error() == "no sandbox" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("no sandbox reported %d times, want 2", n)
	}
}

func TestToolProgressAppliesTheDefaultOutputLimits(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("lines", func(_ context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		for i := 1; i <= 2500; i++ {
			must(t, api.Output([]byte(fmt.Sprintf("%d\n", i)), nil))
		}
		return ToolExecutionResult{}, nil
	}))
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("lines", nil, "c1"))), fauxText("done")}, nil)
	text := resultText(toolResults(entries)[0])
	if !strings.HasPrefix(text, "1\n2\n") || !strings.HasSuffix(text, "\n2000\n|<harness>\n[warn] Output truncated to its beginning: 500 lines, 2500 bytes dropped\n</harness>") {
		t.Fatalf("result = %q…%q", text[:10], text[len(text)-120:])
	}
}

func TestToolProgressSanitizesRunningOutputButKeepsExplicitResultContentAsTheToolReturnedIt(t *testing.T) {
	setup := newChatSetup(t)
	var slotOutput string
	installTool(t, setup.registry, echoTool("noisy", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		must(t, api.Output([]byte("a\u0007b\r\n"), nil))
		must(t, api.Details(ctx, map[string]any{"ready": true}))
		value, err := api.Snapshot(ctx, LiveDoc.Conversation(api.ConversationID()))
		must(t, err)
		var live struct {
			Tools []ToolSlot `json:"tools"`
		}
		must(t, fromValue(value, &live))
		if live.Tools[0].Output != nil {
			slotOutput = *live.Tools[0].Output
		}
		return ToolExecutionResult{}, nil
	}))
	installTool(t, setup.registry, echoTool("explicit", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "c\u001bd"}}}, nil
	}))
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("noisy", nil, "c1"), call("explicit", nil, "c2"))), fauxText("done")}, nil)
	if slotOutput != "ab\n" {
		t.Fatalf("slot output = %q", slotOutput)
	}
	byID := toolResultByID(entries)
	if resultText(byID["c1"]) != "ab\n" || resultText(byID["c2"]) != "c\u001bd" {
		t.Fatalf("results = %q, %q", resultText(byID["c1"]), resultText(byID["c2"]))
	}
}

// pi's case returns control keys set to undefined, which Go cannot; what is
// left is addTools deleting the name from a stored {remove} filter.
func TestToolProgressAddToolsDeletesTheNameFromAStoredRemoveFilter(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("grow", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{}, Control: &ToolControl{AddTools: []string{"extra"}}}, nil
	}))
	extra := echoTool("extra", noOutput)
	installTool(t, setup.registry, extra)
	h, root, _, status := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("grow", nil, "c1"))), fauxText("done")}, func(_ *Harness, conversation *Conversation) {
		must(t, conversation.Configure(bg, AgentChange{Tools: Set(Selection[*ToolRegistration]{Remove: []*ToolRegistration{extra}})}))
	})
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	agent, err := h.Snapshot(bg, AgentDoc.Conversation(root.ID))
	must(t, err)
	if got := jsonOf(t, agent["tools"]); got != `{"remove":[]}` {
		t.Fatalf("stored tools = %s", got)
	}
}

// pi returns `details: null`; Go has no undefined, so a tool returns
// json.RawMessage("null") to say "no details" instead of the last reported
// value. ai.ToolResultMessage then omits them.
func TestToolProgressUsesExplicitNullDetailsInsteadOfTheLastReportedValue(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, echoTool("null", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		must(t, api.Details(ctx, map[string]any{"old": 1}))
		return ToolExecutionResult{Output: ai.ContentList{}, Details: json.RawMessage("null")}, nil
	}))
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("null", nil, "c1"))), fauxText("done")}, nil)
	if details := toolResults(entries)[0].Details; details != nil {
		t.Fatalf("details = %v, want none", details)
	}
}

// pi checks the settle order its microtasks give; Go's waiters settle in
// order but report from their own goroutines, so this checks that each
// settles without error and the last value is the result's.
func TestToolProgressSettlesDetailsCallsWithCoalescedProgressCommitsAndTheTerminalCommit(t *testing.T) {
	setup := newChatSetup(t)
	results := make(chan error, 3)
	installTool(t, setup.registry, echoTool("details", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		// Three updates in one throttle window coalesce; the last is still
		// pending when Execute returns.
		first := make(chan error, 1)
		go func() { first <- api.Details(ctx, map[string]any{"n": 1}) }()
		time.Sleep(5 * time.Millisecond)
		go func() { results <- api.Details(ctx, map[string]any{"n": 2}) }()
		results <- <-first
		time.Sleep(5 * time.Millisecond)
		go func() { results <- api.Details(ctx, map[string]any{"n": 3}) }()
		time.Sleep(5 * time.Millisecond)
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	_, _, entries, _ := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("details", nil, "c1"))), fauxText("done")}, nil)
	for range 3 {
		select {
		case err := <-results:
			must(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("a details call never settled")
		}
	}
	if got := jsonOf(t, toolResults(entries)[0].Details); got != `{"n":3}` {
		t.Fatalf("details = %s", got)
	}
}

func TestToolProgressFinishesACallUnderTheImplementationItResolvedWhenTheToolIsReplacedMidCall(t *testing.T) {
	setup := newChatSetup(t)
	started, finish := make(chan struct{}), make(chan struct{})
	installTool(t, setup.registry, echoTool("work", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		close(started)
		<-finish
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "v1"}}}, nil
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("work", nil, "c1"))), fauxText("done")})
	_, root := openChat(t, memory.New(), setup)
	submission := submit(t, root, inputDraft("go"))
	awaitSignal(t, started, "the tool")
	// The same extension name replaces the old one in place.
	installTool(t, setup.registry, echoTool("work", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "v2"}}}, nil
	}))
	close(finish)
	waitFor(t, submission)
	if got := resultText(toolResults(chatEntries(t, root))[0]); got != "v1" {
		t.Fatalf("result = %q", got)
	}
}

func TestToolProgressUsesASectionAndHookExtensionReloadedMidRunFromTheRunsNextRequest(t *testing.T) {
	setup := newChatSetup(t)
	trace := &eventTrace{}
	prompt := func(version string) *Extension {
		return &Extension{
			Name:     "prompt",
			Sections: []*PromptSection{constSection("mode", version)},
			Hooks: []HookRegistration{Hook(GenerationTask, &GenerationHooks{BeforeRequest: func(context.Context, []ai.Message, *TaskRuntime) ([]ai.Message, error) {
				trace.add(version)
				return nil, nil
			}})},
		}
	}
	must(t, setup.registry.Install(prompt("v1")))
	running, reloaded := make(chan struct{}), make(chan struct{})
	installTool(t, setup.registry, echoTool("work", func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		close(running)
		<-reloaded
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("work", nil, "c1"))), fauxText("done")})
	_, root := openChat(t, memory.New(), setup)
	submission := submit(t, root, inputDraft("go"))
	awaitSignal(t, running, "the tool")
	must(t, setup.registry.Install(prompt("v2")))
	close(reloaded)
	waitFor(t, submission)
	var sections []map[string]string
	for _, entry := range chatEntries(t, root) {
		if system, ok := entry.Model[0].(ai.SystemMessage); ok && system.Sections != nil {
			sections = append(sections, sectionMap(system.Sections))
		}
	}
	if want := []map[string]string{{"mode": "<mode>\nv1\n</mode>"}, {"mode": "<mode>\nv2\n</mode>"}}; !reflect.DeepEqual(sections, want) {
		t.Fatalf("sections = %v", sections)
	}
	if got := trace.all(); !slices.Equal(got, []string{"v1", "v2"}) {
		t.Fatalf("requests = %v", got)
	}
}

func TestToolProgressRejectsInvocationBoundWaitsAndStopsWatchesWhenTheToolsInvocationEnds(t *testing.T) {
	setup := newChatSetup(t)
	// The child's definition is not registered, so it stays pending.
	never := oneStep(t, "test.never", func(context.Context, *stepTask, *TaskRuntime) error { return nil }, nil)
	waited := make(chan error, 1)
	var watch *durable.DocumentWatch
	installTool(t, setup.registry, echoTool("detach", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		child, err := api.CreateTask(ctx, never, map[string]any{}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		must(t, err)
		go func() {
			_, err := api.WaitForTask(context.WithoutCancel(ctx), child)
			waited <- err
		}()
		watch, err = api.WatchDoc(ctx, LiveDoc.Conversation(api.ConversationID()))
		must(t, err)
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("detach", nil, "c1"))), fauxText("done")}, nil)
	select {
	case err := <-waited:
		if err == nil || !strings.Contains(err.Error(), "invocation has ended") {
			t.Fatalf("wait = %v, want rejected", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait outlived the invocation")
	}
	awaitSignal(t, watch.Closed(), "the watch's end")
}

func TestToolProgressRejectsDetailsStillWaitingWhenTheCallIsAbortedDuringAfterTool(t *testing.T) {
	setup := newChatSetup(t)
	pending := make(chan error, 1)
	installTool(t, setup.registry, echoTool("slow", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		must(t, api.Output([]byte("first\n"), nil))
		// The output commit is in flight, so these details wait for the
		// next throttle window.
		go func() { pending <- api.Details(context.WithoutCancel(ctx), map[string]any{"step": 1}) }()
		time.Sleep(5 * time.Millisecond)
		return ToolExecutionResult{}, nil
	}))
	inAfterTool := make(chan struct{})
	var once sync.Once
	installHooks(t, setup.registry, "hooks", ToolTask, &ToolHooks{AfterTool: func(ctx context.Context, _ ToolHookCall, _ ToolExecutionResult, _ *TaskRuntime) (*ToolExecutionResult, error) {
		once.Do(func() { close(inAfterTool) })
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}})
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("slow", nil, "c1"))), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	submission := submit(t, root, inputDraft("go"))
	awaitSignal(t, inAfterTool, "afterTool")
	var live struct {
		Tools []ToolSlot `json:"tools"`
	}
	must(t, fromValue(liveOf(t, h, root), &live))
	_, err := h.AbortTask(bg, *live.Tools[0].TaskID)
	must(t, err)
	select {
	case err := <-pending:
		if err == nil {
			t.Fatal("details settled successfully after the abort")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("details never settled")
	}
	waitFor(t, submission)
}

func TestToolProgressAnswersAnAbortedToolWithOnlyItsDurableOutputDiscardingBufferedOutput(t *testing.T) {
	setup := newChatSetup(t)
	buffered := make(chan struct{})
	installTool(t, setup.registry, echoTool("slow", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		must(t, api.Output([]byte("durable\n"), nil))
		// The first output commits at once; this one waits for the next
		// throttle window.
		time.Sleep(20 * time.Millisecond)
		must(t, api.Output([]byte("buffered\n"), nil))
		close(buffered)
		<-ctx.Done()
		return ToolExecutionResult{}, context.Cause(ctx)
	}))
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("slow", nil, "c1"))), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	submission := submit(t, root, inputDraft("go"))
	awaitSignal(t, buffered, "the buffered output")
	var live struct {
		Tools []ToolSlot `json:"tools"`
	}
	must(t, fromValue(liveOf(t, h, root), &live))
	_, err := h.AbortTask(bg, *live.Tools[0].TaskID)
	must(t, err)
	waitFor(t, submission)
	if got := resultText(toolResults(chatEntries(t, root))[0]); got != "durable\n|<harness>\n[error] Tool slow was aborted\n</harness>" {
		t.Fatalf("result = %q", got)
	}
}

// Ported from pi packages/durable/test/harness-callers.test.ts: the two
// cases without nested calls; the three that probe executeTool are in
// callers_nested_test.go.

func callerTool(name string) *ToolRegistration {
	return &ToolRegistration{
		Tool: ai.Tool{Name: name, Description: name, Parameters: objectSchema()},
		Execute: func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
			return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: name + " ran"}}}, nil
		},
	}
}

func offeredNames(t *testing.T, conversation *Conversation) []string {
	t.Helper()
	agent, err := conversation.Agent(bg)
	must(t, err)
	return toolNames(agent.Tools)
}

func TestCallersAnswerAModelIssuedCallOfAToolItWasNotOfferedAsUnavailable(t *testing.T) {
	setup := newChatSetup(t)
	read, bash := callerTool("read"), callerTool("bash")
	installTool(t, setup.registry, read)
	installTool(t, setup.registry, bash)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("bash", nil, "c1"))), fauxText("done")})
	_, root := openChat(t, memory.New(), setup)
	must(t, root.Configure(bg, AgentChange{ModelTools: Set(Exactly(read))}))
	waitFor(t, submit(t, root, inputDraft("go")))
	result := toolResults(chatEntries(t, root))[0]
	if !strings.Contains(resultText(result), "Tool bash is not available") {
		t.Fatalf("result = %q", resultText(result))
	}
}

func TestCallersApplyAddToolsToModelToolsTooSoTheModelIsOfferedTheAddedTools(t *testing.T) {
	setup := newChatSetup(t)
	extra := callerTool("extra")
	grow := callerTool("grow")
	grow.Execute = func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{}, Control: &ToolControl{AddTools: []string{"extra"}}}, nil
	}
	installTool(t, setup.registry, extra)
	installTool(t, setup.registry, grow)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("grow", nil, "c1"))), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	must(t, root.Configure(bg, AgentChange{ModelTools: Set(Exactly(grow))}))
	waitFor(t, submit(t, root, inputDraft("go")))
	agent, err := h.Snapshot(bg, AgentDoc.Conversation(root.ID))
	must(t, err)
	if got := jsonOf(t, agent["modelTools"]); got != `["grow","extra"]` {
		t.Fatalf("stored modelTools = %s", got)
	}
	if got := offeredNames(t, root); !slices.Equal(got, []string{"grow", "extra"}) {
		t.Fatalf("offered = %v", got)
	}
}

// Ported from pi packages/durable/test/harness-host-inputs.test.ts; pi's
// models function is Options.ModelsFunc.

// switchingModels is a ModelsFunc whose return value a test swaps.
type switchingModels struct {
	mu      sync.Mutex
	current ai.Models
}

func (s *switchingModels) get() ai.Models {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *switchingModels) set(models ai.Models) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = models
}

// answering makes setup's faux answer every request with text.
func answering(setup *chatSetup, text string) {
	steps := make([]providers.FauxResponseStep, 10)
	for i := range steps {
		steps[i] = fauxText(text)
	}
	setup.faux.SetResponses(steps)
}

func TestHostInputsGenerationUsesTheCurrentReturnValueOfAModelsFunctionAtEachRequest(t *testing.T) {
	setup := newChatSetup(t)
	other := newChatSetup(t)
	answering(setup, "first")
	answering(other, "second")
	models := &switchingModels{current: setup.models}
	setup.modelsFunc = models.get
	_, root := openChat(t, memory.New(), setup)
	waitFor(t, submit(t, root, inputDraft("one")))
	if got := textOf(lastEntry(t, root)); got != "first" {
		t.Fatalf("first answer = %q", got)
	}
	models.set(other.models)
	waitFor(t, submit(t, root, inputDraft("two")))
	if got := textOf(lastEntry(t, root)); got != "second" {
		t.Fatalf("second answer = %q", got)
	}
}

// otherModels is the same model access under another identity.
type otherModels struct{ ai.Models }

func TestHostInputsToolsReadTheCurrentModelsAtEachAccessOfAPIModels(t *testing.T) {
	setup := newChatSetup(t)
	first := setup.models
	second := &otherModels{first}
	models := &switchingModels{current: first}
	setup.modelsFunc = models.get
	var seen []ai.Models
	installTool(t, setup.registry, echoTool("probe", func(_ context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		seen = append(seen, api.Models())
		models.set(second)
		seen = append(seen, api.Models())
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	_, root, _, status := runChat(t, setup, []providers.FauxResponseStep{fauxStep(toolCalls(call("probe", nil, "call-1"))), fauxText("done")}, nil)
	if status != durable.SubmissionDone {
		t.Fatalf("status = %s", status)
	}
	if got := textOf(lastEntry(t, root)); got != "done" {
		t.Fatalf("answer = %q", got)
	}
	if len(seen) != 2 || seen[0] != first || seen[1] != ai.Models(second) {
		t.Fatalf("models seen = %v, want first then second", seen)
	}
}

func lastEntry(t *testing.T, root *Conversation) durable.EntryRecord {
	t.Helper()
	entries := chatEntries(t, root)
	return entries[len(entries)-1]
}

func TestHostInputsStillAcceptAPlainModelsObject(t *testing.T) {
	setup := newChatSetup(t)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("plain")})
	_, root := openChat(t, memory.New(), setup)
	waitFor(t, submit(t, root, inputDraft("one")))
	entries := chatEntries(t, root)
	if got := textOf(entries[len(entries)-1]); got != "plain" {
		t.Fatalf("answer = %q", got)
	}
}

func TestHostInputsGenerationReadsTheCurrentReturnValueOfASettingsFunction(t *testing.T) {
	setup := newChatSetup(t)
	var mu sync.Mutex
	var timeouts []int
	reply := func(_ context.Context, _ ai.TranscriptContext, opts *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		mu.Lock()
		timeouts = append(timeouts, opts.TimeoutMs)
		mu.Unlock()
		return fauxAnswer("ok")
	}
	setup.faux.SetResponses([]providers.FauxResponseStep{reply, reply})
	setup.setSettings(func(s *HarnessSettings) { s.Stream = ConversationStreamOptions{TimeoutMs: ptr(1000)} })
	_, root := openChat(t, memory.New(), setup)
	waitFor(t, submit(t, root, inputDraft("one")))
	setup.setSettings(func(s *HarnessSettings) { s.Stream = ConversationStreamOptions{TimeoutMs: ptr(2000)} })
	waitFor(t, submit(t, root, inputDraft("two")))
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(timeouts, []int{1000, 2000}) {
		t.Fatalf("timeouts = %v", timeouts)
	}
}
