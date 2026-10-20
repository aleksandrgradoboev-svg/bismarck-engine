package harness

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-tools.test.ts. The two
// "coding tools" cases need the tools on env (stage 5).

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
