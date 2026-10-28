package providers

import (
	"math"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// isOpenRouterModel is pi's isOpenRouterModel (openrouter-cost.ts, cae8d69e).
func isOpenRouterModel(model *ai.Model) bool {
	return model.Provider == "openrouter" || strings.Contains(model.BaseURL, "openrouter.ai")
}

// isValidCost is `typeof value === "number" && Number.isFinite(value) && value >= 0`.
func isValidCost(value any) bool {
	n, ok := value.(float64)
	return ok && !math.IsInf(n, 0) && !math.IsNaN(n) && n >= 0
}

// applyOpenRouterReportedCost is pi's applyOpenRouterReportedCost: it replaces
// the catalog total with the cost OpenRouter billed, when the streamed usage
// (a DecodeOrderedValue / jsGet value) carries complete, valid accounting
// (pi cae8d69e, #10286). OpenRouter adds these fields at runtime, though
// neither SDK types them. Without a key of the user's own (is_byok false) the
// charge is the cost; with one (is_byok true) the upstream inference cost,
// reported or summed from its prompt and completion parts, is added to it.
func applyOpenRouterReportedCost(usage *ai.Usage, reported any) {
	cost := jsGet(reported, "cost")
	if !isValidCost(cost) {
		return
	}
	switch jsGet(reported, "is_byok") {
	case false:
		usage.Cost.Total = cost.(float64)
		usage.Cost.TotalFromProvider = ai.True
	case true:
		details := jsGet(reported, "cost_details")
		if !jsTruthy(details) {
			return
		}
		upstream := jsGet(details, "upstream_inference_cost")
		if upstream == nil {
			prompt := jsGet(details, "upstream_inference_prompt_cost")
			completions := jsGet(details, "upstream_inference_completions_cost")
			if !isValidCost(prompt) || !isValidCost(completions) {
				return
			}
			upstream = prompt.(float64) + completions.(float64)
		}
		if !isValidCost(upstream) {
			return
		}
		usage.Cost.Total = cost.(float64) + upstream.(float64)
		usage.Cost.TotalFromProvider = ai.True
	}
}
