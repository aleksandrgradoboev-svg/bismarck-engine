package coding

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

func echoTool(name string) agent.AgentTool {
	return testTool(name, func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: name}}}, nil
	})
}

func def(name string, exposure ToolExposure) ToolDefinition {
	return ToolDefinition{Tool: echoTool(name), Exposure: exposure}
}

func registrySession(t *testing.T, opts SessionOptions) *Session {
	t.Helper()
	opts.Model, opts.Cwd, opts.NoContextFiles, opts.NoSkills = chatModel(), t.TempDir(), true, true
	return NewSession(opts)
}

func agentToolNameList(tools []agent.AgentTool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func agentToolNames(s *Session) []string { return agentToolNameList(s.Agent.State().Tools) }

// TestRegistryExposures: registering activates the declarable tools that are
// active by default; the callable tools are the active direct ones plus every
// codemode and deferred tool (pi getActiveToolNames / getCallableToolNames).
func TestRegistryExposures(t *testing.T) {
	off := false
	sess := registrySession(t, SessionOptions{})
	sess.RegisterTools(
		def("helper", ToolExposureDirect),
		ToolDefinition{Tool: echoTool("lazy"), DefaultActive: &off},
		def("script", ToolExposureCodemode),
		def("later", ToolExposureDeferred),
		def("secret", ToolExposureHidden),
		def("chat", ToolExposureModelOnly),
	)
	if got, want := sess.ActiveToolNames(), []string{"read", "bash", "edit", "write", "helper", "chat"}; !slices.Equal(got, want) {
		t.Fatalf("active %v, want %v", got, want)
	}
	if got := agentToolNames(sess); !slices.Equal(got, sess.ActiveToolNames()) {
		t.Fatalf("agent tools %v", got)
	}
	if got, want := agentToolNameList(sess.CallableTools()), []string{"read", "bash", "edit", "write", "helper", "script", "later"}; !slices.Equal(got, want) {
		t.Fatalf("callable %v, want %v", got, want)
	}
	var exposures []string
	for _, d := range sess.AllTools() {
		exposures = append(exposures, d.Tool.Name+"="+string(d.Exposure))
	}
	if !slices.Contains(exposures, "lazy=direct") || !slices.Contains(exposures, "secret=hidden") || !slices.Contains(exposures, "grep=direct") {
		t.Fatalf("all tools %v", exposures)
	}
	if opts := sess.promptOptions(); !slices.Equal(opts.SelectedTools, sess.ActiveToolNames()) {
		t.Fatalf("prompt tools %v", opts.SelectedTools)
	}

	// Naming a tool activates it, unless it is hidden or unknown.
	sess.SetActiveTools([]string{"lazy", "secret", "nope", "read", "lazy"})
	if got, want := sess.ActiveToolNames(), []string{"lazy", "read"}; !slices.Equal(got, want) {
		t.Fatalf("active %v, want %v", got, want)
	}
	if got, want := agentToolNameList(sess.CallableTools()), []string{"read", "lazy", "script", "later"}; !slices.Equal(got, want) {
		t.Fatalf("callable %v, want %v", got, want)
	}
}

// TestRegistryInactiveUntilNamed is pi's "registers codemode and tool_search
// inactive until they are named": a tool registered with defaultActive false
// stays inactive by default and is activated by the --tools allowlist or the
// initial tool names.
func TestRegistryInactiveUntilNamed(t *testing.T) {
	off := false
	extension := []ToolDefinition{
		{Tool: echoTool("codemode"), DefaultActive: &off},
		{Tool: echoTool("tool_search"), DefaultActive: &off},
	}
	plain := registrySession(t, SessionOptions{})
	plain.RegisterTools(extension...)
	if got, want := plain.ActiveToolNames(), []string{"read", "bash", "edit", "write"}; !slices.Equal(got, want) {
		t.Fatalf("plain %v", got)
	}
	var all []string
	for _, d := range plain.AllTools() {
		all = append(all, d.Tool.Name)
	}
	if !slices.Contains(all, "codemode") || !slices.Contains(all, "tool_search") {
		t.Fatalf("all tools %v", all)
	}

	allowed := registrySession(t, SessionOptions{ToolNames: []string{"read", "codemode"}})
	allowed.RegisterTools(extension...)
	if got, want := allowed.ActiveToolNames(), []string{"read", "codemode"}; !slices.Equal(got, want) {
		t.Fatalf("allowed %v", got)
	}

	initial := registrySession(t, SessionOptions{NoTools: NoToolsBuiltin})
	initial.RegisterTools(extension...)
	initial.SetActiveTools([]string{"tool_search"})
	if got, want := initial.ActiveToolNames(), []string{"tool_search"}; !slices.Equal(got, want) {
		t.Fatalf("initial %v", got)
	}
}

// TestRegistryExposureChange: a tool whose exposure changes to direct is
// activated like a new tool; one that becomes hidden leaves the loadout; an
// unregistered tool leaves both.
func TestRegistryExposureChange(t *testing.T) {
	sess := registrySession(t, SessionOptions{NoTools: NoToolsBuiltin})
	sess.RegisterTools(def("x", ToolExposureHidden))
	if got := sess.ActiveToolNames(); len(got) != 0 {
		t.Fatalf("hidden active %v", got)
	}
	sess.RegisterTools(def("x", ToolExposureDirect))
	if got := sess.ActiveToolNames(); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("direct active %v", got)
	}
	// Re-registering an already activated tool does not reactivate it once
	// deactivated.
	sess.SetActiveTools(nil)
	sess.RegisterTools(def("x", ToolExposureDirect))
	if got := sess.ActiveToolNames(); len(got) != 0 {
		t.Fatalf("reactivated %v", got)
	}
	sess.SetActiveTools([]string{"x"})
	sess.RegisterTools(def("x", ToolExposureHidden))
	if got := sess.ActiveToolNames(); len(got) != 0 {
		t.Fatalf("hidden still active %v", got)
	}
	sess.RegisterTools(def("x", ToolExposureDirect), def("y", ToolExposureDirect))
	sess.UnregisterTools("x")
	if got := sess.ActiveToolNames(); !slices.Equal(got, []string{"y"}) {
		t.Fatalf("after unregister %v", got)
	}
	for _, d := range sess.AllTools() {
		if d.Tool.Name == "x" {
			t.Fatal("x still registered")
		}
	}
}

