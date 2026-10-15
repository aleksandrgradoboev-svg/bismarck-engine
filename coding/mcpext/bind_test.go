package mcpext

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding/codemodetool"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding/toolsearch"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/mcp"
)

func TestDiscoveryToolNames(t *testing.T) {
	if codemodeToolName != codemodetool.ToolName || toolSearchToolName != toolsearch.ToolName {
		t.Fatal("discovery tool names drifted from their packages")
	}
}

// TestRelocateSchema: local references move below the pointer; remote ones,
// property names and data keep as they are; keys keep their order.
func TestRelocateSchema(t *testing.T) {
	in := `{"type":"object","properties":{"item":{"$ref":"#/$defs/Item"},"self":{"$ref":"#"},"remote":{"$ref":"https://x/y"},"$ref":{"type":"string"}},` +
		`"$defs":{"Item":{"type":"object","properties":{"name":{"type":"string"},"next":{"$ref":"#/$defs/Item"}}}},"const":{"$ref":"#/kept"},"enum":[{"$ref":"#"}],"anyOf":[{"$ref":"#/$defs/Item"}],"n":1.50}`
	want := `{"type":"object","properties":{"item":{"$ref":"#/properties/structuredContent/$defs/Item"},"self":{"$ref":"#/properties/structuredContent"},"remote":{"$ref":"https://x/y"},"$ref":{"type":"string"}},` +
		`"$defs":{"Item":{"type":"object","properties":{"name":{"type":"string"},"next":{"$ref":"#/properties/structuredContent/$defs/Item"}}}},"const":{"$ref":"#/kept"},"enum":[{"$ref":"#"}],"anyOf":[{"$ref":"#/properties/structuredContent/$defs/Item"}],"n":1.50}`
	var out bytes.Buffer
	dec := json.NewDecoder(strings.NewReader(in))
	dec.UseNumber()
	if err := relocateSchema(dec, &out, "#/properties/structuredContent", 0); err != nil {
		t.Fatal(err)
	}
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

// TestResultSchema: every MCP tool resolves in scripts to the CallToolResult,
// described in one line.
func TestResultSchema(t *testing.T) {
	tool := agent.AgentTool{Name: "mcp__docs__search", Description: "Search.", Parameters: ai.Object(),
		OutputSchema: CreateResultSchema(json.RawMessage(`{"type":"object","properties":{"hits":{"type":"number"}}}`))}
	loadout := coding.ToolLoadout{
		Declared: []agent.AgentTool{{Name: codemodetool.ToolName}, tool},
		Callable: []agent.AgentTool{tool},
		Exposure: func(string) coding.ToolExposure { return coding.ToolExposureDirect },
		Namespace: func(string) *coding.ToolNamespace {
			return nil
		},
		PromptGuidelines: func(string) []string { return nil },
	}
	got := codemodetool.PrepareLoadout(loadout, codemodetool.LoadoutOptions{}).Descriptions[tool.Name]
	if !strings.HasSuffix(got, "resolves to `{ content, structuredContent?: { hits? }, isError? }`.") {
		t.Fatalf("description:\n%s", got)
	}
	if s := CreateResultSchema(nil); s.Properties["structuredContent"] == nil || !slices.Equal(s.Required, []string{"content"}) {
		t.Fatalf("no output schema: %+v", s)
	}
}

// fauxModel answers with queued messages and records each request's declared
// tools and system prompt.
type fauxModel struct {
	mu       sync.Mutex
	queue    []*ai.AssistantMessage
	requests [][]string
	prompts  []string
}

func (f *fauxModel) respond(messages ...*ai.AssistantMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, messages...)
}

func (f *fauxModel) stream(_ context.Context, model *ai.Model, c ai.TranscriptContext, _ *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
	var names []string
	for _, tool := range ai.GetCurrentTools(c.Messages) {
		names = append(names, tool.Name)
	}
	f.mu.Lock()
	f.requests = append(f.requests, names)
	f.prompts = append(f.prompts, ai.GetCurrentSystemPrompt(c.Messages))
	msg := &ai.AssistantMessage{StopReason: ai.StopStop, Content: ai.ContentList{ai.TextContent{Text: "done"}}}
	if len(f.queue) > 0 {
		msg, f.queue = f.queue[0], f.queue[1:]
	}
	f.mu.Unlock()
	out := *msg
	out.Api, out.Provider, out.Model = model.Api, model.Provider, model.ID
	s := ai.NewAssistantMessageEventStream()
	s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: out.StopReason, Message: &out})
	s.End()
	return s
}

