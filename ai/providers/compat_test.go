package providers

import (
	"encoding/json"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// pi resolves every compat key independently — `model.compat?.<key> ?? <default>`
// — so one type-mismatched key cannot disturb its siblings, cannot turn itself
// into an explicit `false`, and an explicit `null` reads as absent. These tests
// pin that per-key contract on the wire (the request body), for all three
// compat readers.
//
// The DEFAULT-TRUE flags carry the load: on a default-false flag "skipped" and
// "set to the zero value" are indistinguishable, so only a flag whose default is
// true can tell a per-key fallback apart from Go's populate-then-zero behavior
// on a type error (encoding/json ALLOCATES the *bool for a mistyped key and
// leaves it pointing at false).

// ---- the shared per-key resolver ----

func TestCompatOverridesPerKey(t *testing.T) {
	// Blobs that must contribute nothing at all: every key resolution falls back.
	for _, blob := range []string{"", "null", `"nope"`, `[1,2]`, `{"a":false,`, `{`} {
		o := newCompatOverrides(json.RawMessage(blob))
		gotBool, gotStr := true, "keep"
		applyCompat(o, "a", &gotBool)
		applyCompat(o, "b", &gotStr)
		if !gotBool || gotStr != "keep" {
			t.Errorf("blob %q applied something: bool=%v str=%q", blob, gotBool, gotStr)
		}
	}

	o := newCompatOverrides(json.RawMessage(
		`{"good":false,"mistyped":"yes","null":null,"str":"x","strBad":7,"strNull":null,"list":[{"model":"m"}],"listBad":{"a":1}}`))

	// A key that decodes cleanly is applied, even to the zero value.
	gotBool := true
	if !applyCompat(o, "good", &gotBool) || gotBool {
		t.Errorf("clean false not applied: %v", gotBool)
	}
	// A type mismatch leaves the DEFAULT, not false.
	gotBool = true
	if applyCompat(o, "mistyped", &gotBool) || !gotBool {
		t.Errorf("mistyped key must leave the default true, got %v", gotBool)
	}
	// An explicit null is absent (pi's `??` triggers on null).
	gotBool = true
	if applyCompat(o, "null", &gotBool) || !gotBool {
		t.Errorf("null must leave the default true, got %v", gotBool)
	}
	// A key the blob does not carry at all.
	gotBool = true
	if applyCompat(o, "absent", &gotBool) || !gotBool {
		t.Errorf("absent key must leave the default true, got %v", gotBool)
	}

	// Same three rules for a string-valued key: a mistyped or null value must
	// not blank the detected default.
	gotStr := "keep"
	if !applyCompat(o, "str", &gotStr) || gotStr != "x" {
		t.Errorf("clean string not applied: %q", gotStr)
	}
	gotStr = "keep"
	if applyCompat(o, "strBad", &gotStr) || gotStr != "keep" {
		t.Errorf("mistyped string must leave the default: %q", gotStr)
	}
	gotStr = "keep"
	if applyCompat(o, "strNull", &gotStr) || gotStr != "keep" {
		t.Errorf("null string must leave the default: %q", gotStr)
	}

	// A structured key follows the same rules — a partially decodable value is
	// not trusted (encoding/json fills a slice element by element on a type
	// error, so a half-decoded list must be discarded whole).
	list := []anthropicAllowedFallbackModel{{Model: "default"}}
	if !applyCompat(o, "list", &list) || len(list) != 1 || list[0].Model != "m" {
		t.Errorf("clean list not applied: %+v", list)
	}
	list = []anthropicAllowedFallbackModel{{Model: "default"}}
	if applyCompat(o, "listBad", &list) || len(list) != 1 || list[0].Model != "default" {
		t.Errorf("mistyped list must leave the default: %+v", list)
	}
}

// ---- openai-responses ----

func TestResponsesCompatPerKeyResolution(t *testing.T) {
	tests := []struct {
		name          string
		compat        string
		wantMaxTokens bool   // max_output_tokens present (supportsMaxOutputTokens, default true)
		wantRole      string // system-prompt role (supportsDeveloperRole, default true → "developer")
		// prompt_cache_retention "24h" on a long-retention request
		// (supportsLongCacheRetention, default true — pi openai-responses.ts:72,86).
		wantLongCacheRetention bool
	}{
		{"clean blob applies every key",
			`{"supportsDeveloperRole":false,"supportsMaxOutputTokens":false,"supportsLongCacheRetention":false}`,
			false, "system", false},
		{"mistyped key leaves valid siblings intact",
			`{"supportsToolSearch":"yes","supportsMaxOutputTokens":false}`,
			false, "developer", true},
		{"mistyped key falls back to its default, not false",
			`{"supportsDeveloperRole":"yes","supportsMaxOutputTokens":"no","supportsLongCacheRetention":"no"}`,
			true, "developer", true},
		{"long-retention override alone drops prompt_cache_retention",
			`{"supportsLongCacheRetention":false}`,
			true, "developer", false},
		{"explicit null is absent",
			`{"supportsDeveloperRole":null,"supportsMaxOutputTokens":null,"supportsLongCacheRetention":null}`,
			true, "developer", true},
		{"invalid JSON applies nothing",
			`{"supportsDeveloperRole":false,"supportsMaxOutputTokens":false,"supportsLongCacheRetention":false`,
			true, "developer", true},
		{"non-object blob applies nothing", `"nope"`, true, "developer", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := reasoningModel()
			model.Compat = json.RawMessage(tc.compat)
			maxTokens := 1024
			body := mustBuildResponsesParams(t, model,
				ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)}},
				&OpenAIResponsesOptions{StreamOptions: ai.StreamOptions{
					MaxTokens: &maxTokens, CacheRetention: ai.CacheLong}})

			if _, has := body["max_output_tokens"]; has != tc.wantMaxTokens {
				t.Errorf("max_output_tokens present = %v, want %v", has, tc.wantMaxTokens)
			}
			// pi sends prompt_cache_retention: "24h" for a long-retention
			// request unless the model opts out — the only wire trace of
			// supportsLongCacheRetention on this provider.
			gotRetention, hasRetention := body["prompt_cache_retention"]
			if hasRetention != tc.wantLongCacheRetention {
				t.Errorf("prompt_cache_retention present = %v, want %v (body: %v)", hasRetention, tc.wantLongCacheRetention, body)
			}
			if hasRetention && gotRetention != "24h" {
				t.Errorf("prompt_cache_retention = %v, want 24h", gotRetention)
			}
			input, _ := body["input"].([]any)
			if len(input) == 0 {
				t.Fatalf("no input items: %v", body["input"])
			}
			first, _ := input[0].(map[string]any)
			if first["role"] != tc.wantRole {
				t.Errorf("system role = %v, want %q", first["role"], tc.wantRole)
			}
		})
	}
}

