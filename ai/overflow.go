package ai

import "regexp"

// Context-overflow detection, ported from pi packages/ai/src/utils/overflow.ts
// (snapshot d2e7d8b7). An overflow is a request larger than the model's
// context window; most providers report it as an error whose text matches one
// of overflowPatterns, some accept it and report it only through usage.
//
// Provider messages the patterns match, by provider:
//
//   - Anthropic: "prompt is too long: 213462 tokens > 200000 maximum", or
//     "request_too_large" (HTTP 413, request byte size)
//   - OpenAI: "Your input exceeds the context window of this model"
//   - OpenAI/LiteLLM: "Requested token count exceeds the model's maximum
//     context length of 131072 tokens"
//   - OpenAI-compatible: "Input length (265330) exceeds model's maximum context
//     length (262144)."
//   - Google: "The input token count (1196265) exceeds the maximum number of
//     tokens allowed (1048575)"
//   - xAI: "This model's maximum prompt length is 131072 but the request
//     contains 537812 tokens"
//   - Groq: "Please reduce the length of the messages or completion"
//   - OpenRouter: "This endpoint's maximum context length is X tokens. However,
//     you requested about Y tokens"
//   - OpenRouter/Poolside: "Input length X exceeds the maximum allowed input
//     length of Y tokens."
//   - Together AI: "The input (X tokens) is longer than the model's context
//     length (Y tokens)."
//   - llama.cpp: "the request exceeds the available context size, try
//     increasing it"
//   - LM Studio: "tokens to keep from the initial prompt is greater than the
//     context length"
//   - GitHub Copilot: "prompt token count of X exceeds the limit of Y"
//   - MiniMax: "invalid params, context window exceeds limit"
//   - Kimi For Coding: "Your request exceeded model token limit: X (requested:
//     Y)"
//   - DS4: "Prompt has X tokens, but the configured context size is Y tokens"
//   - Cerebras: "400/413 status code (no body)"
//   - Mistral: "Prompt contains X tokens ... too large for model with Y
//     maximum context length"
//   - z.ai: {"code":"1261","message":"Prompt too long"}, "Prompt exceeds max
//     length" (CN endpoint), or a silent overflow: usage input above the
//     context window
//   - Xiaomi MiMo: truncates the input to fill the context window and stops
//     with "length" and no output
//   - DashScope/Qwen: "Range of input length should be [1, X]"
//   - Ollama: some deployments truncate silently, others say "prompt too long;
//     exceeded max context length by X tokens"
var overflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)prompt (?:is )?too long`),                                                                   // Anthropic and z.ai token overflow
	regexp.MustCompile(`(?i)prompt exceeds max length`),                                                                 // z.ai CN endpoint token overflow
	regexp.MustCompile(`(?i)request_too_large`),                                                                         // Anthropic request byte-size overflow (HTTP 413)
	regexp.MustCompile(`(?i)input is too long for requested model`),                                                     // Amazon Bedrock
	regexp.MustCompile(`(?i)exceeds the context window`),                                                                // OpenAI (Completions & Responses API)
	regexp.MustCompile(`(?i)exceeds (?:the )?(?:model'?s )?maximum context length(?: of [\d,]+ tokens?|\s*\([\d,]+\))`), // OpenAI-compatible proxies (LiteLLM)
	regexp.MustCompile(`(?i)input token count.*exceeds the maximum`),                                                    // Google (Gemini)
	regexp.MustCompile(`(?i)maximum prompt length is \d+`),                                                              // xAI (Grok)
	regexp.MustCompile(`(?i)reduce the length of the messages`),                                                         // Groq
	regexp.MustCompile(`(?i)maximum context length is \d+ tokens`),                                                      // OpenRouter (most backends)
	regexp.MustCompile(`(?i)exceeds (?:the )?maximum allowed input length of [\d,]+ tokens?`),                           // OpenRouter/Poolside
	regexp.MustCompile(`(?i)input \(\d+ tokens\) is longer than the model'?s context length \(\d+ tokens\)`),            // Together AI
	regexp.MustCompile(`(?i)exceeds the limit of \d+`),                                                                  // GitHub Copilot
	regexp.MustCompile(`(?i)exceeds the available context size`),                                                        // llama.cpp server
	regexp.MustCompile(`(?i)greater than the context length`),                                                           // LM Studio
	regexp.MustCompile(`(?i)context window exceeds limit`),                                                              // MiniMax
	regexp.MustCompile(`(?i)exceeded model token limit`),                                                                // Kimi For Coding
	regexp.MustCompile(`(?i)too large for model with \d+ maximum context length`),                                       // Mistral
	regexp.MustCompile(`(?i)prompt has [\d,]+ tokens?, but the configured context size is [\d,]+ tokens?`),              // DS4 server
	regexp.MustCompile(`(?i)model_context_window_exceeded`),                                                             // z.ai non-standard finish_reason surfaced as error text
	regexp.MustCompile(`(?i)prompt too long; exceeded (?:max )?context length`),                                         // Ollama explicit overflow error
	regexp.MustCompile(`(?i)range of input length should be`),                                                           // DashScope / Qwen Token Plan
	regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded`),                                                             // Generic fallback
	regexp.MustCompile(`(?i)too many tokens`),                                                                           // Generic fallback
	regexp.MustCompile(`(?i)token limit exceeded`),                                                                      // Generic fallback
}

var cerebrasBodylessOverflowPattern = regexp.MustCompile(`(?i)^4(?:00|13)\s*(?:status code)?\s*\(no body\)`)

// nonOverflowPatterns exclude an error from overflow detection even when it
// matches an overflow pattern: Bedrock reports throttling as "ThrottlingException:
// Too many tokens, please wait before trying again.", which /too many tokens/
// would take for an overflow.
var nonOverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(Throttling error|Service unavailable):`), // AWS Bedrock non-overflow errors (formatBedrockError prefixes)
	regexp.MustCompile(`(?i)rate limit`),                               // Generic rate limiting
	regexp.MustCompile(`(?i)too many requests`),                        // Generic HTTP 429 style
}

