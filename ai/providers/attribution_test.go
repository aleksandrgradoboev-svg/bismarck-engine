package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Provider attribution on the wire. pi moved attribution from the coding
// agent (core/provider-attribution.ts) into pi-ai's request identity
// (upstream adc5fa9f, 04a93bad): these assert that the identity's headers
// land on the outgoing request for each API, below model.Headers and the
// consumer's headers. The mapping itself is unit-tested in
// ai/identity_test.go.

const attrDoneSSE = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

// piIdentity attributes requests to pi, as pi's agent loop does by default.
func piIdentity() *ai.ClientIdentity {
	attribution := ai.PIAttribution
	return &ai.ClientIdentity{Attribution: &attribution}
}

func attributedOptions(headers ai.ProviderHeaders) ai.ProviderRequestOptions {
	return ai.ProviderRequestOptions{APIKey: "k", Headers: headers, Identity: piIdentity()}
}

// captureOpenAICompletionsHeaders runs the openai-completions provider against a
// stub server, attributed to pi, and returns the request headers. The model's
// Provider drives attribution; BaseURL is the test server.
func captureOpenAICompletionsHeaders(t *testing.T, provider ai.ProviderId, sessionID string, optsHeaders ai.ProviderHeaders) http.Header {
	t.Helper()
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, attrDoneSSE)
	}))
	defer server.Close()
	model := &ai.Model{
		ID: provider + "-test", Api: ai.APIOpenAICompletions, Provider: provider, BaseURL: server.URL,
		Input: []string{"text"}, MaxTokens: 4096,
	}
	opts := &OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: attributedOptions(optsHeaders), SessionID: sessionID}}
	StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}), opts).Result()
	return got
}

func TestAttributionOpenRouter(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "openrouter", "", nil)
	want := map[string]string{
		"HTTP-Referer":            "https://pi.dev",
		"X-OpenRouter-Title":      "pi",
		"X-OpenRouter-Categories": "cli-agent",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Fatalf("%s = %q, want %q", k, got, v)
		}
	}
}

// 83cbfc65: the Vercel AI Gateway attribution branch was removed — vercel-ai-gateway
// requests must carry no attribution headers.
func TestAttributionVercelGatewayNone(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "vercel-ai-gateway", "", nil)
	if got := h.Get("http-referer"); got != "" {
		t.Fatalf("http-referer = %q, want none", got)
	}
	if got := h.Get("x-title"); got != "" {
		t.Fatalf("x-title = %q, want none", got)
	}
}

func TestAttributionNvidiaNim(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "nvidia", "", nil)
	if got := h.Get("X-BILLING-INVOKE-ORIGIN"); got != "Pi" {
		t.Fatalf("X-BILLING-INVOKE-ORIGIN = %q, want Pi", got)
	}
}

// adc5fa9f dropped the Cloudflare branch: Cloudflare requests carry the
// identity's User-Agent like every other, not "pi-coding-agent".
func TestAttributionCloudflareSendsTheIdentityUserAgent(t *testing.T) {
	for _, p := range []ai.ProviderId{"cloudflare-workers-ai", "cloudflare-ai-gateway"} {
		h := captureOpenAICompletionsHeaders(t, p, "", nil)
		if got, want := h.Get("User-Agent"), ai.FormatUserAgent(piIdentity()); got != want {
			t.Fatalf("provider %s: User-Agent = %q, want %q", p, got, want)
		}
	}
}

func TestAttributionOpenCodeSessionHeaders(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "opencode", "sess-123", nil)
	if got := h.Get("x-opencode-session"); got != "sess-123" {
		t.Fatalf("x-opencode-session = %q, want sess-123", got)
	}
	if got := h.Get("x-opencode-client"); got != "pi" {
		t.Fatalf("x-opencode-client = %q, want pi", got)
	}
}

// The session header needs a session id; x-opencode-client is attribution
// now and does not (pi getAttributionHeaders).
func TestAttributionOpenCodeNoSessionWithoutID(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "opencode", "", nil)
	if got := h.Get("x-opencode-session"); got != "" {
		t.Fatalf("x-opencode-session must be absent without a session id, got %q", got)
	}
	if got := h.Get("x-opencode-client"); got != "pi" {
		t.Fatalf("x-opencode-client = %q, want pi", got)
	}
}

