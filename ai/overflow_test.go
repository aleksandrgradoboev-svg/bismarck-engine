package ai

import "testing"

// Ported from pi packages/ai/test/overflow.test.ts (snapshot d2e7d8b7).

func overflowErrorMessage(text string, provider ProviderId) AssistantMessage {
	if provider == "" {
		provider = "ollama"
	}
	return AssistantMessage{Api: "openai-completions", Provider: provider, Model: "qwen3.5:35b", StopReason: StopError, ErrorMessage: text}
}

type lengthStop struct {
	input, cacheRead, output, cacheWrite int
}

func lengthStopMessage(o lengthStop) AssistantMessage {
	return AssistantMessage{
		Api: "openai-completions", Provider: "test-provider", Model: "test-model", StopReason: StopLength,
		Usage: Usage{Input: o.input, Output: o.output, CacheRead: o.cacheRead, CacheWrite: o.cacheWrite, TotalTokens: o.input + o.cacheRead + o.cacheWrite + o.output},
	}
}

func TestIsContextOverflowErrors(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		provider ProviderId
		window   int
		want     bool
	}{
		{"detects explicit Ollama prompt-too-long errors", "400 `prompt too long; exceeded max context length by 100918 tokens`", "", 32768, true},
		// Regression for #9805.
		{"detects z.ai prompt-too-long errors", `400 {"code":"1261","message":"Prompt too long"}`, "zai", 1048576, true},
		// Regression for #10208.
		{"detects z.ai CN endpoint prompt-exceeds-max-length errors", `400 {"code":"1261","message":"Prompt exceeds max length"}`, "zai", 1048576, true},
		{"detects Together AI context length errors", "400 The input (516368 tokens) is longer than the model's context length (262144 tokens).", "", 262144, true},
		{"detects LiteLLM-wrapped OpenAI maximum context length errors", "Error: 503 litellm.ServiceUnavailableError: litellm.MidStreamFallbackError: litellm.APIConnectionError: APIConnectionError: OpenAIException - Requested token count exceeds the model's maximum context length of 131072 tokens.", "", 131072, true},
		// Regression for #11492 (pi d3398f82).
		{"detects LiteLLM pre-call ContextWindowExceededError", "400: {\"message\":\"litellm.ContextWindowExceededError: litellm.BadRequestError: litellm._pre_call_checks: Context Window exceeded for given call. No models have context window large enough for this call.\nModel=Qwen3.8-Flash-Next, Max Input Tokens=262144, Got=265262\nmodel=Qwen3.8-Flash-Next. context_window_fallbacks=None. fallbacks=[].\"}", "", 262144, true},
		{"detects OpenAI-compatible parenthesized maximum context length errors", "Error: 400 Input length (265330) exceeds model's maximum context length (262144).", "", 262144, true},
		{"detects OpenRouter Poolside maximum allowed input length errors", "Provider returned error: Input length 131393 exceeds the maximum allowed input length of 131040 tokens.", "", 131072, true},
		{"detects DS4 configured context size errors", "400 Prompt has 256468 tokens, but the configured context size is 256000 tokens", "", 256000, true},
		{"detects DS4 configured context size errors with commas", "Prompt has 5,958,968 tokens, but the configured context size is 256,000 tokens", "", 256000, true},
		{"does not treat generic non-overflow Ollama errors as overflow", "500 `model runner crashed unexpectedly`", "", 32768, false},
		// Bedrock returns this for HTTP 429 rate limiting, not context overflow.
		{"does not treat Bedrock throttling 'Too many tokens' as overflow", "Throttling error: Too many tokens, please wait before trying again.", "", 200000, false},
		{"does not treat Bedrock service unavailable as overflow", "Service unavailable: The service is temporarily unavailable.", "", 200000, false},
		{"does not treat generic rate limit errors as overflow", "Rate limit exceeded, please retry after 30 seconds.", "", 200000, false},
		{"does not treat HTTP 429 style errors as overflow", "Too many requests. Please slow down.", "", 200000, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsContextOverflow(overflowErrorMessage(c.text, c.provider), c.window); got != c.want {
				t.Fatalf("IsContextOverflow(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}

// Regression for #9482. HTTP 413 is a request-size error for every provider,
// but a bodyless HTTP 400 is only known to indicate overflow for Cerebras.
func TestIsContextOverflowBodylessOnlyForCerebras(t *testing.T) {
	const text = "400 status code (no body)"
	if !IsContextOverflow(overflowErrorMessage(text, "cerebras"), 131072) {
		t.Fatalf("cerebras %q is not an overflow", text)
	}
	if IsContextOverflow(overflowErrorMessage(text, "opencode-go"), 1000000) {
		t.Fatalf("opencode-go %q is an overflow", text)
	}
}

// The 413 pattern is provider-agnostic, so a single Copilot-prefixed string and
// a single non-Copilot string cover both branches of the regex; the bare forms
// cover the raw SDK output (pi 73f8de80).
func TestIsContextOverflowHTTP413(t *testing.T) {
	for _, text := range []string{
		`github-copilot API error (413): {"message":"failed to parse request","code":""}`, // formatProviderError prefix
		"custom-provider API error (413): Payload Too Large",                              // any provider, not just Copilot
		`413 {"error":{"message":"failed to parse request","code":""}}`,                   // raw SDK error
		"413", // bare status at end of string
	} {
		// The request can exceed the byte limit even with zero usage and a large token window.
		if !IsContextOverflow(overflowErrorMessage(text, ""), 1_000_000) {
			t.Errorf("%q with a 1M window is not an overflow", text)
		}
		if !IsContextOverflow(overflowErrorMessage(text, ""), 0) {
			t.Errorf("%q without a window is not an overflow", text)
		}
	}
	for _, text := range []string{
		`github-copilot API error (400): {"message":"failed to parse request","code":""}`, // other status, same body
		"custom-provider API error (429): Too many requests",
		`github-copilot API error (400): {"message":"failed to parse request: invalid value 413"}`,  // 413 in the body only
		"custom-provider API error (400): upstream mentioned OpenAI API error (413): invalid input", // 413 not at start
		"custom-provider API error (4130): invalid input",                                           // 413 is a prefix, not the status
		"4130 failed to parse request",                                                              // bare 4130 is not 413
	} {
		if IsContextOverflow(overflowErrorMessage(text, ""), 0) {
			t.Errorf("%q is an overflow", text)
		}
	}
	// pi "limits HTTP-status matching to failed responses".
	message := overflowErrorMessage(`OpenAI API error (413): {"message":"failed to parse request"}`, "")
	for _, stop := range []StopReason{StopAborted, StopStop} {
		message.StopReason = stop
		if IsContextOverflow(message, 0) {
			t.Errorf("stop %s is an overflow", stop)
		}
	}
}

// The ways the 413 pattern's JavaScript semantics differ from RE2's: \s takes
// the vertical tab, no-break space and BOM, and "i" folds the letters of
// "API error" but lets no Unicode fold into \w. Beyond pi.
func TestHTTPRequestSizeOverflowPatternJavaScriptSemantics(t *testing.T) {
	r := func(code rune) string { return string(code) }
	for text, want := range map[string]bool{
		"413\vfailed":                     true,
		"413" + r(0xa0) + "failed":        true,  // NO-BREAK SPACE
		"413" + r(0xfeff) + "failed":      true,  // BOM
		"413" + r(0x85) + "failed":        false, // NEL is not \s in JavaScript
		"x api ERROR (413): y":            true,
		r(0x212a) + " API error (413): y": false, // KELVIN SIGN is not \w
		r(0x17f) + " API error (413): y":  false, // LONG S is not \w
		"API error (413): y":              false, // [\w -]+ needs a character
		"github copilot API error (413):": true,
	} {
		if got := httpRequestSizeOverflowPattern.MatchString(text); got != want {
			t.Errorf("match %+q = %v, want %v", text, got, want)
		}
	}
}

func TestIsContextOverflowLengthStops(t *testing.T) {
	xiaomi := lengthStopMessage(lengthStop{input: 58, cacheRead: 1048512})
	xiaomi.Provider, xiaomi.Model = "xiaomi", "mimo-v2.6-pro"
	if !IsContextOverflow(xiaomi, 1048576) {
		t.Fatal("a length stop with zero output and a filled context is not an overflow")
	}
	if IsContextOverflow(lengthStopMessage(lengthStop{input: 1000, output: 4096}), 200000) {
		t.Fatal("a normal length stop with output is an overflow")
	}
	if IsContextOverflow(lengthStopMessage(lengthStop{input: 100}), 200000) {
		t.Fatal("a zero-output length stop far below the context is an overflow")
	}
}

func TestIsRecoverableLength(t *testing.T) {
	below := lengthStopMessage(lengthStop{input: 3, cacheRead: 253584, cacheWrite: 25554, output: 16})
	below.Api, below.Provider, below.Model = "openai-responses", "openai", "gpt-5.6-sol"
	if !IsRecoverableLength(below, 128000) {
		t.Fatal("a length stop below the desired output limit is not recoverable")
	}
	if IsRecoverableLength(lengthStopMessage(lengthStop{input: 4062, output: 1024}), 1024) {
		t.Fatal("a length stop that reached the desired output limit is recoverable")
	}
	if !IsRecoverableLength(lengthStopMessage(lengthStop{input: 100}), 128000) {
		t.Fatal("a zero-output length stop is not recoverable without context metadata")
	}
}

// Beyond pi: the silent overflow of a clean stop (z.ai) and the absent window,
// which disables both usage-based checks.
func TestIsContextOverflowSilentStop(t *testing.T) {
	stop := AssistantMessage{StopReason: StopStop, Usage: Usage{Input: 900, CacheRead: 200}}
	if !IsContextOverflow(stop, 1000) {
		t.Fatal("a stop whose input and cache reads exceed the window is not an overflow")
	}
	if IsContextOverflow(stop, 0) {
		t.Fatal("a stop is an overflow without a window")
	}
	if IsContextOverflow(lengthStopMessage(lengthStop{input: 58, cacheRead: 1048512}), 0) {
		t.Fatal("a length stop is an overflow without a window")
	}
}
