package providers

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Anthropic rejects error tool results that contain images, so their images
// move after the tool results of the turn (pi 5ab7fe74, #10794). The cases
// transliterate packages/ai/test/anthropic-error-tool-result-images.test.ts.

func errorImagesModel() *ai.Model {
	return &ai.Model{
		ID: "claude-test", Name: "Claude Test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		BaseURL: "http://127.0.0.1:9", Input: []string{"text", "image"}, ContextWindow: 200000, MaxTokens: 1024,
	}
}

func errorImagesAssistant(calls ...ai.ToolCall) ai.AssistantMessage {
	content := make(ai.ContentList, len(calls))
	for i, call := range calls {
		content[i] = call
	}
	return ai.AssistantMessage{
		Content: content, Api: ai.APIAnthropicMessages, Provider: "anthropic", Model: "claude-test",
		StopReason: ai.StopToolUse, Timestamp: 2,
	}
}

// errorImagesThirdMessage captures the request body without cache retention,
// as pi's case does, and returns its third message as plain JSON.
func errorImagesThirdMessage(t *testing.T, messages []ai.Message) any {
	t.Helper()
	var captured map[string]any
	opts := &ai.SimpleStreamOptions{}
	opts.APIKey = "fake-key"
	opts.CacheRetention = ai.CacheNone
	opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
		captured, _ = payload.(map[string]any)
		return nil, errors.New("payload captured")
	}
	final := ai.StreamSimple(context.Background(), errorImagesModel(), ai.Context{Messages: messages}, opts).Result()
	if captured == nil {
		t.Fatalf("no payload captured (stream ended %s: %q)", final.StopReason, final.ErrorMessage)
	}
	return payloadJSON(t, captured)["messages"].([]any)[2]
}

func expectJSON(t *testing.T, got any, want string) {
	t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, w) {
		g, _ := json.Marshal(got)
		t.Fatalf("got  %s\nwant %s", g, want)
	}
}

func TestAnthropicMovesErrorToolResultImagesAfterTheToolResultsOfTheTurn(t *testing.T) {
	got := errorImagesThirdMessage(t, []ai.Message{
		ai.NewUserText("Show the screenshots", 1),
		errorImagesAssistant(ai.ToolCall{ID: "call-1", Name: "codemode"}, ai.ToolCall{ID: "call-2", Name: "read"}),
		ai.ToolResultMessage{ToolCallID: "call-1", ToolName: "codemode", IsError: true, Timestamp: 3, Content: ai.ContentList{
			ai.TextContent{Text: "Script failed"},
			ai.ImageContent{Data: "AAAA", MimeType: "image/png"},
			ai.ImageContent{Data: "CCCC", MimeType: "image/png"},
			ai.TextContent{Text: "Script error"},
		}},
		ai.ToolResultMessage{ToolCallID: "call-2", ToolName: "read", Timestamp: 4, Content: ai.ContentList{
			ai.ImageContent{Data: "BBBB", MimeType: "image/png"},
		}},
	})
	expectJSON(t, got, `{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"call-1","content":"Script failed\n(image attached after the tool results)\nScript error","is_error":true},
		{"type":"tool_result","tool_use_id":"call-2","content":[{"type":"text","text":"(see attached image)"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"BBBB"}}],"is_error":false},
		{"type":"text","text":"Images from the error result of tool call call-1 (codemode):"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"CCCC"}}]}`)
}

func TestAnthropicKeepsErrorToolResultsWithoutImagesUnchanged(t *testing.T) {
	got := errorImagesThirdMessage(t, []ai.Message{
		ai.NewUserText("Run it", 1),
		errorImagesAssistant(ai.ToolCall{ID: "call-1", Name: "codemode"}),
		ai.ToolResultMessage{ToolCallID: "call-1", ToolName: "codemode", IsError: true, Timestamp: 3, Content: ai.ContentList{
			ai.TextContent{Text: "Script failed"},
		}},
	})
	expectJSON(t, got, `{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"call-1","content":"Script failed","is_error":true}]}`)
}
