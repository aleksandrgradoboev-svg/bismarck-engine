package providers

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// A finished tool call's arguments are parsed strictly: malformed text gives
// empty arguments and an argumentsError holding the reason and the raw text,
// instead of the prefix the streaming parser kept (pi 87f9a364, #10897). The
// cases transliterate packages/ai/test/json-parse.test.ts parseToolCallArguments.
func TestParseToolCallArguments(t *testing.T) {
	t.Run("parses complete JSON and repairs invalid escapes", func(t *testing.T) {
		args, _, errText := parseToolCallArguments(`{"kind":"seek","note":"\url"}`)
		if errText != "" || !reflect.DeepEqual(args, map[string]any{"kind": "seek", "note": `\url`}) {
			t.Fatalf("args = %#v, error %q", args, errText)
		}
	})
	t.Run("uses empty arguments for empty text", func(t *testing.T) {
		args, _, errText := parseToolCallArguments("")
		if errText != "" || !reflect.DeepEqual(args, map[string]any{}) {
			t.Fatalf("args = %#v, error %q", args, errText)
		}
	})
	t.Run("does not keep the valid prefix of malformed JSON", func(t *testing.T) {
		raw := `{"action":"apply","actions":[{"kind":"seek","time":.5}]}`
		args, _, errText := parseToolCallArguments(raw)
		if len(args) != 0 || !strings.Contains(errText, "\n\nReceived arguments:\n") || !strings.HasSuffix(errText, raw) {
			t.Fatalf("args = %#v, error %q", args, errText)
		}
	})
	t.Run("does not complete cut-off JSON", func(t *testing.T) {
		raw := `{"kind":"seek","clipId":"clip-fl`
		args, _, errText := parseToolCallArguments(raw)
		if len(args) != 0 || !strings.HasSuffix(errText, raw) {
			t.Fatalf("args = %#v, error %q", args, errText)
		}
	})
	t.Run("rejects JSON that is not an object", func(t *testing.T) {
		args, _, errText := parseToolCallArguments("[1,2]")
		if len(args) != 0 || errText != "Tool arguments must be a JSON object\n\nReceived arguments:\n[1,2]" {
			t.Fatalf("args = %#v, error %q", args, errText)
		}
	})
}

// pi's openai-responses-partial-json-cleanup.test.ts case for #10897: the
// finished call carries the error, not the fields before the bad number.
func TestResponsesMarksMalformedFinalArguments(t *testing.T) {
	argumentsJSON := `{"path":"README.md","content":"updated","mode":.5}`
	quoted := strings.ReplaceAll(argumentsJSON, `"`, `\"`)
	sse := `data: {"type":"response.created","response":{"id":"r"}}

data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"write","arguments":""}}

data: {"type":"response.function_call_arguments.delta","delta":"` + quoted + `"}

data: {"type":"response.function_call_arguments.done","arguments":"` + quoted + `"}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"write","arguments":"` + quoted + `"}}

data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`
	final := runResponsesSSE(t, reasoningModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, sse)
	if len(final.Content) == 0 {
		t.Fatalf("no content: %s %q", final.StopReason, final.ErrorMessage)
	}
	call, ok := final.Content[0].(ai.ToolCall)
	if !ok {
		t.Fatalf("content[0] = %T, want a tool call", final.Content[0])
	}
	if len(call.Arguments) != 0 || !strings.HasSuffix(call.ArgumentsError, "Received arguments:\n"+argumentsJSON) {
		t.Fatalf("arguments = %#v, error %q", call.Arguments, call.ArgumentsError)
	}
}