func (f *fauxModel) last() ([]string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1], f.prompts[len(f.prompts)-1]
}

func toolCall(name string, args map[string]any) *ai.AssistantMessage {
	return &ai.AssistantMessage{StopReason: ai.StopToolUse, Content: ai.ContentList{
		ai.ToolCall{ID: "c" + name + time.Now().Format("150405.000000000"), Name: name, Arguments: args},
	}}
}

type bound struct {
	sess     *coding.Session
	m        *Manager
	model    *fauxModel
	release  func()
	warnings *[]string
}

// bindWith binds a manager over fake servers to a session with codemode and
// tool_search registered, as pi's built-in extensions are. The servers'
// transports open only when release is called, so a test can run before they
// connect.
func bindWith(t *testing.T, entries []ServerEntry, servers map[string]fakeServerOptions, autoEnable *bool) *bound {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	m := NewManager(ManagerOptions{
		Config: LoadedConfig{Servers: entries, AutoEnableCodemode: autoEnable},
		Cwd:    t.TempDir(),
		NewTransport: func(e ServerEntry, _ string, _ mcp.AuthProvider) (mcp.Transport, error) {
			<-gate
			_, tr := newFakeServer(servers[e.Name])
			return tr, nil
		},
	})
	t.Cleanup(m.Close)
	model := &fauxModel{}
	sess := coding.NewSession(coding.SessionOptions{
		Model: &ai.Model{ID: "m", Api: "faux", Provider: "faux", ContextWindow: 100000, MaxTokens: 8192},
		Cwd:   t.TempDir(), NoTools: coding.NoToolsBuiltin, NoContextFiles: true, NoSkills: true,
		StreamFn: model.stream,
	})
	sess.RegisterTools(codemodetool.Definition(codemodetool.Options{Session: sess, Store: &codemodetool.Store{}}), toolsearch.Definition(sess))
	var warnings []string
	var mu sync.Mutex
	Bind(sess, m, BindOptions{Warn: func(message string) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, message)
	}})
	m.Start()
	return &bound{sess: sess, m: m, model: model, release: release, warnings: &warnings}
}

func docsEntry(exposure Exposure) []ServerEntry {
	return []ServerEntry{{Name: "docs", Source: "test", Config: ServerConfig{Command: "x", Description: "Project documentation", Exposure: exposure}}}
}

