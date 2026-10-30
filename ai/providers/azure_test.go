package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// The azure provider over Chat Completions (pi a37306d4, #9645). The cases
// transliterate packages/ai/test/azure-openai-completions.test.ts at v1.4.0;
// its 'still routes Responses models to the Responses api' waits on the
// azure-openai-responses adapter (Scope queue row 4). The request-shape cases
// hold because of the catalog's compat for azure/deepseek-v4-pro, which is
// what pi says they test.

// azureRequest is what the fake transport saw: the URL and the decoded body.
type azureRequest struct {
	url  string
	body map[string]any
}

// azureClient answers every request with a stopped completion, recording it.
func azureClient(seen *azureRequest) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen.url = r.URL.String()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen.body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"prompt_tokens_details\":{\"cached_tokens\":0}}}\n\ndata: [DONE]\n\n")),
			Request: r,
		}, nil
	})}
}

// azureEnv is the suite's beforeEach: the Foundry endpoint set, no cache
// retention and no deployment map.
func azureEnv(t *testing.T) {
	t.Setenv("AZURE_OPENAI_BASE_URL", "https://my-resource.services.ai.azure.com")
	t.Setenv("AZURE_OPENAI_RESOURCE_NAME", "")
	t.Setenv("AZURE_OPENAI_DEPLOYMENT_NAME_MAP", "")
	t.Setenv("PI_CACHE_RETENTION", "")
}

func azureDeepSeek(t *testing.T) *ai.Model {
	t.Helper()
	model := ai.GetModel("azure", "deepseek-v4-pro")
	if model == nil {
		t.Fatal("catalog has no azure/deepseek-v4-pro")
	}
	if model.Api != ai.APIOpenAICompletions {
		t.Fatalf("azure/deepseek-v4-pro api = %q, want %q", model.Api, ai.APIOpenAICompletions)
	}
	return model
}

var azureContext = ai.NormalizeContext(ai.Context{
	SystemPrompt: "sys",
	Messages:     []ai.Message{ai.NewUserText("hi", 1)},
})

// azureStream is the suite's azure.stream: provider-native options.
func azureStream(t *testing.T, transcript ai.TranscriptContext, opts *OpenAIOptions) (azureRequest, *ai.AssistantMessage) {
	t.Helper()
	var seen azureRequest
	opts.APIKey = "test-key"
	opts.HTTPClient = azureClient(&seen)
	msg := StreamOpenAICompletions(context.Background(), azureDeepSeek(t), transcript, opts).Result()
	return seen, msg
}

// azureStreamSimple is the suite's azure.streamSimple.
func azureStreamSimple(t *testing.T, opts *ai.SimpleStreamOptions) (azureRequest, *ai.AssistantMessage) {
	t.Helper()
	var seen azureRequest
	opts.APIKey = "test-key"
	opts.HTTPClient = azureClient(&seen)
	msg := StreamSimpleOpenAICompletions(context.Background(), azureDeepSeek(t), azureContext, opts).Result()
	return seen, msg
}

func TestAzureDeepSeekTurnsThinkingOnWithReasoningEffort(t *testing.T) {
	azureEnv(t)
	seen, _ := azureStreamSimple(t, &ai.SimpleStreamOptions{Reasoning: "high"})
	if got := seen.body["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning_effort = %v, want high", got)
	}
	if got, present := seen.body["thinking"]; present {
		t.Errorf("thinking = %v, want none", got)
	}
}

func TestAzureDeepSeekClampsThinkingLevelsTheDeploymentDoesNotAccept(t *testing.T) {
	azureEnv(t)
	seen, _ := azureStreamSimple(t, &ai.SimpleStreamOptions{Reasoning: "max"})
	if got := seen.body["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning_effort = %v, want high", got)
	}
}

func TestAzureDeepSeekSendsNoReasoningEffortWithoutAThinkingLevel(t *testing.T) {
	azureEnv(t)
	seen, _ := azureStreamSimple(t, &ai.SimpleStreamOptions{})
	for _, key := range []string{"reasoning_effort", "thinking"} {
		if got, present := seen.body[key]; present {
			t.Errorf("%s = %v, want none", key, got)
		}
	}
}

func TestAzureDeepSeekOmitsPromptCacheParameters(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		opts *OpenAIOptions
	}{
		{name: "long retention from PI_CACHE_RETENTION", env: "long",
			opts: &OpenAIOptions{StreamOptions: ai.StreamOptions{SessionID: "session-env"}}},
		{name: "long retention requested",
			opts: &OpenAIOptions{StreamOptions: ai.StreamOptions{CacheRetention: ai.CacheLong, SessionID: "session-1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			azureEnv(t)
			t.Setenv("PI_CACHE_RETENTION", tc.env)
			seen, _ := azureStream(t, azureContext, tc.opts)
			for _, key := range []string{"prompt_cache_key", "prompt_cache_retention"} {
				if got, present := seen.body[key]; present {
					t.Errorf("%s = %v, want none", key, got)
				}
			}
		})
	}
}

