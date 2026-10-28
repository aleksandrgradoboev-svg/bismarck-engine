package coding

import (
	"slices"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// pi's model-resolver.test.ts at 984cf664: the resolveModelReference table
// (d7f7e0df, #10160, with 984cf664's provider id containing a slash) and the
// resolveCliModel cases, transliterated. Each reference case runs against the
// catalog in both orders: catalog order must never decide the result.

func referenceModel(provider, id string) *ai.Model {
	return &ai.Model{ID: id, Name: id, Api: "openai-completions", Provider: ai.ProviderId(provider), BaseURL: "https://example.invalid",
		Reasoning: true, Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 8192}
}

var referenceCatalog = []*ai.Model{
	referenceModel("anthropic", "claude-sonnet-4-5"),
	referenceModel("anthropic", "claude-sonnet-4-5-20250929"),
	referenceModel("anthropic", "claude-opus-4-8"),
	referenceModel("anthropic", "exact"),
	// Fuzzy-matches "shared"; an ambiguous exact "shared" must not fall through to it.
	referenceModel("anthropic", "shared-pro"),
	referenceModel("openai", "gpt-4o"),
	referenceModel("openrouter", "qwen/qwen3-coder:exacto"),
	referenceModel("openrouter", "openai/gpt-4o:extended"),
	referenceModel("gw-a", "shared"),
	referenceModel("gw-a", "anthropic/future"),
	referenceModel("gw-a", "anthropic/exact"),
	referenceModel("gw-a", "anthropic/claude-opus-4.8"),
	referenceModel("gw-a", "anthropic/claude-opus"),
	referenceModel("gw-b", "shared"),
	referenceModel("gw-b", "anthropic/future"),
	referenceModel("gw-b", "claude-opus-4-8"),
	referenceModel("gw-b", "gw-b/free"),
	referenceModel("gw-b", "zz:free"),
	// URL-derived provider id containing "/" (#11394).
	referenceModel("llama=http://host:10800", "unsloth/qwen-gguf:BF16"),
}

type referenceExpected struct {
	notFound      bool
	model         string
	thinkingLevel string
	warning       string
	ambiguous     []string
	usableCount   int
}

type referenceCase struct {
	name     string
	text     string
	expected referenceExpected
	usable   []string
	provider string
	suffix   *thinkingSuffix
	fuzzy    *bool
}

var (
	lenient        = thinkingSuffixLenient
	none           = thinkingSuffixNone
	noFuzzy        = false
	expectNotFound = referenceExpected{notFound: true}
)

func resolvedTo(model string) referenceExpected { return referenceExpected{model: model} }

var referenceCases = []referenceCase{
	// Reference syntax
	{name: "exact id", text: "claude-sonnet-4-5", expected: resolvedTo("anthropic/claude-sonnet-4-5")},
	{name: "exact id is case-insensitive", text: "GPT-4O", expected: resolvedTo("openai/gpt-4o")},
	{name: "fuzzy prefers aliases over dated ids", text: "sonnet", expected: resolvedTo("anthropic/claude-sonnet-4-5")},
	{name: "thinking suffix", text: "sonnet:high", expected: referenceExpected{model: "anthropic/claude-sonnet-4-5", thinkingLevel: "high"}},
	{name: "colon in a literal id is not a thinking suffix", text: "qwen/qwen3-coder:exacto", expected: resolvedTo("openrouter/qwen/qwen3-coder:exacto")},
	{name: "provider prefix, colon id, and thinking suffix", text: "openrouter/qwen/qwen3-coder:exacto:high", expected: referenceExpected{model: "openrouter/qwen/qwen3-coder:exacto", thinkingLevel: "high"}},
	{name: "provider id containing a slash", text: "llama=http://host:10800/unsloth/qwen-gguf:BF16", expected: resolvedTo("llama=http://host:10800/unsloth/qwen-gguf:BF16")},
	{name: "provider id containing a slash, with thinking suffix", text: "llama=http://host:10800/unsloth/qwen-gguf:BF16:high", expected: referenceExpected{model: "llama=http://host:10800/unsloth/qwen-gguf:BF16", thinkingLevel: "high"}},
	{name: "raw id wins when the provider prefix names no matching model", text: "openai/gpt-4o:extended", expected: resolvedTo("openrouter/openai/gpt-4o:extended")},
	{name: "fuzzy searches the prefixed provider first", text: "openrouter/qwen", expected: resolvedTo("openrouter/qwen/qwen3-coder:exacto")},
	{name: "strict keeps an invalid suffix in the id", text: "gpt-4o:random", expected: expectNotFound},
	{name: "lenient drops an invalid suffix with a warning", text: "gpt-4o:random", suffix: &lenient, expected: referenceExpected{model: "openai/gpt-4o", warning: `Invalid thinking level "random"`}},
	{name: "lenient invalid outer suffix discards the inner thinking level", text: "qwen/qwen3-coder:exacto:high:random", suffix: &lenient, expected: referenceExpected{model: "openrouter/qwen/qwen3-coder:exacto", warning: `Invalid thinking level "random"`}},
	// Beyond pi's table: a valid outer level over an inner invalid suffix is
	// dropped, since the inner recursion warned.
	{name: "lenient valid outer suffix over an invalid inner one is dropped", text: "gpt-4o:random:high", suffix: &lenient, expected: referenceExpected{model: "openai/gpt-4o", warning: `Invalid thinking level "random"`}},
	{name: "none keeps the suffix in the id", text: "gpt-4o:high", suffix: &none, expected: expectNotFound},
	{name: "exact-only skips fuzzy matches", text: "sonnet", fuzzy: &noFuzzy, expected: expectNotFound},
	{name: "empty text", text: "  ", expected: expectNotFound},

	// Exact matches across providers (#7327)
	{name: "sole usable provider wins", text: "shared", usable: []string{"gw-b"}, expected: resolvedTo("gw-b/shared")},
	{name: "no usable provider is ambiguous instead of falling through to fuzzy", text: "shared", expected: referenceExpected{ambiguous: []string{"gw-a/shared", "gw-b/shared"}}},
	{name: "several usable providers are ambiguous", text: "shared", usable: []string{"gw-a", "gw-b"}, expected: referenceExpected{ambiguous: []string{"gw-a/shared", "gw-b/shared"}, usableCount: 2}},
	{name: "thinking suffix does not change provider selection", text: "shared:high", usable: []string{"gw-b"}, expected: referenceExpected{model: "gw-b/shared", thinkingLevel: "high"}},
	{name: "thinking suffix does not bypass ambiguity", text: "shared:high", usable: []string{"gw-a", "gw-b"}, expected: referenceExpected{ambiguous: []string{"gw-a/shared", "gw-b/shared"}, usableCount: 2}},
	// #10160
	{name: "provider prefix without a direct model selects the usable raw id", text: "anthropic/future", usable: []string{"gw-b"}, expected: resolvedTo("gw-b/anthropic/future")},
	// #5823
	{name: "usable qualified match beats a usable raw id", text: "anthropic/exact", usable: []string{"anthropic", "gw-a"}, expected: resolvedTo("anthropic/exact")},
	// #5643
	{name: "usable raw id beats an unusable qualified match", text: "anthropic/exact", usable: []string{"gw-a"}, expected: resolvedTo("gw-a/anthropic/exact")},
	{name: "unusable qualified match is kept when nothing is usable", text: "anthropic/exact", expected: resolvedTo("anthropic/exact")},
	// Like mistral/codestral -> mistral/codestral-latest despite gateway id mistral/codestral.
	{name: "named provider's usable fuzzy match beats a raw id elsewhere", text: "anthropic/claude-opus", usable: []string{"anthropic"}, expected: resolvedTo("anthropic/claude-opus-4-8")},
	{name: "usable raw id beats the named provider's unusable fuzzy match", text: "anthropic/claude-opus", usable: []string{"gw-a"}, expected: resolvedTo("gw-a/anthropic/claude-opus")},
	{name: "named provider's fuzzy match is kept when nothing is usable", text: "anthropic/claude-opus", expected: resolvedTo("anthropic/claude-opus-4-8")},
	{name: "named provider's literal id beats its fuzzy matches", text: "gw-b/free", expected: resolvedTo("gw-b/gw-b/free")},

	// Fuzzy matches across providers
	{name: "fuzzy prefers usable providers", text: "opus", usable: []string{"gw-a"}, expected: resolvedTo("gw-a/anthropic/claude-opus-4.8")},
	{name: "fuzzy id tie across usable providers is ambiguous", text: "opus", usable: []string{"anthropic", "gw-a", "gw-b"}, expected: referenceExpected{ambiguous: []string{"anthropic/claude-opus-4-8", "gw-b/claude-opus-4-8"}, usableCount: 2}},
	{name: "fuzzy id tie resolves to the sole usable provider", text: "opus", usable: []string{"anthropic"}, expected: resolvedTo("anthropic/claude-opus-4-8")},

	// Explicit provider constraint
	{name: "explicit provider ignores usable providers elsewhere", text: "shared", provider: "gw-a", usable: []string{"gw-b"}, expected: resolvedTo("gw-a/shared")},
	{name: "explicit provider accepts its own prefix", text: "GW-B/shared", provider: "gw-b", expected: resolvedTo("gw-b/shared")},
	{name: "explicit provider allows fuzzy matches", text: "4o", provider: "openai", expected: resolvedTo("openai/gpt-4o")},
	{name: "explicit provider never leaves the provider", text: "gpt-4o", provider: "gw-a", expected: expectNotFound},
}

func modelRef(m *ai.Model) string { return string(m.Provider) + "/" + m.ID }

func TestResolveModelReference(t *testing.T) {
	reversed := slices.Clone(referenceCatalog)
	slices.Reverse(reversed)
	for _, tc := range referenceCases {
		t.Run(tc.name, func(t *testing.T) {
			request := modelReferenceRequest{text: tc.text, provider: tc.provider, thinkingSuffix: thinkingSuffixStrict, fuzzy: true}
			if tc.suffix != nil {
				request.thinkingSuffix = *tc.suffix
			}
			if tc.fuzzy != nil {
				request.fuzzy = *tc.fuzzy
			}
			usable := func(provider string) bool { return slices.Contains(tc.usable, provider) }
			for _, models := range [][]*ai.Model{referenceCatalog, reversed} {
				result := resolveModelReference(request, models, usable)
				want := tc.expected
				switch {
				case want.notFound:
					if result.status != referenceNotFound {
						t.Fatalf("status %d, want not-found", result.status)
					}
				case want.ambiguous != nil:
					if result.status != referenceAmbiguous {
						t.Fatalf("status %d (%v), want ambiguous", result.status, result.model)
					}
					var got []string
					for _, m := range result.candidates {
						got = append(got, modelRef(m))
					}
					slices.Sort(got)
					if !slices.Equal(got, want.ambiguous) || result.usableCount != want.usableCount {
						t.Fatalf("candidates %v usable %d, want %v %d", got, result.usableCount, want.ambiguous, want.usableCount)
					}
				default:
					if result.status != referenceResolved {
						t.Fatalf("status %d, want %s", result.status, want.model)
					}
					if modelRef(result.model) != want.model || result.thinkingLevel != want.thinkingLevel {
						t.Fatalf("resolved %s level %q, want %s %q", modelRef(result.model), result.thinkingLevel, want.model, want.thinkingLevel)
					}
					if want.warning != "" && !strings.Contains(result.warning, want.warning) || want.warning == "" && result.warning != "" {
						t.Fatalf("warning %q, want %q", result.warning, want.warning)
					}
				}
			}
		})
	}
}

// pi's mock catalogs for the resolveCliModel cases.
var (
	cliMockModels = []*ai.Model{
		{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: "https://api.anthropic.com", Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 200000, MaxTokens: 8192},
		{ID: "gpt-4o", Name: "GPT-4o", Api: ai.APIAnthropicMessages, Provider: "openai", BaseURL: "https://api.openai.com", Input: []string{"text", "image"}, ContextWindow: 128000, MaxTokens: 4096},
	}
	cliOpenRouterModels = []*ai.Model{
		{ID: "qwen/qwen3-coder:exacto", Name: "Qwen3 Coder Exacto", Api: ai.APIAnthropicMessages, Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Reasoning: true, Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 8192},
		{ID: "openai/gpt-4o:extended", Name: "GPT-4o Extended", Api: ai.APIAnthropicMessages, Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Input: []string{"text", "image"}, ContextWindow: 128000, MaxTokens: 4096},
	}
	cliAllModels = slices.Concat(cliMockModels, cliOpenRouterModels)
)

func TestResolveCliModelKeepsAnInvalidSuffixInTheID(t *testing.T) {
	r, err := resolveCliModel(CliModelOptions{Provider: "openai", Model: "gpt-4o:extended"}, cliAllModels)
	if err != nil || r.Model.Provider != "openai" || r.Model.ID != "gpt-4o:extended" {
		t.Fatalf("%v %v", r.Model, err)
	}
}

func TestResolveCliModelCustomIDForAnExplicitProviderIsNotDoublePrefixed(t *testing.T) {
	r, err := resolveCliModel(CliModelOptions{Provider: "openrouter", Model: "openrouter/openai/ghost-model"}, cliAllModels)
	if err != nil || r.Model.Provider != "openrouter" || r.Model.ID != "openai/ghost-model" {
		t.Fatalf("%v %v", r.Model, err)
	}
}

func TestResolveCliModelWithoutModels(t *testing.T) {
	r, err := resolveCliModel(CliModelOptions{Provider: "openai", Model: "gpt-4o"}, nil)
	if r.Model != nil || err == nil || !strings.Contains(err.Error(), "No models available") {
		t.Fatalf("%v %v", r.Model, err)
	}
}

// Beyond pi's suite: an unknown --provider is pi's error, not a not-found.
func TestResolveCliModelUnknownProvider(t *testing.T) {
	_, err := resolveCliModel(CliModelOptions{Provider: "nobody", Model: "gpt-4o"}, cliAllModels)
	if err == nil || err.Error() != `Unknown provider "nobody". Use --list-models to see available providers/models.` {
		t.Fatalf("%v", err)
	}
}

// Duplicate raw ids behind a known provider prefix (#10160).
func TestResolveCliModelDuplicateRawIDsBehindAKnownProviderPrefix(t *testing.T) {
	base := *cliMockModels[0]
	direct, openrouter, vercel := base, base, base
	openrouter.ID, openrouter.Provider = "anthropic/claude-opus-5.5", "openrouter"
	vercel.ID, vercel.Provider = "anthropic/claude-opus-5.5", "vercel-ai-gateway"
	models := []*ai.Model{&direct, &openrouter, &vercel}
	configured := func(providers ...string) func(string) bool {
		return func(p string) bool { return slices.Contains(providers, p) }
	}

	t.Run("selects the authenticated gateway, with or without a thinking suffix", func(t *testing.T) {
		for _, tc := range []struct{ model, level string }{{"anthropic/claude-opus-5.5", ""}, {"anthropic/claude-opus-5.5:high", "high"}} {
			r, err := resolveCliModel(CliModelOptions{Model: tc.model, HasAuth: configured("vercel-ai-gateway")}, models)
			if err != nil || r.Model.Provider != "vercel-ai-gateway" || r.ThinkingLevel != tc.level {
				t.Fatalf("%s: %v %q %v", tc.model, r.Model, r.ThinkingLevel, err)
			}
		}
	})
	t.Run("reports ambiguity instead of choosing by catalog order", func(t *testing.T) {
		r, err := resolveCliModel(CliModelOptions{Model: "anthropic/claude-opus-5.5", HasAuth: configured()}, models)
		if r.Model != nil || err == nil ||
			!strings.Contains(err.Error(), "openrouter/anthropic/claude-opus-5.5, vercel-ai-gateway/anthropic/claude-opus-5.5") ||
			!strings.Contains(err.Error(), "No matching provider is authenticated. Use --provider or provider/model.") {
			t.Fatalf("%v %v", r.Model, err)
		}
	})
	t.Run("does not let stored credentials choose the provider for --api-key", func(t *testing.T) {
		r, err := resolveCliModel(CliModelOptions{Model: "anthropic/claude-opus-5.5", InlineAPIKey: true, HasAuth: configured("vercel-ai-gateway")}, models)
		if r.Model != nil || err == nil || !strings.Contains(err.Error(), "--api-key does not choose a provider.") {
			t.Fatalf("%v %v", r.Model, err)
		}
	})
}

// Custom model fallback with a :thinking suffix (#5552).
func TestResolveCliModelCustomFallbackThinkingSuffix(t *testing.T) {
	neuralwatt := &ai.Model{ID: "some-base-model", Name: "Some Base Model", Api: ai.APIAnthropicMessages, Provider: "neuralwatt", BaseURL: "https://api.neuralwatt.com", Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 8192}
	models := append(slices.Clone(cliAllModels), neuralwatt)
	resolve := func(t *testing.T, opts CliModelOptions) ResolvedModel {
		t.Helper()
		r, err := resolveCliModel(opts, models)
		if err != nil || r.Model.Provider != "neuralwatt" {
			t.Fatalf("%v %v", r.Model, err)
		}
		return r
	}
	t.Run("strips the suffix from the custom id", func(t *testing.T) {
		r := resolve(t, CliModelOptions{Model: "neuralwatt/zai-org/GLM-5.1-FP8:high"})
		if r.Model.ID != "zai-org/GLM-5.1-FP8" || !r.Model.Reasoning || r.ThinkingLevel != "high" {
			t.Fatalf("%+v %q", r.Model, r.ThinkingLevel)
		}
	})
	t.Run("without a suffix", func(t *testing.T) {
		r := resolve(t, CliModelOptions{Model: "neuralwatt/zai-org/GLM-5.1-FP8"})
		if r.Model.ID != "zai-org/GLM-5.1-FP8" || r.ThinkingLevel != "" {
			t.Fatalf("%+v %q", r.Model, r.ThinkingLevel)
		}
	})
	t.Run("every valid level", func(t *testing.T) {
		for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
			r := resolve(t, CliModelOptions{Model: "neuralwatt/zai-org/GLM-5.1-FP8:" + level})
			if r.Model.ID != "zai-org/GLM-5.1-FP8" || r.ThinkingLevel != level {
				t.Fatalf("%s: %+v %q", level, r.Model, r.ThinkingLevel)
			}
		}
	})
	t.Run("an invalid suffix stays in the id", func(t *testing.T) {
		r := resolve(t, CliModelOptions{Model: "neuralwatt/zai-org/GLM-5.1-FP8:banana"})
		if r.Model.ID != "zai-org/GLM-5.1-FP8:banana" || r.ThinkingLevel != "" {
			t.Fatalf("%+v %q", r.Model, r.ThinkingLevel)
		}
	})
	t.Run("explicit --provider", func(t *testing.T) {
		r := resolve(t, CliModelOptions{Provider: "neuralwatt", Model: "zai-org/GLM-5.1-FP8:high"})
		if r.Model.ID != "zai-org/GLM-5.1-FP8" || r.ThinkingLevel != "high" {
			t.Fatalf("%+v %q", r.Model, r.ThinkingLevel)
		}
	})
	t.Run("explicit --thinking keeps the suffix in the id", func(t *testing.T) {
		r := resolve(t, CliModelOptions{Model: "neuralwatt/zai-org/GLM-5.1-FP8:high", Thinking: "medium"})
		if r.Model.ID != "zai-org/GLM-5.1-FP8:high" || r.ThinkingLevel != "" {
			t.Fatalf("%+v %q", r.Model, r.ThinkingLevel)
		}
	})
}