// TestRegistryPromptFollowsLoadout: the tools section lists the active tools'
// snippets, a registered snippet included, and the rules their guidelines.
func TestRegistryPromptFollowsLoadout(t *testing.T) {
	sess := registrySession(t, SessionOptions{NoTools: NoToolsBuiltin})
	tool := echoTool("lookup")
	tool.PromptGuidelines = []string{"  Use lookup first.  ", "Use lookup first."}
	sess.RegisterTools(ToolDefinition{Tool: tool, PromptSnippet: "Look\r\n  things   up"})
	prompt, err := sess.SystemPrompt()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "- lookup: Look things up") || strings.Count(prompt, "Use lookup first.") != 1 {
		t.Fatalf("prompt:\n%s", prompt)
	}
	sess.SetActiveTools(nil)
	if prompt, _ = sess.SystemPrompt(); strings.Contains(prompt, "lookup") {
		t.Fatalf("inactive tool in prompt:\n%s", prompt)
	}
}

// TestReplacedBuiltinPrompt: only a built-in's definition carries a snippet
// and guidelines, as in pi. A Tools override (createToolDefinitionFromAgentTool)
// keeps neither; a custom tool keeps its guidelines but, without a snippet, is
// not listed, also when it replaces a built-in of the same name.
func TestReplacedBuiltinPrompt(t *testing.T) {
	read := echoTool("read")
	read.PromptGuidelines = []string{"Override guideline."}
	prompt := func(opts SessionOptions) string {
		t.Helper()
		p, err := registrySession(t, opts).SystemPrompt()
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	builtin := "- read: " + ToolSnippets["read"]
	if p := prompt(SessionOptions{}); !strings.Contains(p, builtin) {
		t.Fatalf("built-in read not listed:\n%s", p)
	}
	if p := prompt(SessionOptions{Tools: []agent.AgentTool{read}}); strings.Contains(p, "- read:") || strings.Contains(p, "Override guideline.") {
		t.Fatalf("override listed or kept its guideline:\n%s", p)
	}
	if p := prompt(SessionOptions{CustomTools: []agent.AgentTool{read}}); strings.Contains(p, "- read:") || !strings.Contains(p, "Override guideline.") {
		t.Fatalf("custom read listed or lost its guideline:\n%s", p)
	}
}

// TestSetPromptSection: a section is added, replaced in its slot, and removed
// by empty content.
func TestSetPromptSection(t *testing.T) {
	sess := registrySession(t, SessionOptions{})
	sess.SetPromptSection("mcp_servers", "one")
	sess.SetPromptSection("other", "x")
	sess.SetPromptSection("mcp_servers", "two")
	var names []string
	for _, e := range sess.promptOptions().Sections.Entries() {
		names = append(names, e.Name+"="+*e.Value)
	}
	if !slices.Equal(names, []string{"mcp_servers=two", "other=x"}) {
		t.Fatalf("sections %v", names)
	}
	prompt, _ := sess.SystemPrompt()
	if !strings.Contains(prompt, "<mcp_servers>\ntwo\n</mcp_servers>") {
		t.Fatalf("prompt:\n%s", prompt)
	}
	sess.SetPromptSection("mcp_servers", "")
	if _, ok := sess.promptOptions().Sections.Get("mcp_servers"); ok {
		t.Fatal("section not removed")
	}
}

// TestRegistryChangeMidRun: a tool that changes the loadout and a prompt
// section while it runs reaches the next request of the same run, and a
// deferred tool is callable through ExecuteTool without being declared.
func TestRegistryChangeMidRun(t *testing.T) {
	var sess *Session
	var nested string
	loader := testTool("loader", func(ctx context.Context, id string, _ map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		out := sess.ExecuteTool(ctx, id, "later", map[string]any{}, nil)
		nested = textOf(out.Result.Content)
		if out.IsError {
			nested = "error: " + nested
		}
		sess.SetActiveTools([]string{"loader", "chat"})
		sess.SetPromptSection("mcp_servers", "servers")
		return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "loaded"}}}, nil
	})
	var mu sync.Mutex
	var requests [][]string
	var prompts []string
	inner := toolCallStream(ai.ToolCall{ID: "t1", Name: "loader", Arguments: map[string]any{}})
	stream := func(ctx context.Context, model *ai.Model, c ai.TranscriptContext, o *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		var names []string
		for _, tool := range ai.GetCurrentTools(c.Messages) {
			names = append(names, tool.Name)
		}
		mu.Lock()
		requests = append(requests, names)
		prompts = append(prompts, ai.GetCurrentSystemPrompt(c.Messages))
		mu.Unlock()
		return inner(ctx, model, c, o)
	}
	sess = registrySession(t, SessionOptions{Tools: []agent.AgentTool{loader}, StreamFn: stream})
	sess.RegisterTools(def("later", ToolExposureDeferred), def("chat", ToolExposureModelOnly))
	sess.SetActiveTools([]string{"loader"})
	if _, err := sess.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if nested != "later" {
		t.Fatalf("nested call: %q", nested)
	}
	if len(requests) != 2 || !slices.Equal(requests[0], []string{"loader"}) || !slices.Equal(requests[1], []string{"loader", "chat"}) {
		t.Fatalf("requests %v", requests)
	}
	if strings.Contains(prompts[0], "<mcp_servers>") || !strings.Contains(prompts[1], "<mcp_servers>\nservers\n</mcp_servers>") {
		t.Fatalf("prompts %q", prompts)
	}
}

// TestExecuteToolSkipsModelOnly: a model-only tool is declared but not
// callable from other tools.
func TestExecuteToolSkipsModelOnly(t *testing.T) {
	sess := registrySession(t, SessionOptions{NoTools: NoToolsBuiltin})
	sess.RegisterTools(def("chat", ToolExposureModelOnly))
	out := sess.ExecuteTool(context.Background(), "p", "chat", map[string]any{}, nil)
	if !out.IsError {
		t.Fatalf("model-only tool ran: %+v", out)
	}
}
