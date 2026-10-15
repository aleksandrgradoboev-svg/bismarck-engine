package mcpext

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/mcp"
)

// "MCP servers section" is ported from pi v1.2.0
// packages/coding-agent/test/mcp-extension.test.ts; the rest covers what
// pi's AgentSession MCP tests check of the extension itself.

func listing(name, description string, exposure Exposure) ServerListing {
	return ServerListing{Entry: ServerEntry{Name: name, Source: "test", Config: ServerConfig{Command: "x", Description: description, Exposure: exposure}}}
}

func TestServersSectionListsIndirectServers(t *testing.T) {
	plain := listing("plain", "", "")
	plain.Instructions = "From instructions."
	section := RenderServersSection([]ServerListing{
		listing("docs", "Docs search.\nMore.", ""),
		listing("later", "", ExposureDeferred),
		listing("direct", "Declared.", ExposureDirect),
		plain,
	})
	lines := strings.Split(section, "\n")
	want := []string{"- mcp__docs (codemode): Docs search.", "- mcp__later (tool_search)", "- mcp__plain (codemode): From instructions."}
	if strings.Join(lines[1:], "\n") != strings.Join(want, "\n") {
		t.Fatalf("section:\n%s", section)
	}
	if lines[0] != "MCP servers whose tools are not declared to you. Call the tools of `codemode` servers from codemode scripts. Load the tools of `tool_search` servers with `tool_search`." {
		t.Fatalf("intro %q", lines[0])
	}
	if s := RenderServersSection([]ServerListing{listing("direct", "Declared.", ExposureDirect)}); s != "" {
		t.Fatalf("direct-only section %q", s)
	}
}

func TestServersSectionShortensDescriptions(t *testing.T) {
	var servers []ServerListing
	for i := range 40 {
		servers = append(servers, listing(fmt.Sprintf("server%d", i), strings.Repeat("x", 400), ""))
	}
	section := RenderServersSection(servers)
	if utf16Length(section) > MaxServersSectionChars || len(strings.Split(section, "\n")) != 41 || !strings.Contains(section, "- mcp__server39 (codemode): x") {
		t.Fatalf("%d chars, %d lines", utf16Length(section), len(strings.Split(section, "\n")))
	}
}

func TestServersSectionLeavesOutServersThatDoNotFit(t *testing.T) {
	var servers []ServerListing
	for i := range 200 {
		servers = append(servers, listing(fmt.Sprintf("server-with-a-long-name-%d", i), "desc", ""))
	}
	section := RenderServersSection(servers)
	if utf16Length(section) > MaxServersSectionChars {
		t.Fatalf("%d chars", utf16Length(section))
	}
	lines := strings.Split(section, "\n")
	m := regexp.MustCompile(`^- … (\d+) more servers; find their tools with searchTools\(\)$`).FindStringSubmatch(lines[len(lines)-1])
	if m == nil {
		t.Fatalf("last line %q", lines[len(lines)-1])
	}
	omitted, _ := strconv.Atoi(m[1])
	if len(lines)-2+omitted != 200 {
		t.Fatalf("%d lines, %d omitted", len(lines), omitted)
	}
}

