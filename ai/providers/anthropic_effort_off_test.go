package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// The "off" thinking level on managed-effort models (pi dc939913, #10753):
// where the model accepts disabled thinking, "off" sends it without effort
// markers or block_binding; where it does not, adaptive thinking stays. The
// cases transliterate packages/ai/test/anthropic-mid-conversation-effort.test.ts
// at dc939913, its catalog case as of the 1.4.0 catalog.

// effortManagedModel is the suite's managedModel.
func effortManagedModel() *ai.Model {
	str := func(s string) *string { return &s }
	return &ai.Model{
		ID: "claude-fable-5-1", Name: "Claude Fable 5.1", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		Reasoning: true, Input: []string{"text"}, ContextWindow: 200000, MaxTokens: 32000,
		ThinkingLevelMap: ai.ThinkingLevelMap{
			"off": nil, "minimal": str("low"), "low": str("low"), "medium": str("medium"), "high": str("high"), "max": str("max"),
		},
		Compat: json.RawMessage(`{"forceAdaptiveThinking":true,"supportsMidConvoEffort":true}`),
	}
}

// effortAssistant is the suite's assistant: a reply with thinking that
// recorded level, if any.
func effortAssistant(model *ai.Model, level string) *ai.AssistantMessage {
	return &ai.AssistantMessage{
		Content: ai.ContentList{
			ai.ThinkingContent{Thinking: "reasoning", ThinkingSignature: "signature"},
			ai.TextContent{Text: "answer"},
		},
		Api: ai.APIAnthropicMessages, Provider: model.Provider, Model: model.ID,
		ProviderThinkingLevel: level, StopReason: ai.StopStop, Timestamp: 1,
	}
}

// effortCapture is the suite's capture with thinkingEnabled false: the request
// body and the reply, which records the effort it ran at.
func effortCapture(t *testing.T, model *ai.Model, messages ...ai.Message) (map[string]any, *ai.AssistantMessage) {
	t.Helper()
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, anthropicSSE)
	}))
	defer server.Close()
	model.BaseURL = server.URL
	opts := apiKeyOptions("test-key")
	opts.CacheRetention = ai.CacheNone
	opts.ThinkingProvided = true
	opts.ThinkingEnabled = false
	message := StreamAnthropic(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: messages}), opts).Result()
	if body == nil {
		t.Fatalf("no request (stream ended %s: %q)", message.StopReason, message.ErrorMessage)
	}
	return body, message
}

// effortSystemMessages is the suite's effortMessages.
func effortSystemMessages(body map[string]any) []any {
	out := []any{}
	for _, m := range body["messages"].([]any) {
		if m.(map[string]any)["role"] == "system" {
			out = append(out, m)
		}
	}
	return out
}

func TestAnthropicOffSendsPlainDisabledThinkingWithoutEffortWhenSupported(t *testing.T) {
	model := effortManagedModel()
	delete(model.ThinkingLevelMap, "off")
	model.ThinkingLevelMap["minimal"] = nil
	// Anthropic rejects block_binding with disabled thinking, and rejects effort
	// markers that change level (here low, then medium) while thinking is
	// disabled.
	body, message := effortCapture(t, model,
		ai.NewUserText("one", 1), effortAssistant(model, "low"),
		ai.NewUserText("two", 2), effortAssistant(model, "medium"),
		ai.NewUserText("three", 3))
	if want := map[string]any{"type": "disabled"}; !reflect.DeepEqual(body["thinking"], want) {
		t.Fatalf("thinking = %#v, want %#v", body["thinking"], want)
	}
	if got, present := body["output_config"]; present {
		t.Fatalf("output_config = %#v, want none", got)
	}
	if got := effortSystemMessages(body); len(got) != 0 {
		t.Fatalf("effort messages = %#v, want none", got)
	}
	if message.ProviderThinkingLevel != "" {
		t.Fatalf("providerThinkingLevel = %q, want none", message.ProviderThinkingLevel)
	}
}

func TestAnthropicOffKeepsAdaptiveThinkingWhenUnsupported(t *testing.T) {
	body, message := effortCapture(t, effortManagedModel(), ai.NewUserText("one", 1))
	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Fatalf("thinking = %#v, want adaptive", body["thinking"])
	}
	want := []any{map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"effort": "high"}}}
	if got := effortSystemMessages(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("effort messages = %#v, want %#v", got, want)
	}
	if message.ProviderThinkingLevel != "high" {
		t.Fatalf("providerThinkingLevel = %q, want high", message.ProviderThinkingLevel)
	}
}

// 'supports off only on managed models that accept disabled thinking'.
func TestAnthropicCatalogSupportsOffOnlyWhereDisabledThinkingIsAccepted(t *testing.T) {
	for _, id := range []string{"claude-opus-5", "claude-haiku-5-5"} {
		model := ai.GetModel("anthropic", id)
		if model == nil {
			t.Fatalf("catalog has no anthropic/%s", id)
		}
		var compat struct{ SupportsMidConvoEffort bool }
		if err := json.Unmarshal(model.Compat, &compat); err != nil || !compat.SupportsMidConvoEffort {
			t.Errorf("%s compat.supportsMidConvoEffort = %t (%v), want true", id, compat.SupportsMidConvoEffort, err)
		}
		if off, present := model.ThinkingLevelMap["off"]; present && off == nil {
			t.Errorf("%s thinkingLevelMap.off is null, want it not null", id)
		}
	}
	for _, id := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"} {
		model := ai.GetModel("anthropic", id)
		if model == nil {
			t.Fatalf("catalog has no anthropic/%s", id)
		}
		if off, present := model.ThinkingLevelMap["off"]; !present || off != nil {
			t.Errorf("%s thinkingLevelMap.off = %v (present %t), want null", id, off, present)
		}
	}
}