// ---- openai-completions ----

func openAICompletionsCompatModel(compat string) *ai.Model {
	m := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAICompletions, Provider: "openai", Reasoning: true, MaxTokens: 4096}
	if compat != "" {
		m.Compat = json.RawMessage(compat)
	}
	return m
}

func TestOpenAICompletionsCompatPerKeyResolution(t *testing.T) {
	tests := []struct {
		name string
		// detected defaults for provider "openai": supportsDeveloperRole true,
		// supportsStore true, maxTokensField "max_completion_tokens".
		compat         string
		wantRole       string
		wantTokenField string
		wantStore      bool
		// stream_options:{include_usage:true} (supportsUsageInStreaming,
		// detected true for every provider — pi openai-completions.ts:813).
		wantUsageInStreaming bool
		// `strict` on each function tool (supportsStrictMode, detected false for
		// every provider since 890f92088 — the catalog opts capable models in).
		wantStrict bool
		// Anthropic-style cache_control markers on the system prompt, the last
		// tool and the last message (cacheControlFormat, detected "" for
		// "openai" — pi openai-completions.ts:1057).
		wantAnthropicCacheControl bool
	}{
		{"clean blob applies every key",
			`{"supportsDeveloperRole":false,"maxTokensField":"max_tokens","supportsStore":false,` +
				`"supportsUsageInStreaming":false,"supportsStrictMode":false,"cacheControlFormat":"anthropic"}`,
			"system", "max_tokens", false, false, false, true},
		{"mistyped key leaves valid siblings intact",
			`{"supportsStore":"yes","maxTokensField":"max_tokens","supportsDeveloperRole":false}`,
			"system", "max_tokens", true, true, false, false},
		// supportsStrictMode is left out: its default is false since 890f92088 and
		// pi's `!== false` keeps a mistyped "yes" strict — docs/UPSTREAM.md D3.
		{"mistyped bool falls back to its default, not false",
			`{"supportsDeveloperRole":"yes","supportsStore":"yes","supportsUsageInStreaming":"yes"}`,
			"developer", "max_completion_tokens", true, true, false, false},
		{"mistyped string falls back to the detected default, not empty",
			`{"maxTokensField":false,"cacheControlFormat":7}`,
			"developer", "max_completion_tokens", true, true, false, false},
		{"cache-control format override alone reshapes the body",
			`{"cacheControlFormat":"anthropic"}`,
			"developer", "max_completion_tokens", true, true, false, true},
		// One-key rows for the two keys the clean blob would otherwise be the
		// sole guard for: a failure here names the key instead of handing you a
		// six-key body dump to bisect.
		{"usage-in-streaming override alone drops stream_options",
			`{"supportsUsageInStreaming":false}`,
			"developer", "max_completion_tokens", true, false, false, false},
		{"strict-mode override alone adds the tool strict field",
			`{"supportsStrictMode":true}`,
			"developer", "max_completion_tokens", true, true, true, false},
		{"explicit null is absent",
			`{"supportsDeveloperRole":null,"maxTokensField":null,"supportsStore":null,` +
				`"supportsUsageInStreaming":null,"supportsStrictMode":null,"cacheControlFormat":null}`,
			"developer", "max_completion_tokens", true, true, false, false},
		{"invalid JSON applies nothing",
			`{"supportsDeveloperRole":false,"maxTokensField":"max_tokens","supportsUsageInStreaming":false`,
			"developer", "max_completion_tokens", true, true, false, false},
		{"non-object blob applies nothing", `"nope"`,
			"developer", "max_completion_tokens", true, true, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			maxTokens := 1024
			body := mustBuildOpenAIParams(t, openAICompletionsCompatModel(tc.compat),
				ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)},
					Tools: []ai.Tool{compatProbeTool()}},
				&OpenAIOptions{StreamOptions: ai.StreamOptions{MaxTokens: &maxTokens}})

			if got := body[tc.wantTokenField]; got != maxTokens {
				t.Errorf("%s = %v, want %d (body: %v)", tc.wantTokenField, got, maxTokens, body)
			}
			for _, other := range []string{"max_tokens", "max_completion_tokens", ""} {
				if other == tc.wantTokenField {
					continue
				}
				if _, has := body[other]; has {
					t.Errorf("unexpected token field %q in body: %v", other, body)
				}
			}
			if _, has := body["store"]; has != tc.wantStore {
				t.Errorf("store present = %v, want %v", has, tc.wantStore)
			}
			if _, has := body["stream_options"]; has != tc.wantUsageInStreaming {
				t.Errorf("stream_options present = %v, want %v (body: %v)", has, tc.wantUsageInStreaming, body)
			}
			messages, _ := body["messages"].([]map[string]any)
			if len(messages) == 0 {
				t.Fatalf("no messages: %v", body["messages"])
			}
			if messages[0]["role"] != tc.wantRole {
				t.Errorf("system role = %v, want %q", messages[0]["role"], tc.wantRole)
			}

			tools, _ := body["tools"].([]map[string]any)
			if len(tools) != 1 {
				t.Fatalf("tools = %v, want exactly one", body["tools"])
			}
			fn, _ := tools[0]["function"].(map[string]any)
			if fn == nil {
				t.Fatalf("tool is not a function tool: %v", tools[0])
			}
			if _, has := fn["strict"]; has != tc.wantStrict {
				t.Errorf("tool strict present = %v, want %v (tool: %v)", has, tc.wantStrict, tools[0])
			}

			// cacheControlFormat "anthropic" rewrites the system prompt from a
			// bare string into a text block carrying cache_control, and marks
			// the last tool and the last conversation message the same way.
			if _, has := tools[0]["cache_control"]; has != tc.wantAnthropicCacheControl {
				t.Errorf("tool cache_control present = %v, want %v (tool: %v)", has, tc.wantAnthropicCacheControl, tools[0])
			}
			if got := textBlockCacheControl(messages[0]); got != tc.wantAnthropicCacheControl {
				t.Errorf("system prompt cache_control = %v, want %v (message: %v)", got, tc.wantAnthropicCacheControl, messages[0])
			}
			if got := textBlockCacheControl(messages[len(messages)-1]); got != tc.wantAnthropicCacheControl {
				t.Errorf("last message cache_control = %v, want %v (message: %v)", got, tc.wantAnthropicCacheControl, messages[len(messages)-1])
			}
		})
	}
}