// The deployment discards a developer system message once reasoning_effort is
// set, so the system prompt goes out under the system role.
func TestAzureDeepSeekSendsTheSystemPromptUnderTheSystemRole(t *testing.T) {
	azureEnv(t)
	seen, _ := azureStream(t, azureContext, &OpenAIOptions{ReasoningEffort: "low"})
	messages, _ := seen.body["messages"].([]any)
	if len(messages) == 0 {
		t.Fatalf("messages = %v, want the system prompt first", seen.body["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "sys" {
		t.Errorf("messages[0] = %v, want role system, content sys", first)
	}
}

func TestAzureDeepSeekKeepsMidConversationSystemMessagesInPlace(t *testing.T) {
	azureEnv(t)
	resumed := ai.NormalizeContext(ai.Context{
		SystemPrompt: "first",
		Messages: []ai.Message{
			ai.NewUserText("hi", 1),
			ai.NewSystemText("second", 2),
			ai.NewUserText("again", 3),
		},
	})
	seen, _ := azureStream(t, resumed, &OpenAIOptions{})
	messages, _ := seen.body["messages"].([]any)
	var roles []string
	for _, m := range messages {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if want := []string{"system", "user", "system", "user"}; strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Errorf("roles = %v, want %v", roles, want)
	}
}

func TestAzureDeepSeekReplaysReasoningContentOnAssistantTurns(t *testing.T) {
	azureEnv(t)
	assistant := &ai.AssistantMessage{
		Content: ai.ContentList{
			ai.ThinkingContent{Thinking: "internal reasoning", ThinkingSignature: "reasoning_content"},
			ai.TextContent{Text: "answer"},
		},
		Api: ai.APIOpenAICompletions, Provider: "azure", Model: "deepseek-v4-pro",
		StopReason: ai.StopStop, Timestamp: 2,
	}
	resumed := ai.NormalizeContext(ai.Context{
		SystemPrompt: "sys",
		Messages:     []ai.Message{ai.NewUserText("first", 1), assistant, ai.NewUserText("second", 3)},
	})
	seen, _ := azureStream(t, resumed, &OpenAIOptions{})
	messages, _ := seen.body["messages"].([]any)
	for _, m := range messages {
		if msg := m.(map[string]any); msg["role"] == "assistant" {
			if got := msg["reasoning_content"]; got != "internal reasoning" {
				t.Errorf("assistant reasoning_content = %v, want %q", got, "internal reasoning")
			}
			return
		}
	}
	t.Fatalf("no assistant message in %v", messages)
}

// 'normalizes the Azure endpoint the completions client is built with', and
// 'routes openai-completions models to chat completions'.
func TestAzureCompletionsNormalizesTheEndpoint(t *testing.T) {
	azureEnv(t)
	seen, msg := azureStream(t, azureContext, &OpenAIOptions{})
	if want := "https://my-resource.services.ai.azure.com/openai/v1/chat/completions"; seen.url != want {
		t.Errorf("request URL = %q, want %q (stream ended %s: %q)", seen.url, want, msg.StopReason, msg.ErrorMessage)
	}
}

func TestAzureCompletionsSurfacesAnUnconfiguredEndpointAsAnErrorEvent(t *testing.T) {
	azureEnv(t)
	t.Setenv("AZURE_OPENAI_BASE_URL", "")
	seen, msg := azureStream(t, azureContext, &OpenAIOptions{})
	if msg.StopReason != ai.StopError || !strings.Contains(msg.ErrorMessage, "Azure OpenAI base URL is required") {
		t.Errorf("stream ended %s: %q, want an error about the base URL", msg.StopReason, msg.ErrorMessage)
	}
	if seen.url != "" {
		t.Errorf("a request went to %q", seen.url)
	}
}

// The id is persisted on the assistant message and read back by name, so it
// has to stay a catalog id.
func TestAzureCompletionsSendsTheModelIDAsTheRequestModel(t *testing.T) {
	azureEnv(t)
	seen, msg := azureStream(t, azureContext, &OpenAIOptions{})
	if seen.body["model"] != "deepseek-v4-pro" || msg.Model != "deepseek-v4-pro" {
		t.Errorf("request model = %v, message model = %q; want deepseek-v4-pro for both", seen.body["model"], msg.Model)
	}
}

func TestAzureCompletionsSendsTheMappedDeploymentName(t *testing.T) {
	azureEnv(t)
	t.Setenv("AZURE_OPENAI_DEPLOYMENT_NAME_MAP", "deepseek-v4-pro=my-deepseek")
	seen, msg := azureStreamSimple(t, &ai.SimpleStreamOptions{})
	if seen.body["model"] != "my-deepseek" || msg.Model != "deepseek-v4-pro" {
		t.Errorf("request model = %v, message model = %q; want my-deepseek, deepseek-v4-pro", seen.body["model"], msg.Model)
	}
}

func TestAzureCompletionsPassesTheDeploymentNameThroughOnPayload(t *testing.T) {
	azureEnv(t)
	t.Setenv("AZURE_OPENAI_DEPLOYMENT_NAME_MAP", "deepseek-v4-pro=my-deepseek")
	var seenModel any
	opts := &OpenAIOptions{}
	opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
		params := payload.(map[string]any)
		seenModel = params["model"]
		next := map[string]any{}
		for k, v := range params {
			next[k] = v
		}
		next["temperature"] = 0.1
		return next, nil
	}
	seen, _ := azureStream(t, azureContext, opts)
	if seenModel != "my-deepseek" {
		t.Errorf("onPayload saw model %v, want my-deepseek", seenModel)
	}
	if seen.body["model"] != "my-deepseek" || seen.body["temperature"] != 0.1 {
		t.Errorf("request model = %v, temperature = %v; want my-deepseek, 0.1", seen.body["model"], seen.body["temperature"])
	}
}

// The options win over the environment, as pi's AzureEndpointOptions do.
func TestAzureCompletionsEndpointOptions(t *testing.T) {
	azureEnv(t)
	t.Setenv("AZURE_OPENAI_DEPLOYMENT_NAME_MAP", "deepseek-v4-pro=from-env")
	seen, _ := azureStream(t, azureContext, &OpenAIOptions{Azure: AzureEndpointOptions{
		BaseURL: "https://other.openai.azure.com", DeploymentName: "from-options",
	}})
	if want := "https://other.openai.azure.com/openai/v1/chat/completions"; seen.url != want {
		t.Errorf("request URL = %q, want %q", seen.url, want)
	}
	if seen.body["model"] != "from-options" {
		t.Errorf("request model = %v, want from-options", seen.body["model"])
	}

	t.Setenv("AZURE_OPENAI_BASE_URL", "")
	seen, _ = azureStream(t, azureContext, &OpenAIOptions{Azure: AzureEndpointOptions{ResourceName: "res"}})
	if want := "https://res.openai.azure.com/openai/v1/chat/completions"; seen.url != want {
		t.Errorf("request URL from the resource name = %q, want %q", seen.url, want)
	}
}

// The endpoint cases of packages/ai/test/azure-openai-base-url.test.ts at
// v1.4.0 ('azure base URL normalization'). pi runs them through the Responses
// adapter; the normalization is the shared azure-openai-config.ts that the
// Chat Completions path resolves through, so they run here on it.
func TestAzureBaseURLNormalization(t *testing.T) {
	model := &ai.Model{ID: "gpt-4o-mini", Provider: "azure"}
	for _, tc := range []struct{ in, want string }{
		{"https://marc-quicktests-resource.cognitiveservices.azure.com", "https://marc-quicktests-resource.cognitiveservices.azure.com/openai/v1"},
		{"https://marc-quicktests-resource.ai.azure.com", "https://marc-quicktests-resource.ai.azure.com/openai/v1"},
		{"https://my-resource.openai.azure.com", "https://my-resource.openai.azure.com/openai/v1"},
		{"https://my-resource.cognitiveservices.azure.com/openai", "https://my-resource.cognitiveservices.azure.com/openai/v1"},
		{"https://my-resource.cognitiveservices.azure.com/openai/v1", "https://my-resource.cognitiveservices.azure.com/openai/v1"},
		{"https://my-resource.services.ai.azure.com/openai/v1/responses", "https://my-resource.services.ai.azure.com/openai/v1"},
		{"https://my-proxy.example.com/v1", "https://my-proxy.example.com/v1"},
		{"https://my-resource.openai.azure.com/openai?api-version=2024-12-01", "https://my-resource.openai.azure.com/openai/v1"},
		{"https://my-proxy.example.com/v1?custom=true", "https://my-proxy.example.com/v1?custom=true"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Setenv("AZURE_OPENAI_BASE_URL", tc.in)
			got, err := resolveAzureBaseURL(model, AzureEndpointOptions{}, nil)
			if err != nil || got != tc.want {
				t.Errorf("resolveAzureBaseURL = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	t.Run("invalid", func(t *testing.T) {
		t.Setenv("AZURE_OPENAI_BASE_URL", "not-a-url")
		if _, err := resolveAzureBaseURL(model, AzureEndpointOptions{}, nil); err == nil ||
			!strings.Contains(err.Error(), "Invalid Azure OpenAI base URL") {
			t.Errorf("err = %v, want an invalid base URL error", err)
		}
	})
}

func TestParseAzureDeploymentNameMap(t *testing.T) {
	got := parseAzureDeploymentNameMap(" a=one , ,b=two=extra,=none,c=, d = four ")
	want := map[string]string{"a": "one", "b": "two", "d": "four"}
	if len(got) != len(want) {
		t.Fatalf("map = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("map[%q] = %q, want %q", k, got[k], v)
		}
	}
}
