package coding

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// DefaultModelSpec is the model used when none is specified. Deliberate
// divergence from pi: pi's default model comes from settings.json /
// findInitialModel (first available provider default); the Go port has no
// settings manager, so an empty spec resolves to this fixed default.
const DefaultModelSpec = "anthropic/claude-sonnet-4-5"

// defaultModelPerProvider ports pi's defaultModelPerProvider
// (model-resolver.ts): the per-provider default model id used by
// buildFallbackModel when synthesizing a custom-id model.
var defaultModelPerProvider = map[string]string{
	"amazon-bedrock":             "us.anthropic.claude-opus-4-6-v1",
	"ant-ling":                   "Ring-2.6-1T",
	"anthropic":                  "claude-opus-4-8",
	"openai":                     "gpt-6.1-sol",
	"azure":                      "gpt-5.4",
	"openai-codex":               "gpt-6.1-sol",
	"radius":                     "balanced",
	"nvidia":                     "nvidia/nemotron-3.5-super-vl-120b-a12b",
	"deepseek":                   "deepseek-v4-pro",
	"google":                     "gemini-3.1-pro-preview",
	"google-vertex":              "gemini-3.1-pro-preview",
	"github-copilot":             "gpt-6.1-sol",
	"openrouter":                 "moonshotai/kimi-k2.6",
	"vercel-ai-gateway":          "zai/glm-5.1",
	"xai":                        "grok-4.7",
	"groq":                       "openai/gpt-oss-120b",
	"cerebras":                   "gpt-oss-120b",
	"zai":                        "glm-5.3",
	"zai-coding-cn":              "glm-5.3",
	"mistral":                    "devstral-medium-latest",
	"minimax":                    "MiniMax-M2.7",
	"minimax-cn":                 "MiniMax-M2.7",
	"moonshotai":                 "kimi-k2.6",
	"moonshotai-cn":              "kimi-k2.6",
	"huggingface":                "moonshotai/Kimi-K2.6",
	"fireworks":                  "accounts/fireworks/models/kimi-k3",
	"together":                   "moonshotai/Kimi-K3",
	"baseten":                    "zai-org/GLM-5.2",
	"opencode":                   "kimi-k2.6",
	"opencode-go":                "kimi-k3",
	"kimi-coding":                "kimi-for-coding",
	"meta":                       "muse-spark-1.3",
	"cloudflare-workers-ai":      "@cf/moonshotai/kimi-k2.6",
	"cloudflare-ai-gateway":      "workers-ai/@cf/moonshotai/kimi-k2.6",
	"qwen-token-plan":            "qwen3.7-max",
	"qwen-token-plan-cn":         "qwen3.7-max",
	"qwen-token-plan-individual": "qwen3.8-max",
	"xiaomi":                     "mimo-v2.6-pro",
	"xiaomi-token-plan-cn":       "mimo-v2.6-pro",
	"xiaomi-token-plan-ams":      "mimo-v2.6-pro",
	"xiaomi-token-plan-sgp":      "mimo-v2.6-pro",
}

