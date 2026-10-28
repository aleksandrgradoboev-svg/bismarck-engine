package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Ported from pi packages/ai/test/identity.test.ts (upstream 04a93bad), the
// "client identity in requests" block: the identity on the wire of a real
// adapter, through Models and WithClientIdentity. pi's capturing fetch is a
// capturing HTTPClient here.

const identityDoneSSE = "data: {\"id\":\"chatcmpl-identity\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null,\"index\":0}]}\n\n" +
	"data: {\"id\":\"chatcmpl-identity\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
	"data: [DONE]\n\n"

// capturingClient answers every request with a finished completions stream
// and keeps the last request's headers.
type capturingClient struct{ headers http.Header }

func (c *capturingClient) Do(r *http.Request) (*http.Response, error) {
	c.headers = r.Header.Clone()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(identityDoneSSE)),
		Request:    r,
	}, nil
}

func (c *capturingClient) sent(t *testing.T) http.Header {
	t.Helper()
	if c.headers == nil {
		t.Fatal("no request was sent")
	}
	return c.headers
}

func identityCompletionsModel(provider ai.ProviderId, baseURL string, headers ai.ProviderHeaders) *ai.Model {
	return &ai.Model{
		ID: "test-model", Name: "Test Model", Api: ai.APIOpenAICompletions, Provider: provider, BaseURL: baseURL,
		Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 4096, Headers: headers,
	}
}

var identityContext = ai.Context{Messages: []ai.Message{ai.NewUserText("hello", 1)}}

func TestIdentityAppliesBelowModelAndRequestHeaders(t *testing.T) {
	client := &capturingClient{}
	model := identityCompletionsModel("openrouter", "https://openrouter.ai/api/v1", ai.ProviderHeaders{"X-OpenRouter-Title": strPtr("model-title")})
	short := ai.UserAgentShort
	attribution := ai.PIAttribution
	StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(identityContext), &OpenAIOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{
			APIKey:     "key",
			Identity:   &ai.ClientIdentity{Attribution: &attribution, UserAgent: &short},
			Headers:    ai.ProviderHeaders{"HTTP-Referer": nil},
			HTTPClient: client,
		},
	}}).Result()
	h := client.sent(t)
	if got := h.Get("User-Agent"); got != "pi" {
		t.Errorf("User-Agent = %q, want pi", got)
	}
	if got := h.Get("X-OpenRouter-Title"); got != "model-title" {
		t.Errorf("X-OpenRouter-Title = %q, want model-title", got)
	}
	if values, ok := h["Http-Referer"]; ok {
		t.Errorf("HTTP-Referer = %q, want the marker to delete it", values)
	}
}

func TestImpersonatingModelHeadersStayOverTheIdentityUserAgent(t *testing.T) {
	client := &capturingClient{}
	model := identityCompletionsModel("github-copilot", "https://api.githubcopilot.com", ai.ProviderHeaders{"User-Agent": strPtr("GitHubCopilotChat/0.35.0")})
	custom := ai.CustomUserAgent("acme")
	StreamOpenAICompletions(context.Background(), model, ai.NormalizeContext(identityContext), &OpenAIOptions{StreamOptions: ai.StreamOptions{
		ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "key", Identity: &ai.ClientIdentity{UserAgent: &custom}, HTTPClient: client},
	}}).Result()
	if got := client.sent(t).Get("User-Agent"); got != "GitHubCopilotChat/0.35.0" {
		t.Errorf("User-Agent = %q, want the model's", got)
	}
}

func identityProvider(id ai.ProviderId, model *ai.Model) ai.Provider {
	return ai.CreateProvider(ai.CreateProviderOptions{
		ID:     id,
		Auth:   ai.ProviderAuth{APIKey: ai.EnvAPIKeyAuth("Test", "IDENTITY_TEST_KEY")},
		Models: []*ai.Model{model},
		API: &ai.ProviderStreams{
			Stream: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.StreamOptions) *ai.AssistantMessageEventStream {
				return StreamOpenAICompletions(ctx, model, req, &OpenAIOptions{StreamOptions: *opts})
			},
			StreamSimple: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				return StreamSimpleOpenAICompletions(ctx, model, req, opts)
			},
		},
	})
}