// Non-attributed providers (e.g. plain openai) get no attribution headers.
func TestAttributionAbsentForUnattributedProvider(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "openai", "sess", nil)
	for _, k := range []string{"HTTP-Referer", "X-OpenRouter-Title", "X-BILLING-INVOKE-ORIGIN", "x-opencode-session", "x-title"} {
		if got := h.Get(k); got != "" {
			t.Fatalf("%s should be absent for plain openai, got %q", k, got)
		}
	}
}

// Without an identity (pi-ai's default) nothing is attributed, and the
// OpenCode session header, which is not attribution, still flows.
func TestAttributionOffWithoutIdentity(t *testing.T) {
	for _, provider := range []ai.ProviderId{"openrouter", "nvidia", "opencode"} {
		var got http.Header
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, attrDoneSSE)
		}))
		model := &ai.Model{ID: "m", Api: ai.APIOpenAICompletions, Provider: provider, BaseURL: server.URL, MaxTokens: 4096}
		opts := &OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"}, SessionID: "sess"}}
		StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}), opts).Result()
		server.Close()
		for _, k := range []string{"HTTP-Referer", "X-OpenRouter-Title", "X-OpenRouter-Categories", "X-BILLING-INVOKE-ORIGIN", "x-opencode-client"} {
			if v := got.Get(k); v != "" {
				t.Fatalf("%s: %s = %q without an identity", provider, k, v)
			}
		}
		if v := got.Get("x-opencode-session"); (provider == "opencode") != (v == "sess") {
			t.Fatalf("%s: x-opencode-session = %q", provider, v)
		}
		if v := got.Get("User-Agent"); v != ai.FormatUserAgent(nil) {
			t.Fatalf("%s: User-Agent = %q, want the default identity's", provider, v)
		}
	}
}

// pi precedence: the attribution bundle is applied as options.headers, but
// consumer-supplied opts.Headers are Object.assigned over the defaults inside
// the bundle and win. (pi test: "lets provider and request headers override the
// defaults".)
func TestAttributionOptsHeadersOverrideDefaults(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "openrouter", "", ai.ProviderHeaders{
		"HTTP-Referer":       strPtr("https://consumer.example"),
		"X-OpenRouter-Title": strPtr("consumer"),
	})
	if got := h.Get("HTTP-Referer"); got != "https://consumer.example" {
		t.Fatalf("opts.Headers must override attribution default: HTTP-Referer = %q", got)
	}
	if got := h.Get("X-OpenRouter-Title"); got != "consumer" {
		t.Fatalf("opts.Headers must override attribution default: X-OpenRouter-Title = %q", got)
	}
	// Untouched default still present.
	if got := h.Get("X-OpenRouter-Categories"); got != "cli-agent" {
		t.Fatalf("X-OpenRouter-Categories = %q, want cli-agent", got)
	}
}

// pi precedence: auth.headers (= {...model.headers, ...providerHeaders,
// ...modelHeaders}) are Object.assigned over the attribution defaults in
// mergeProviderAttributionHeaders (sdk.ts), so model.Headers override an
// attribution default. This is the regression guarded by the header-precedence
// fix: a model whose Headers set HTTP-Referer to a custom value on an openrouter
// model must win over the https://pi.dev default.
func TestAttributionModelHeadersOverrideDefaults(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, attrDoneSSE)
	}))
	defer server.Close()
	model := &ai.Model{
		ID: "or", Api: ai.APIOpenAICompletions, Provider: "openrouter", BaseURL: server.URL, MaxTokens: 4096,
		Headers: ai.ProviderHeaders{"HTTP-Referer": strPtr("https://custom.example")},
	}
	opts := &OpenAIOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: attributedOptions(nil)}}
	StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}), opts).Result()
	if v := got.Get("HTTP-Referer"); v != "https://custom.example" {
		t.Fatalf("model.Headers must override attribution default: HTTP-Referer = %q, want https://custom.example", v)
	}
	// An attribution default the model did not override stays at pi's value.
	if v := got.Get("X-OpenRouter-Title"); v != "pi" {
		t.Fatalf("X-OpenRouter-Title = %q, want pi", v)
	}
}