// managerWith runs a manager over fake servers, one per entry, keyed by name.
func managerWith(t *testing.T, entries []ServerEntry, servers map[string]fakeServerOptions) (*Manager, map[string]*fakeServer) {
	t.Helper()
	var mu sync.Mutex
	fakes := map[string]*fakeServer{}
	m := NewManager(ManagerOptions{
		Config: LoadedConfig{Servers: entries},
		Cwd:    t.TempDir(),
		NewTransport: func(e ServerEntry, _ string, _ mcp.AuthProvider) (mcp.Transport, error) {
			f, tr := newFakeServer(servers[e.Name])
			mu.Lock()
			fakes[e.Name] = f
			mu.Unlock()
			return tr, nil
		},
	})
	t.Cleanup(m.Close)
	m.Start()
	if err := m.WaitForServers(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m, fakes
}

func toolExposures(m *Manager) string {
	var parts []string
	for _, d := range m.Tools() {
		parts = append(parts, d.Tool.Name+"="+string(d.Exposure))
	}
	return strings.Join(parts, " ")
}

func TestManagerToolsAndExposure(t *testing.T) {
	docs := stdioEntry("docs")
	direct := stdioEntry("my-direct")
	direct.Config.Exposure = ExposureDirect
	direct.Config.ToolExposure = []ToolExposureEntry{{Tool: "secret", Exposure: ExposureHidden}, {Tool: "lazy*", Exposure: ExposureDeferred}}
	m, _ := managerWith(t, []ServerEntry{docs, direct}, map[string]fakeServerOptions{
		"docs":      {tools: []mcp.Tool{tool("search")}},
		"my-direct": {tools: []mcp.Tool{tool("run"), tool("secret"), tool("lazy-load")}},
	})
	got := toolExposures(m)
	for _, want := range []string{"mcp__docs__search=codemode", "mcp__my_direct__run=direct", "mcp__my_direct__secret=hidden", "mcp__my_direct__lazy_load=deferred"} {
		if !strings.Contains(got, want) {
			t.Errorf("tools %s lack %s", got, want)
		}
	}
	direct2 := m.DirectTools()
	if len(direct2) != 1 || direct2[0].Name != "mcp__my_direct__run" {
		t.Fatalf("direct tools %v", direct2)
	}
	r, err := direct2[0].Execute(context.Background(), "1", map[string]any{}, nil)
	if err != nil || r.Content[0].(ai.TextContent).Text != "ok" {
		t.Fatalf("execute %+v %v", r, err)
	}
	codemode, toolSearch := m.Discovery()
	if !codemode || !toolSearch {
		t.Fatalf("discovery %v %v", codemode, toolSearch)
	}
	if !strings.Contains(m.ServersSection(), "- mcp__docs (codemode)") {
		t.Fatalf("section %q", m.ServersSection())
	}
}

func TestManagerHidesDroppedToolsAndRestoresThem(t *testing.T) {
	entry := stdioEntry("docs")
	entry.Config.Exposure = ExposureDirect
	m, fakes := managerWith(t, []ServerEntry{entry}, map[string]fakeServerOptions{"docs": {tools: []mcp.Tool{tool("a"), tool("b")}}})
	fakes["docs"].setTools([]mcp.Tool{tool("a")})
	waitUntil(t, func() bool { return strings.Contains(toolExposures(m), "mcp__docs__b=hidden") })
	if _, err := m.callerFor("docs", "b"); err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("dropped tool still callable: %v", err)
	}
	fakes["docs"].setTools([]mcp.Tool{tool("a"), tool("b")})
	waitUntil(t, func() bool { return strings.Contains(toolExposures(m), "mcp__docs__b=direct") })
	if len(m.Tools()) != 2 {
		t.Fatalf("tools %s", toolExposures(m))
	}
}

func TestManagerNamesCollidingToolsWithHashes(t *testing.T) {
	m, _ := managerWith(t, []ServerEntry{stdioEntry("docs")}, map[string]fakeServerOptions{"docs": {tools: []mcp.Tool{tool("a-b"), tool("a.b")}}})
	names := toolExposures(m)
	if strings.Contains(names, "mcp__docs__a_b=") || strings.Count(names, "mcp__docs__a_b_") != 2 {
		t.Fatalf("tools %s", names)
	}
}

