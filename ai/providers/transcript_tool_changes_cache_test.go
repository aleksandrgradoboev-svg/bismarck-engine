package providers

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Tool changes keep the prompt cache: Responses models with additional_tools
// declare every tool in place and announce removals as text, while tool search
// and Kimi tool messages move the top-level list once per removal (pi caa6967b,
// 530015d9, #10940). The cases transliterate the OpenAI and Kimi cases of
// packages/ai/test/transcript-tool-changes.test.ts at e3bc05e8. The prefix case
// runs on a Moonshot model with the compat flags of the catalog's kimi-coding
// models; the Kimi Coding cases (#10720) read that catalog, kimi-coding on Chat
// Completions since the 1.4.0 catalog (a3f265b1).

func additionalToolsModel() *ai.Model {
	return gpt54(`{"supportsMidConvoSystemMessages":true,"supportsAdditionalTools":true}`)
}

func kimiK3Model() *ai.Model {
	m := foldModel("kimi-k3", ai.APIOpenAICompletions, "moonshotai", `{"supportsMidConvoSystemMessages":true,"supportsMidConvoToolAdditions":true}`)
	m.Name = "Kimi K3"
	return m
}

// captureToolHistory is the suite's captureToolHistory: one request after each
// step — the initial tools, the removal of drop_tool, then two loads.
func captureToolHistory(t *testing.T, model *ai.Model) []map[string]any {
	t.Helper()
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool"), transcriptTool("drop_tool")}
	removal := ai.NewSystemText("", 2)
	removal.ToolsRemoved = []ai.ToolReference{{Name: "drop_tool"}}
	firstLoad := ai.NewSystemText("", 4)
	firstLoad.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	secondLoad := ai.NewSystemText("", 6)
	secondLoad.ToolsAdded = []ai.Tool{transcriptTool("later_tool")}
	steps := [][]ai.Message{
		{ai.NewUserText("first", 1)},
		{removal, ai.NewUserText("after removal", 3)},
		{firstLoad, ai.NewUserText("after first load", 5)},
		{secondLoad, ai.NewUserText("after second load", 7)},
	}
	messages := []ai.Message{leading}
	var payloads []map[string]any
	for _, step := range steps {
		messages = append(messages, step...)
		payloads = append(payloads, payloadJSON(t, captureStreamSimplePayload(t, model, ai.Context{Messages: slices.Clone(messages)})))
	}
	return payloads
}

// expectGrowingPrefix: every request's list starts with the previous one's.
func expectGrowingPrefix(t *testing.T, lists [][]any) {
	t.Helper()
	for i := 1; i < len(lists); i++ {
		previous := lists[i-1]
		if len(lists[i]) < len(previous) || !reflect.DeepEqual(lists[i][:len(previous)], previous) {
			t.Fatalf("request %d does not extend request %d:\n%v\nthen\n%v", i+1, i, previous, lists[i])
		}
	}
}

func jsonList(payload map[string]any, key string) []any {
	list, _ := payload[key].([]any)
	return list
}

func itemsContaining(input []any, text string) []string {
	var out []string
	for _, item := range input {
		if s, _ := item.(map[string]any)["content"].(string); strings.Contains(s, text) {
			out = append(out, s)
		}
	}
	return out
}

func TestRedefinesAnOpenAIToolWithAdditionalToolsInsteadOfAnnouncingARemoval(t *testing.T) {
	base := transcriptTool("base_tool")
	changed := base
	changed.Description = "changed"
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{base}
	redefinition := ai.NewSystemText("", 2)
	redefinition.ToolsRemoved = []ai.ToolReference{{Name: "base_tool"}}
	redefinition.ToolsAdded = []ai.Tool{changed}
	body := payloadJSON(t, captureStreamSimplePayload(t, additionalToolsModel(), ai.Context{Messages: []ai.Message{
		leading, ai.NewUserText("before", 1), redefinition,
	}}))

	if tools, present := body["tools"]; present {
		t.Fatalf("tools = %v, want undefined", tools)
	}
	input := jsonItems(body["input"])
	if len(input) != 5 {
		t.Fatalf("input = %v, want 5 items", input)
	}
	description := func(item map[string]any) any { return jsonItems(item["tools"])[0]["description"] }
	notice, _ := input[3]["content"].(string)
	switch {
	case input[0]["type"] != "additional_tools" || description(input[0]) != "base_tool tool":
		t.Fatalf("input[0] = %v, want the original base_tool", input[0])
	case input[1]["role"] != "developer" || input[1]["content"] != "base prompt":
		t.Fatalf("input[1] = %v, want the prompt", input[1])
	case input[2]["role"] != "user":
		t.Fatalf("input[2] = %v, want the user message", input[2])
	case input[3]["role"] != "developer" || !strings.Contains(notice, "latest definition replaces the earlier one"):
		t.Fatalf("input[3] = %v, want the incremental notice", input[3])
	case input[4]["type"] != "additional_tools" || description(input[4]) != "changed":
		t.Fatalf("input[4] = %v, want the changed base_tool", input[4])
	}
}

