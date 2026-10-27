package ai

import (
	"fmt"
	"regexp"
	"strings"
)

// providerErrorPattern is pi's buildProviderErrorPattern result,
// new RegExp(patterns.join("|"), "i"), matched with that RegExp's semantics
// rather than RE2's. A JavaScript RegExp without the "u" flag differs from RE2
// in two ways these patterns can observe:
//
//   - "i" compares the toUpperCase of each character but never lets a
//     non-ASCII character canonicalize to an ASCII one, so against all-ASCII
//     patterns it folds ASCII letters only. RE2's (?i) folds by Unicode, under
//     which U+212A KELVIN SIGN matches "k" and U+017F LATIN SMALL LETTER LONG S
//     matches "s". The alternatives are therefore compiled lowercased, without
//     (?i), and matched against the input with only A-Z lowercased.
//   - "." matches one UTF-16 code unit other than a line terminator (\n, \r,
//     U+2028, U+2029), where RE2's matches one code point other than \n. Every
//     "." in these lists sits between ASCII literals. A surrogate always has
//     its partner on one side, so it never sits between two ASCII characters,
//     and the code units such a "." can match are exactly the BMP code points:
//     an astral character is excluded, not counted as two. Go strings cannot
//     hold a lone surrogate; an invalid UTF-8 byte decodes as one U+FFFD,
//     which "." matches in both.
//
// Beyond literals, "." and "?", the alternatives use a few more constructs
// (pi 0fbdbd3f, 6e491cfa), each translated by translateProviderErrorAlternative:
// "(?:...)" groups with "|" inside, "\S" and "+", and the lookarounds
// "(?<![a-z0-9])" and "(?![a-z0-9])", which RE2 lacks.
type providerErrorPattern struct {
	source string // RegExp.prototype.source: the alternatives joined with "|"
	re     *regexp.Regexp
}

// jsDot is "." of a JavaScript RegExp without the "u" flag, as the RE2 class
// matching the same text where every "." is flanked by ASCII literals.
const jsDot = `[^\n\r\x{2028}\x{2029}\x{10000}-\x{10FFFF}]`

// jsSpaceChars are the characters of \s in a JavaScript RegExp, WhiteSpace and
// LineTerminator, as the inside of an RE2 class. RE2's \s is [\t\n\f\r ] only.
const jsSpaceChars = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// jsSpace is \s of a JavaScript RegExp. jsNonSpace is its \S: RE2's would also
// match \v, U+00A0, U+FEFF and the Unicode spaces. An astral character is two
// code units \S matches one by one, so "\S+" spans it as RE2 does a code point.
const (
	jsSpace    = `[` + jsSpaceChars + `]`
	jsNonSpace = `[^` + jsSpaceChars + `]`
)

// The lookarounds the alternatives use, and their RE2 stand-ins. A stand-in
// consumes the character the lookaround only looks at, so it tests the same
// only at the edge of an alternative, where nothing else of the alternative
// claims that character; RegExp.prototype.test asks nothing more than whether
// some match exists. "[a-z0-9]" under "i" is [A-Za-z0-9], which the
// lowercased input reduces to [a-z0-9]; no non-ASCII character folds into it
// without the "u" flag.
const (
	jsNotAfterAlnum   = `(?<![a-z0-9])`
	jsNotBeforeAlnum  = `(?![a-z0-9])`
	re2NotAfterAlnum  = `(?:^|[^a-z0-9])`
	re2NotBeforeAlnum = `(?:[^a-z0-9]|$)`
)

func buildProviderErrorPattern(patterns []string) providerErrorPattern {
	alternatives := make([]string, len(patterns))
	for i, pattern := range patterns {
		alternatives[i] = translateProviderErrorAlternative(pattern)
	}
	return providerErrorPattern{
		// RegExp.prototype.source escapes "/" (EscapeRegExpPattern), which
		// "http/?2 stream failed" carries since upstream 698a2d23.
		source: strings.ReplaceAll(strings.Join(patterns, "|"), "/", `\/`),
		re:     regexp.MustCompile(strings.Join(alternatives, "|")),
	}
}

