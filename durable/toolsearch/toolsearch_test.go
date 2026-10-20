package toolsearch

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/internal/chattest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/tool-search.test.ts.

var bg = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	chattest.Must(t, err)
}

func tool(name, description string, deferred bool) *harness.ToolRegistration {
	return &harness.ToolRegistration{
		Tool:     ai.Tool{Name: name, Description: description, Parameters: ai.Object(ai.Opt("number", ai.Number("Issue number")))},
		Deferred: deferred,
		Execute: func(context.Context, map[string]any, harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			return harness.ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: name + " ran"}}}, nil
		},
	}
}

func install(t *testing.T, c *chattest.Chat, tools ...*harness.ToolRegistration) {
	t.Helper()
	for _, each := range tools {
		c.Install(t, "tool:"+each.Name, each)
	}
}

// setup is alpha, the deferred github_get_issue and github_list_repos, then
// omega, and tool_search last.
func setup(t *testing.T) *chattest.Chat {
	t.Helper()
	c := chattest.New(t)
	install(t, c,
		tool("alpha", "Alpha tool", false),
		tool("github_get_issue", "Get a GitHub issue by number", true),
		tool("github_list_repos", "List the repositories of a user", true),
		tool("omega", "Omega tool", false))
	must(t, c.Registry.Install(NewExtension()))
	return c
}

func call(name string, args map[string]any, id string) providers.FauxResponseStep {
	return chattest.ToolCalls(chattest.Call{Name: name, Args: args, ID: id})
}

func names(tools []*harness.ToolRegistration) []string {
	out := []string{}
	for _, each := range tools {
		out = append(out, each.Name)
	}
	return out
}

func agentOf(t *testing.T, conversation *harness.Conversation) *harness.Agent {
	t.Helper()
	agent, err := conversation.Agent(bg)
	must(t, err)
	return agent
}

func offered(t *testing.T, conversation *harness.Conversation) []string {
	t.Helper()
	return names(agentOf(t, conversation).Tools)
}

// modelTools is the conversation's stored modelTools as JSON, "" when absent.
func modelTools(t *testing.T, h *harness.Harness, id durable.ConversationID) string {
	t.Helper()
	value, err := h.Snapshot(bg, harness.AgentDoc.Conversation(id))
	must(t, err)
	stored, ok := value["modelTools"]
	if !ok {
		return ""
	}
	b, err := json.Marshal(stored)
	must(t, err)
	return string(b)
}

func systemMessages(entries []durable.EntryRecord) []ai.SystemMessage {
	var out []ai.SystemMessage
	for _, entry := range entries {
		if entry.Kind == durable.SystemEntry.Kind {
			message, _ := entry.Model[0].(ai.SystemMessage)
			out = append(out, message)
		}
	}
	return out
}

func added(message ai.SystemMessage) []string {
	var out []string
	for _, each := range message.ToolsAdded {
		out = append(out, each.Name)
	}
	return out
}

func toolResultTexts(entries []durable.EntryRecord) []string {
	var out []string
	for _, entry := range chattest.ToolResults(entries) {
		out = append(out, chattest.ResultText(chattest.ResultMessage(entry)))
	}
	return out
}