// Same precedence rule for the Anthropic API path: model.Headers override an
// attribution default (HTTP-Referer on an openrouter-routed Anthropic model).
func TestAttributionAnthropicModelHeadersOverrideDefaults(t *testing.T) {
	model := &ai.Model{
		ID: "claude", Api: ai.APIAnthropicMessages, Provider: "openrouter", MaxTokens: 4096,
		Headers: ai.ProviderHeaders{"HTTP-Referer": strPtr("https://custom.example")},
	}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	h, _ := anthropicCapture(t, model, req, &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: attributedOptions(nil)}}, anthropicSSE)
	if v := h.Get("HTTP-Referer"); v != "https://custom.example" {
		t.Fatalf("model.Headers must override attribution default: HTTP-Referer = %q, want https://custom.example", v)
	}
	if v := h.Get("X-OpenRouter-Title"); v != "pi" {
		t.Fatalf("X-OpenRouter-Title = %q, want pi", v)
	}
}

// pi test: "lets configured OpenCode headers override the defaults". Consumer
// opts.Headers override the session headers too.
func TestAttributionOptsHeadersOverrideSessionHeaders(t *testing.T) {
	h := captureOpenAICompletionsHeaders(t, "opencode", "sess-123", ai.ProviderHeaders{
		"x-opencode-session": strPtr("configured-session"),
		"x-opencode-client":  strPtr("configured-client"),
	})
	if got := h.Get("x-opencode-session"); got != "configured-session" {
		t.Fatalf("x-opencode-session = %q, want configured-session", got)
	}
	if got := h.Get("x-opencode-client"); got != "configured-client" {
		t.Fatalf("x-opencode-client = %q, want configured-client", got)
	}
}

// 83cbfc65: vercel-ai-gateway no longer receives attribution on any API —
// confirm the responses path also emits no attribution headers.
func TestAttributionResponsesVercelNone(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, responsesSSE)
	}))
	defer server.Close()
	model := &ai.Model{ID: "m", Api: ai.APIOpenAIResponses, Provider: "vercel-ai-gateway", BaseURL: server.URL, MaxTokens: 4096}
	StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: attributedOptions(nil)}}).
		Result()
	if got.Get("http-referer") != "" || got.Get("x-title") != "" {
		t.Fatalf("responses vercel attribution should be absent: http-referer=%q x-title=%q", got.Get("http-referer"), got.Get("x-title"))
	}
}

func TestAttributionAnthropicOpenRouter(t *testing.T) {
	model := &ai.Model{ID: "claude", Api: ai.APIAnthropicMessages, Provider: "openrouter", MaxTokens: 4096}
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	h, _ := anthropicCapture(t, model, req, &AnthropicOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: attributedOptions(nil)}}, anthropicSSE)
	if h.Get("HTTP-Referer") != "https://pi.dev" || h.Get("X-OpenRouter-Title") != "pi" {
		t.Fatalf("anthropic openrouter attribution wrong: HTTP-Referer=%q X-OpenRouter-Title=%q",
			h.Get("HTTP-Referer"), h.Get("X-OpenRouter-Title"))
	}
}

// 83cbfc65: the google path likewise emits no attribution for vercel-ai-gateway.
func TestAttributionGoogleVercelNone(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, googleSSE)
	}))
	defer server.Close()
	model := &ai.Model{ID: "gemini", Api: ai.APIGoogleGenerativeAI, Provider: "vercel-ai-gateway", BaseURL: server.URL, MaxTokens: 4096}
	StreamGoogle(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
		&GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: attributedOptions(nil)}}).
		Result()
	if got.Get("http-referer") != "" || got.Get("x-title") != "" {
		t.Fatalf("google vercel attribution should be absent: http-referer=%q x-title=%q", got.Get("http-referer"), got.Get("x-title"))
	}
}