var docsServer = map[string]fakeServerOptions{"docs": {tools: []mcp.Tool{
	{Name: "search", Description: "Search the documentation.", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
}, instructions: "Use search first."}}

func lastResult(t *testing.T, sess *coding.Session, name string) *ai.ToolResultMessage {
	t.Helper()
	var result *ai.ToolResultMessage
	for _, m := range sess.History() {
		switch r := m.(type) {
		case ai.ToolResultMessage:
			if r.ToolName == name {
				result = &r
			}
		case *ai.ToolResultMessage:
			if r.ToolName == name {
				result = r
			}
		}
	}
	if result == nil {
		t.Fatalf("no %s result", name)
	}
	return result
}

func resultTexts(m *ai.ToolResultMessage) string {
	var parts []string
	for _, c := range m.Content {
		if t, ok := c.(ai.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// TestBindCodemodeServer: with the default codemode exposure, codemode is
// activated from the config before the server connects; a script that names
// the server waits for it, then calls its tool and receives the
// CallToolResult; the prompt lists the server in mcp_servers.
func TestBindCodemodeServer(t *testing.T) {
	b := bindWith(t, docsEntry(""), docsServer, nil)
	if active := b.sess.ActiveToolNames(); !slices.Equal(active, []string{codemodetool.ToolName}) {
		t.Fatalf("active %v", active)
	}
	// The server connects only after the script has started waiting for it.
	go func() {
		time.Sleep(150 * time.Millisecond)
		b.release()
	}()
	b.model.respond(toolCall(codemodetool.ToolName, map[string]any{"code": `return await tools.mcp__docs__search({ q: "x" });`}))
	if _, err := b.sess.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	result := lastResult(t, b.sess, codemodetool.ToolName)
	if text := resultTexts(result); result.IsError || !strings.Contains(text, `{"content":[{"type":"text","text":"ok"}]}`) {
		t.Fatalf("script result (error %v):\n%s", result.IsError, text)
	}
	_, prompt := b.model.last()
	if !strings.Contains(prompt, "<mcp_servers>") || !strings.Contains(prompt, "- mcp__docs (codemode): Project documentation") {
		t.Fatalf("prompt:\n%s", prompt)
	}
	// The tool carries its server's namespace.
	for _, d := range b.sess.AllTools() {
		if d.Tool.Name == "mcp__docs__search" && (d.Namespace == nil || d.Namespace.Name != "mcp__docs" || d.Namespace.Instructions != "Use search first.") {
			t.Fatalf("namespace %+v", d.Namespace)
		}
	}
}

// TestBindDirectServer: a direct server's tools are declared in the first
// request: the prompt waits for the server to connect.
func TestBindDirectServer(t *testing.T) {
	b := bindWith(t, docsEntry(ExposureDirect), docsServer, nil)
	go func() {
		time.Sleep(150 * time.Millisecond)
		b.release()
	}()
	if _, err := b.sess.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if tools, prompt := b.model.last(); !slices.Contains(tools, "mcp__docs__search") || strings.Contains(prompt, "<mcp_servers>") {
		t.Fatalf("tools %v, prompt:\n%s", tools, prompt)
	}
	if slices.Contains(b.sess.ActiveToolNames(), codemodetool.ToolName) {
		t.Fatal("codemode activated for a direct server")
	}
}

// TestBindDeferredServer: deferred tools activate tool_search, which loads
// them for the next request.
func TestBindDeferredServer(t *testing.T) {
	b := bindWith(t, docsEntry(ExposureDeferred), docsServer, nil)
	if active := b.sess.ActiveToolNames(); !slices.Equal(active, []string{toolsearch.ToolName}) {
		t.Fatalf("active %v", active)
	}
	b.release()
	b.model.respond(toolCall(toolsearch.ToolName, map[string]any{"query": "search documentation"}))
	if _, err := b.sess.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if tools, _ := b.model.last(); !slices.Contains(tools, "mcp__docs__search") {
		t.Fatalf("tools after tool_search %v (result %s)", tools, resultTexts(lastResult(t, b.sess, toolsearch.ToolName)))
	}
}

// TestBindWarnsWhenUnreachable: with autoEnableCodemode false and nothing
// else to reach codemode tools, Bind warns once, saying why.
func TestBindWarnsWhenUnreachable(t *testing.T) {
	off := false
	b := bindWith(t, docsEntry(""), docsServer, &off)
	if slices.Contains(b.sess.ActiveToolNames(), codemodetool.ToolName) {
		t.Fatal("codemode activated with autoEnableCodemode false")
	}
	want := "MCP tools are only reachable from the codemode or tool_search tool, but neither is active (autoEnableCodemode is false); they cannot be called."
	if len(*b.warnings) != 1 || (*b.warnings)[0] != want {
		t.Fatalf("warnings %q", *b.warnings)
	}
}

// TestBindDropsToolsThatLeaveDirect: the resource tools take the widest
// exposure of the servers with resources; when the direct one is disabled
// they become codemode tools and leave the active set, as in pi.
func TestBindDropsToolsThatLeaveDirect(t *testing.T) {
	entries := []ServerEntry{
		{Name: "a", Source: "test", Config: ServerConfig{Command: "x", Exposure: ExposureDirect}},
		{Name: "b", Source: "test", Config: ServerConfig{Command: "x", Exposure: ExposureCodemode}},
	}
	b := bindWith(t, entries, map[string]fakeServerOptions{"a": {resources: true}, "b": {resources: true}}, nil)
	b.release()
	if err := b.m.WaitForServers(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return slices.Contains(b.sess.ActiveToolNames(), ListResourcesTool) })
	if err := b.m.SetEnabled("a", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !slices.Contains(b.sess.ActiveToolNames(), ListResourcesTool) })
	for _, d := range b.sess.AllTools() {
		if d.Tool.Name == ListResourcesTool && d.Exposure != ExposureCodemode {
			t.Fatalf("resource tools %s", d.Exposure)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
