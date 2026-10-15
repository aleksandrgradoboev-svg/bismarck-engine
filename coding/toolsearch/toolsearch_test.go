package toolsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
)

func tool(name, description string, properties string) agent.AgentTool {
	var schema ai.Schema
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{`+properties+`}}`), &schema); err != nil {
		panic(err)
	}
	return agent.AgentTool{Name: name, Label: name, Description: description, Parameters: &schema,
		Execute: func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: name}}}, nil
		}}
}

func names(matches []Match) []string {
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.Name
	}
	return out
}

// TestTokenize is pi's "splits camelCase and snake_case, drops stop words, and
// folds plurals".
func TestTokenize(t *testing.T) {
	if got := Tokenize("listIssues for the GitHub_repo"); !slices.Equal(got, []string{"list", "issue", "git", "hub", "repo"}) {
		t.Fatalf("got %q", got)
	}
	if got := Tokenize("searches queries HTTPServer"); !slices.Equal(got, []string{"search", "query", "http", "server"}) {
		t.Fatalf("got %q", got)
	}
	// Only ASCII letters and digits make terms, as in pi.
	if got := Tokenize("реквизиты документа"); len(got) != 0 {
		t.Fatalf("Cyrillic made terms: %q", got)
	}
}

var documents = func() []Document {
	tools := []agent.AgentTool{
		tool("mcp__github__list_issues", "List issues in a repository.", `"state":{"type":"string","description":"open or closed"}`),
		tool("mcp__github__create_pull_request", "Open a pull request.", ""),
		tool("mcp__linear__search_issues", "Search Linear issues by text.", ""),
		tool("mcp__docs__search", "Search the documentation.", ""),
	}
	out := make([]Document, len(tools))
	for i, t := range tools {
		out[i] = NewDocument(t, nil)
	}
	return out
}()

// TestRankerRanks is pi's "ranks by term relevance and respects the limit".
func TestRankerRanks(t *testing.T) {
	r := Ranker{}
	if got := names(r.Rank("issue", documents, 8)); !slices.Equal(got, []string{"mcp__linear__search_issues", "mcp__github__list_issues"}) {
		t.Fatalf("issue: %v", got)
	}
	if got := r.Rank("pull requests", documents, 8); len(got) == 0 || got[0].Name != "mcp__github__create_pull_request" {
		t.Fatalf("pull requests: %v", got)
	}
	if got := r.Rank("search", documents, 1); len(got) != 1 {
		t.Fatalf("limit: %v", got)
	}
	// Property names and descriptions are searchable.
	if got := names(r.Rank("closed", documents, 8)); !slices.Equal(got, []string{"mcp__github__list_issues"}) {
		t.Fatalf("closed: %v", got)
	}
}

// TestRankerScoresMatchPi pins the scores pi's Bm25Ranker gives the same
// documents, captured by running tool.ts v1.2.0 under Node 24.
func TestRankerScoresMatchPi(t *testing.T) {
	for _, c := range []struct {
		query string
		want  string
	}{
		{"issue", "mcp__linear__search_issues=1.084287722691 mcp__github__list_issues=1.046298079853"},
		{"pull requests", "mcp__github__create_pull_request=3.699577651331"},
		{"search", "mcp__docs__search=1.169191165904 mcp__linear__search_issues=1.084287722691"},
		{"closed", "mcp__github__list_issues=1.116509309124"},
		{"search issues docs", "mcp__docs__search=2.987902458341 mcp__linear__search_issues=2.168575445383 mcp__github__list_issues=1.046298079853"},
	} {
		var parts []string
		for _, m := range (Ranker{}).Rank(c.query, documents, 8) {
			parts = append(parts, fmt.Sprintf("%s=%.12f", m.Name, m.Score))
		}
		if got := strings.Join(parts, " "); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.query, got, c.want)
		}
	}
}

// TestRankerFindsNothing is pi's "returns nothing for unknown or empty
// queries".
func TestRankerFindsNothing(t *testing.T) {
	r := Ranker{}
	for _, query := range []string{"kubernetes", "the", "tickets"} {
		if got := r.Rank(query, documents, 8); len(got) != 0 {
			t.Fatalf("%s: %v", query, got)
		}
	}
}

// TestDocumentNamespace is pi's "includes the namespace in the search text".
func TestDocumentNamespace(t *testing.T) {
	document := NewDocument(tool("mcp__x__run", "Run it.", ""), &coding.ToolNamespace{Name: "mcp__x", Description: "Kubernetes cluster tools"})
	got := Ranker{}.Rank("kubernetes", []Document{document}, 8)
	if len(got) != 1 || got[0].Name != "mcp__x__run" || got[0].Score <= 0 {
		t.Fatalf("got %v", got)
	}
}

func session(t *testing.T) *coding.Session {
	t.Helper()
	return coding.NewSession(coding.SessionOptions{
		Model: &ai.Model{ID: "m", Api: "faux", Provider: "faux", ContextWindow: 100000, MaxTokens: 8192},
		Cwd:   t.TempDir(), NoTools: coding.NoToolsBuiltin, NoContextFiles: true, NoSkills: true,
	})
}

func run(t *testing.T, def coding.ToolDefinition, args map[string]any) (agent.AgentToolResult, error) {
	t.Helper()
	return def.Tool.Execute(context.Background(), "call", args, nil)
}

// TestToolSearchLoads: tool_search registers inactive and model-only, finds
// codemode and deferred tools that are not active, activates the matches, and
// says so; direct, hidden and already active tools are not candidates.
func TestToolSearchLoads(t *testing.T) {
	sess := session(t)
	def := Definition(sess)
	sess.RegisterTools(def)
	if slices.Contains(sess.ActiveToolNames(), ToolName) {
		t.Fatal("tool_search registered active")
	}
	if def.Exposure != coding.ToolExposureModelOnly {
		t.Fatalf("exposure %q", def.Exposure)
	}
	sess.RegisterTools(
		coding.ToolDefinition{Tool: tool("mcp__docs__search", "Search the documentation.\nSecond line.", ""), Exposure: coding.ToolExposureDeferred},
		coding.ToolDefinition{Tool: tool("mcp__docs__index", "Index documentation pages.", ""), Exposure: coding.ToolExposureCodemode},
		coding.ToolDefinition{Tool: tool("docs_direct", "Direct documentation tool.", "")},
		coding.ToolDefinition{Tool: tool("docs_hidden", "Hidden documentation tool.", ""), Exposure: coding.ToolExposureHidden},
	)
	sess.SetActiveTools([]string{ToolName, "docs_direct"})

	result, err := run(t, def, map[string]any{"query": "documentation", "limit": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(ai.TextContent).Text
	// Both mention "documentation" once; BM25 normalizes by length, so the
	// shorter index document ranks first. Only the first description line is
	// shown.
	want := "Loaded 2 tools. They are available from your next call:\n- mcp__docs__index: Index documentation pages.\n- mcp__docs__search: Search the documentation."
	if text != want {
		t.Fatalf("text:\n%s\nwant:\n%s", text, want)
	}
	if got := result.Details.(Details).Loaded; !slices.Equal(got, []string{"mcp__docs__index", "mcp__docs__search"}) {
		t.Fatalf("loaded %v", got)
	}
	if got := sess.ActiveToolNames(); !slices.Equal(got, []string{ToolName, "docs_direct", "mcp__docs__index", "mcp__docs__search"}) {
		t.Fatalf("active %v", got)
	}

	// Loaded tools are no longer candidates.
	result, err = run(t, def, map[string]any{"query": "documentation"})
	if err != nil {
		t.Fatal(err)
	}
	if text := result.Content[0].(ai.TextContent).Text; text != "No matching tools found." {
		t.Fatalf("second search: %q", text)
	}
	if b, _ := json.Marshal(result.Details); string(b) != `{"loaded":[]}` {
		t.Fatalf("details %s", b)
	}
}

// TestToolSearchRejects: an empty query and a limit that is not a positive
// integer fail the call with pi's messages.
func TestToolSearchRejects(t *testing.T) {
	def := Definition(session(t))
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"query": "  "}, "query must not be empty"},
		{map[string]any{"query": "x", "limit": float64(0)}, "limit must be a positive integer"},
		{map[string]any{"query": "x", "limit": 1.5}, "limit must be a positive integer"},
	} {
		if _, err := run(t, def, c.args); err == nil || err.Error() != c.want {
			t.Errorf("%v: err %v, want %q", c.args, err, c.want)
		}
	}
	// Without session tools it finds nothing.
	result, err := Definition(nil).Tool.Execute(context.Background(), "c", map[string]any{"query": "docs"}, nil)
	if err != nil || result.Content[0].(ai.TextContent).Text != "No matching tools found." {
		t.Fatalf("nil tools: %v %v", result, err)
	}
}

// TestDescriptionIsStable: the description names no tool, so registering tools
// does not change it.
func TestDescriptionIsStable(t *testing.T) {
	if strings.Contains(Description, "mcp__") || !strings.Contains(Description, "always use `tool_search`") {
		t.Fatalf("description:\n%s", Description)
	}
}