// validThinkingLevels mirrors pi's VALID_THINKING_LEVELS (cli/args.ts). "max"
// arrived upstream with fbdd46389; the port took that commit's ai-side half
// (ai.ThinkingMax) and missed this list, so every ":max" spec fell through to
// the custom-id fallback with the suffix still glued to the model id.
var validThinkingLevels = map[string]bool{
	"off": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// ResolvedModel is the result of resolving a model spec.
type ResolvedModel struct {
	Model *ai.Model
	// ThinkingLevel is the level parsed from a ":<level>" suffix in the spec
	// (pi parseModelPattern), or "" when the spec carried none.
	ThinkingLevel string
	// Warning carries pi's non-fatal resolution warnings (e.g. the custom-id
	// fallback for an unknown model under a known provider).
	Warning string
}

// ResolveModel resolves a model spec to a Model from the catalog (an empty
// spec resolves to DefaultModelSpec). Kept for source compatibility; the
// parsed thinking level and warnings are available via ResolveModelPattern.
func ResolveModel(spec string) (*ai.Model, error) {
	r, err := ResolveModelPattern(spec)
	return r.Model, err
}

// ResolveModelPattern is ResolveCliModel with the spec as --model, no
// --provider or --thinking, and a provider counted as authenticated when the
// environment holds its API key (EnvHasAuth), as pi's CLI counts an
// environment key in hasConfiguredAuth. A reference that matches several
// providers equally is an error rather than a pick by catalog order (pi
// d7f7e0df, #10160); a host that keeps keys elsewhere calls ResolveCliModel
// with its own HasAuth. An empty spec resolves to DefaultModelSpec.
func ResolveModelPattern(spec string) (ResolvedModel, error) {
	if spec == "" {
		spec = DefaultModelSpec
	}
	return ResolveCliModel(CliModelOptions{Model: spec, HasAuth: EnvHasAuth})
}

// EnvHasAuth reports whether the process environment holds an API key for
// the provider (ai.GetEnvApiKey).
func EnvHasAuth(provider string) bool { return ai.GetEnvApiKey(provider, nil) != "" }

// CliModelOptions are pi resolveCliModel's inputs.
type CliModelOptions struct {
	// Provider is --provider: only its models are considered. Empty means any.
	Provider string
	// Model is --model: a bare id or provider/id, optionally followed by
	// ":<thinking>".
	Model string
	// Thinking is --thinking. When set, a ":<suffix>" on a custom model id
	// stays part of the id.
	Thinking string
	// InlineAPIKey is whether --api-key was given for the selected provider;
	// stored credentials then do not choose among providers.
	InlineAPIKey bool
	// HasAuth reports whether credentials are configured for a provider (pi
	// modelRuntime.hasConfiguredAuth). It only breaks ties between providers;
	// nil counts none.
	HasAuth func(provider string) bool
}

// ResolveCliModel ports pi's resolveCliModel (model-resolver.ts, d7f7e0df):
// every candidate is collected before a provider is chosen, so catalog order
// never decides the provider, a ":<thinking>" suffix cannot change it, and an
// ambiguous exact match is an error instead of falling through to a fuzzy
// match. A provider/id reference prefers the named provider; a gateway that
// exposes provider/id as a raw model id serves it when the named provider has
// no match, or lacks credentials while the gateway has them. An unknown model
// under a known provider becomes a custom model id cloned from the provider's
// default (pi buildFallbackModel), with a warning. An empty Model resolves
// nothing.
func ResolveCliModel(opts CliModelOptions) (ResolvedModel, error) {
	// All models, not only those with credentials, so --api-key can set up a
	// provider for the first time.
	return resolveCliModel(opts, allCatalogModels())
}

// resolveCliModel is ResolveCliModel over models.
func resolveCliModel(opts CliModelOptions, models []*ai.Model) (ResolvedModel, error) {
	if opts.Model == "" {
		return ResolvedModel{}, nil
	}
	if len(models) == 0 {
		return ResolvedModel{}, fmt.Errorf("No models available. Check your installation or add models to models.json.")
	}
	provider := ""
	if opts.Provider != "" {
		for _, m := range models {
			if strings.EqualFold(string(m.Provider), opts.Provider) {
				provider = string(m.Provider)
				break
			}
		}
		if provider == "" {
			return ResolvedModel{}, fmt.Errorf("Unknown provider %q. Use --list-models to see available providers/models.", opts.Provider)
		}
	}
	// An inline key goes to whichever provider is selected, so stored
	// credentials must not choose it.
	usable := func(string) bool { return false }
	if !opts.InlineAPIKey && opts.HasAuth != nil {
		usable = opts.HasAuth
	}
	result := resolveModelReference(modelReferenceRequest{text: opts.Model, provider: provider, thinkingSuffix: thinkingSuffixStrict, fuzzy: true}, models, usable)
	switch result.status {
	case referenceResolved:
		return ResolvedModel{Model: result.model, ThinkingLevel: result.thinkingLevel, Warning: result.warning}, nil
	case referenceAmbiguous:
		matches := make([]string, 0, len(result.candidates))
		for _, m := range result.candidates {
			matches = append(matches, string(m.Provider)+"/"+m.ID)
		}
		slices.SortFunc(matches, newLocaleCompare())
		authHint := "More than one matching provider is authenticated."
		switch {
		case opts.InlineAPIKey:
			authHint = "--api-key does not choose a provider."
		case result.usableCount == 0:
			authHint = "No matching provider is authenticated."
		}
		return ResolvedModel{}, fmt.Errorf("Model %q is ambiguous across providers: %s. %s Use --provider or provider/model.", opts.Model, strings.Join(matches, ", "), authHint)
	}
	target := result.target
	if target == nil {
		return ResolvedModel{}, fmt.Errorf("Model %q not found. Use --list-models to see available models.", opts.Model)
	}
	// The thinking suffix applies only when --thinking is not given;
	// otherwise it stays part of the custom model id. e.g.
	// "zai-org/GLM-5.1-FP8:high" → id "zai-org/GLM-5.1-FP8", level "high".
	modelID, fallbackThinking := target.modelID, target.thinkingLevel
	if opts.Thinking != "" {
		modelID, fallbackThinking = target.modelText, ""
	}
	fallback := buildFallbackModel(target.providerModels, modelID)
	requested := opts.Thinking
	if requested == "" {
		requested = fallbackThinking
	}
	// pi 1fc80f4f (#5552): a requested level keeps a template without
	// reasoning from dropping it. The clone is fresh, so no catalog entry
	// changes.
	if requested != "" && requested != "off" {
		fallback.Reasoning = true
	}
	return ResolvedModel{Model: fallback, ThinkingLevel: fallbackThinking,
		Warning: fmt.Sprintf("Model %q not found for provider %q. Using custom model id.", modelID, target.provider)}, nil
}

// allCatalogModels lists every registered model in deterministic order
// (providers and ids sorted; pi's registry order is models.json order). The
// resolution does not depend on it, except which of a provider's models a
// custom id is cloned from when the provider has no default.
func allCatalogModels() []*ai.Model {
	providers := ai.GetProviders()
	sort.Strings(providers)
	var out []*ai.Model
	for _, p := range providers {
		models := ai.GetModels(p)
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
		out = append(out, models...)
	}
	return out
}

// thinkingSuffix is pi ModelReferenceRequest.thinkingSuffix: how a trailing
// ":<suffix>" is read when the full text matches no model, since ids such as
// OpenRouter's qwen/qwen3-coder:exacto contain colons.
type thinkingSuffix int

const (
	// thinkingSuffixNone keeps the suffix part of the id.
	thinkingSuffixNone thinkingSuffix = iota
	// thinkingSuffixStrict parses a valid thinking level; any other suffix
	// stays part of the id.
	thinkingSuffixStrict
	// thinkingSuffixLenient is strict, but drops an invalid suffix with a
	// warning.
	thinkingSuffixLenient
)

// modelReferenceRequest is pi's ModelReferenceRequest.
type modelReferenceRequest struct {
	// text is a bare model id or provider/id, optionally followed by
	// ":<thinking>".
	text string
	// provider, when set, limits the candidates to its models.
	provider       string
	thinkingSuffix thinkingSuffix
	// fuzzy matches id and name substrings when no exact match exists.
	fuzzy bool
}

type referenceStatus int

const (
	referenceNotFound referenceStatus = iota
	referenceResolved
	referenceAmbiguous
)

// modelReferenceResult is pi's ModelReferenceResult.
type modelReferenceResult struct {
	status        referenceStatus
	model         *ai.Model   // resolved
	candidates    []*ai.Model // ambiguous
	usableCount   int         // ambiguous
	thinkingLevel string      // resolved, ambiguous
	warning       string      // resolved, ambiguous
	// target is set on not-found when a provider is explicit or named by a
	// provider/ prefix, for building a custom model id.
	target *modelReferenceTarget
}

// modelReferenceTarget is pi's ModelReferenceTarget.
type modelReferenceTarget struct {
	provider string
	// providerModels are the provider's models, the anchor first.
	providerModels []*ai.Model
	// modelText is the text after the provider prefix as typed, including any
	// ":<suffix>".
	modelText string
	// modelID is modelText without a valid thinking suffix (unless the
	// request's thinkingSuffix is none).
	modelID       string
	thinkingLevel string
}

// splitThinkingSuffix is pi's: text up to its last colon and the valid
// thinking level after it.
func splitThinkingSuffix(text string) (base, level string, ok bool) {
	colon := strings.LastIndex(text, ":")
	if colon == -1 || !validThinkingLevels[text[colon+1:]] {
		return "", "", false
	}
	return text[:colon], text[colon+1:], true
}

// splitProviderPrefix is pi's (984cf664): the catalog provider whose id,
// followed by "/", starts the text, the longest one winning, since provider
// ids may contain "/" themselves (URL-derived ids such as
// llama=http://host:10800).
func splitProviderPrefix(text string, models []*ai.Model) (provider, modelText string, ok bool) {
	// pi lowercases the text and finds the first "/" at or after the provider
	// id's length; the offsets here are the original text's, so a rune whose
	// lowercase has another UTF-8 length cannot shift the split.
	for _, m := range models {
		prefix := jstext.Trim(string(m.Provider))
		if prefix == "" || (ok && len(provider) >= len(m.Provider)) || len(prefix) > len(text) {
			continue
		}
		slash := strings.Index(text[len(prefix):], "/")
		if slash == -1 {
			continue
		}
		slash += len(prefix)
		if !strings.EqualFold(jstext.Trim(text[:slash]), prefix) {
			continue
		}
		if rest := jstext.Trim(text[slash+1:]); rest != "" {
			provider, modelText, ok = string(m.Provider), rest, true
		}
	}
	return provider, modelText, ok
}

// resolveModelReference is pi's: usable only breaks ties between providers,
// so a unique match resolves even without credentials (--api-key first-time
// setup).
func resolveModelReference(request modelReferenceRequest, models []*ai.Model, usable func(string) bool) modelReferenceResult {
	text := jstext.Trim(request.text)
	if text == "" {
		return modelReferenceResult{}
	}
	pool := models
	if request.provider != "" {
		pool = nil
		for _, m := range models {
			if strings.EqualFold(string(m.Provider), request.provider) {
				pool = append(pool, m)
			}
		}
	}
	result := resolveText(text, request, pool, usable)
	if result.status != referenceNotFound {
		return result
	}
	splitProvider, splitText, split := splitProviderPrefix(text, pool)
	var anchor *ai.Model
	if request.provider != "" {
		if len(pool) > 0 {
			anchor = pool[0]
		}
	} else if split {
		for _, m := range pool {
			if string(m.Provider) == splitProvider {
				anchor = m
				break
			}
		}
	}
	if anchor == nil {
		return result
	}
	modelText := text
	if split {
		modelText = splitText
	}
	target := &modelReferenceTarget{provider: string(anchor.Provider), providerModels: []*ai.Model{anchor}, modelText: modelText, modelID: modelText}
	for _, m := range pool {
		if m.Provider == anchor.Provider && m != anchor {
			target.providerModels = append(target.providerModels, m)
		}
	}
	if request.thinkingSuffix != thinkingSuffixNone {
		if base, level, ok := splitThinkingSuffix(modelText); ok {
			target.modelID, target.thinkingLevel = base, level
		}
	}
	return modelReferenceResult{target: target}
}

// findExactModelReferenceMatch is pi's: an exact provider/id or bare id
// lookup, without thinking suffixes or fuzzy matching.
func findExactModelReferenceMatch(reference string, models []*ai.Model) *ai.Model {
	result := resolveModelReference(modelReferenceRequest{text: reference, thinkingSuffix: thinkingSuffixNone}, models, func(string) bool { return true })
	if result.status == referenceResolved {
		return result.model
	}
	return nil
}

func resolveText(text string, request modelReferenceRequest, pool []*ai.Model, usable func(string) bool) modelReferenceResult {
	if reference, ok := selectReference(text, pool, request.fuzzy, usable); ok {
		return reference
	}
	if request.fuzzy {
		if fuzzy, ok := selectFuzzy(fuzzyMatches(text, pool), usable); ok {
			return fuzzy
		}
	}
	if request.thinkingSuffix == thinkingSuffixNone {
		return modelReferenceResult{}
	}
	if base, level, ok := splitThinkingSuffix(text); ok {
		inner := resolveText(base, request, pool, usable)
		if inner.status == referenceNotFound {
			return inner
		}
		// Only use this thinking level if no warning from inner recursion.
		inner.thinkingLevel = level
		if inner.warning != "" {
			inner.thinkingLevel = ""
		}
		return inner
	}
	// Keep an invalid suffix as part of the id and fail, rather than resolve a
	// different model.
	if request.thinkingSuffix == thinkingSuffixStrict {
		return modelReferenceResult{}
	}
	colon := strings.LastIndex(text, ":")
	if colon == -1 {
		return modelReferenceResult{}
	}
	inner := resolveText(text[:colon], request, pool, usable)
	if inner.status == referenceNotFound {
		return inner
	}
	inner.thinkingLevel = ""
	inner.warning = fmt.Sprintf("Invalid thinking level %q in pattern %q. Using default instead.", text[colon+1:], text)
	return inner
}

// selectReference is pi's: provider/rest is only a hint, since the whole text
// may also be a model id on another provider, such as gateway id zai/glm-5.
// The named provider wins when usable, since that is what the syntax usually
// means; otherwise a sole usable raw match wins, since the named provider
// could not serve the request. An unusable named match is still preferred
// over unusable raw matches, so --api-key can set up the named provider.
func selectReference(text string, pool []*ai.Model, fuzzy bool, usable func(string) bool) (modelReferenceResult, bool) {
	lowerText := strings.ToLower(text)
	var raw []*ai.Model
	for _, m := range pool {
		if strings.ToLower(m.ID) == lowerText {
			raw = append(raw, m)
		}
	}
	var named *ai.Model
	if splitProvider, splitText, ok := splitProviderPrefix(text, pool); ok {
		var providerModels []*ai.Model
		for _, m := range pool {
			if string(m.Provider) == splitProvider {
				providerModels = append(providerModels, m)
			}
		}
		lowerModelText := strings.ToLower(splitText)
		// The provider's own literal id (OpenRouter's openrouter/free) beats
		// its fuzzy matches.
		for _, m := range providerModels {
			if strings.ToLower(m.ID) == lowerModelText {
				named = m
				break
			}
		}
		if named == nil {
			for _, m := range raw {
				if string(m.Provider) == splitProvider {
					named = m
					break
				}
			}
		}
		if named == nil && fuzzy {
			named = bestFuzzyMatch(fuzzyMatches(splitText, providerModels))
		}
	}
	var others []*ai.Model
	for _, m := range raw {
		if m != named {
			others = append(others, m)
		}
	}
	candidates := others
	if named != nil {
		candidates = append([]*ai.Model{named}, others...)
	}
	if len(candidates) == 0 {
		return modelReferenceResult{}, false
	}
	if providerCount(candidates) == 1 {
		return modelReferenceResult{status: referenceResolved, model: candidates[0]}, true
	}
	if named != nil && usable(string(named.Provider)) {
		return modelReferenceResult{status: referenceResolved, model: named}, true
	}
	var usableOthers []*ai.Model
	for _, m := range others {
		if usable(string(m.Provider)) {
			usableOthers = append(usableOthers, m)
		}
	}
	if len(usableOthers) == 1 {
		return modelReferenceResult{status: referenceResolved, model: usableOthers[0]}, true
	}
	if len(usableOthers) == 0 && named != nil {
		return modelReferenceResult{status: referenceResolved, model: named}, true
	}
	return modelReferenceResult{status: referenceAmbiguous, candidates: candidates, usableCount: len(usableOthers)}, true
}

func providerCount(models []*ai.Model) int {
	providers := map[ai.ProviderId]bool{}
	for _, m := range models {
		providers[m.Provider] = true
	}
	return len(providers)
}

// fuzzyMatches are the models whose id or name contains text,
// case-insensitively.
func fuzzyMatches(text string, models []*ai.Model) []*ai.Model {
	lowerText := strings.ToLower(text)
	var out []*ai.Model
	for _, m := range models {
		if strings.Contains(strings.ToLower(m.ID), lowerText) || strings.Contains(strings.ToLower(m.Name), lowerText) {
			out = append(out, m)
		}
	}
	return out
}

// selectFuzzy is pi's: across several providers the usable ones are
// preferred, and the same id on several providers is ambiguous rather than
// decided by catalog order.
func selectFuzzy(matches []*ai.Model, usable func(string) bool) (modelReferenceResult, bool) {
	if len(matches) == 0 {
		return modelReferenceResult{}, false
	}
	var usableMatches []*ai.Model
	if providerCount(matches) > 1 {
		for _, m := range matches {
			if usable(string(m.Provider)) {
				usableMatches = append(usableMatches, m)
			}
		}
	}
	pool := matches
	if len(usableMatches) > 0 {
		pool = usableMatches
	}
	best := bestFuzzyMatch(pool)
	if best == nil {
		return modelReferenceResult{}, false
	}
	var tied []*ai.Model
	for _, m := range pool {
		if m.ID == best.ID {
			tied = append(tied, m)
		}
	}
	if providerCount(tied) == 1 {
		return modelReferenceResult{status: referenceResolved, model: best}, true
	}
	usableCount := 0
	if len(usableMatches) > 0 {
		usableCount = len(tied)
	}
	return modelReferenceResult{status: referenceAmbiguous, candidates: tied, usableCount: usableCount}, true
}

// bestFuzzyMatch is pi's: aliases (claude-sonnet-4-5) over dated versions
// (claude-sonnet-4-5-20250929), then the latest — the first by
// b.id.localeCompare(a.id).
func bestFuzzyMatch(matches []*ai.Model) *ai.Model {
	var aliases []*ai.Model
	for _, m := range matches {
		if isModelAlias(m.ID) {
			aliases = append(aliases, m)
		}
	}
	pool := matches
	if len(aliases) > 0 {
		pool = aliases
	}
	if len(pool) == 0 {
		return nil
	}
	compare := newLocaleCompare()
	sorted := slices.Clone(pool)
	slices.SortStableFunc(sorted, func(a, b *ai.Model) int { return compare(b.ID, a.ID) })
	return sorted[0]
}

// newLocaleCompare approximates JavaScript's localeCompare (ICU root
// collation), as the codemode tool's description does. A Collator is not safe
// for concurrent use, so each sort makes its own.
func newLocaleCompare() func(a, b string) int { return collate.New(language.Und).CompareString }

var modelDatePattern = regexp.MustCompile(`-\d{8}$`)

// isModelAlias ports pi's isAlias: -latest ids and ids without a -YYYYMMDD
// date suffix are aliases.
func isModelAlias(id string) bool {
	if strings.HasSuffix(id, "-latest") {
		return true
	}
	return !modelDatePattern.MatchString(id)
}

// buildFallbackModel ports pi's buildFallbackModel: clone the provider's
// default model, or its first, with the requested custom id. providerModels
// are one provider's models, never empty.
func buildFallbackModel(providerModels []*ai.Model, modelID string) *ai.Model {
	base := providerModels[0]
	if defaultID, ok := defaultModelPerProvider[string(base.Provider)]; ok {
		for _, m := range providerModels {
			if m.ID == defaultID {
				base = m
				break
			}
		}
	}
	clone := *base
	clone.ID = modelID
	clone.Name = modelID
	return &clone
}