func TestIdentityMergesTheRequestOverTheWrapperAndModels(t *testing.T) {
	t.Setenv("IDENTITY_TEST_KEY", "key")
	model := identityCompletionsModel("openrouter", "https://openrouter.ai/api/v1", nil)
	attribution := func(name string) *ai.ClientAttribution {
		return &ai.ClientAttribution{Name: name, URL: "https://acme.example"}
	}
	short := ai.UserAgentShort
	models := ai.CreateModels(&ai.CreateModelsOptions{Identity: &ai.ClientIdentity{Attribution: attribution("models"), UserAgent: &short}})
	models.SetProvider(identityProvider("openrouter", model))
	send := func(target ai.Models, identity *ai.ClientIdentity) http.Header {
		client := &capturingClient{}
		options := &ai.ModelsSimpleStreamOptions{}
		options.HTTPClient = client
		options.Identity = identity
		target.StreamSimple(context.Background(), model, identityContext, options).Result()
		return client.sent(t)
	}

	fromModels := send(models, nil)
	if fromModels.Get("User-Agent") != "pi" || fromModels.Get("X-OpenRouter-Title") != "models" {
		t.Fatalf("from Models: User-Agent %q, title %q", fromModels.Get("User-Agent"), fromModels.Get("X-OpenRouter-Title"))
	}

	wrapped := ai.WithClientIdentity(models, ai.ClientIdentity{Attribution: attribution("wrapper")})
	fromWrapper := send(wrapped, nil)
	if fromWrapper.Get("User-Agent") != "pi" || fromWrapper.Get("X-OpenRouter-Title") != "wrapper" {
		t.Fatalf("from the wrapper: User-Agent %q, title %q", fromWrapper.Get("User-Agent"), fromWrapper.Get("X-OpenRouter-Title"))
	}
	if wrapped.GetModel("openrouter", "test-model") != model {
		t.Fatal("the wrapper does not delegate GetModel")
	}
	// A MutableModels stays mutable.
	wrapped.(ai.MutableModels).DeleteProvider("openrouter")
	if models.GetProvider("openrouter") != nil {
		t.Fatal("DeleteProvider did not reach the wrapped Models")
	}
	models.SetProvider(identityProvider("openrouter", model))

	fromRequest := send(wrapped, &ai.ClientIdentity{NoAttribution: true})
	if _, ok := fromRequest["X-Openrouter-Title"]; ok {
		t.Fatalf("X-OpenRouter-Title = %q, want none", fromRequest.Get("X-OpenRouter-Title"))
	}
	if got := fromRequest.Get("User-Agent"); got != "pi" {
		t.Fatalf("User-Agent = %q, want pi", got)
	}
}

func TestOpenCodeSessionHeaderReachesCustomProvidersPointingAtOpenCode(t *testing.T) {
	t.Setenv("IDENTITY_TEST_KEY", "key")
	model := identityCompletionsModel("custom-opencode", "https://opencode.ai/zen/v1", nil)
	models := ai.CreateModels(nil)
	models.SetProvider(identityProvider("custom-opencode", model))
	send := func(headers ai.ProviderHeaders) http.Header {
		client := &capturingClient{}
		options := &ai.ModelsSimpleStreamOptions{}
		options.HTTPClient = client
		options.SessionID = "session-1"
		options.Headers = headers
		models.StreamSimple(context.Background(), model, identityContext, options).Result()
		return client.sent(t)
	}
	if got := send(nil).Get("x-opencode-session"); got != "session-1" {
		t.Errorf("x-opencode-session = %q, want session-1", got)
	}
	if got := send(ai.ProviderHeaders{"X-OpenCode-Session": strPtr("configured")}).Get("x-opencode-session"); got != "configured" {
		t.Errorf("configured x-opencode-session = %q, want configured", got)
	}
}

// Added: pi-messages opens its record with the identity's User-Agent.
func TestPiMessagesSendsTheIdentityUserAgent(t *testing.T) {
	if got, want := capturePiMessagesHeaders(t, nil).Get("User-Agent"), ai.FormatUserAgent(nil); got != want {
		t.Fatalf("User-Agent = %q, want %q", got, want)
	}
}

// Added: Models adds the OpenCode session header itself, so it reaches an
// OpenCode target through an adapter that does not add it (pi-messages).
func TestModelsAddsTheOpenCodeSessionHeaderForEveryAdapter(t *testing.T) {
	t.Setenv("IDENTITY_TEST_KEY", "key")
	model := piMessagesTestModel("https://opencode.ai/zen/v1")
	model.Provider = "custom-opencode"
	models := ai.CreateModels(nil)
	models.SetProvider(ai.CreateProvider(ai.CreateProviderOptions{
		ID:     "custom-opencode",
		Auth:   ai.ProviderAuth{APIKey: ai.EnvAPIKeyAuth("Test", "IDENTITY_TEST_KEY")},
		Models: []*ai.Model{model},
		API: &ai.ProviderStreams{
			StreamSimple: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				return StreamPiMessages(ctx, model, req, &PiMessagesOptions{StreamOptions: opts.StreamOptions})
			},
		},
	}))
	client := &capturingClient{}
	options := &ai.ModelsSimpleStreamOptions{}
	options.HTTPClient = client
	options.SessionID = "session-1"
	models.StreamSimple(context.Background(), model, identityContext, options).Result()
	if got := client.sent(t).Get("x-opencode-session"); got != "session-1" {
		t.Fatalf("x-opencode-session = %q, want session-1", got)
	}
}