func equal(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func TestToolSearchOffersADeferredToolOnlyOnceToolSearchLoadsItByAppendingItsDeclaration(t *testing.T) {
	c := setup(t)
	c.Faux.SetResponses([]providers.FauxResponseStep{
		call("tool_search", map[string]any{"query": "github issue", "limit": 1.0}, "c1"),
		call("github_get_issue", map[string]any{"number": 42.0}, "c2"),
		chattest.Done,
	})
	h, root := c.Open(t, memory.New(), nil)
	equal(t, "offered", offered(t, root), []string{"alpha", "omega", "tool_search"})
	equal(t, "deferred", names(agentOf(t, root).Deferred), []string{"github_get_issue", "github_list_repos"})
	// Deferred tools stay callable by other tools, such as code mode scripts.
	if !slices.Contains(names(agentOf(t, root).Callable), "github_get_issue") {
		t.Fatal("github_get_issue is not callable")
	}
	chattest.SubmitAndWait(t, root, "go")
	entries := chattest.Entries(t, root)
	texts := toolResultTexts(entries)
	equal(t, "results", texts, []string{
		"Loaded 1 tool. They are available from your next call:\n- github_get_issue: Get a GitHub issue by number",
		"github_get_issue ran",
	})
	equal(t, "modelTools", modelTools(t, h, root.ID), `{"add":["github_get_issue"]}`)
	equal(t, "offered", offered(t, root), []string{"alpha", "omega", "tool_search", "github_get_issue"})
	// A later search sees only the tools not loaded yet.
	equal(t, "deferred", names(agentOf(t, root).Deferred), []string{"github_list_repos"})
	// The load appended the declaration: no tool was removed and declared
	// again, so the cached prefix holds.
	system := systemMessages(entries)
	if len(system) < 2 {
		t.Fatalf("%d system messages", len(system))
	}
	equal(t, "baseline", added(system[0]), []string{"alpha", "omega", "tool_search"})
	equal(t, "load", added(system[1]), []string{"github_get_issue"})
	if system[1].ToolsRemoved != nil {
		t.Fatalf("load removed %v", system[1].ToolsRemoved)
	}
}

func TestToolSearchAppendsEachLoadedToolInLoadOrderNotRegistryOrder(t *testing.T) {
	c := setup(t)
	c.Faux.SetResponses([]providers.FauxResponseStep{
		call("tool_search", map[string]any{"query": "repositories", "limit": 1.0}, "c1"),
		call("tool_search", map[string]any{"query": "issue", "limit": 1.0}, "c2"),
		chattest.Done,
	})
	_, root := c.Open(t, memory.New(), nil)
	chattest.SubmitAndWait(t, root, "go")
	equal(t, "offered", offered(t, root), []string{"alpha", "omega", "tool_search", "github_list_repos", "github_get_issue"})
	loads := systemMessages(chattest.Entries(t, root))[1:]
	var got [][]string
	for _, message := range loads {
		got = append(got, added(message))
		if message.ToolsRemoved != nil {
			t.Fatalf("a load removed %v", message.ToolsRemoved)
		}
	}
	equal(t, "loads", got, [][]string{{"github_list_repos"}, {"github_get_issue"}})
}

func TestToolSearchRecordsLoadsByNameUnknownAndNonDeferredNamesChangeNothingRemoveWinsAndALoadLiftsIt(t *testing.T) {
	c := setup(t)
	grow := &harness.ToolRegistration{
		Tool: ai.Tool{Name: "grow", Description: "grow", Parameters: ai.Object()},
		Execute: func(context.Context, map[string]any, harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			return harness.ToolExecutionResult{Output: ai.ContentList{}, Control: &harness.ToolControl{AddTools: []string{"github_get_issue", "alpha", "ghost"}}}, nil
		},
	}
	install(t, c, grow)
	c.Faux.SetResponses([]providers.FauxResponseStep{call("grow", nil, "c1"), chattest.Done, call("grow", nil, "c1"), chattest.Done})
	h, root := c.Open(t, memory.New(), nil)
	chattest.SubmitAndWait(t, root, "go")
	equal(t, "modelTools", modelTools(t, h, root.ID), `{"add":["github_get_issue","alpha","ghost"]}`)
	equal(t, "offered", offered(t, root), []string{"alpha", "omega", "tool_search", "grow", "github_get_issue"})
	// A name in both remove and add is not offered.
	var getIssue *harness.ToolRegistration
	for _, each := range agentOf(t, root).Callable {
		if each.Name == "github_get_issue" {
			getIssue = each
		}
	}
	both := []*harness.ToolRegistration{getIssue}
	must(t, root.Configure(bg, harness.AgentChange{ModelTools: harness.Set(harness.Edit(both, both))}))
	if slices.Contains(offered(t, root), "github_get_issue") {
		t.Fatal("offered while removed")
	}
	// Loading it again drops it from remove.
	chattest.SubmitAndWait(t, root, "again")
	equal(t, "modelTools", modelTools(t, h, root.ID), `{"add":["github_get_issue","alpha","ghost"]}`)
	if !slices.Contains(offered(t, root), "github_get_issue") {
		t.Fatal("not offered after the load")
	}
}

func TestToolSearchKeepsALoadedToolWhenItsExtensionIsGoneForAWhileAndAppendsToAnExplicitArray(t *testing.T) {
	c := setup(t)
	issue := tool("github_get_issue", "Get a GitHub issue by number", true)
	c.Faux.SetResponses([]providers.FauxResponseStep{call("tool_search", map[string]any{"query": "github issue", "limit": 1.0}, "c1"), chattest.Done})
	h, root := c.Open(t, memory.New(), nil)
	chattest.SubmitAndWait(t, root, "go")
	// The server goes away: its extension is installed again without the
	// tool.
	must(t, c.Registry.Install(&harness.Extension{Name: "tool:github_get_issue"}))
	if slices.Contains(offered(t, root), "github_get_issue") {
		t.Fatal("offered while its extension has no such tool")
	}
	must(t, c.Registry.Install(&harness.Extension{Name: "tool:github_get_issue", Tools: []*harness.ToolRegistration{issue}}))
	if !slices.Contains(offered(t, root), "github_get_issue") {
		t.Fatal("not offered once the tool is back")
	}
	// With an explicit array, a load appends to it as before.
	var alpha, search *harness.ToolRegistration
	for _, each := range agentOf(t, root).Tools {
		switch each.Name {
		case "alpha":
			alpha = each
		case "tool_search":
			search = each
		}
	}
	must(t, root.Configure(bg, harness.AgentChange{ModelTools: harness.Set(harness.Exactly(alpha, search))}))
	c.Faux.SetResponses([]providers.FauxResponseStep{call("tool_search", map[string]any{"query": "repositories", "limit": 1.0}, "c1"), chattest.Done})
	chattest.SubmitAndWait(t, root, "more")
	equal(t, "modelTools", modelTools(t, h, root.ID), `["alpha","tool_search","github_list_repos"]`)
}

func TestToolSearchKeepsLoadedToolsPerBranchAndInTheBaselineAfterAReset(t *testing.T) {
	c := setup(t)
	c.Faux.SetResponses([]providers.FauxResponseStep{call("tool_search", map[string]any{"query": "github issue", "limit": 1.0}, "c1"), chattest.Done})
	_, root := c.Open(t, memory.New(), nil)
	chattest.SubmitAndWait(t, root, "go")
	entries := chattest.Entries(t, root)
	before, err := root.Fork(bg, entries[0].ID, harness.ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	after, err := root.Fork(bg, entries[len(entries)-1].ID, harness.ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	if slices.Contains(offered(t, before), "github_get_issue") {
		t.Fatal("the branch before the load offers it")
	}
	if !slices.Contains(offered(t, after), "github_get_issue") {
		t.Fatal("the branch after the load does not offer it")
	}
	must(t, root.Reset(bg, nil))
	c.Faux.SetResponses([]providers.FauxResponseStep{chattest.Done})
	chattest.SubmitAndWait(t, root, "fresh")
	system := systemMessages(chattest.Entries(t, root))
	equal(t, "baseline", added(system[len(system)-1]), []string{"alpha", "omega", "tool_search", "github_get_issue"})
}

func TestToolSearchFindsNothingForAnUnrelatedQueryAndLoadsNothing(t *testing.T) {
	c := setup(t)
	c.Faux.SetResponses([]providers.FauxResponseStep{call("tool_search", map[string]any{"query": "weather forecast"}, "c1"), chattest.Done})
	h, root := c.Open(t, memory.New(), nil)
	chattest.SubmitAndWait(t, root, "go")
	equal(t, "results", toolResultTexts(chattest.Entries(t, root)), []string{"No matching tools found."})
	equal(t, "modelTools", modelTools(t, h, root.ID), "")
}
