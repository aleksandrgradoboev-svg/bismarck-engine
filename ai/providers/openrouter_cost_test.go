package providers

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// OpenRouter's billed cost replaces the catalog total when its accounting is
// complete (pi cae8d69e, #10286); the flag says which total it is (7c22a93d).
// The cases transliterate packages/ai/test/openrouter-cost.test.ts; the
// reported usage goes in as the JSON text a stream carries.

func openRouterUsage() ai.Usage {
	return ai.Usage{Input: 1, Output: 1, TotalTokens: 2, Cost: ai.CostBreakdown{Input: 0.25, Output: 0.75, Total: 1}}
}

func applyReported(t *testing.T, usage *ai.Usage, reported string) {
	t.Helper()
	value, err := ai.DecodeOrderedValue([]byte(reported))
	if err != nil {
		t.Fatal(err)
	}
	applyOpenRouterReportedCost(usage, value)
}

func TestIsOpenRouterModel(t *testing.T) {
	for _, tc := range []struct {
		provider, baseURL string
		want              bool
	}{
		{"openrouter", "https://example.com", true},
		{"openrouter-work", "https://openrouter.ai/api/v1", true},
		{"custom", "https://example.com/v1", false},
	} {
		if got := isOpenRouterModel(&ai.Model{Provider: tc.provider, BaseURL: tc.baseURL}); got != tc.want {
			t.Errorf("isOpenRouterModel(%s, %s) = %v, want %v", tc.provider, tc.baseURL, got, tc.want)
		}
	}
}

func TestApplyOpenRouterReportedCost(t *testing.T) {
	t.Run("non-BYOK uses the reported cost", func(t *testing.T) {
		u := openRouterUsage()
		applyReported(t, &u, `{"cost":0,"is_byok":false,"cost_details":{"upstream_inference_cost":0,"upstream_inference_prompt_cost":0,"upstream_inference_completions_cost":0}}`)
		if u.Cost.Total != 0 || u.Cost.TotalFromProvider != ai.True {
			t.Fatalf("cost = %+v, want total 0 from the provider", u.Cost)
		}
	})
	t.Run("BYOK adds the upstream inference cost", func(t *testing.T) {
		u := openRouterUsage()
		applyReported(t, &u, `{"cost":0.1,"is_byok":true,"cost_details":{"upstream_inference_cost":0.9,"upstream_inference_prompt_cost":0.6,"upstream_inference_completions_cost":0.3}}`)
		if u.Cost.Total != 1 || u.Cost.TotalFromProvider != ai.True {
			t.Fatalf("cost = %+v, want total 1 from the provider", u.Cost)
		}
	})
	t.Run("BYOK sums the upstream parts when their total is null", func(t *testing.T) {
		u := openRouterUsage()
		applyReported(t, &u, `{"cost":0.1,"is_byok":true,"cost_details":{"upstream_inference_cost":null,"upstream_inference_prompt_cost":0.6,"upstream_inference_completions_cost":0.3}}`)
		if math.Abs(u.Cost.Total-1) > 1e-9 || u.Cost.TotalFromProvider != ai.True {
			t.Fatalf("cost = %+v, want total ~1 from the provider", u.Cost)
		}
	})
	t.Run("incomplete accounting keeps the catalog total", func(t *testing.T) {
		u := openRouterUsage()
		u.Cost.TotalFromProvider = ai.False
		applyReported(t, &u, `{"cost":null,"is_byok":false,"cost_details":null}`)
		applyReported(t, &u, `{"cost":0.1,"is_byok":true,"cost_details":null}`)
		if u.Cost.Total != 1 || u.Cost.TotalFromProvider != ai.False {
			t.Fatalf("cost = %+v, want the catalog total", u.Cost)
		}
	})
	// Beyond pi's suite: isValidCost rejects a negative cost, so the catalog
	// total stays.
	t.Run("a negative cost keeps the catalog total", func(t *testing.T) {
		u := openRouterUsage()
		applyReported(t, &u, `{"cost":-0.1,"is_byok":false}`)
		applyReported(t, &u, `{"cost":0.1,"is_byok":true,"cost_details":{"upstream_inference_cost":-0.2}}`)
		if u.Cost.Total != 1 || u.Cost.TotalFromProvider != ai.Absent {
			t.Fatalf("cost = %+v, want the catalog total", u.Cost)
		}
	})
	// Beyond pi's suite: an absent upstream total is not null, so it does not
	// fall back to the parts.
	t.Run("an absent upstream total is not null", func(t *testing.T) {
		u := openRouterUsage()
		applyReported(t, &u, `{"cost":0.1,"is_byok":true,"cost_details":{"upstream_inference_prompt_cost":0.6,"upstream_inference_completions_cost":0.3}}`)
		if u.Cost.Total != 1 || u.Cost.TotalFromProvider != ai.Absent {
			t.Fatalf("cost = %+v, want the catalog total", u.Cost)
		}
	})
}

