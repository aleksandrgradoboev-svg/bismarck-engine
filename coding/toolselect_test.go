package coding

import (
	"slices"
	"strings"
	"testing"
)

func registeredNames(s *Session) []string {
	var names []string
	for _, d := range s.AllTools() {
		names = append(names, d.Tool.Name)
	}
	return names
}

func sorted(names []string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return out
}

// TestToolListError is pi's args tests "rejects tool names mixed with
// modifiers" and "rejects patterns in tool modifiers", and
// default-tools-setting's "rejects invalid tool modifier options".
func TestToolListError(t *testing.T) {
	for _, c := range []struct {
		entries []string
		want    string
	}{
		{[]string{"read", "mcp__x__*"}, ""},
		{[]string{"+codemode", "-write"}, ""},
		{nil, ""},
		{[]string{"read", "+grep"}, "tool names cannot be mixed with +name or -name entries"},
		{[]string{"-gr*"}, "+name and -name entries take exact tool names, not patterns: -gr*"},
		{[]string{"+mcp__radius__*"}, "+name and -name entries take exact tool names, not patterns: +mcp__radius__*"},
	} {
		if got := ToolListError(c.entries); got != c.want {
			t.Errorf("ToolListError(%q) = %q, want %q", c.entries, got, c.want)
		}
	}
	if got := ApplyToolModifiers([]string{"read", "bash"}, []string{"+grep", "-read", "+bash", "-nope", "+"}); !slices.Equal(got, []string{"bash", "grep"}) {
		t.Errorf("ApplyToolModifiers = %v", got)
	}
}

// TestToolModifiers is pi's "applies +name and -name tool options to the
// default selection": a list of modifiers changes the default selection, an
// inactive-by-default tool named with +name is activated, and under NoTools
// "all" the result is also the allowlist.
func TestToolModifiers(t *testing.T) {
	off := false
	extension := []ToolDefinition{
		{Tool: echoTool("inactive_tool"), DefaultActive: &off},
		{Tool: echoTool("active_tool")},
	}
	sess := registrySession(t, SessionOptions{ToolNames: []string{"+grep", "+inactive_tool", "-write"}})
	sess.RegisterTools(extension...)
	if got, want := sorted(sess.ActiveToolNames()), []string{"active_tool", "bash", "edit", "grep", "inactive_tool", "read"}; !slices.Equal(got, want) {
		t.Fatalf("active %v, want %v", got, want)
	}

	toolLess := registrySession(t, SessionOptions{NoTools: NoToolsAll, ToolNames: []string{"+inactive_tool"}})
	toolLess.RegisterTools(extension...)
	if got := toolLess.ActiveToolNames(); !slices.Equal(got, []string{"inactive_tool"}) {
		t.Fatalf("NoTools all: active %v", got)
	}
	if got := registeredNames(toolLess); !slices.Equal(got, []string{"inactive_tool"}) {
		t.Fatalf("NoTools all: registered %v", got)
	}

	// A +name the host never registers stays pending until the first run.
	pending := registrySession(t, SessionOptions{ToolNames: []string{"+late"}})
	if got := pending.PendingToolNames(); !slices.Equal(got, []string{"late"}) {
		t.Fatalf("pending %v", got)
	}
}

// TestExcludeTools is pi's regression 5109: excluded tools leave the registry
// and the prompt, override the allowlist, and both lists take patterns.
func TestExcludeTools(t *testing.T) {
	dynamic := ToolDefinition{Tool: echoTool("dynamic_tool"), PromptSnippet: "Run dynamic test behavior"}
	ask := ToolDefinition{Tool: echoTool("ask_question")}

	sess := registrySession(t, SessionOptions{ExcludeTools: []string{"read", "ask_question"}})
	sess.RegisterTools(dynamic, ask)
	all := registeredNames(sess)
	if slices.Contains(all, "read") || slices.Contains(all, "ask_question") || !slices.Contains(all, "bash") || !slices.Contains(all, "dynamic_tool") {
		t.Fatalf("registered %v", all)
	}
	if got := sorted(sess.ActiveToolNames()); !slices.Equal(got, []string{"bash", "dynamic_tool", "edit", "write"}) {
		t.Fatalf("active %v", got)
	}
	prompt, _ := sess.SystemPrompt()
	if strings.Contains(prompt, "- read:") || strings.Contains(prompt, "ask_question") || !strings.Contains(prompt, "- dynamic_tool: Run dynamic test behavior") {
		t.Fatalf("prompt:\n%s", prompt)
	}

	// Excluded tools override the allowlist.
	sess = registrySession(t, SessionOptions{ToolNames: []string{"read", "bash", "ask_question"}, ExcludeTools: []string{"read", "ask_question"}})
	sess.RegisterTools(dynamic, ask)
	if got := registeredNames(sess); !slices.Equal(got, []string{"bash"}) {
		t.Fatalf("registered %v", got)
	}
	if got := sess.ActiveToolNames(); !slices.Equal(got, []string{"bash"}) {
		t.Fatalf("active %v", got)
	}

	// Patterns that match a tool activate it like its name.
	sess = registrySession(t, SessionOptions{ToolNames: []string{"*_tool", "ask_*", "re*"}, ExcludeTools: []string{"ask*"}})
	sess.RegisterTools(dynamic, ask)
	if got := sorted(registeredNames(sess)); !slices.Equal(got, []string{"dynamic_tool", "read"}) {
		t.Fatalf("registered %v", got)
	}
	if got := sorted(sess.ActiveToolNames()); !slices.Equal(got, []string{"dynamic_tool", "read"}) {
		t.Fatalf("active %v", got)
	}
	if got := sess.PendingToolNames(); len(got) != 0 {
		t.Fatalf("patterns pending %v", got)
	}
}

