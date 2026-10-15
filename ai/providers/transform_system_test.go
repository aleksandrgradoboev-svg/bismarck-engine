package providers

import (
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// transformMessages at upstream 9e05370b2: system messages pass the first pass
// untouched, and a system message that lands while tool calls are pending is
// held back and emitted after their results — synthetic ones included — so it
// never splits a tool call from its answer. Expected orders captured by running
// pi's transformMessages on the same transcripts under node at the sha.
func TestTransformHoldsSystemMessagesUntilPendingToolCallsClose(t *testing.T) {
	model := &ai.Model{ID: "m", Api: ai.APIOpenAICompletions, Provider: "p", Input: []string{"text"}}
	toolCallTurn := func() ai.AssistantMessage {
		return ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{ID: "call_1", Name: "base_tool", Arguments: map[string]any{}}},
			Api:     ai.APIOpenAICompletions, Provider: "p", Model: "m", StopReason: ai.StopToolUse, Timestamp: 2,
		}
	}
	mid := ai.NewSystemText("mid", 3)
	result := ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "base_tool", Content: ai.ContentList{ai.TextContent{Text: "r"}}, Timestamp: 4}
	textTurn := ai.AssistantMessage{
		Content: ai.ContentList{ai.TextContent{Text: "t"}},
		Api:     ai.APIOpenAICompletions, Provider: "p", Model: "m", StopReason: ai.StopStop, Timestamp: 6,
	}

	cases := []struct {
		name     string
		messages []ai.Message
		want     string
	}{
		{"answered", []ai.Message{ai.NewUserText("u", 1), toolCallTurn(), mid, result, ai.NewUserText("next", 5)},
			"user,assistant,toolResult:call_1:r,system:mid,user"},
		{"orphaned by a user turn", []ai.Message{ai.NewUserText("u", 1), toolCallTurn(), mid, ai.NewUserText("next", 5)},
			"user,assistant,toolResult:call_1:No result provided,system:mid,user"},
		{"trailing", []ai.Message{ai.NewUserText("u", 1), toolCallTurn(), mid},
			"user,assistant,toolResult:call_1:No result provided,system:mid"},
		{"before the next assistant", []ai.Message{ai.NewUserText("u", 1), toolCallTurn(), mid, textTurn},
			"user,assistant,toolResult:call_1:No result provided,system:mid,assistant"},
		{"nothing pending", []ai.Message{mid, ai.NewUserText("u", 1), ai.NewSystemText("after user", 5)},
			"system:mid,user,system:after user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := transformMessages(tc.messages, model, nil)
			parts := make([]string, len(got))
			for i, m := range got {
				parts[i] = string(m.MessageRole())
				switch v := m.(type) {
				case ai.ToolResultMessage:
					parts[i] += ":" + v.ToolCallID + ":" + ai.ContentText(v.Content)
				case ai.SystemMessage:
					parts[i] += ":" + ai.GetSystemMessageText(v)
				}
			}
			if joined := strings.Join(parts, ","); joined != tc.want {
				t.Fatalf("transformed order\n got %s\nwant %s", joined, tc.want)
			}
		})
	}
}
