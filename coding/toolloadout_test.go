package coding

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// orchestratorTools is pi's orchestratorExtension: a model-only tool that calls
// other tools, built only on the registry — its exposure, its loadout hook and
// ExecuteTool.
func orchestratorTools(sess **Session) []ToolDefinition {
	echo := testTool("echo", func(_ context.Context, _ string, params map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		text, _ := params["text"].(string)
		return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "echo: " + text}}}, nil
	})
	echo.Description = "Echo text."
	echo.PromptGuidelines = []string{"Echo carefully."}
	helper := testTool("helper", func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "helped"}}}, nil
	})
	runTools := testTool("run_tools", func(ctx context.Context, id string, _ map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		s := *sess
		var texts []string
		for _, call := range []struct {
			name string
			args map[string]any
		}{{"helper", map[string]any{}}, {"echo", map[string]any{"text": "hi"}}, {"run_tools", map[string]any{}}} {
			texts = append(texts, textOf(s.ExecuteTool(ctx, id, call.name, call.args, nil).Result.Content))
		}
		return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: strings.Join(texts, " | ")}}}, nil
	})
	runTools.Description = "Runs tools."
	return []ToolDefinition{
		{Tool: echo},
		{Tool: helper, Exposure: ToolExposureCodemode},
		{Tool: runTools, Exposure: ToolExposureModelOnly, PrepareLoadout: func(loadout ToolLoadout) (ToolLoadoutChanges, error) {
			return ToolLoadoutChanges{
				Descriptions: map[string]string{
					"run_tools": "Runs tools: " + strings.Join(agentToolNameList(loadout.Callable), ", "),
					"echo":      "Echo text (also callable from run_tools).",
				},
				HiddenDeclarations: []string{"echo"},
			}, nil
		}},
	}
}

// recordingStream answers with the given turns in order and records each
// request's declared tools.
func recordingStream(turns ...*ai.AssistantMessage) (agent.StreamFn, func() [][]string) {
	var mu sync.Mutex
	var requests [][]string
	stream := func(_ context.Context, model *ai.Model, c ai.TranscriptContext, _ *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		var names []string
		for _, tool := range ai.GetCurrentTools(c.Messages) {
			names = append(names, tool.Name)
		}
		mu.Lock()
		msg := turns[min(len(requests), len(turns)-1)]
		requests = append(requests, names)
		mu.Unlock()
		out := *msg
		out.Api, out.Provider, out.Model = model.Api, model.Provider, model.ID
		s := ai.NewAssistantMessageEventStream()
		s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: out.StopReason, Message: &out})
		s.End()
		return s
	}
	return stream, func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
}

func callTurn(calls ...ai.ToolCall) *ai.AssistantMessage {
	content := ai.ContentList{}
	for _, c := range calls {
		content = append(content, c)
	}
	return &ai.AssistantMessage{StopReason: ai.StopToolUse, Content: content}
}

func doneTurn() *ai.AssistantMessage {
	return &ai.AssistantMessage{StopReason: ai.StopStop, Content: ai.ContentList{ai.TextContent{Text: "done"}}}
}

// TestToolOrchestration is pi's "supports tools that call other tools under
// any name through the extension API": the hook rewrites descriptions and
// hides echo's declaration from requests, while echo stays active, callable
// and declared in the transcript.
func TestToolOrchestration(t *testing.T) {
	var sess *Session
	stream, requests := recordingStream(callTurn(ai.ToolCall{ID: "t1", Name: "run_tools", Arguments: map[string]any{}}), doneTurn())
	sess = registrySession(t, SessionOptions{NoTools: NoToolsBuiltin, StreamFn: stream})
	sess.RegisterTools(orchestratorTools(&sess)...)

	if got := sess.ActiveToolNames(); !slices.Equal(got, []string{"echo", "run_tools"}) {
		t.Fatalf("active %v", got)
	}
	if got := agentToolNameList(sess.CallableTools()); !slices.Equal(got, []string{"echo", "helper"}) {
		t.Fatalf("callable %v", got)
	}
	descriptions := map[string]string{}
	for _, tool := range sess.Agent.State().Tools {
		descriptions[tool.Name] = tool.Description
	}
	if descriptions["run_tools"] != "Runs tools: echo, helper" || descriptions["echo"] != "Echo text (also callable from run_tools)." {
		t.Fatalf("descriptions %v", descriptions)
	}
	// The rules leave out a hidden declaration's guidelines (upstream 028c0ec5).
	if prompt, _ := sess.SystemPrompt(); strings.Contains(prompt, "Echo carefully.") {
		t.Fatalf("hidden tool's guideline in prompt:\n%s", prompt)
	}

	if _, err := sess.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	// echo stays active, but its declaration is left out of requests.
	if got := requests(); len(got) == 0 || !slices.Equal(got[0], []string{"run_tools"}) {
		t.Fatalf("requests %v", got)
	}
	var transcriptTools []string
	for _, tool := range ai.GetCurrentTools(sess.History()) {
		transcriptTools = append(transcriptTools, tool.Name)
	}
	if !slices.Equal(transcriptTools, []string{"echo", "run_tools"}) {
		t.Fatalf("transcript declares %v", transcriptTools)
	}
	var result *ai.ToolResultMessage
	for _, m := range sess.History() {
		if r, ok := messageAsToolResult(m); ok {
			result = r
		}
	}
	if result == nil {
		t.Fatal("no tool result")
	}
	if got := textOf(result.Content); got != "helped | echo: hi | Tool run_tools not found" {
		t.Fatalf("result %q", got)
	}
	var calls []string
	for _, call := range result.NestedCalls.Calls {
		calls = append(calls, call.ID+" "+call.Name+" "+string(call.Status))
	}
	if !slices.Equal(calls, []string{"t1/1 helper ok", "t1/2 echo ok", "t1/3 run_tools error"}) {
		t.Fatalf("nested calls %v", calls)
	}
}