// compatProbeTool is a plain function tool: it carries no constrained-sampling
// config, so `strict` resolves to false and the only thing its presence in the
// body proves is whether the provider was told it supports strict mode at all
// (pi emits the key only then — "some reject unknown fields").
func compatProbeTool() ai.Tool {
	return ai.Tool{Name: "probe", Description: "probe tool", Parameters: ai.Object(ai.Prop("q", ai.String()))}
}

// textBlockCacheControl reports whether a chat-completions message carries its
// content as text blocks with a cache_control marker (the anthropic
// cacheControlFormat shape); a plain string content has none.
func textBlockCacheControl(m map[string]any) bool {
	blocks, ok := m["content"].([]any)
	if !ok {
		return false
	}
	for _, b := range blocks {
		part, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if _, has := part["cache_control"]; has {
			return true
		}
	}
	return false
}

// sessionAffinityFormat has no body trace at all — it picks the shape of the
// session headers — so the completions reader for it is only observable on the
// wire. Both directions are pinned: the override must be able to turn the
// openai trio into openrouter's single header AND back again, so a key that
// stops being read reds whichever model it was detected against.
func TestOpenAICompletionsCompatSessionAffinityFormatOverride(t *testing.T) {
	tests := []struct {
		name             string
		provider         string
		compat           string
		wantSessionIDHdr string // x-session-id (openrouter shape)
		wantOpenAITrio   bool   // session_id + x-client-request-id + x-session-affinity
	}{
		{"detected openai shape", "openai",
			`{"sendSessionAffinityHeaders":true}`, "", true},
		{"override to openrouter on an openai model", "openai",
			`{"sendSessionAffinityHeaders":true,"sessionAffinityFormat":"openrouter"}`, "sess-1", false},
		{"detected openrouter shape", "openrouter",
			`{"sendSessionAffinityHeaders":true}`, "sess-1", false},
		{"override to openai on an openrouter model", "openrouter",
			`{"sendSessionAffinityHeaders":true,"sessionAffinityFormat":"openai"}`, "", true},
		{"mistyped override keeps the detected shape", "openrouter",
			`{"sendSessionAffinityHeaders":true,"sessionAffinityFormat":false}`, "sess-1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := openAITestModel()
			model.Provider = tc.provider
			model.Compat = json.RawMessage(tc.compat)
			h := captureOpenAIHeaders(t, model, ai.StreamOptions{
				ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "k"},
				SessionID:              "sess-1",
			})
			if got := h.Get("x-session-id"); got != tc.wantSessionIDHdr {
				t.Errorf("x-session-id = %q, want %q", got, tc.wantSessionIDHdr)
			}
			for _, name := range []string{"session_id", "x-client-request-id", "x-session-affinity"} {
				got := h.Get(name)
				want := ""
				if tc.wantOpenAITrio {
					want = "sess-1"
				}
				if got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// ---- anthropic-messages ----

func anthropicCompatModel(compat string) *ai.Model {
	m := &ai.Model{ID: "claude-x", Api: ai.APIAnthropicMessages, Provider: "anthropic", Input: []string{"text"}, MaxTokens: 100}
	if compat != "" {
		m.Compat = json.RawMessage(compat)
	}
	return m
}

func TestAnthropicCompatPerKeyResolution(t *testing.T) {
	const fallback = `[{"provider":"anthropic","model":"claude-y"}]`
	tests := []struct {
		name string
		// defaults: supportsTemperature true, allowedFallbackModels absent.
		compat          string
		wantTemperature bool
		wantFallbacks   bool
		// ttl on every cache_control marker of a long-retention request
		// (supportsLongCacheRetention, default true → "1h"; pi
		// anthropic-messages.ts:69,188).
		wantCacheTTL string
		// cache_control on the last tool (supportsCacheControlOnTools, default
		// true; pi anthropic-messages.ts:1048).
		wantToolCacheControl bool
	}{
		{"clean blob applies every key",
			`{"supportsTemperature":false,"allowedFallbackModels":` + fallback +
				`,"supportsLongCacheRetention":false,"supportsCacheControlOnTools":false}`, false, true, "", false},
		{"mistyped bool leaves valid sibling bools intact",
			`{"supportsCacheControlOnTools":"yes","supportsTemperature":false}`, false, false, "1h", true},
		{"mistyped bool leaves the fallback list intact",
			`{"supportsTemperature":"yes","allowedFallbackModels":` + fallback + `}`, true, true, "1h", true},
		{"mistyped fallback list leaves the bools intact",
			`{"supportsTemperature":false,"allowedFallbackModels":"nope"}`, false, false, "1h", true},
		{"mistyped key falls back to its default, not false",
			`{"supportsTemperature":"yes","supportsLongCacheRetention":"yes"}`, true, false, "1h", true},
		{"long-retention override alone drops the ttl, keeping the tool marker",
			`{"supportsLongCacheRetention":false}`, true, false, "", true},
		{"tool-cache override alone strips the tool marker, keeping the ttl",
			`{"supportsCacheControlOnTools":false}`, true, false, "1h", false},
		{"explicit null is absent",
			`{"supportsTemperature":null,"allowedFallbackModels":null,` +
				`"supportsLongCacheRetention":null,"supportsCacheControlOnTools":null}`, true, false, "1h", true},
		{"invalid JSON applies nothing",
			`{"supportsTemperature":false,"allowedFallbackModels":` + fallback, true, false, "1h", true},
		{"non-object blob applies nothing", `"nope"`, true, false, "1h", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			temperature := 0.5
			body := mustBuildAnthropicParams(t, anthropicCompatModel(tc.compat),
				ai.Context{SystemPrompt: "sys", Messages: []ai.Message{ai.NewUserText("hi", 1)},
					Tools: []ai.Tool{compatProbeTool()}}, false,
				&AnthropicOptions{StreamOptions: ai.StreamOptions{
					Temperature: &temperature, CacheRetention: ai.CacheLong}})

			if _, has := body["temperature"]; has != tc.wantTemperature {
				t.Errorf("temperature present = %v, want %v", has, tc.wantTemperature)
			}

			// The system block always carries a marker on a long-retention
			// request; supportsLongCacheRetention decides only its ttl.
			system, _ := body["system"].([]any)
			if len(system) != 1 {
				t.Fatalf("system = %v, want exactly one block", body["system"])
			}
			block, _ := system[0].(map[string]any)
			cc, _ := block["cache_control"].(*cacheControl)
			if cc == nil {
				t.Fatalf("system block carries no cache_control: %v", block)
			}
			if cc.TTL != tc.wantCacheTTL {
				t.Errorf("system cache_control ttl = %q, want %q", cc.TTL, tc.wantCacheTTL)
			}

			tools, _ := body["tools"].([]map[string]any)
			if len(tools) != 1 {
				t.Fatalf("tools = %v, want exactly one", body["tools"])
			}
			if _, has := tools[0]["cache_control"]; has != tc.wantToolCacheControl {
				t.Errorf("tool cache_control present = %v, want %v (tool: %v)", has, tc.wantToolCacheControl, tools[0])
			}
			got, has := body["fallbacks"]
			if has != tc.wantFallbacks {
				t.Errorf("fallbacks present = %v, want %v", has, tc.wantFallbacks)
			}
			if has {
				want := []anthropicFallbackWire{{Model: "claude-y"}}
				if list, ok := got.([]anthropicFallbackWire); !ok || len(list) != 1 || list[0] != want[0] {
					t.Errorf("fallbacks = %#v, want %#v", got, want)
				}
			}
		})
	}
}