func TestKeepsTheOpenAIRequestPrefixAcrossARemovalAndLaterLoads(t *testing.T) {
	payloads := captureToolHistory(t, additionalToolsModel())
	var inputs [][]any
	for _, payload := range payloads {
		if tools, present := payload["tools"]; present {
			t.Fatalf("tools = %v, want undefined", tools)
		}
		inputs = append(inputs, jsonList(payload, "input"))
	}
	expectGrowingPrefix(t, inputs)
	last := inputs[len(inputs)-1]
	var declared [][]string
	for _, item := range jsonItems(last) {
		if item["type"] == "additional_tools" {
			declared = append(declared, toolNames(jsonItems(item["tools"])))
		}
	}
	if want := [][]string{{"base_tool", "drop_tool"}, {"late_tool"}, {"later_tool"}}; !reflect.DeepEqual(declared, want) {
		t.Fatalf("additional_tools = %v, want %v", declared, want)
	}
	if len(itemsContaining(last, "no longer available. Do not call them:\n- drop_tool")) == 0 {
		t.Fatal("no removal notice for drop_tool")
	}
	// Each later load restates the tools that are still unavailable.
	notices := itemsContaining(last, "incremental tools update")
	if len(notices) != 2 {
		t.Fatalf("notices = %q, want 2", notices)
	}
	for _, notice := range notices {
		if !strings.Contains(notice, "\nStill unavailable: drop_tool.") {
			t.Fatalf("notice %q does not restate drop_tool", notice)
		}
	}
}

func TestStopsListingAToolAsUnavailableOnceItIsDeclaredAgain(t *testing.T) {
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool"), transcriptTool("drop_tool")}
	removal := ai.NewSystemText("", 1)
	removal.ToolsRemoved = []ai.ToolReference{{Name: "drop_tool"}}
	again := ai.NewSystemText("", 2)
	again.ToolsAdded = []ai.Tool{transcriptTool("drop_tool")}
	body := payloadJSON(t, captureStreamSimplePayload(t, additionalToolsModel(), ai.Context{Messages: []ai.Message{
		leading, removal, again, ai.NewUserText("go", 3),
	}}))

	input := jsonList(body, "input")
	notices := itemsContaining(input, "incremental tools update")
	if len(notices) == 0 || strings.Contains(notices[0], "Still unavailable") {
		t.Fatalf("notices = %q, want one without Still unavailable", notices)
	}
	beforeLast := input[len(input)-2].(map[string]any)
	if beforeLast["type"] != "additional_tools" || toolNames(jsonItems(beforeLast["tools"]))[0] != "drop_tool" {
		t.Fatalf("input[-2] = %v, want the drop_tool additional_tools", beforeLast)
	}
}

