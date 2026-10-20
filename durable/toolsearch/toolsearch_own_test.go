package toolsearch

import (
	"context"
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
)

// Cases beyond pi's tests: the tool's checks, declaration and search text,
// which no pi case reaches.

// deferredAPI offers deferred tools and nothing else.
type deferredAPI struct {
	harness.ToolExecutionAPI
	deferred []*harness.ToolRegistration
}

func (a *deferredAPI) Agent(context.Context) (*harness.Agent, error) {
	return &harness.Agent{Deferred: a.deferred}, nil
}

func execute(t *testing.T, args map[string]any, deferred ...*harness.ToolRegistration) (harness.ToolExecutionResult, error) {
	t.Helper()
	return NewTool().Execute(bg, args, &deferredAPI{deferred: deferred})
}

func TestToolSearchRejectsAnEmptyQueryAndALimitThatIsNotAPositiveInteger(t *testing.T) {
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"query": "  "}, "query must not be empty"},
		{map[string]any{"query": "issue", "limit": 1.5}, "limit must be a positive integer"},
		{map[string]any{"query": "issue", "limit": 0.0}, "limit must be a positive integer"},
	} {
		if _, err := execute(t, c.args); err == nil || err.Error() != c.want {
			t.Errorf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}
}

func TestToolSearchIsCalledOnlyByTheModelAndRerunsSafely(t *testing.T) {
	tool := NewTool()
	if !reflect.DeepEqual(tool.Callers, []string{harness.CallerModel}) || tool.Replay != harness.ReplaySafe {
		t.Fatalf("callers %v, replay %q", tool.Callers, tool.Replay)
	}
}

func TestToolSearchRequestsNoControlWhenNothingMatches(t *testing.T) {
	result, err := execute(t, map[string]any{"query": "weather"}, tool("github_get_issue", "Get a GitHub issue", true))
	must(t, err)
	if result.Control != nil || !reflect.DeepEqual(result.Details, &Details{Loaded: []string{}}) {
		t.Fatalf("control %+v, details %+v", result.Control, result.Details)
	}
}

func TestToolSearchListsOnlyTheFirstLineOfADescription(t *testing.T) {
	result, err := execute(t, map[string]any{"query": "issue"}, tool("github_get_issue", "  Get an issue.\r\nMore detail here.\n", true))
	must(t, err)
	want := "Loaded 1 tool. They are available from your next call:\n- github_get_issue: Get an issue."
	if got := result.Output[0].(ai.TextContent).Text; got != want {
		t.Fatalf("text = %q", got)
	}
}

func TestToolSearchSearchesTheNameTheSpacedNameTheDescriptionAndTheParameters(t *testing.T) {
	got := searchText(tool("github_get_issue", "Get an issue", true))
	if want := "github_get_issue github get issue Get an issue number Issue number"; got != want {
		t.Fatalf("search text = %q, want %q", got, want)
	}
}

func TestToolSearchExtensionIsAFreshToolSearchExtension(t *testing.T) {
	extension := NewExtension()
	if extension.Name != "tool-search" || len(extension.Tools) != 1 || extension.Tools[0].Name != ToolName {
		t.Fatalf("extension %q with %d tools", extension.Name, len(extension.Tools))
	}
	if NewExtension() == extension {
		t.Fatal("extensions are shared")
	}
}