// translateProviderErrorAlternative is one alternative as RE2 matching the
// same text as JavaScript's RegExp with "i" and without "u" does, against
// input lowercased by asciiLower. It panics on any syntax it was not taught:
// lowercasing would corrupt escapes and classes (\S is not \s), and RE2 and
// JavaScript disagree on several of them.
func translateProviderErrorAlternative(pattern string) string {
	unsupported := func(what string) {
		panic(fmt.Sprintf("ai: provider error pattern %q uses %s; "+
			"teach translateProviderErrorAlternative its JavaScript semantics before adding it", pattern, what))
	}
	var out strings.Builder
	rest := pattern
	if strings.HasPrefix(rest, jsNotAfterAlnum) {
		out.WriteString(re2NotAfterAlnum)
		rest = rest[len(jsNotAfterAlnum):]
	}
	suffix := ""
	if strings.HasSuffix(rest, jsNotBeforeAlnum) {
		suffix = re2NotBeforeAlnum
		rest = rest[:len(rest)-len(jsNotBeforeAlnum)]
	}
	depth := 0
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "(?:"):
			// A non-capturing group means the same in RE2 and JavaScript
			// (upstream 7bd01fb9: "provider.?returned.?(?:an.?)?error").
			out.WriteString("(?:")
			rest = rest[3:]
			depth++
			continue
		case strings.HasPrefix(rest, `\S`):
			out.WriteString(jsNonSpace)
			rest = rest[2:]
			continue
		case strings.HasPrefix(rest, jsNotAfterAlnum), strings.HasPrefix(rest, jsNotBeforeAlnum):
			unsupported("a lookaround away from the edge of the alternative")
		}
		c := rest[0]
		rest = rest[1:]
		switch c {
		case ')':
			if depth == 0 {
				unsupported(`an unbalanced ")"`)
			}
			depth--
			out.WriteByte(c)
		case '|':
			// At the top level "|" would split the alternative itself.
			if depth == 0 {
				unsupported(`"|" outside a group`)
			}
			out.WriteByte(c)
		case '?', '+':
			out.WriteByte(c)
		case '.':
			// jsDot is JavaScript's "." only between ASCII literals: next to
			// \S or a non-ASCII character, a code unit and a code point part.
			before := pattern[:len(pattern)-len(rest)-1]
			after := strings.TrimPrefix(rest, "?")
			if strings.HasSuffix(before, `\S`) || strings.HasSuffix(before, `\S+`) ||
				(before != "" && before[len(before)-1] >= 0x80) ||
				strings.HasPrefix(after, `\`) || (after != "" && after[0] >= 0x80) {
				unsupported(`"." beside something other than an ASCII literal`)
			}
			out.WriteString(jsDot)
		case '\\', '^', '$', '*', '(', '[', ']', '{', '}':
			unsupported(fmt.Sprintf("RegExp syntax %q", c))
		default:
			// Byte by byte: UTF-8 sequences pass through untouched.
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			out.WriteByte(c)
		}
	}
	if depth != 0 {
		unsupported(`an unclosed "(?:"`)
	}
	out.WriteString(suffix)
	return out.String()
}

// MatchString is RegExp.prototype.test on s.
func (p providerErrorPattern) MatchString(s string) bool {
	return p.re.MatchString(asciiLower(s))
}

// asciiLower lowercases A-Z and leaves every other byte as it is, so UTF-8
// sequences pass through untouched.
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// nonRetryableProviderLimitErrorPattern matches provider error text that
// indicates a subscription/account/billing limit rather than a transient
// failure. Matches here suppress retries.
var nonRetryableProviderLimitErrorPattern = buildProviderErrorPattern([]string{
	// OpenCode Go/free-tier limits returned as 429 JSON error types by OpenCode's
	// Zen API. These are subscription/account limits, not transient throttles.
	"GoUsageLimitError",
	"FreeUsageLimitError",

	// OpenCode Go subscription-limit text asks users to enable available-balance
	// usage after rolling/weekly/monthly limits are reached.
	"Monthly usage limit reached",
	"available balance",

	// Generic quota/budget/billing exhaustion. `insufficient_quota` is OpenAI's
	// quota/billing error code; the other strings cover common gateway wording.
	"insufficient_quota",
	"out of budget",
	"quota exceeded",
	"billing",

	// Sign in with ChatGPT: the subscription's shared usage limit, which resets
	// after hours rather than seconds.
	"subscription_sharing_usage_limit_exceeded",
	// Sign in with ChatGPT on a ChatGPT Business plan: a usage limit set by the
	// workspace admin. Only the admin can raise it (upstream cedefc53).
	"subscription_sharing_workplace_individual_usage_limit_exceeded",
})

// retryableProviderErrorPattern matches provider/transport error text that
// looks like a transient failure worth retrying.
var retryableProviderErrorPattern = buildProviderErrorPattern([]string{
	// Generic provider load, HTTP status, and server-side transient failures.
	"overloaded",
	"server_busy", // upstream 8b5708db
	"servers are currently busy",
	// Azure OpenAI's peak-load capacity refusal (#9669): a transient capacity
	// message, not an account limit.
	"currently experiencing high demand",
	"model is at capacity", // upstream 3874b3e9
	"rate.?limit",
	"too many requests",
	// 520 is Cloudflare's unknown-error status (#9627), 524 its origin timeout
	// (#6239). Status codes must not be part of a longer word or number, e.g. a
	// request id or byte count (#11057, upstream 6e491cfa).
	"(?<![a-z0-9])(?:429|500|502|503|504|520|524)(?![a-z0-9])",
	// Also matches "The service is temporarily unavailable" (#10965, upstream 550f1f3e).
	"service.?(?:is.?)?(?:temporarily.?)?unavailable",
	"server.?error",
	"internal.?error",

	// Wrapper/provider text for transient upstream failures, including OpenRouter
	// "Provider returned error" responses (#2264) and gateway wording such as
	// "The upstream provider returned an error" (#10842, upstream 7bd01fb9).
	"provider.?returned.?(?:an.?)?error",
	"exceeded request buffer limit while retrying upstream",
	// Gateway failure relaying the upstream response stream, sent as an OpenAI
	// Responses `response.failed` event without an error code (#10920,
	// upstream 7efebff1).
	"unable to forward upstream",
	// Other gateway upstream failures relayed as `response.failed` error codes
	// (#10920, upstream 698a2d23).
	"upstream.?request.?failed",
	"upstream_stream_read_error",
	"stream was interrupted",
	"http/?2 stream failed",

	// Network, proxy, and fetch transport failures. This includes OpenAI Codex
	// raw-fetch failures such as "upstream connect", "connection refused", and
	// "reset before headers" (#733), plus OpenRouter connection drops (#3317).
	"network.?error",
	"connection.?error",
	"connection.?refused",
	"connection.?lost",
	"other side closed",
	"fetch failed",
	// node-fetch system errors, e.g. the Google ADC OAuth token refresh used by
	// google-vertex. The reason is often empty (#10996, upstream 0fbdbd3f).
	"request to \\S+ failed, reason:",
	// DNS resolution failures surface as node/libuv transport errors (#6904).
	"getaddrinfo",
	"ENOTFOUND",
	"EAI_AGAIN",
	"upstream.?connect",
	"reset before headers",
	"socket hang up",
	"socket connection was closed",
	"timed? out",
	"timeout",
	"terminated",

	// WebSocket transports can report close/error text instead of HTTP/fetch text.
	"websocket.?closed",
	"websocket.?error",

	// Premature stream endings from SDKs and transports. Anthropic can throw
	// "stream ended without ..." and "Anthropic stream ended before message_stop"
	// (#4433); Bedrock/Smithy can throw an HTTP/2 no-response error (#3594).
	"ended without",
	"stream ended before message_stop",
	"stream ended before a terminal response event",
	"http2 request did not get a response",
	// Codex CLI mid-stream disconnect, relayed by proxies that wrap the Codex
	// CLI, e.g. "stream disconnected before completion: stream closed before
	// response.completed" (#11057, upstream 29057ece).
	"stream disconnected before completion",
	// Node ERR_HTTP2_STREAM_CANCEL: the HTTP/2 session died before the request was
	// sent, e.g. after the Bedrock SDK's 5-minute session timeout (#10379,
	// upstream 5b6c792b). Kept for parity with pi's classifier.
	"pending stream has been canceled",

	// Provider-requested retry delay cap failures should flow through the outer
	// retry policy so callers can surface/abort the backoff (#1123).
	"retry delay",

	// Explicit retry guidance emitted mid-stream by OpenAI Responses and Bedrock
	// stream exceptions (#6019).
	"you can retry your request",
	"try your request again",
	"please retry your request",

	// gRPC based providers (e.g. NVIDIA NIM) surface transient throttles as a
	// ResourceExhausted status (#6449).
	"ResourceExhausted",

	// Sign in with ChatGPT: usage or user data temporarily unavailable. Usage
	// failures can arrive mid-stream without an HTTP 503 in the message.
	"subscription_sharing_usage_unavailable",
	"subscription_sharing_user_unavailable",
})

// IsRetryableAssistantError classifies whether a failed assistant message looks
// like a transient provider or transport error, so callers can decide if the
// last assistant turn should be restarted.
//
// This does not implement retry policy. Callers should first handle context
// overflow separately, then apply their own retry budget, backoff, and reporting
// before restarting the assistant turn.
func IsRetryableAssistantError(m AssistantMessage) bool {
	if m.StopReason != StopError || m.ErrorMessage == "" {
		return false
	}
	if nonRetryableProviderLimitErrorPattern.MatchString(m.ErrorMessage) {
		return false
	}
	return retryableProviderErrorPattern.MatchString(m.ErrorMessage)
}