func TestDoesNotListAToolAsStillUnavailableBeforeItsRemovalIsAnnounced(t *testing.T) {
	leading := ai.NewSystemText("base prompt", 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool"), transcriptTool("drop_tool")}
	change := ai.NewSystemText("", 1)
	change.ToolsAdded = []ai.Tool{transcriptTool("late_tool")}
	change.ToolsRemoved = []ai.ToolReference{{Name: "drop_tool"}}
	body := payloadJSON(t, captureStreamSimplePayload(t, additionalToolsModel(), ai.Context{Messages: []ai.Message{
		leading, change, ai.NewUserText("go", 2),
	}}))

	// Like Codex: notice, then the tools, then the removal notice.
	input := jsonItems(body["input"])
	tail := input[len(input)-4:]
	notice, _ := tail[0]["content"].(string)
	removal, _ := tail[2]["content"].(string)
	switch {
	case !strings.Contains(notice, "incremental tools update") || strings.Contains(notice, "Still unavailable"):
		t.Fatalf("notice = %q, want one without Still unavailable", notice)
	case tail[1]["type"] != "additional_tools" || toolNames(jsonItems(tail[1]["tools"]))[0] != "late_tool":
		t.Fatalf("tail[1] = %v, want late_tool's additional_tools", tail[1])
	case !strings.Contains(removal, "no longer available. Do not call them:\n- drop_tool"):
		t.Fatalf("tail[2] = %v, want the drop_tool removal notice", tail[2])
	case tail[3]["role"] != "user":
		t.Fatalf("tail[3] = %v, want the user message", tail[3])
	}
}

func TestKeepsTheOpenAIToolSearchPrefixAcrossLoadsAfterARemoval(t *testing.T) {
	payloads := captureToolHistory(t, gpt54(`{"supportsMidConvoSystemMessages":true,"supportsToolSearch":true}`))
	if got := toolNames(jsonItems(payloads[0]["tools"])); !slices.Equal(got, []string{"base_tool", "drop_tool"}) {
		t.Fatalf("tools before the removal = %v", got)
	}
	var inputs [][]any
	for _, payload := range payloads[1:] {
		if got := toolNames(jsonItems(payload["tools"])); !slices.Equal(got, []string{"base_tool"}) {
			t.Fatalf("tools after the removal = %v, want [base_tool]", got)
		}
		inputs = append(inputs, jsonList(payload, "input"))
	}
	expectGrowingPrefix(t, inputs)
	var loaded []string
	for _, item := range jsonItems(inputs[len(inputs)-1]) {
		if item["type"] == "tool_search_output" {
			loaded = append(loaded, toolNames(jsonItems(item["tools"]))[0])
		}
	}
	if want := []string{"late_tool", "later_tool"}; !slices.Equal(loaded, want) {
		t.Fatalf("tool_search_output = %v, want %v", loaded, want)
	}
}

func TestKeepsTheKimiPrefixAcrossLoadsAfterARemoval(t *testing.T) {
	payloads := captureToolHistory(t, kimiK3Model())
	functionNames := func(list any) []string {
		names := []string{}
		for _, tool := range jsonItems(list) {
			fn, _ := tool["function"].(map[string]any)
			name, _ := fn["name"].(string)
			names = append(names, name)
		}
		return names
	}
	if got := functionNames(payloads[0]["tools"]); !slices.Equal(got, []string{"base_tool", "drop_tool"}) {
		t.Fatalf("tools before the removal = %v", got)
	}
	var lists [][]any
	for _, payload := range payloads[1:] {
		if got := functionNames(payload["tools"]); !slices.Equal(got, []string{"base_tool"}) {
			t.Fatalf("tools after the removal = %v, want [base_tool]", got)
		}
		lists = append(lists, jsonList(payload, "messages"))
	}
	expectGrowingPrefix(t, lists)
	var anchored []string
	for _, message := range jsonItems(lists[len(lists)-1]) {
		if tools, ok := message["tools"]; ok {
			anchored = append(anchored, functionNames(tools)[0])
		}
	}
	if want := []string{"late_tool", "later_tool"}; !slices.Equal(anchored, want) {
		t.Fatalf("tool messages = %v, want %v", anchored, want)
	}
}

// kimiCodingToolNames reads a capture's top-level tools and the tools of its
// first tool-bearing message.
func kimiCodingToolNames(t *testing.T, modelID string) (top, anchored []string, anyAnchored bool) {
	t.Helper()
	model := ai.GetModel("kimi-coding", modelID)
	if model == nil {
		t.Fatalf("catalog has no kimi-coding/%s", modelID)
	}
	if model.Api != ai.APIOpenAICompletions {
		t.Fatalf("kimi-coding/%s api = %q, want %q", modelID, model.Api, ai.APIOpenAICompletions)
	}
	payload := payloadJSON(t, captureStreamSimplePayload(t, model, transcriptAdditionContext()))
	names := func(list any) []string {
		out := []string{}
		for _, tool := range jsonItems(list) {
			fn, _ := tool["function"].(map[string]any)
			name, _ := fn["name"].(string)
			out = append(out, name)
		}
		return out
	}
	top = names(payload["tools"])
	for _, message := range jsonItems(jsonList(payload, "messages")) {
		if tools, ok := message["tools"]; ok {
			if !anyAnchored {
				anchored = names(tools)
			}
			anyAnchored = true
		}
	}
	return top, anchored, anyAnchored
}

// #10720: Kimi Coding uses Chat Completions so K3-style models keep added
// tools out of the top-level list ('anchors additions for Kimi Coding %s').
func TestAnchorsAdditionsForKimiCoding(t *testing.T) {
	for _, id := range []string{"k3", "k3-256k", "kimi-for-coding"} {
		t.Run(id, func(t *testing.T) {
			top, anchored, _ := kimiCodingToolNames(t, id)
			if !slices.Equal(top, []string{"base_tool"}) || !slices.Equal(anchored, []string{"late_tool"}) {
				t.Errorf("top-level tools %v, anchored %v; want [base_tool], [late_tool]", top, anchored)
			}
		})
	}
}

// #10720: Kimi Code Highspeed rejects tool-bearing system messages ('keeps Kimi
// Coding Highspeed additions in top-level tools').
func TestKeepsKimiCodingHighspeedAdditionsInTopLevelTools(t *testing.T) {
	top, _, anyAnchored := kimiCodingToolNames(t, "kimi-for-coding-highspeed")
	if !slices.Equal(top, []string{"base_tool", "late_tool"}) || anyAnchored {
		t.Errorf("top-level tools %v, a tool-bearing message %t; want [base_tool late_tool], none", top, anyAnchored)
	}
}

// "resolves the top-level tool boundary per transport": removals and
// redeclarations move the boundary once; plain additions stay anchored.
func TestResolvesTheTopLevelToolBoundaryPerTransport(t *testing.T) {
	base, drop := transcriptTool("base_tool"), transcriptTool("drop_tool")
	lateX, lateY := transcriptTool("late_tool"), transcriptTool("late_tool")
	lateX.Description, lateY.Description = "x", "y"
	system := func(text string, ts int64, edit func(*ai.SystemMessage)) ai.Message {
		m := ai.NewSystemText(text, ts)
		edit(&m)
		return m
	}
	messages := []ai.Message{
		system("base prompt", 0, func(m *ai.SystemMessage) { m.ToolsAdded = []ai.Tool{base, drop} }),
		system("", 1, func(m *ai.SystemMessage) { m.ToolsRemoved = []ai.ToolReference{{Name: "drop_tool"}} }),
		system("", 2, func(m *ai.SystemMessage) { m.ToolsAdded = []ai.Tool{drop} }),
		system("", 3, func(m *ai.SystemMessage) { m.ToolsAdded = []ai.Tool{lateX} }),
		system("", 4, func(m *ai.SystemMessage) { m.ToolsAdded = []ai.Tool{lateY} }),
	}
	for _, tc := range []struct {
		name     string
		messages []ai.Message
		support  ai.TranscriptToolSupport
		tools    []ai.Tool
		folded   int
	}{
		{"removal", messages[:2], ai.ToolSupportAdditions, []ai.Tool{base}, 2},
		// Kimi K3 ignores a removed tool that a later tool message declares
		// again, so the re-added drop_tool moves the boundary; the new late_tool
		// after it stays anchored.
		{"re-added after removal", messages[:4], ai.ToolSupportAdditions, []ai.Tool{base, drop}, 3},
		{"redefinition", messages, ai.ToolSupportAdditions, []ai.Tool{base, drop, lateY}, 5},
		{"changes", messages, ai.ToolSupportChanges, nil, 0},
		{"none", messages, ai.ToolSupportNone, []ai.Tool{base, drop, lateY}, 5},
	} {
		got := ai.ResolveTranscriptTools(tc.messages, tc.support)
		if got.FoldedSystemMessages != tc.folded || !reflect.DeepEqual(got.RequestTools, tc.tools) {
			t.Fatalf("%s: got %+v, want tools %+v folded %d", tc.name, got, tc.tools, tc.folded)
		}
	}
}
