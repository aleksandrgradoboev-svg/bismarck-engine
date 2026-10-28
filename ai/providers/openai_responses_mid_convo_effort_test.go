package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// The thinking level travels as configuration_update input items on OpenAI
// GPT-6 with an API key, so a change keeps the prompt cache (pi 60ac3bcc,
// #10853). The cases transliterate
// packages/ai/test/openai-responses-mid-convo-effort.test.ts at e3bc05e8; the
// fetch mock is a custom HTTP client, so nothing leaves the process.

func midConvoEffortModel(compat string) *ai.Model {
	str := func(s string) *string { return &s }
	return &ai.Model{
		ID: "gpt-6-luna", Name: "GPT-6 Luna", Api: ai.APIOpenAIResponses, Provider: "openai",
		BaseURL: "https://api.openai.com/v1", Reasoning: true, Input: []string{"text"},
		ThinkingLevelMap: ai.ThinkingLevelMap{
			"off": str("none"), "minimal": nil, "low": str("low"), "medium": str("medium"),
			"high": str("high"), "xhigh": str("xhigh"), "max": str("max"),
		},
		ContextWindow: 272000, MaxTokens: 128000, Compat: json.RawMessage(compat),
	}
}

const midConvoEffortCompat = `{"supportsMidConvoEffort":true}`

func midConvoEffortUpdate(effort string) map[string]any {
	return map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": effort}}
}

