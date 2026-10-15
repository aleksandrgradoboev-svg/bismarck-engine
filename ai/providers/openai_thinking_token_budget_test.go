package providers

// Mirrors pi's packages/ai/test/openai-completions-thinking-token-budget.test.ts
// (upstream d07889da0, generalized in b23741269): a reasoning model gets its
// thinking budget on the compat-selected top-level field, or inside
// chat_template_kwargs / chat_template_args via {"$var": "thinking.budget"},
// clamped so at least minAnswerTokens remain for the answer.

import (
	"encoding/json"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// budgetModel is the upstream test's vLLM-served reasoning model: reasoning and
// the answer share max_tokens.
func budgetModel(overrides func(*ai.Model)) *ai.Model {
	m := &ai.Model{
		ID: "zai-org/glm-5.2", Name: "GLM 5.2 (local vLLM)", Api: ai.APIOpenAICompletions,
		Provider: "local-vllm", BaseURL: "http://localhost:8000/v1", Reasoning: true,
		Input: []string{"text"}, ContextWindow: 262144, MaxTokens: 16384,
		Compat: json.RawMessage(`{"thinkingFormat":"zai","supportsThinkingTokenBudget":true}`),
	}
	if overrides != nil {
		overrides(m)
	}
	return m
}

func captureBudgetBody(t *testing.T, model *ai.Model, reasoning ai.ThinkingLevel, budgets *ai.ThinkingBudgets, maxTokens *int) map[string]any {
	t.Helper()
	return capturedSimplePayload(t, StreamSimpleOpenAICompletions, model, ai.SimpleStreamOptions{
		StreamOptions:   ai.StreamOptions{MaxTokens: maxTokens},
		Reasoning:       reasoning,
		ThinkingBudgets: budgets,
	})
}

// "sends the configured budget for the requested level"
func TestOpenAIThinkingTokenBudgetConfiguredLevel(t *testing.T) {
	body := captureBudgetBody(t, budgetModel(nil), ai.ThinkingMedium, &ai.ThinkingBudgets{Medium: intp(4096)}, nil)
	if body["thinking_token_budget"] != 4096 {
		t.Fatalf("thinking_token_budget = %v, want 4096", body["thinking_token_budget"])
	}
}

// "omits the budget when neither the field nor the alias is set"
func TestOpenAIThinkingTokenBudgetFlagOff(t *testing.T) {
	model := budgetModel(func(m *ai.Model) {
		m.Compat = json.RawMessage(`{"thinkingFormat":"zai"}`)
	})
	body := captureBudgetBody(t, model, ai.ThinkingMedium, &ai.ThinkingBudgets{Medium: intp(4096)}, nil)
	for _, field := range []string{"thinking_token_budget", "thinking_budget", "thinking_budget_tokens"} {
		if has(body, field) {
			t.Fatalf("neither field nor alias set must omit %s, got %v", field, body[field])
		}
	}
}

// "omits the budget when thinking is off"
func TestOpenAIThinkingTokenBudgetThinkingOff(t *testing.T) {
	body := captureBudgetBody(t, budgetModel(nil), "", &ai.ThinkingBudgets{High: intp(8192)}, nil)
	if has(body, "thinking_token_budget") {
		t.Fatalf("no reasoning must omit thinking_token_budget, got %v", body["thinking_token_budget"])
	}
}

// "clamps xhigh and max to the high budget"
func TestOpenAIThinkingTokenBudgetXHighMaxClampToHigh(t *testing.T) {
	// On the plain model clampThinkingLevel already collapses xhigh/max to high
	// (extended levels are opt-in), matching the upstream test.
	for _, level := range []ai.ThinkingLevel{ai.ThinkingXHigh, ai.ThinkingMax} {
		body := captureBudgetBody(t, budgetModel(nil), level, &ai.ThinkingBudgets{High: intp(8192)}, nil)
		if body["thinking_token_budget"] != 8192 {
			t.Fatalf("%s: thinking_token_budget = %v, want 8192", level, body["thinking_token_budget"])
		}
	}
	// A model that exposes xhigh/max natively reaches the budget block with the
	// raw level; the block's own clampReasoning must still pick high's budget.
	extended := budgetModel(func(m *ai.Model) {
		m.ThinkingLevelMap = ai.ThinkingLevelMap{"xhigh": strPtr("xhigh"), "max": strPtr("max")}
	})
	for _, level := range []ai.ThinkingLevel{ai.ThinkingXHigh, ai.ThinkingMax} {
		body := captureBudgetBody(t, extended, level, &ai.ThinkingBudgets{High: intp(8192)}, nil)
		if body["thinking_token_budget"] != 8192 {
			t.Fatalf("native %s: thinking_token_budget = %v, want 8192", level, body["thinking_token_budget"])
		}
	}
}

// Default budgets per level (minimal 1024, low 2048, medium 8192, high 16384);
// a 32768 ceiling leaves them all unclamped.
func TestOpenAIThinkingTokenBudgetDefaults(t *testing.T) {
	for level, want := range map[ai.ThinkingLevel]int{
		ai.ThinkingMinimal: 1024, ai.ThinkingLow: 2048, ai.ThinkingMedium: 8192, ai.ThinkingHigh: 16384,
	} {
		body := captureBudgetBody(t, budgetModel(nil), level, nil, intp(32768))
		if body["thinking_token_budget"] != want {
			t.Fatalf("%s: thinking_token_budget = %v, want %d", level, body["thinking_token_budget"], want)
		}
	}
}

// "leaves room for the answer when the budget meets the response ceiling"
func TestOpenAIThinkingTokenBudgetLeavesRoomForAnswer(t *testing.T) {
	// Default high budget (16384) equals the model ceiling, which would leave no answer.
	body := captureBudgetBody(t, budgetModel(nil), ai.ThinkingHigh, nil, nil)
	if body["thinking_token_budget"] != 16384-1024 {
		t.Fatalf("thinking_token_budget = %v, want %d", body["thinking_token_budget"], 16384-1024)
	}
}

// "uses the caller max_tokens as the ceiling when it is lower than the model cap"
func TestOpenAIThinkingTokenBudgetCallerMaxTokensCeiling(t *testing.T) {
	body := captureBudgetBody(t, budgetModel(nil), ai.ThinkingHigh, &ai.ThinkingBudgets{High: intp(8192)}, intp(4096))
	if body["thinking_token_budget"] != 4096-1024 {
		t.Fatalf("thinking_token_budget = %v, want %d", body["thinking_token_budget"], 4096-1024)
	}
}

// The ceiling read covers both max-token field names (pi reads
// params.max_tokens ?? params.max_completion_tokens ?? model.maxTokens).
func TestOpenAIThinkingTokenBudgetMaxTokensFieldCeiling(t *testing.T) {
	model := budgetModel(func(m *ai.Model) {
		m.Compat = json.RawMessage(`{"thinkingFormat":"zai","supportsThinkingTokenBudget":true,"maxTokensField":"max_tokens"}`)
	})
	body := captureBudgetBody(t, model, ai.ThinkingHigh, nil, intp(4096))
	if !has(body, "max_tokens") || has(body, "max_completion_tokens") {
		t.Fatalf("expected max_tokens field, got max_tokens=%v max_completion_tokens=%v",
			body["max_tokens"], body["max_completion_tokens"])
	}
	if body["thinking_token_budget"] != 4096-1024 {
		t.Fatalf("thinking_token_budget = %v, want %d", body["thinking_token_budget"], 4096-1024)
	}
}

// A budget that would leave no answer room (ceiling <= minAnswerTokens) is
// omitted entirely, never sent as 0.
func TestOpenAIThinkingTokenBudgetNonPositiveOmitted(t *testing.T) {
	for _, mt := range []int{1024, 512} {
		body := captureBudgetBody(t, budgetModel(nil), ai.ThinkingHigh, nil, intp(mt))
		if has(body, "thinking_token_budget") {
			t.Fatalf("maxTokens %d: thinking_token_budget must be omitted, got %v", mt, body["thinking_token_budget"])
		}
	}
}

// The block also gates on model.reasoning: a native-options caller can set an
// effort on a non-reasoning model (streamSimple clamps it to off before here).
func TestOpenAIThinkingTokenBudgetNonReasoningModel(t *testing.T) {
	model := budgetModel(func(m *ai.Model) { m.Reasoning = false })
	body := mustBuildOpenAIParams(t, model, baseReq(), &OpenAIOptions{ReasoningEffort: "high"})
	if has(body, "thinking_token_budget") {
		t.Fatalf("non-reasoning model must omit thinking_token_budget, got %v", body["thinking_token_budget"])
	}
}

// ---- Upstream b23741269: compat-selected budget field + thinking.budget $var ----

// "sends %s when thinkingTokenBudgetField is set"
func TestOpenAIThinkingTokenBudgetFieldNames(t *testing.T) {
	for _, field := range []string{"thinking_budget", "thinking_budget_tokens"} {
		t.Run(field, func(t *testing.T) {
			model := budgetModel(func(m *ai.Model) {
				m.Compat = json.RawMessage(`{"thinkingFormat":"qwen","thinkingTokenBudgetField":"` + field + `"}`)
			})
			body := captureBudgetBody(t, model, ai.ThinkingMedium, &ai.ThinkingBudgets{Medium: intp(4096)}, nil)
			if body[field] != 4096 {
				t.Fatalf("%s = %v, want 4096", field, body[field])
			}
			if has(body, "thinking_token_budget") {
				t.Fatalf("vLLM field must stay absent, got thinking_token_budget = %v", body["thinking_token_budget"])
			}
		})
	}
}

// "lets thinkingTokenBudgetField win over the boolean alias"
func TestOpenAIThinkingTokenBudgetFieldWinsOverAlias(t *testing.T) {
	model := budgetModel(func(m *ai.Model) {
		m.Compat = json.RawMessage(
			`{"thinkingFormat":"zai","supportsThinkingTokenBudget":true,"thinkingTokenBudgetField":"thinking_budget"}`)
	})
	body := captureBudgetBody(t, model, ai.ThinkingMedium, &ai.ThinkingBudgets{Medium: intp(4096)}, nil)
	if body["thinking_budget"] != 4096 {
		t.Fatalf("thinking_budget = %v, want 4096", body["thinking_budget"])
	}
	if has(body, "thinking_token_budget") {
		t.Fatalf("alias must not win: thinking_token_budget = %v", body["thinking_token_budget"])
	}
}

// chatTemplateBudgetModel is the upstream test's chat-template variant: the
// budget rides inside chat_template_kwargs, with no budget field configured.
func chatTemplateBudgetModel() *ai.Model {
	return budgetModel(func(m *ai.Model) {
		m.Compat = json.RawMessage(`{"thinkingFormat":"chat-template","chatTemplateKwargs":` +
			`{"enable_thinking":{"$var":"thinking.enabled"},"thinking_budget":{"$var":"thinking.budget"}}}`)
	})
}

// "puts the clamped budget in chat_template_kwargs when $var is thinking.budget".
// streamSimple sends the model cap (16384) as the max-token field, so the
// default high budget clamps to 16384-1024. The absent thinking_token_budget is
// what proves the $var path needs no configured budget field.
func TestOpenAIThinkingBudgetVarInChatTemplateKwargs(t *testing.T) {
	body := captureBudgetBody(t, chatTemplateBudgetModel(), ai.ThinkingHigh, nil, nil)
	if got, want := ctkParam(t, body), `{"enable_thinking":true,"thinking_budget":15360}`; got != want {
		t.Fatalf("chat_template_kwargs = %s, want %s", got, want)
	}
	if has(body, "thinking_token_budget") {
		t.Fatalf("$var budget must not imply a top-level field, got %v", body["thinking_token_budget"])
	}
}

// "omits thinking.budget from chat_template_kwargs when thinking is off": pi
// returns undefined, which drops the key entirely rather than sending 0.
func TestOpenAIThinkingBudgetVarOmittedWhenThinkingOff(t *testing.T) {
	body := captureBudgetBody(t, chatTemplateBudgetModel(), "", nil, nil)
	if got, want := ctkParam(t, body), `{"enable_thinking":false}`; got != want {
		t.Fatalf("chat_template_kwargs = %s, want %s", got, want)
	}
}

// No upstream twin: pi threads the budget into both buildChatTemplateValues call
// sites but only tests the chat-template one. The Go dispatch has the same two
// sites, so the baseten one needs its own lock.
func TestOpenAIThinkingBudgetVarInChatTemplateArgs(t *testing.T) {
	model := budgetModel(func(m *ai.Model) {
		m.Compat = json.RawMessage(
			`{"thinkingFormat":"baseten","chatTemplateArgs":{"thinking_budget":{"$var":"thinking.budget"}}}`)
	})
	body := captureBudgetBody(t, model, ai.ThinkingHigh, nil, nil)
	if got, want := basetenArgs(t, body), `{"thinking_budget":15360}`; got != want {
		t.Fatalf("chat_template_args = %s, want %s", got, want)
	}
}
