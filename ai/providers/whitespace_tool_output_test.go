package providers

import (
	"fmt"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Whitespace-only tool output (a bare \r\n from bash) counts as empty on
// OpenAI Completions, OpenAI Responses and Google: some providers reject it
// with a 400 that then repeats on every request (pi 4dd2b149, #8720). The cases
// transliterate the whitespace cases pi added to
// openai-completions-tool-result-images.test.ts,
// openai-responses-empty-tool-result.test.ts and
// google-shared-image-tool-result-routing.test.ts.

var emptyToolOutputTexts = []string{"", "\r\n", " \n\t"}

func emptyToolOutputContext(api ai.Api, model, callID, text string) ai.Context {
	return ai.Context{Messages: []ai.Message{
		ai.NewUserText("Run the command", 1),
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{ID: callID, Name: "bash", Arguments: map[string]any{"command": "true"}}},
			Api:     api, Provider: "openai", Model: model, StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{ToolCallID: callID, ToolName: "bash", Content: ai.ContentList{ai.TextContent{Text: text}}, Timestamp: 2},
	}}
}

func TestOpenAICompletionsWhitespaceToolOutputIsNoToolOutput(t *testing.T) {
	model := &ai.Model{ID: "gpt-4o-mini", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: "https://api.openai.com/v1", Input: []string{"text", "image"}}
	for _, text := range emptyToolOutputTexts {
		t.Run(fmt.Sprintf("%q", text), func(t *testing.T) {
			body := mustBuildOpenAIParams(t, model, emptyToolOutputContext(ai.APIOpenAICompletions, model.ID, "tool-1", text), &OpenAIOptions{})
			msgs, _ := body["messages"].([]map[string]any)
			for _, m := range msgs {
				if m["role"] == "tool" {
					if m["content"] != "(no tool output)" {
						t.Fatalf("tool content = %#v, want (no tool output)", m["content"])
					}
					return
				}
			}
			t.Fatalf("no tool message: %#v", msgs)
		})
	}
}

func TestOpenAIResponsesWhitespaceToolOutputIsNoToolOutput(t *testing.T) {
	model := &ai.Model{ID: "gpt-4o-mini", Api: ai.APIOpenAIResponses, Provider: "openai", Input: []string{"text", "image"}}
	for _, text := range emptyToolOutputTexts {
		t.Run(fmt.Sprintf("%q", text), func(t *testing.T) {
			in := mustResponsesInput(t, model, emptyToolOutputContext(ai.APIOpenAIResponses, model.ID, "tool-1|fc_1", text))
			for _, item := range in {
				if m, ok := item.(map[string]any); ok && m["type"] == "function_call_output" {
					if m["output"] != "(no tool output)" {
						t.Fatalf("output = %#v, want (no tool output)", m["output"])
					}
					return
				}
			}
			t.Fatalf("no function_call_output: %#v", in)
		})
	}
}

// "treats whitespace-only tool result text as no text": whitespace next to an
// image leaves the image placeholder, and on its own the empty output.
func TestGoogleWhitespaceToolResultTextIsNoText(t *testing.T) {
	model := &ai.Model{ID: "gemini-3-pro-preview", Api: ai.APIGoogleGenerativeAI, Provider: "google", Input: []string{"text", "image"}}
	req := ai.Context{Messages: []ai.Message{
		ai.NewUserText("hi", 1),
		ai.AssistantMessage{Provider: "google", Model: model.ID, Api: ai.APIGoogleGenerativeAI, Content: ai.ContentList{
			ai.ToolCall{ID: "call_a", Name: "a", Arguments: map[string]any{}},
			ai.ToolCall{ID: "call_img", Name: "shot", Arguments: map[string]any{}},
			ai.ToolCall{ID: "call_b", Name: "b", Arguments: map[string]any{}},
		}},
		ai.ToolResultMessage{ToolCallID: "call_a", ToolName: "a", Content: ai.ContentList{ai.TextContent{Text: "\r\n"}}},
		ai.ToolResultMessage{ToolCallID: "call_img", ToolName: "shot", Content: ai.ContentList{
			ai.TextContent{Text: " \n"}, ai.ImageContent{MimeType: "image/png", Data: "AAAA"},
		}},
		ai.ToolResultMessage{ToolCallID: "call_b", ToolName: "b", Content: ai.ContentList{ai.TextContent{Text: "beta text"}}},
	}}
	body := roundtripBody(t, mustBuildGoogleParams(t, model, req, &GoogleOptions{}))
	contents := body["contents"].([]any)
	parts := contents[2].(map[string]any)["parts"].([]any)
	var outputs []any
	for _, part := range parts {
		if fr, ok := part.(map[string]any)["functionResponse"].(map[string]any); ok {
			outputs = append(outputs, fr["response"].(map[string]any)["output"])
		}
	}
	want := []any{"", "(see attached image)", "beta text"}
	if fmt.Sprint(outputs) != fmt.Sprint(want) {
		t.Fatalf("outputs = %q, want %q", outputs, want)
	}
}