func midConvoEffortSSE(withToolCall bool) string {
	item := `{"type":"message","id":"msg_resp_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer","annotations":[]}]}`
	if withToolCall {
		item = `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}","status":"completed"}`
	}
	events := []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":` + item + `}`,
		`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
	}
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	return b.String()
}

type midConvoEffortRun struct {
	apiKey       string
	model        *ai.Model
	withToolCall bool
}

// runMidConvoEffort is the suite's runOpenAI: the request body and the reply.
func runMidConvoEffort(t *testing.T, messages []ai.Message, effort string, run midConvoEffortRun) (map[string]any, *ai.AssistantMessage) {
	t.Helper()
	if run.apiKey == "" {
		run.apiKey = "sk-test"
	}
	if run.model == nil {
		run.model = midConvoEffortModel(midConvoEffortCompat)
	}
	var payload map[string]any
	opts := &OpenAIResponsesOptions{ReasoningEffort: effort}
	opts.APIKey = run.apiKey
	opts.HTTPClient = doerFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		return &http.Response{
			StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(midConvoEffortSSE(run.withToolCall))), Request: r,
		}, nil
	})
	message := StreamOpenAIResponses(context.Background(), run.model, ai.NormalizeContext(ai.Context{Messages: messages}), opts).Result()
	if payload == nil {
		t.Fatalf("no request (stream ended %s: %q)", message.StopReason, message.ErrorMessage)
	}
	if message.StopReason == ai.StopError {
		t.Fatalf("stream failed: %q", message.ErrorMessage)
	}
	return payload, message
}

func midConvoEffortInput(payload map[string]any) []any {
	input, _ := payload["input"].([]any)
	return input
}

func midConvoEffortOf(payload map[string]any) any {
	reasoning, _ := payload["reasoning"].(map[string]any)
	return reasoning["effort"]
}

func midConvoEffortUpdates(input []any) []any {
	out := []any{}
	for _, item := range input {
		if m, _ := item.(map[string]any); m["type"] == "configuration_update" {
			out = append(out, item)
		}
	}
	return out
}

// asJSON turns built Go values into the plain JSON shape a decoded body has.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestResponsesConfigurationUpdateKeepsEffortConstantAndOnlyAppends(t *testing.T) {
	history := []ai.Message{ai.NewUserText("one", 0)}
	first, firstMessage := runMidConvoEffort(t, history, "high", midConvoEffortRun{})
	history = append(history, firstMessage, ai.NewUserText("two", 0))
	second, secondMessage := runMidConvoEffort(t, history, "high", midConvoEffortRun{})
	history = append(history, secondMessage, ai.NewUserText("three", 0))
	third, thirdMessage := runMidConvoEffort(t, history, "low", midConvoEffortRun{})

	want := asJSON(t, []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "one"}}},
		midConvoEffortUpdate("high"),
	})
	if got := midConvoEffortInput(first); !reflect.DeepEqual(any(got), want) {
		t.Fatalf("first input = %v, want %v", got, want)
	}
	for i, payload := range []map[string]any{first, second, third} {
		if effort := midConvoEffortOf(payload); effort != "medium" {
			t.Fatalf("request %d reasoning.effort = %v, want medium", i+1, effort)
		}
	}
	for i, pair := range [][2]map[string]any{{first, second}, {second, third}} {
		before, after := midConvoEffortInput(pair[0]), midConvoEffortInput(pair[1])
		if !reflect.DeepEqual(after[:len(before)], before) {
			t.Fatalf("request %d does not extend request %d: %v, then %v", i+2, i+1, before, after)
		}
	}
	updates := midConvoEffortUpdates(midConvoEffortInput(third))
	if want := asJSON(t, []any{midConvoEffortUpdate("high"), midConvoEffortUpdate("high"), midConvoEffortUpdate("low")}); !reflect.DeepEqual(any(updates), want) {
		t.Fatalf("updates = %v, want %v", updates, want)
	}
	input := midConvoEffortInput(third)
	if !reflect.DeepEqual(input[len(input)-1], asJSON(t, midConvoEffortUpdate("low"))) {
		t.Fatalf("last item = %v, want the low update", input[len(input)-1])
	}
	var levels []string
	for _, m := range []*ai.AssistantMessage{firstMessage, secondMessage, thirdMessage} {
		levels = append(levels, m.ProviderThinkingLevel)
	}
	if want := []string{"high", "high", "low"}; !reflect.DeepEqual(levels, want) {
		t.Fatalf("recorded levels = %v, want %v", levels, want)
	}
}

func TestResponsesConfigurationUpdateAppliesAToolLoopChangeToTheNextReply(t *testing.T) {
	history := []ai.Message{ai.NewUserText("one", 0)}
	toolCall, toolCallMessage := runMidConvoEffort(t, history, "high", midConvoEffortRun{withToolCall: true})
	history = append(history, toolCallMessage, ai.ToolResultMessage{
		ToolCallID: "call_1|fc_1", ToolName: "lookup", Content: ai.ContentList{ai.TextContent{Text: "result"}},
	})
	afterTool, afterToolMessage := runMidConvoEffort(t, history, "low", midConvoEffortRun{})

	before, after := midConvoEffortInput(toolCall), midConvoEffortInput(afterTool)
	if !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatalf("the request after the tool does not extend the tool-call request: %v, then %v", before, after)
	}
	output, _ := after[len(after)-2].(map[string]any)
	if output["type"] != "function_call_output" || output["call_id"] != "call_1" {
		t.Fatalf("second to last item = %v, want the call_1 function_call_output", output)
	}
	if !reflect.DeepEqual(after[len(after)-1], asJSON(t, midConvoEffortUpdate("low"))) {
		t.Fatalf("last item = %v, want the low update", after[len(after)-1])
	}
	if afterToolMessage.ProviderThinkingLevel != "low" {
		t.Fatalf("recorded level = %q, want low", afterToolMessage.ProviderThinkingLevel)
	}
}

func TestResponsesConfigurationUpdateOnlyBeforeRepliesRecordedByThisModel(t *testing.T) {
	_, unstamped := runMidConvoEffort(t, []ai.Message{ai.NewUserText("one", 0)}, "high", midConvoEffortRun{model: midConvoEffortModel(`{}`)})
	otherProvider, otherModel, recorded := *unstamped, *unstamped, *unstamped
	otherProvider.Provider, otherProvider.ProviderThinkingLevel = "opencode", "high"
	otherModel.Model, otherModel.ProviderThinkingLevel = "gpt-6-sol", "high"
	recorded.ProviderThinkingLevel = "high"

	payload, _ := runMidConvoEffort(t, []ai.Message{
		ai.NewUserText("one", 0), unstamped, ai.NewUserText("two", 0), &otherProvider,
		ai.NewUserText("three", 0), &otherModel, ai.NewUserText("four", 0), &recorded, ai.NewUserText("five", 0),
	}, "low", midConvoEffortRun{})

	var kinds []any
	for _, item := range midConvoEffortInput(payload) {
		m, _ := item.(map[string]any)
		if kind, ok := m["type"]; ok {
			kinds = append(kinds, kind)
		} else {
			kinds = append(kinds, m["role"])
		}
	}
	want := []any{"user", "message", "user", "message", "user", "message", "user", "configuration_update", "message", "user", "configuration_update"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("input kinds = %v, want %v", kinds, want)
	}
	updates := midConvoEffortUpdates(midConvoEffortInput(payload))
	if want := asJSON(t, []any{midConvoEffortUpdate("high"), midConvoEffortUpdate("low")}); !reflect.DeepEqual(any(updates), want) {
		t.Fatalf("updates = %v, want %v", updates, want)
	}
}

func TestResponsesConfigurationUpdateFallsBackToReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		name, apiKey, compat, level, effort string
	}{
		{"Sign in with ChatGPT", "chatgpt-access-token", midConvoEffortCompat, "low", "low"},
		{"the compat flag turned off", "sk-test", `{"supportsMidConvoEffort":false}`, "low", "low"},
		{"thinking off", "sk-test", midConvoEffortCompat, "", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := midConvoEffortRun{apiKey: tc.apiKey, model: midConvoEffortModel(tc.compat)}
			_, first := runMidConvoEffort(t, []ai.Message{ai.NewUserText("one", 0)}, "high", run)
			payload, message := runMidConvoEffort(t, []ai.Message{ai.NewUserText("one", 0), first, ai.NewUserText("two", 0)}, tc.level, run)
			if effort := midConvoEffortOf(payload); effort != tc.effort {
				t.Fatalf("reasoning.effort = %v, want %s", effort, tc.effort)
			}
			if updates := midConvoEffortUpdates(midConvoEffortInput(payload)); len(updates) != 0 {
				t.Fatalf("updates = %v, want none", updates)
			}
			if message.ProviderThinkingLevel != "" {
				t.Fatalf("recorded level = %q, want none", message.ProviderThinkingLevel)
			}
		})
	}
}