func TestManagerResourceTools(t *testing.T) {
	entry := stdioEntry("docs")
	entry.Config.Exposure = ExposureDirect
	m, _ := managerWith(t, []ServerEntry{entry, stdioEntry("plain")}, map[string]fakeServerOptions{"docs": {resources: true}, "plain": {}})
	tools := map[string]func(map[string]any) (string, error){}
	for _, d := range m.DirectTools() {
		tool := d
		tools[d.Name] = func(params map[string]any) (string, error) {
			r, err := tool.Execute(context.Background(), "1", params, nil)
			if err != nil {
				return "", err
			}
			out, _ := json.Marshal(r.StructuredContent)
			return string(out), nil
		}
	}
	if len(tools) != 3 {
		t.Fatalf("direct tools %s", toolExposures(m))
	}
	got, err := tools[ListResourcesTool](map[string]any{})
	if err != nil || got != `{"resources":[{"server":"docs","uri":"docs://a","name":"a"}]}` {
		t.Fatalf("list %s %v", got, err)
	}
	got, err = tools[ListResourceTemplatesTool](map[string]any{"server": "docs"})
	if err != nil || got != `{"server":"docs","resourceTemplates":[]}` {
		t.Fatalf("templates %s %v", got, err)
	}
	got, err = tools[ReadResourceTool](map[string]any{"server": "docs", "uri": "docs://a"})
	if err != nil || got != `{"server":"docs","uri":"docs://a","contents":[{"uri":"docs://a","text":"ok"}]}` {
		t.Fatalf("read %s %v", got, err)
	}
	if _, err := tools[ReadResourceTool](map[string]any{"server": "plain", "uri": "x"}); err == nil || err.Error() != `MCP server "plain" has no resources. Servers with resources: docs` {
		t.Fatalf("read from a server without resources: %v", err)
	}
	if _, err := tools[ListResourcesTool](map[string]any{"cursor": "c"}); err == nil || !strings.Contains(err.Error(), "cursor can only be used") {
		t.Fatalf("cursor without server: %v", err)
	}
}

func TestManagerWaitForStartupIsBounded(t *testing.T) {
	entry := stdioEntry("slow")
	entry.Config.Exposure = ExposureDirect
	release := make(chan struct{})
	m := NewManager(ManagerOptions{
		Config:      LoadedConfig{Servers: []ServerEntry{entry}},
		Cwd:         t.TempDir(),
		StartupWait: 50 * time.Millisecond,
		NewTransport: func(ServerEntry, string, mcp.AuthProvider) (mcp.Transport, error) {
			<-release
			_, tr := newFakeServer(fakeServerOptions{})
			return tr, nil
		},
	})
	defer m.Close()
	defer close(release)
	m.Start()
	start := time.Now()
	if m.WaitForStartup(context.Background()) {
		t.Fatal("a hanging server counted as ready")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("waited %v", time.Since(start))
	}
	if !m.WaitForStartup(context.Background()) {
		t.Fatal("the second prompt waited again")
	}
}

func TestManagerDisableHidesToolsAndReports(t *testing.T) {
	entry := stdioEntry("docs")
	entry.Config.Exposure = ExposureDirect
	m, _ := managerWith(t, []ServerEntry{entry}, map[string]fakeServerOptions{"docs": {tools: []mcp.Tool{tool("a")}}})
	if !strings.HasPrefix(m.Status(), "docs: connected, 1 tools (direct)") {
		t.Fatalf("status %q", m.Status())
	}
	if err := m.SetEnabled("docs", false); err != nil {
		t.Fatal(err)
	}
	if toolExposures(m) != "mcp__docs__a=hidden" || len(m.DirectTools()) != 0 {
		t.Fatalf("tools %s", toolExposures(m))
	}
	if _, err := m.callerFor("docs", "a"); err == nil || !strings.Contains(err.Error(), "is disabled") {
		t.Fatalf("caller %v", err)
	}
	if m.Status() != "docs: disabled (direct)" {
		t.Fatalf("status %q", m.Status())
	}
	if err := m.SetEnabled("docs", true); err != nil {
		t.Fatal(err)
	}
	if err := m.WaitForServers(context.Background(), "docs"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return toolExposures(m) == "mcp__docs__a=direct" })
}

func TestManagerProblems(t *testing.T) {
	m := NewManager(ManagerOptions{
		Config: LoadedConfig{Servers: []ServerEntry{stdioEntry("broken")}, Errors: []string{"bad.json: oops"}},
		Cwd:    t.TempDir(),
		NewTransport: func(ServerEntry, string, mcp.AuthProvider) (mcp.Transport, error) {
			return nil, fmt.Errorf("spawn failed\nsecond line")
		},
	})
	defer m.Close()
	m.Start()
	if err := m.WaitForServers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.Problems(), "|"); got != "config: bad.json: oops|broken: failed: spawn failed" {
		t.Fatalf("problems %q", got)
	}
}
