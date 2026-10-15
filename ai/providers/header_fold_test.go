package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// pi folds header names with JavaScript's toLowerCase, which applies the full
// Unicode mapping: U+0130 lowers to "i" + U+0307 there, and a capital sigma
// ending a word lowers to U+03C2. strings.ToLower lowers them to "i" and
// U+03C3, so a fold through it matches names pi keeps apart. Every want here
// is pi's, measured by running 8676a0dcd's src under node v26.4.0 (with
// @google/genai 2.21.0, the version its package-lock names) against a local
// server.
const (
	dottedI    = "\u0130" // İ: toLowerCase gives "i\u0307"
	capitalSig = "\u03a3" // Σ: toLowerCase gives "\u03c2" at the end of a word
	smallSig   = "\u03c3" // σ
)

// runFoldWire sends one request through stream and returns the headers that
// reached the server (nil when none did) and the stream's final message.
func runFoldWire(t *testing.T, sse string, stream func(baseURL string) *ai.AssistantMessage) (http.Header, *ai.AssistantMessage) {
	t.Helper()
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer server.Close()
	return got, stream(server.URL)
}

// google spreads model.headers and options.headers into one object and folds
// it with providerHeadersToRecord. A marker whose name only strings.ToLower
// folds onto the model's name leaves the model's header standing in pi, where
// it reaches the wire or fails the request.
func TestGoogleRecordFoldsNamesLikeJS(t *testing.T) {
	for _, tc := range []struct {
		name   string
		model  string
		marker string
		sent   string // the model header's value on the wire; "" when not sent
		err    string // the stream's error when not sent
	}{
		{"dotted capital I", "x-id", "X-" + dottedI + "D", "v", ""},
		{"final sigma", "x" + smallSig, "X" + capitalSig, "", byteStringError(1, 0x3c3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, final := runFoldWire(t, googleSSE, func(baseURL string) *ai.AssistantMessage {
				model := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", BaseURL: baseURL,
					Input: []string{"text"}, MaxTokens: 4096, Headers: ai.ProviderHeaders{tc.model: strPtr("v")}}
				return StreamGoogle(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
					&GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
						APIKey: "g-key", Headers: ai.ProviderHeaders{tc.marker: nil}}}}).Result()
			})
			if tc.err != "" {
				wantNotSent(t, h, final, tc.err)
				return
			}
			wantSent(t, h, final, tc.model, tc.sent)
		})
	}
}

// pi-messages folds options.headers alone. Within one Go map the slot order is
// sorted name order, which here is the JS literal's order: the value first,
// then the marker, so the marker decides whether the value survives.
func TestPiMessagesRecordFoldsNamesLikeJS(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers ai.ProviderHeaders
		err     string
	}{
		{"dotted capital I", ai.ProviderHeaders{"X-" + dottedI: strPtr("v"), "x-i": nil}, byteStringError(2, 0x130)},
		{"final sigma", ai.ProviderHeaders{"X" + capitalSig: strPtr("v"), "x" + smallSig: nil}, byteStringError(1, 0x3a3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sse := piMessagesSSE(`{"type":"start"}`, `{"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+`,"responseId":"resp_1"}`)
			h, final := runFoldWire(t, sse, func(baseURL string) *ai.AssistantMessage {
				return StreamPiMessages(context.Background(), piMessagesTestModel(baseURL+"/v1"), ai.NormalizeContext(piMessagesTestContext()),
					&PiMessagesOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
						APIKey: "test-key", Headers: tc.headers}}}).Result()
			})
			wantNotSent(t, h, final, tc.err)
		})
	}
}

// With no api key, a header counts as the credential only when its name's
// toLowerCase is one of the gate's names. "Author\u0130zation" is not
// "authorization" in pi, so the stream fails with pi's message before it builds
// a request; strings.ToLower would let it through to fail on the name instead.
func TestAuthGatesFoldNamesLikeJS(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   string
		stream func(opts ai.StreamOptions) *ai.AssistantMessage
	}{
		{"openai-completions authorization", "Author" + dottedI + "zation", "No API key for provider: openai", func(opts ai.StreamOptions) *ai.AssistantMessage {
			return StreamOpenAICompletions(context.Background(), openAITestModel(), ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
				&OpenAIOptions{StreamOptions: opts}).Result()
		}},
		{"openai-responses cf-aig-authorization", "cf-aig-author" + dottedI + "zation", "No API key for provider: openai", func(opts ai.StreamOptions) *ai.AssistantMessage {
			model := &ai.Model{ID: "gpt-test", Api: ai.APIOpenAIResponses, Provider: "openai", Input: []string{"text"}, MaxTokens: 4096}
			return StreamOpenAIResponses(context.Background(), model, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
				&OpenAIResponsesOptions{StreamOptions: opts}).Result()
		}},
		{"anthropic x-api-key", "X-Ap" + dottedI + "-Key", "No API key for provider: anthropic", func(opts ai.StreamOptions) *ai.AssistantMessage {
			return StreamAnthropic(context.Background(), anthropicUAModel(), ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
				&AnthropicOptions{StreamOptions: opts}).Result()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payloads := 0
			final := tc.stream(ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
				Headers:   ai.ProviderHeaders{tc.header: strPtr("Bearer x")},
				OnPayload: func(any, *ai.Model) (any, error) { payloads++; return nil, nil },
			}})
			if final.StopReason != ai.StopError || final.ErrorMessage != tc.want {
				t.Fatalf("stream = %s %q, want error %q", final.StopReason, final.ErrorMessage, tc.want)
			}
			if payloads != 0 {
				t.Fatalf("onPayload ran %d times, want 0: pi fails before it builds the payload", payloads)
			}
		})
	}
}
