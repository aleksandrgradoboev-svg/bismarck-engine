package providers

import (
	"context"
	"slices"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// A JS object enumerates an array-index key — the canonical decimal form of
// an integer from 0 to 2^32-2 — ahead of every other key, in ascending numeric
// order, whatever order the keys were added in. Every object pi builds its
// headers in does, so a header named "1" is converted, and fails, ahead of the
// others. Every want here was measured on pi's wire: 8676a0dcd's src under
// node v26.4.0 with the SDK versions its package-lock names.

func TestArrayIndex(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  uint32
		index bool
	}{
		{"0", 0, true}, {"1", 1, true}, {"10", 10, true}, {"4294967294", 4294967294, true},
		{"4294967295", 0, false}, {"42949672940", 0, false}, {"01", 0, false}, {"00", 0, false},
		{"", 0, false}, {"1.5", 0, false}, {"-1", 0, false}, {"+1", 0, false}, {" 1", 0, false}, {"1a", 0, false},
	} {
		if got, ok := arrayIndex(tc.name); got != tc.want || ok != tc.index {
			t.Errorf("arrayIndex(%q) = %d, %v, want %d, %v", tc.name, got, ok, tc.want, tc.index)
		}
	}
}

// The object's own key order: array indexes first and ascending, then every
// other name in the order it was added.
func TestHeaderObjectEnumeratesKeysAsJS(t *testing.T) {
	o := &headerObject{}
	o.merge(ai.ProviderHeaders{"X-A": strPtr("a")})
	o.merge(ai.ProviderHeaders{"10": strPtr("b"), "9": strPtr("c"), "01": strPtr("d"), "4294967295": strPtr("e")})
	o.set("0", "f")
	want := []string{"0", "9", "10", "X-A", "01", "4294967295"}
	if !slices.Equal(o.names, want) {
		t.Fatalf("names = %q, want %q", o.names, want)
	}
}

// Model {"X-A": U+65E5} under opts {<name>: U+0100}: pi converts the opts
// header first exactly when its name is an array index.
func TestArrayIndexHeaderConvertsFirst(t *testing.T) {
	cjkValue, lowValue := string(rune(cjk)), string(rune(0x100))
	runners := map[string]func(t *testing.T, model, opts ai.ProviderHeaders) *ai.AssistantMessage{
		"openai-completions": func(t *testing.T, model, opts ai.ProviderHeaders) *ai.AssistantMessage {
			_, final := runSDKWire(t, "openai-completions", model, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", Headers: opts}})
			return final
		},
		"anthropic-messages": func(t *testing.T, model, opts ai.ProviderHeaders) *ai.AssistantMessage {
			_, final := runSDKWire(t, "anthropic-messages", model, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", Headers: opts}})
			return final
		},
		"google-generative-ai": func(t *testing.T, model, opts ai.ProviderHeaders) *ai.AssistantMessage {
			_, final := runFoldWire(t, googleSSE, func(baseURL string) *ai.AssistantMessage {
				m := &ai.Model{ID: "gemini-2.5-flash", Api: ai.APIGoogleGenerativeAI, Provider: "google", BaseURL: baseURL,
					Input: []string{"text"}, MaxTokens: 4096, Headers: model}
				return StreamGoogle(context.Background(), m, ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}),
					&GoogleOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "g-key", Headers: opts}}}).Result()
			})
			return final
		},
	}
	for adapter, run := range runners {
		for _, tc := range []struct {
			name  string
			index bool
		}{
			{"0", true}, {"4294967294", true},
			{"4294967295", false}, {"01", false}, {"1.5", false}, {"-1", false}, {"+1", false},
		} {
			t.Run(adapter+"/"+tc.name, func(t *testing.T) {
				want := byteStringError(0, cjk)
				if tc.index {
					want = byteStringError(0, 0x100)
				}
				final := run(t, ai.ProviderHeaders{"X-A": &cjkValue}, ai.ProviderHeaders{tc.name: &lowValue})
				if final.StopReason != ai.StopError || final.ErrorMessage != want {
					t.Fatalf("stream = %s %q, want error %q", final.StopReason, final.ErrorMessage, want)
				}
			})
		}
	}
}

// Within one source "9" converts ahead of "10", where sorted name order would
// put "10" first; on every adapter.
func TestArrayIndexHeadersConvertInNumericOrder(t *testing.T) {
	opts := ai.ProviderHeaders{"9": strPtr(string(rune(cjk))), "10": strPtr(string(rune(0x100)))}
	for _, adapter := range wireAdapters() {
		t.Run(adapter.name, func(t *testing.T) {
			h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k", Headers: opts}})
			wantNotSent(t, h, final, byteStringError(0, cjk))
		})
	}
}

// pi-messages spreads the record over `{authorization, accept,
// "content-type"}`, and fetch converts that object in its own order: an
// array-index name ahead of the literals, so ahead of the api key too.
func TestPiMessagesArrayIndexHeaderConvertsAheadOfTheLiterals(t *testing.T) {
	adapter := wireAdapters()[0]
	if adapter.name != "pi-messages" {
		t.Fatalf("wireAdapters()[0] = %s, want pi-messages", adapter.name)
	}
	for _, tc := range []struct {
		name    string
		key     string
		headers ai.ProviderHeaders
	}{
		{"ahead of the key", "k" + string(rune(cjk)), ai.ProviderHeaders{"1": strPtr(string(rune(0x100)))}},
		{"ahead of an overridden literal", "k", ai.ProviderHeaders{"1": strPtr(string(rune(0x100))), "authorization": strPtr(string(rune(cjk)))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, final := runWire(t, adapter, ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: tc.key, Headers: tc.headers}})
			wantNotSent(t, h, final, byteStringError(0, 0x100))
		})
	}
}

// The SDKs' <SDK>_CUSTOM_HEADERS objects enumerate the same way: a "1:" line
// converts ahead of the lines above it.
func TestCustomHeadersEnvArrayIndexConvertsFirst(t *testing.T) {
	lines := "X-A: " + string(rune(cjk)) + "\n1: " + string(rune(0x100))
	t.Run("openai", func(t *testing.T) {
		sdkEnvRow{env: map[string]string{"OPENAI_CUSTOM_HEADERS": lines}, err: byteStringError(0, 0x100)}.run(t, "openai-completions")
	})
	t.Run("anthropic", func(t *testing.T) {
		sdkEnvRow{env: map[string]string{"ANTHROPIC_CUSTOM_HEADERS": lines}, err: byteStringError(0, 0x100), payloads: 1}.run(t, "anthropic-messages")
	})
}
