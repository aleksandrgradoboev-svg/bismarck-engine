package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Upstream 027a5847 makes the HTTP client injectable per request
// (pi StreamOptions.fetch). These tests pin that each ported provider honors the
// override, that http.DefaultClient means "unset" as globalThis.fetch does in
// pi, and that google rejects an override with pi's byte-exact message.

// countingClient records how many requests it performed and delegates to the
// real transport, so the stream under test still parses a live response.
type countingClient struct {
	calls atomic.Int64
	inner *http.Client
}

func (c *countingClient) Do(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.inner.Do(req)
}

const completionsInjectionSSE = `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}

data: [DONE]

`

// injectionProviders covers every ported provider that issues HTTP requests:
// the first three share the retry loop, pi-messages calls out directly.
var injectionProviders = []struct {
	name string
	api  ai.Api
	sse  string
	run  func(baseURL string, opts ai.StreamOptions) *ai.AssistantMessage
}{
	{
		name: "anthropic-messages", api: ai.APIAnthropicMessages, sse: anthropicSSE,
		run: func(baseURL string, opts ai.StreamOptions) *ai.AssistantMessage {
			model := &ai.Model{ID: "claude-test", Api: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: baseURL, MaxTokens: 4096}
			return StreamAnthropic(context.Background(), model, ai.NormalizeContext(injectionContext()), &AnthropicOptions{StreamOptions: opts}).Result()
		},
	},
	{
		name: "openai-completions", api: ai.APIOpenAICompletions, sse: completionsInjectionSSE,
		run: func(baseURL string, opts ai.StreamOptions) *ai.AssistantMessage {
			model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAICompletions, Provider: "openai", BaseURL: baseURL, MaxTokens: 4096}
			return StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(injectionContext()), &OpenAIOptions{StreamOptions: opts}).Result()
		},
	},
	{
		name: "openai-responses", api: ai.APIOpenAIResponses, sse: responsesSSE,
		run: func(baseURL string, opts ai.StreamOptions) *ai.AssistantMessage {
			model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAIResponses, Provider: "openai", BaseURL: baseURL, MaxTokens: 4096}
			return StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(injectionContext()), &OpenAIResponsesOptions{StreamOptions: opts}).Result()
		},
	},
	{
		name: "pi-messages", api: ai.APIPiMessages,
		sse: piMessagesSSE(`{"type":"start"}`, `{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`}`),
		run: func(baseURL string, opts ai.StreamOptions) *ai.AssistantMessage {
			return StreamPiMessages(context.Background(), piMessagesTestModel(baseURL+"/v1"), ai.NormalizeContext(piMessagesTestContext()),
				&PiMessagesOptions{StreamOptions: opts}).
				Result()
		},
	},
}

func injectionContext() ai.Context {
	return ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
}

func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestProvidersUseInjectedHTTPClient(t *testing.T) {
	for _, p := range injectionProviders {
		t.Run(p.name, func(t *testing.T) {
			server := sseServer(t, p.sse)
			client := &countingClient{inner: server.Client()}

			final := p.run(server.URL, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key", HTTPClient: client}})

			if final.StopReason == ai.StopError {
				t.Fatalf("stream failed: %s", final.ErrorMessage)
			}
			if got := client.calls.Load(); got != 1 {
				t.Fatalf("injected client performed %d requests, want 1", got)
			}
		})
	}
}

// pi blesses `fetch === globalThis.fetch` as equivalent to unset, so its Go
// stand-in must not displace the provider's own default client.
func TestDefaultHTTPClientCountsAsUnset(t *testing.T) {
	for _, p := range injectionProviders {
		t.Run(p.name, func(t *testing.T) {
			server := sseServer(t, p.sse)

			final := p.run(server.URL, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "test-key", HTTPClient: http.DefaultClient}})

			if final.StopReason == ai.StopError {
				t.Fatalf("stream failed: %s", final.ErrorMessage)
			}
		})
	}
	if _, custom := customHTTPClient(http.DefaultClient); custom {
		t.Fatalf("http.DefaultClient must not count as an override")
	}
	// The normalization has to happen in retryFromOptions, not just in the
	// google guard: otherwise the default client displaces sharedClient and the
	// TimeoutMs response-header cap goes with it.
	if cfg := retryFromOptions(ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{TimeoutMs: 1234, HTTPClient: http.DefaultClient}}, nil); cfg.httpClient != nil {
		t.Fatalf("http.DefaultClient must leave the retry loop on sharedClient, got %#v", cfg.httpClient)
	}
	if _, custom := customHTTPClient(nil); custom {
		t.Fatalf("nil must not count as an override")
	}
	if _, custom := customHTTPClient(&countingClient{inner: http.DefaultClient}); !custom {
		t.Fatalf("a real client must count as an override")
	}
}

// The retry loop falls back to sharedClient, whose transport carries the
// TimeoutMs response-header cap, when no override is set.
func TestSendWithRetryFallsBackToSharedClient(t *testing.T) {
	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.WriteHeader(200)
	}))
	defer server.Close()

	cfg := retryFromOptions(ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{TimeoutMs: 1234}}, nil)
	if cfg.httpClient != nil {
		t.Fatalf("expected no override, got %#v", cfg.httpClient)
	}
	resp, err := sendWithRetry(context.Background(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, server.URL, nil)
	}, cfg)
	if err != nil {
		t.Fatalf("sendWithRetry: %v", err)
	}
	resp.Body.Close()
	if served.Load() != 1 {
		t.Fatalf("server saw %d requests, want 1", served.Load())
	}
	if got := sharedClient(1234).Transport.(*http.Transport).ResponseHeaderTimeout; got.Milliseconds() != 1234 {
		t.Fatalf("shared client header timeout = %v, want 1234ms", got)
	}
}

// pi's google adapter throws rather than silently ignoring a custom fetch,
// because @google/genai cannot accept one. The message is byte-exact.
func TestGoogleRejectsCustomHTTPClient(t *testing.T) {
	var reached atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, googleSSE)
	}))
	defer server.Close()

	model := &ai.Model{
		ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
		BaseURL: server.URL, MaxTokens: 8192,
	}

	final := StreamGoogle(context.Background(), model, ai.NormalizeContext(injectionContext()), &GoogleOptions{
		StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "g-key", HTTPClient: &countingClient{inner: server.Client()}}},
	}).
		Result()

	if final.StopReason != ai.StopError {
		t.Fatalf("expected error stop, got %s", final.StopReason)
	}
	if final.ErrorMessage != "Custom fetch is not supported by the Google Generative AI adapter" {
		t.Fatalf("error message wrong: %q", final.ErrorMessage)
	}
	if reached.Load() {
		t.Fatalf("request must not be issued when the override is rejected")
	}
}

// The guard precedes the api-key check, as it does in pi: a request carrying
// both faults reports the fetch error, not the missing key.
func TestGoogleRejectsCustomHTTPClientBeforeAPIKeyCheck(t *testing.T) {
	final := StreamGoogle(context.Background(),
		&ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", MaxTokens: 8192}, ai.NormalizeContext(

			injectionContext()),

		&GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{HTTPClient: &countingClient{inner: http.DefaultClient}}}}).
		Result()

	if final.ErrorMessage != "Custom fetch is not supported by the Google Generative AI adapter" {
		t.Fatalf("error message wrong: %q", final.ErrorMessage)
	}
}

// pi rejects only a fetch that is not globalThis.fetch; http.DefaultClient is
// the Go stand-in for that default and must stream normally.
func TestGoogleAcceptsDefaultHTTPClient(t *testing.T) {
	server := sseServer(t, googleSSE)
	model := &ai.Model{
		ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google",
		BaseURL: server.URL, MaxTokens: 8192,
	}

	final := StreamGoogle(context.Background(), model, ai.NormalizeContext(injectionContext()), &GoogleOptions{
		StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "g-key", HTTPClient: http.DefaultClient}},
	}).
		Result()

	if final.StopReason == ai.StopError {
		t.Fatalf("default client must be accepted, got error: %s", final.ErrorMessage)
	}
}
