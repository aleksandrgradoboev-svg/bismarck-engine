package providers

import (
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Another model's thinking is dropped instead of sent as text, which made the
// new model treat it as part of its visible reply (pi 91130942, #10995). The
// cases transliterate packages/ai/test/transform-messages-cross-model-thinking.test.ts.

func crossModelKimi() *ai.Model {
	return &ai.Model{
		ID: "moonshotai/Kimi-K3", Name: "Kimi K3", Api: ai.APIOpenAICompletions, Provider: "sference",
		BaseURL: "https://api.sference.com/v1", Reasoning: true, Input: []string{"text", "image"},
		ContextWindow: 1048576, MaxTokens: 32768,
	}
}

func crossModelAssistant(model string, stop ai.StopReason, content ...ai.Content) ai.AssistantMessage {
	return ai.AssistantMessage{
		Content: content, Api: ai.APIOpenAICompletions, Provider: "sference", Model: model, StopReason: stop, Timestamp: 1,
	}
}

func assistantContents(messages []ai.Message) []ai.ContentList {
	var out []ai.ContentList
	for _, m := range messages {
		if am, ok := asAssistantMsg(m); ok {
			out = append(out, am.Content)
		}
	}
	return out
}

func TestDoesNotSendAnotherModelsReadableThinkingAsText(t *testing.T) {
	toolCall := ai.ToolCall{ID: "call_1", Name: "bash", Arguments: map[string]any{"command": "curl -v"}}
	result := transformMessages([]ai.Message{
		ai.NewUserText("Why does curl work but node fails?", 1),
		crossModelAssistant("zai-org/GLM-5.3", ai.StopToolUse,
			ai.ThinkingContent{Thinking: "The user asks about ECONNRESET. Candidates: proxy, IPv6, TLS.", ThinkingSignature: "reasoning_content"},
			ai.TextContent{Text: "Let me check the TLS versions."},
			toolCall,
		),
		ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "bash", Content: ai.ContentList{ai.TextContent{Text: "TLSv1.2"}}, Timestamp: 2},
	}, crossModelKimi(), nil)

	got := assistantContents(result)
	want := []ai.ContentList{{ai.TextContent{Text: "Let me check the TLS versions."}, toolCall}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("assistant content = %#v, want %#v", got, want)
	}
}

func TestKeepsThinkingOnlyFromTheTargetModelWhenModelsAlternate(t *testing.T) {
	thinking := func(text string) ai.ThinkingContent {
		return ai.ThinkingContent{Thinking: text, ThinkingSignature: "reasoning_content"}
	}
	text := func(s string) ai.TextContent { return ai.TextContent{Text: s} }
	result := transformMessages([]ai.Message{
		ai.NewUserText("1", 1),
		crossModelAssistant("moonshotai/Kimi-K3", ai.StopStop, thinking("kimi 1"), text("a")),
		ai.NewUserText("2", 1),
		crossModelAssistant("zai-org/GLM-5.3", ai.StopStop, thinking("glm 1"), text("b")),
		ai.NewUserText("3", 1),
		crossModelAssistant("moonshotai/Kimi-K3", ai.StopStop, thinking("kimi 2"), text("c")),
		ai.NewUserText("4", 1),
		crossModelAssistant("zai-org/GLM-5.3", ai.StopStop, thinking("glm 2"), text("d")),
		ai.NewUserText("hi", 1),
	}, crossModelKimi(), nil)

	want := []ai.ContentList{
		{thinking("kimi 1"), text("a")},
		{text("b")},
		{thinking("kimi 2"), text("c")},
		{text("d")},
	}
	if got := assistantContents(result); !reflect.DeepEqual(got, want) {
		t.Fatalf("assistant contents = %#v, want %#v", got, want)
	}
}
