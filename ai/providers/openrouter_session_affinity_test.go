package providers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// OpenRouter session affinity is on by default and uses its own header name
// (pi bbb61e34a, closes upstream #9102). The assertions here are pi's own, from
// packages/ai/test/fireworks-models.test.ts and
// packages/ai/test/openai-completions-prompt-cache.test.ts at that sha.
//
// x-session-id REPLACES x-session-affinity rather than joining it, so every
// case pins the absence of the other name too.

func openRouterAnthropicModel() *ai.Model {
	return &ai.Model{
		ID: "anthropic/claude-opus-4.8", Api: ai.APIAnthropicMessages, Provider: "openrouter",
		BaseURL: "https://openrouter.ai/api", Input: []string{"text"}, MaxTokens: 4096,
	}
}

func anthropicAffinityHeaders(t *testing.T, model *ai.Model, retention ai.CacheRetention) http.Header {
	t.Helper()
	h := captureAnthropicHeaders(t, model, &AnthropicOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"},
		SessionID:              "openrouter-session-1",
		CacheRetention:         retention,
	}})
	// anthropicCapture discards the stream result, so a request that never left
	// yields nil headers — on which every Get is "" and every absence assertion
	// below would pass vacuously. anthropic-version rides on all four auth arms.
	if got := h.Get("anthropic-version"); got != anthropicVersion {
		t.Fatalf("no request reached the server: anthropic-version = %q, want %q", got, anthropicVersion)
	}
	return h
}

func TestAnthropicOpenRouterSessionAffinity(t *testing.T) {
	// pi: "sends only x-session-id for OpenRouter models". No compat at all —
	// the default does the work, which is the whole point of the upstream fix.
	t.Run("sends only x-session-id", func(t *testing.T) {
		h := anthropicAffinityHeaders(t, openRouterAnthropicModel(), ai.CacheShort)
		if got := h.Get("x-session-id"); got != "openrouter-session-1" {
			t.Fatalf("x-session-id = %q, want openrouter-session-1", got)
		}
		if got := h.Get("x-session-affinity"); got != "" {
			t.Fatalf("x-session-id must REPLACE x-session-affinity, got %q", got)
		}
	})

	// The realistic path: no retention supplied at all resolves to "short",
	// so the header rides on an ordinary request.
	t.Run("sends on the default retention", func(t *testing.T) {
		h := anthropicAffinityHeaders(t, openRouterAnthropicModel(), "")
		if got := h.Get("x-session-id"); got != "openrouter-session-1" {
			t.Fatalf("x-session-id = %q, want openrouter-session-1", got)
		}
	})

	// pi: "omits OpenRouter session headers when cacheRetention is none". The
	// retention gate is at the call site upstream and must cover the new header
	// name too, not just the legacy one.
	t.Run("cacheRetention none suppresses both names", func(t *testing.T) {
		h := anthropicAffinityHeaders(t, openRouterAnthropicModel(), ai.CacheNone)
		if got := h.Get("x-session-id"); got != "" {
			t.Fatalf("x-session-id must be suppressed at retention=none, got %q", got)
		}
		if got := h.Get("x-session-affinity"); got != "" {
			t.Fatalf("x-session-affinity must be suppressed at retention=none, got %q", got)
		}
	})

	// pi: "allows OpenRouter session headers to be disabled". The catalog value
	// wins in BOTH directions, so the explicit false must beat the detection —
	// note it sets only the boolean, leaving the format resolving to openrouter.
	t.Run("explicit opt-out wins over detection", func(t *testing.T) {
		model := openRouterAnthropicModel()
		model.Compat = json.RawMessage(`{"sendSessionAffinityHeaders":false}`)
		h := anthropicAffinityHeaders(t, model, ai.CacheShort)
		if got := h.Get("x-session-id"); got != "" {
			t.Fatalf("explicit sendSessionAffinityHeaders:false must suppress, got %q", got)
		}
		if got := h.Get("x-session-affinity"); got != "" {
			t.Fatalf("explicit sendSessionAffinityHeaders:false must suppress, got %q", got)
		}
	})

	// The default stays false off OpenRouter: nothing else may start sending.
	t.Run("non-openrouter still sends nothing by default", func(t *testing.T) {
		model := &ai.Model{
			ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic",
			Input: []string{"text"}, MaxTokens: 4096,
		}
		h := anthropicAffinityHeaders(t, model, ai.CacheShort)
		if got := h.Get("x-session-id"); got != "" {
			t.Fatalf("x-session-id must not be sent off OpenRouter, got %q", got)
		}
		if got := h.Get("x-session-affinity"); got != "" {
			t.Fatalf("x-session-affinity must not be sent off OpenRouter, got %q", got)
		}
	})

	// An explicit opt-in off OpenRouter keeps the LEGACY name: the format is
	// undefined there, and only "openrouter" selects x-session-id. This is the
	// shipped fireworks configuration.
	t.Run("explicit opt-in off openrouter keeps x-session-affinity", func(t *testing.T) {
		model := &ai.Model{
			ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "fireworks",
			Input: []string{"text"}, MaxTokens: 4096,
			Compat: json.RawMessage(`{"sendSessionAffinityHeaders":true}`),
		}
		h := anthropicAffinityHeaders(t, model, ai.CacheShort)
		if got := h.Get("x-session-affinity"); got != "openrouter-session-1" {
			t.Fatalf("x-session-affinity = %q, want openrouter-session-1", got)
		}
		if got := h.Get("x-session-id"); got != "" {
			t.Fatalf("x-session-id must not be sent without the openrouter format, got %q", got)
		}
	})
}