// IsContextOverflow reports whether message is a context overflow: an error
// whose text matches a known provider overflow message and no non-overflow
// one; or, with contextWindow > 0 (0 is pi's absent window), a silent
// overflow, a clean stop whose input and cache reads exceed the window (z.ai),
// or a length stop with no output whose input fills 99% of it (Xiaomi MiMo).
//
// Detection is reliable for providers that report overflow with a matching
// error, and not for those that truncate silently without saying so (some
// Ollama setups). A custom provider whose message no pattern matches is not
// detected; check its ErrorMessage before calling this.
func IsContextOverflow(message AssistantMessage, contextWindow int) bool {
	if message.StopReason == StopError && message.ErrorMessage != "" {
		text := message.ErrorMessage
		if !matchesAny(nonOverflowPatterns, text) {
			if matchesAny(overflowPatterns, text) {
				return true
			}
			if message.Provider == "cerebras" && cerebrasBodylessOverflowPattern.MatchString(text) {
				return true
			}
		}
	}
	input := message.Usage.Input + message.Usage.CacheRead
	if contextWindow > 0 && message.StopReason == StopStop && input > contextWindow {
		return true
	}
	if contextWindow > 0 && message.StopReason == StopLength && message.Usage.Output == 0 && float64(input) >= float64(contextWindow)*0.99 {
		return true
	}
	return false
}

// IsRecoverableLength reports whether a length stop ended below
// desiredMaxOutput, the output limit before any context-based clamping: such
// a stop may come from context pressure or provider-side truncation, so a
// caller can make one bounded compact-and-retry attempt.
func IsRecoverableLength(message AssistantMessage, desiredMaxOutput int) bool {
	return message.StopReason == StopLength && desiredMaxOutput > 0 && message.Usage.Output < desiredMaxOutput
}

// OverflowPatterns returns a copy of the overflow patterns, for tests.
func OverflowPatterns() []*regexp.Regexp {
	return append([]*regexp.Regexp(nil), overflowPatterns...)
}

func matchesAny(patterns []*regexp.Regexp, text string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}