// TestPrepareLoadoutFailure: a hook that errors or panics contributes nothing
// and is reported; the other hooks still apply.
func TestPrepareLoadoutFailure(t *testing.T) {
	var reported []string
	sess := registrySession(t, SessionOptions{NoTools: NoToolsBuiltin, OnPrepareLoadoutError: func(tool string, err error) {
		reported = append(reported, tool+": "+err.Error())
	}})
	hook := func(changes ToolLoadoutChanges, err error, panicValue any) func(ToolLoadout) (ToolLoadoutChanges, error) {
		return func(ToolLoadout) (ToolLoadoutChanges, error) {
			if panicValue != nil {
				panic(panicValue)
			}
			return changes, err
		}
	}
	sess.RegisterTools(
		ToolDefinition{Tool: echoTool("fails"), PrepareLoadout: hook(ToolLoadoutChanges{HiddenDeclarations: []string{"works"}}, errors.New("boom"), nil)},
		ToolDefinition{Tool: echoTool("panics"), PrepareLoadout: hook(ToolLoadoutChanges{}, nil, "oops")},
		ToolDefinition{Tool: echoTool("works"), PrepareLoadout: hook(ToolLoadoutChanges{Descriptions: map[string]string{"works": "Rewritten."}}, nil, nil)},
	)
	if len(reported) == 0 || reported[len(reported)-2] != "fails: boom" || !strings.Contains(reported[len(reported)-1], "panics: prepareLoadout panicked: oops") {
		t.Fatalf("reported %v", reported)
	}
	if hidden := sess.promptOptions().HiddenTools; len(hidden) != 0 {
		t.Fatalf("failed hook hid %v", hidden)
	}
	for _, tool := range sess.Agent.State().Tools {
		if tool.Name == "works" && tool.Description != "Rewritten." {
			t.Fatalf("works: %q", tool.Description)
		}
	}
}

func declaringTranscript(names ...string) []agent.AgentMessage {
	tools := make([]ai.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, ai.Tool{Name: name, Description: name, Parameters: objectSchema()})
	}
	return []agent.AgentMessage{ai.SystemMessage{Content: ai.ContentList{ai.TextContent{Text: "prompt"}}, ToolsAdded: tools}}
}

// TestRestoreToolsFromTranscript: the transcript's declarations become the
// loadout; a declared tool that has not registered yet is pending and becomes
// active when it registers.
func TestRestoreToolsFromTranscript(t *testing.T) {
	sess := registrySession(t, SessionOptions{})
	sess.LoadHistory(declaringTranscript("read", "mcp__docs__search"))
	sess.RestoreToolsFromTranscript()
	if got := sess.ActiveToolNames(); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("active %v", got)
	}
	if got := sess.PendingToolNames(); !slices.Equal(got, []string{"mcp__docs__search"}) {
		t.Fatalf("pending %v", got)
	}
	off := false
	sess.RegisterTools(ToolDefinition{Tool: echoTool("mcp__docs__search"), DefaultActive: &off})
	if got := sess.ActiveToolNames(); !slices.Equal(got, []string{"read", "mcp__docs__search"}) {
		t.Fatalf("active after register %v", got)
	}
	if got := sess.PendingToolNames(); len(got) != 0 {
		t.Fatalf("pending after register %v", got)
	}

	// A transcript without a system message leaves the loadout alone.
	empty := registrySession(t, SessionOptions{})
	empty.RestoreToolsFromTranscript()
	if got := empty.ActiveToolNames(); !slices.Equal(got, []string{"read", "bash", "edit", "write"}) {
		t.Fatalf("empty transcript %v", got)
	}
}

// TestPendingToolsDropped: pending tools survive a loadout that only adds
// tools, and are dropped by one that deactivates a tool or by the next run.
func TestPendingToolsDropped(t *testing.T) {
	restored := func(t *testing.T, opts SessionOptions) *Session {
		sess := registrySession(t, opts)
		sess.LoadHistory(declaringTranscript("read", "late"))
		sess.RestoreToolsFromTranscript()
		return sess
	}
	sess := restored(t, SessionOptions{})
	sess.SetActiveTools([]string{"read", "bash"})
	if got := sess.PendingToolNames(); !slices.Equal(got, []string{"late"}) {
		t.Fatalf("adding dropped pending: %v", got)
	}
	sess.SetActiveTools([]string{"bash"})
	if got := sess.PendingToolNames(); len(got) != 0 {
		t.Fatalf("deactivating kept pending: %v", got)
	}

	stream, _ := recordingStream(doneTurn())
	sess = restored(t, SessionOptions{StreamFn: stream})
	if _, err := sess.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := sess.PendingToolNames(); len(got) != 0 {
		t.Fatalf("run kept pending: %v", got)
	}
	sess.RegisterTools(ToolDefinition{Tool: echoTool("late"), DefaultActive: new(bool)})
	if slices.Contains(sess.ActiveToolNames(), "late") {
		t.Fatal("dropped pending tool activated")
	}
}