// TestGetAnthropicCompatSessionAffinity covers the resolver directly, because
// two cells are unreachable from the wire tests: the baseURL half of the
// detection (the capture helper rewrites BaseURL to the test server, which is
// also why pi's own suite only ever exercises the provider half), and an
// explicit format on a model that is not OpenRouter.
func TestGetAnthropicCompatSessionAffinity(t *testing.T) {
	cases := []struct {
		name       string
		provider   ai.ProviderId
		baseURL    string
		compat     string
		wantSend   bool
		wantFormat string
	}{
		{"provider detection", "openrouter", "https://example.test", "", true, sessionAffinityOpenRouter},
		{"baseurl detection", "some-proxy", "https://openrouter.ai/api", "", true, sessionAffinityOpenRouter},
		{"neither", "anthropic", "https://api.anthropic.com", "", false, ""},
		{"explicit false beats detection", "openrouter", "https://openrouter.ai/api",
			`{"sendSessionAffinityHeaders":false}`, false, sessionAffinityOpenRouter},
		{"explicit true off openrouter has no format", "fireworks", "https://api.fireworks.ai",
			`{"sendSessionAffinityHeaders":true}`, true, ""},
		{"explicit format beats detection", "openrouter", "https://openrouter.ai/api",
			`{"sessionAffinityFormat":"legacy"}`, true, "legacy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &ai.Model{ID: "m", Api: ai.APIAnthropicMessages, Provider: tc.provider, BaseURL: tc.baseURL}
			if tc.compat != "" {
				model.Compat = json.RawMessage(tc.compat)
			}
			got := getAnthropicCompat(model)
			if got.sendSessionAffinityHeaders != tc.wantSend {
				t.Fatalf("sendSessionAffinityHeaders = %v, want %v", got.sendSessionAffinityHeaders, tc.wantSend)
			}
			if got.sessionAffinityFormat != tc.wantFormat {
				t.Fatalf("sessionAffinityFormat = %q, want %q", got.sessionAffinityFormat, tc.wantFormat)
			}
		})
	}
}

// TestDetectOpenAICompatSessionAffinity covers the completions send-default at
// the resolver, where the baseURL half of the detection is reachable — the wire
// helper rewrites BaseURL to the test server, exactly as on the anthropic side.
func TestDetectOpenAICompatSessionAffinity(t *testing.T) {
	cases := []struct {
		name     string
		provider ai.ProviderId
		baseURL  string
		want     bool
	}{
		{"provider detection", "openrouter", "https://example.test", true},
		{"baseurl detection", "some-proxy", "https://openrouter.ai/api/v1", true},
		{"neither", "openai", "https://api.openai.com/v1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &ai.Model{ID: "m", Api: ai.APIOpenAICompletions, Provider: tc.provider, BaseURL: tc.baseURL}
			if got := getOpenAICompat(model).SendSessionAffinityHeaders; got != tc.want {
				t.Fatalf("SendSessionAffinityHeaders = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOpenAICompletionsOpenRouterSessionAffinity is pi's "sends OpenRouter
// session-affinity header by default for built-in OpenRouter models". The
// format was already openrouter-detected port-side; the SEND default is what
// bbb61e34a flips, and no test paired an OpenRouter model with a session id
// without setting the boolean explicitly, so nothing here was covered before.
func TestOpenAICompletionsOpenRouterSessionAffinity(t *testing.T) {
	mk := func(t *testing.T, compat string) http.Header {
		t.Helper()
		model := openAITestModel()
		model.Provider = "openrouter"
		if compat != "" {
			model.Compat = json.RawMessage(compat)
		}
		return captureOpenAIHeaders(t, model, ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"},
			SessionID:              "session-openrouter",
			CacheRetention:         ai.CacheShort,
		})
	}

	t.Run("sends x-session-id by default", func(t *testing.T) {
		h := mk(t, "")
		if got := h.Get("x-session-id"); got != "session-openrouter" {
			t.Fatalf("x-session-id = %q, want session-openrouter", got)
		}
		// The openrouter shape sends that header ALONE.
		for _, name := range []string{"session_id", "x-client-request-id", "x-session-affinity"} {
			if got := h.Get(name); got != "" {
				t.Fatalf("%s must not accompany x-session-id, got %q", name, got)
			}
		}
	})

	t.Run("explicit opt-out wins over detection", func(t *testing.T) {
		h := mk(t, `{"sendSessionAffinityHeaders":false}`)
		if got := h.Get("x-session-id"); got != "" {
			t.Fatalf("explicit sendSessionAffinityHeaders:false must suppress, got %q", got)
		}
	})

	t.Run("non-openrouter still sends nothing by default", func(t *testing.T) {
		h := captureOpenAIHeaders(t, openAITestModel(), ai.StreamOptions{
			ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"},
			SessionID:              "session-openrouter",
			CacheRetention:         ai.CacheShort,
		})
		for _, name := range []string{"x-session-id", "session_id", "x-client-request-id", "x-session-affinity"} {
			if got := h.Get(name); got != "" {
				t.Fatalf("%s must not be sent off OpenRouter by default, got %q", name, got)
			}
		}
	})
}