func mcpTools(exposure ToolExposure, names ...string) []ToolDefinition {
	var defs []ToolDefinition
	for _, name := range names {
		defs = append(defs, def(name, exposure))
	}
	return defs
}

// TestAllowlistKeepsMCPTools ports pi's MCP allowlist cases (upstream
// 04b97ef0) to the registry: the allowlist keeps MCP tools registered unless
// it is empty or names one, and an MCP tool it keeps without naming is never
// declared directly — only tool_search could load it.
func TestAllowlistKeepsMCPTools(t *testing.T) {
	off := false
	codemode := ToolDefinition{Tool: echoTool("codemode"), DefaultActive: &off}
	docs := []string{"mcp__docs__search", "mcp__docs__fail", ListMCPResourcesTool, ReadMCPResourceTool}

	// "keeps MCP tools when --tools names no MCP tool"
	for _, exposure := range []ToolExposure{ToolExposureCodemode, ToolExposureDeferred} {
		sess := registrySession(t, SessionOptions{ToolNames: []string{"read", "codemode"}})
		sess.RegisterTools(codemode)
		sess.RegisterTools(mcpTools(exposure, docs...)...)
		if got := sess.ActiveToolNames(); !slices.Equal(got, []string{"read", "codemode"}) {
			t.Fatalf("%s: active %v", exposure, got)
		}
		callable := agentToolNameList(sess.CallableTools())
		for _, name := range docs {
			if !slices.Contains(callable, name) {
				t.Fatalf("%s: %s not callable: %v", exposure, name, callable)
			}
		}
		if slices.Contains(registeredNames(sess), "bash") {
			t.Fatalf("%s: bash registered", exposure)
		}
	}
	// A direct MCP tool stays registered but is declared only when the
	// allowlist names it — also when set or restored by name.
	sess := registrySession(t, SessionOptions{ToolNames: []string{"codemode"}})
	sess.RegisterTools(codemode)
	sess.RegisterTools(mcpTools(ToolExposureDirect, "mcp__docs__fail")...)
	if !slices.Contains(registeredNames(sess), "mcp__docs__fail") {
		t.Fatal("direct MCP tool not registered")
	}
	sess.SetActiveTools([]string{"codemode", "mcp__docs__fail"})
	if got := sess.ActiveToolNames(); !slices.Equal(got, []string{"codemode"}) {
		t.Fatalf("direct MCP tool declared: %v", got)
	}

	// "removes MCP tools with --no-tools"
	none := registrySession(t, SessionOptions{ToolNames: []string{}})
	none.RegisterTools(mcpTools(ToolExposureDirect, docs...)...)
	if got := registeredNames(none); len(got) != 0 {
		t.Fatalf("--no-tools registered %v", got)
	}

	// "lets tool_search declare unnamed MCP tools when --tools names it"
	search := registrySession(t, SessionOptions{ToolNames: []string{"tool_search"}})
	search.RegisterTools(ToolDefinition{Tool: echoTool("tool_search"), DefaultActive: &off})
	search.RegisterTools(mcpTools(ToolExposureDeferred, "mcp__docs__search")...)
	search.SetActiveTools([]string{"tool_search", "mcp__docs__search"})
	if got := search.ActiveToolNames(); !slices.Equal(got, []string{"tool_search", "mcp__docs__search"}) {
		t.Fatalf("tool_search: active %v", got)
	}

	// "filters MCP tools by the mcp__ entries of --tools"
	filtered := registrySession(t, SessionOptions{ToolNames: []string{"codemode", "mcp__docs__s*"}})
	filtered.RegisterTools(codemode)
	filtered.RegisterTools(mcpTools(ToolExposureCodemode, "mcp__docs__search", "mcp__docs__fail", ListMCPResourcesTool)...)
	filtered.RegisterTools(mcpTools(ToolExposureDirect, "mcp__docs__shot")...)
	if got := filtered.ActiveToolNames(); !slices.Equal(got, []string{"codemode", "mcp__docs__shot"}) {
		t.Fatalf("mcp__ entries: active %v", got)
	}
	registered := registeredNames(filtered)
	if !slices.Contains(registered, "mcp__docs__search") || slices.Contains(registered, "mcp__docs__fail") || slices.Contains(registered, ListMCPResourcesTool) {
		t.Fatalf("mcp__ entries: registered %v", registered)
	}

	// "removes MCP tools matching --exclude-tools patterns"
	excluded := registrySession(t, SessionOptions{ToolNames: []string{"codemode"}, ExcludeTools: []string{"mcp__docs__f*"}})
	excluded.RegisterTools(mcpTools(ToolExposureCodemode, "mcp__docs__search", "mcp__docs__fail")...)
	if registered := registeredNames(excluded); !slices.Contains(registered, "mcp__docs__search") || slices.Contains(registered, "mcp__docs__fail") {
		t.Fatalf("exclude patterns: registered %v", registered)
	}
}

// TestRestoreSkipsUnnamedMCPTools is pi's "does not declare unnamed MCP tools
// restored from the transcript": a transcript that declared a direct MCP tool
// restores without it when the allowlist does not name it.
func TestRestoreSkipsUnnamedMCPTools(t *testing.T) {
	sess := registrySession(t, SessionOptions{ToolNames: []string{"read", "codemode"}})
	sess.RegisterTools(mcpTools(ToolExposureDirect, "mcp__docs__search")...)
	sess.LoadHistory(declaringTranscript("mcp__docs__search"))
	sess.RestoreToolsFromBranch(branchOf(sess.History()...))
	if got := sess.ActiveToolNames(); len(got) != 0 {
		t.Fatalf("active %v", got)
	}
	if !slices.Contains(registeredNames(sess), "mcp__docs__search") {
		t.Fatal("MCP tool not registered")
	}
}