// CalculateCost marks its total as a catalog estimate, which serializes as an
// explicit false, as pi's does (7c22a93d); an unpriced usage leaves it out.
func TestCostTotalFromProviderSerialization(t *testing.T) {
	u := ai.Usage{Input: 1}
	raw, _ := json.Marshal(u.Cost)
	if string(raw) != `{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}` {
		t.Fatalf("unpriced = %s", raw)
	}
	ai.CalculateCost(&ai.Model{Cost: ai.ModelCost{Input: 1_000_000}}, &u)
	raw, _ = json.Marshal(u.Cost)
	if string(raw) != `{"input":1,"output":0,"cacheRead":0,"cacheWrite":0,"total":1,"totalFromProvider":false}` {
		t.Fatalf("priced = %s", raw)
	}
	u.Cost.TotalFromProvider = ai.True
	raw, _ = json.Marshal(u.Cost)
	var back ai.CostBreakdown
	if err := json.Unmarshal(raw, &back); err != nil || back != u.Cost {
		t.Fatalf("round trip = %+v (%v), want %+v", back, err, u.Cost)
	}
}

// The streamed usage carries OpenRouter's accounting on both transports pi
// reads it from: Chat Completions chunks and Anthropic message_delta.
func TestOpenRouterReportedCostReachesTheStreamedUsage(t *testing.T) {
	completions := `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":0.42,"is_byok":false}}

data: [DONE]

`
	got := runOpenAIStream(t, completions, func(m *ai.Model) { m.Provider = "openrouter"; m.Cost = ai.ModelCost{Input: 1, Output: 1} })
	if got.Usage.Cost.Total != 0.42 || got.Usage.Cost.TotalFromProvider != ai.True {
		t.Fatalf("completions cost = %+v, want total 0.42 from the provider", got.Usage.Cost)
	}
	plain := runOpenAIStream(t, completions, func(m *ai.Model) { m.Cost = ai.ModelCost{Input: 1, Output: 1} })
	if plain.Usage.Cost.TotalFromProvider != ai.False || plain.Usage.Cost.Total == 0.42 {
		t.Fatalf("non-OpenRouter cost = %+v, want the catalog estimate", plain.Usage.Cost)
	}

	anthropic := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":0}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5,"cost":0.1,"is_byok":true,"cost_details":{"upstream_inference_cost":0.2}}}

event: message_stop
data: {"type":"message_stop"}

`
	model := anthropicPlainModel()
	model.Provider = "openrouter"
	model.Cost = ai.ModelCost{Input: 1, Output: 1}
	result := streamAnthropicAgainst(t, model, anthropic)
	if math.Abs(result.Usage.Cost.Total-0.3) > 1e-12 || result.Usage.Cost.TotalFromProvider != ai.True {
		t.Fatalf("anthropic cost = %+v (%s %q), want total 0.3 from the provider", result.Usage.Cost, result.StopReason, result.ErrorMessage)
	}
}
