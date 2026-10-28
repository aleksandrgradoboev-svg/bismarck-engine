package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// Anthropic "prefer" strict tools fall back once to their original non-strict
// schemas when the provider rejects the strict tool set for its limits (pi
// d4d53612, #10409). The cases transliterate
// packages/ai/test/anthropic-strict-tool-fallback.test.ts at e3bc05e8.

const strictGrammarError = "The compiled grammar is too large, which would cause performance issues. Simplify your tool schemas or reduce the number of strict tools."

func strictFallbackModel(baseURL string) *ai.Model {
	return &ai.Model{
		ID: "claude-haiku-4-5", Name: "Test Claude", Api: ai.APIAnthropicMessages, Provider: "anthropic",
		BaseURL: baseURL, Input: []string{"text"}, ContextWindow: 200000, MaxTokens: 100,
		Compat: json.RawMessage(`{"supportsStrictTools":true}`),
	}
}

func strictFallbackTool(name string, strict ai.ConstrainedSamplingStrictness) ai.Tool {
	t := ai.Tool{
		Name: name, Description: "Look up a value",
		Parameters: ai.Object(ai.Prop("value", ai.String()), ai.Opt("optional", ai.Number())),
	}
	if strict != "" {
		t.ConstrainedSampling = &ai.ConstrainedSamplingConfig{Type: ai.ConstrainedSamplingJSONSchema, Strict: strict}
	}
	return t
}

func strictFallbackContext(tools ...ai.Tool) ai.TranscriptContext {
	return ai.NormalizeContext(ai.Context{Tools: tools, Messages: []ai.Message{ai.NewUserText("Hello", 1)}})
}

func strictFallbackErrorBody(message, errType string) string {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": errType, "message": message}})
	return string(body)
}

const strictFallbackSuccessSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"model\":\"claude-haiku-4-5\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// strictFallbackHarness is the suite's harness: a server that records each
// request body and answers attempt n (from 1) with respond.
type strictFallbackHarness struct {
	server   *httptest.Server
	mu       sync.Mutex
	payloads []map[string]any
}

type strictFallbackReply struct {
	status int
	body   string
}

func newStrictFallbackHarness(t *testing.T, respond func(attempt int) strictFallbackReply) *strictFallbackHarness {
	t.Helper()
	h := &strictFallbackHarness{}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		h.mu.Lock()
		h.payloads = append(h.payloads, payload)
		attempt := len(h.payloads)
		h.mu.Unlock()
		reply := respond(attempt)
		if reply.status == 0 {
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, reply.body)
			return
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(reply.status)
		io.WriteString(w, reply.body)
	}))
	t.Cleanup(h.server.Close)
	return h
}

func (h *strictFallbackHarness) bodies() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.payloads...)
}

func strictFallbackOptions() *AnthropicOptions {
	return apiKeyOptions("test-key")
}

func strictFallbackRejectFirst(attempt int) strictFallbackReply {
	if attempt == 1 {
		return strictFallbackReply{400, strictFallbackErrorBody(strictGrammarError, "invalid_request_error")}
	}
	return strictFallbackReply{body: strictFallbackSuccessSSE}
}

func payloadTools(payload map[string]any) []map[string]any {
	return jsonItems(payload["tools"])
}

func expectJSONEqual(t *testing.T, what string, got any, want string) {
	t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, w) {
		g, _ := json.Marshal(got)
		t.Fatalf("%s = %s, want %s", what, g, want)
	}
}

func TestAnthropicPreferredStrictToolsFallBackToOriginalSchemas(t *testing.T) {
	for _, message := range []string{
		strictGrammarError,
		// Observed from the live API: 32 strict tools, then 20 tools with 24
		// optional fields each.
		"Too many strict tools (32). The maximum number of strict tools supported is 20. Try reducing the number of tools marked as strict.",
		"Schemas contains too many parameters with union types (480 parameters with type arrays or anyOf). This causes exponential compilation cost. Reduce the number of nullable or union-typed parameters (limit: 16 parameters with unions).",
	} {
		t.Run(message[:24], func(t *testing.T) {
			h := newStrictFallbackHarness(t, func(attempt int) strictFallbackReply {
				if attempt == 1 {
					return strictFallbackReply{400, strictFallbackErrorBody(message, "invalid_request_error")}
				}
				return strictFallbackReply{body: strictFallbackSuccessSSE}
			})
			transcript := strictFallbackContext(
				strictFallbackTool("preferred", ai.ConstrainedSamplingPrefer),
				strictFallbackTool("required", ai.ConstrainedSamplingRequire),
				strictFallbackTool("ordinary", ""),
			)
			original, _ := json.Marshal(transcript)
			onPayload, onResponse := 0, 0
			opts := strictFallbackOptions()
			opts.CacheRetention = ai.CacheLong
			opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
				onPayload++
				next := map[string]any{}
				for k, v := range payload.(map[string]any) {
					next[k] = v
				}
				next["inference_geo"] = "us"
				next["stream"] = false
				return next, nil
			}
			opts.OnResponse = func(ai.ProviderResponse, *ai.Model) error {
				onResponse++
				return nil
			}
			s := StreamAnthropic(context.Background(), strictFallbackModel(h.server.URL), transcript, opts)
			var events []string
			for ev := range s.Events() {
				events = append(events, string(ev.Type))
			}
			result := s.Result()

			if result.StopReason != ai.StopStop {
				t.Fatalf("stop = %s (%q)", result.StopReason, result.ErrorMessage)
			}
			if want := []string{"start", "done"}; !reflect.DeepEqual(events, want) {
				t.Fatalf("events = %v, want %v", events, want)
			}
			payloads := h.bodies()
			if len(payloads) != 2 || onPayload != 2 || onResponse != 1 {
				t.Fatalf("requests %d, onPayload %d, onResponse %d; want 2, 2, 1", len(payloads), onPayload, onResponse)
			}
			first, second := payloadTools(payloads[0]), payloadTools(payloads[1])
			if first[0]["strict"] != true {
				t.Fatalf("first[0].strict = %v, want true", first[0]["strict"])
			}
			schema := first[0]["input_schema"].(map[string]any)
			expectJSONEqual(t, "first[0].input_schema.required", schema["required"], `["value","optional"]`)
			expectJSONEqual(t, "first[0] optional", schema["properties"].(map[string]any)["optional"],
				`{"anyOf":[{"type":"number"},{"type":"null"}]}`)
			if _, ok := second[0]["strict"]; ok {
				t.Fatalf("second[0].strict = %v, want undefined", second[0]["strict"])
			}
			expectJSONEqual(t, "second[0].input_schema", second[0]["input_schema"],
				`{"type":"object","properties":{"value":{"type":"string"},"optional":{"type":"number"}},"required":["value"]}`)
			if !reflect.DeepEqual(second[1], first[1]) || second[1]["strict"] != true {
				t.Fatalf("second[1] = %v, want first[1] (strict) %v", second[1], first[1])
			}
			if !reflect.DeepEqual(second[2], first[2]) {
				t.Fatalf("second[2] = %v, want first[2] %v", second[2], first[2])
			}
			expectJSONEqual(t, "second[2].cache_control", second[2]["cache_control"], `{"type":"ephemeral","ttl":"1h"}`)
			for i, payload := range payloads {
				if payload["stream"] != true {
					t.Fatalf("payloads[%d].stream = %v, want true", i, payload["stream"])
				}
			}
			if payloads[1]["inference_geo"] != "us" {
				t.Fatalf("payloads[1].inference_geo = %v, want us", payloads[1]["inference_geo"])
			}
			if after, _ := json.Marshal(transcript); string(after) != string(original) {
				t.Fatal("the transcript changed")
			}
			if len(result.Diagnostics) == 0 || result.Diagnostics[0].Type != "anthropic_strict_tools_fallback" {
				t.Fatalf("diagnostics = %+v, want anthropic_strict_tools_fallback first", result.Diagnostics)
			}
			d := result.Diagnostics[0]
			if reason, _ := d.Details.Get("reason"); reason != "strict_tool_limits_exceeded" {
				t.Fatalf("details.reason = %v", reason)
			}
			if d.Error == nil || !strings.Contains(d.Error.Message, message) || !strings.HasPrefix(d.Error.Message, "400 ") {
				t.Fatalf("diagnostic error = %+v, want the APIError message", d.Error)
			}
		})
	}
}

func TestAnthropicStrictFallbackDowngradesInlineAdditionsWithoutDowngradingRequire(t *testing.T) {
	h := newStrictFallbackHarness(t, strictFallbackRejectFirst)
	transcript := strictFallbackContext(strictFallbackTool("lookup", ai.ConstrainedSamplingRequire))
	prefer := ai.NewSystemText("", 2)
	prefer.ToolsAdded = []ai.Tool{strictFallbackTool("lookup", ai.ConstrainedSamplingPrefer)}
	require := ai.NewSystemText("", 3)
	require.ToolsAdded = []ai.Tool{strictFallbackTool("lookup", ai.ConstrainedSamplingRequire)}
	transcript.Messages = append(transcript.Messages, prefer, require)
	model := strictFallbackModel(h.server.URL)
	model.Compat = json.RawMessage(`{"supportsStrictTools":true,"supportsMidConvoSystemMessages":true,"supportsMidConvoToolChanges":true}`)

	result := StreamAnthropic(context.Background(), model, transcript, apiKeyOptions("sk-ant-oat-test")).Result()
	if result.StopReason != ai.StopStop {
		t.Fatalf("stop = %s (%q)", result.StopReason, result.ErrorMessage)
	}
	payloads := h.bodies()
	if len(payloads) != 2 {
		t.Fatalf("%d requests, want 2", len(payloads))
	}
	if !reflect.DeepEqual(payloads[1]["tools"], payloads[0]["tools"]) {
		t.Fatalf("tools changed: %v, then %v", payloads[0]["tools"], payloads[1]["tools"])
	}
	additions := make([][]map[string]any, len(payloads))
	for i, payload := range payloads {
		for _, message := range jsonItems(payload["messages"]) {
			for _, block := range jsonItems(message["content"]) {
				if block["type"] == "tool_addition" {
					if definition := toolDefinition(block); definition != nil {
						additions[i] = append(additions[i], definition)
					}
				}
			}
		}
	}
	strictness := func(definitions []map[string]any) []any {
		var out []any
		for _, definition := range definitions {
			out = append(out, definition["strict"])
		}
		return out
	}
	if got := strictness(additions[0]); !reflect.DeepEqual(got, []any{true, true}) {
		t.Fatalf("first additions strict = %v, want [true true]", got)
	}
	if got := strictness(additions[1]); !reflect.DeepEqual(got, []any{nil, true}) {
		t.Fatalf("second additions strict = %v, want [undefined true]", got)
	}
	expectJSONEqual(t, "second additions[0].input_schema.required",
		additions[1][0]["input_schema"].(map[string]any)["required"], `["value"]`)
	if !reflect.DeepEqual(additions[1][1], additions[0][1]) {
		t.Fatalf("require addition changed: %v, then %v", additions[0][1], additions[1][1])
	}
}

func TestAnthropicStrictFallbackDoesNotRetryWithoutAPreferredStrictTool(t *testing.T) {
	unsupported := strictFallbackTool("lookup", ai.ConstrainedSamplingPrefer)
	minimum := 1.0
	value := ai.Number()
	value.Minimum = &minimum
	unsupported.Parameters = ai.Object(ai.Prop("value", value))
	for name, tool := range map[string]ai.Tool{
		"required":                   strictFallbackTool("lookup", ai.ConstrainedSamplingRequire),
		"ordinary":                   strictFallbackTool("lookup", ""),
		"locally unsupported prefer": unsupported,
	} {
		t.Run(name, func(t *testing.T) {
			h := newStrictFallbackHarness(t, func(int) strictFallbackReply {
				return strictFallbackReply{400, strictFallbackErrorBody(strictGrammarError, "invalid_request_error")}
			})
			result := StreamAnthropic(context.Background(), strictFallbackModel(h.server.URL), strictFallbackContext(tool), strictFallbackOptions()).Result()
			if result.StopReason != ai.StopError || !strings.Contains(result.ErrorMessage, strictGrammarError) {
				t.Fatalf("stop = %s (%q), want error with the grammar message", result.StopReason, result.ErrorMessage)
			}
			if n := len(h.bodies()); n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
		})
	}
}

func TestAnthropicStrictFallbackDoesNotDowngradeOtherRejections(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		status        int
		errType       string
	}{
		{"unrelated 400", "Invalid tool schema", 400, "invalid_request_error"},
		{"wrong error type", strictGrammarError, 400, "authentication_error"},
		{"wrong HTTP status", strictGrammarError, 500, "invalid_request_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newStrictFallbackHarness(t, func(int) strictFallbackReply {
				return strictFallbackReply{tc.status, strictFallbackErrorBody(tc.message, tc.errType)}
			})
			result := StreamAnthropic(context.Background(), strictFallbackModel(h.server.URL),
				strictFallbackContext(strictFallbackTool("lookup", ai.ConstrainedSamplingPrefer)), strictFallbackOptions()).Result()
			if result.StopReason != ai.StopError {
				t.Fatalf("stop = %s, want error", result.StopReason)
			}
			if n := len(h.bodies()); n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
		})
	}
}

func TestAnthropicStrictFallbackSurfacesASecondRejection(t *testing.T) {
	h := newStrictFallbackHarness(t, func(attempt int) strictFallbackReply {
		message := strictGrammarError
		if attempt > 1 {
			message = "Required grammar is still too large"
		}
		return strictFallbackReply{400, strictFallbackErrorBody(message, "invalid_request_error")}
	})
	result := StreamAnthropic(context.Background(), strictFallbackModel(h.server.URL), strictFallbackContext(
		strictFallbackTool("lookup", ai.ConstrainedSamplingPrefer), strictFallbackTool("required", ai.ConstrainedSamplingRequire),
	), strictFallbackOptions()).Result()
	if result.StopReason != ai.StopError || !strings.Contains(result.ErrorMessage, "Required grammar is still too large") {
		t.Fatalf("stop = %s (%q)", result.StopReason, result.ErrorMessage)
	}
	payloads := h.bodies()
	if len(payloads) != 2 || payloadTools(payloads[1])[1]["strict"] != true {
		t.Fatalf("%d requests, second tools %v; want 2 with required strict", len(payloads), payloads[len(payloads)-1]["tools"])
	}
}

func TestAnthropicStrictFallbackDoesNotDowngradeAHookError(t *testing.T) {
	h := newStrictFallbackHarness(t, func(int) strictFallbackReply { return strictFallbackReply{body: strictFallbackSuccessSSE} })
	opts := strictFallbackOptions()
	opts.OnPayload = func(any, *ai.Model) (any, error) {
		return nil, fmt.Errorf("400 %s", strictFallbackErrorBody(strictGrammarError, "invalid_request_error"))
	}
	result := StreamAnthropic(context.Background(), strictFallbackModel(h.server.URL),
		strictFallbackContext(strictFallbackTool("lookup", ai.ConstrainedSamplingPrefer)), opts).Result()
	if result.StopReason != ai.StopError || len(h.bodies()) != 0 || result.Diagnostics != nil {
		t.Fatalf("stop = %s, %d requests, diagnostics %+v; want error, 0, none", result.StopReason, len(h.bodies()), result.Diagnostics)
	}
}

func TestAnthropicStrictFallbackHonorsCancellationBeforeTheFallbackAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newStrictFallbackHarness(t, func(int) strictFallbackReply {
		cancel()
		return strictFallbackReply{400, strictFallbackErrorBody(strictGrammarError, "invalid_request_error")}
	})
	result := StreamAnthropic(ctx, strictFallbackModel(h.server.URL),
		strictFallbackContext(strictFallbackTool("lookup", ai.ConstrainedSamplingPrefer)), strictFallbackOptions()).Result()
	if result.StopReason != ai.StopAborted {
		t.Fatalf("stop = %s (%q), want aborted", result.StopReason, result.ErrorMessage)
	}
	if n := len(h.bodies()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

func TestAnthropicStrictFallbackDoesNotRetryAnSSEErrorAfterResponseHeaders(t *testing.T) {
	h := newStrictFallbackHarness(t, func(int) strictFallbackReply {
		return strictFallbackReply{body: "event: error\ndata: " + strictFallbackErrorBody(strictGrammarError, "invalid_request_error") + "\n\n"}
	})
	result := StreamAnthropic(context.Background(), strictFallbackModel(h.server.URL),
		strictFallbackContext(strictFallbackTool("lookup", ai.ConstrainedSamplingPrefer)), strictFallbackOptions()).Result()
	if result.StopReason != ai.StopError {
		t.Fatalf("stop = %s, want error", result.StopReason)
	}
	if n := len(h.bodies()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

func TestAnthropicStrictFallbackDoesNotDisableStrictSamplingOnLaterRequests(t *testing.T) {
	h := newStrictFallbackHarness(t, strictFallbackRejectFirst)
	model := strictFallbackModel(h.server.URL)
	transcript := strictFallbackContext(strictFallbackTool("lookup", ai.ConstrainedSamplingPrefer))
	StreamAnthropic(context.Background(), model, transcript, strictFallbackOptions()).Result()
	StreamAnthropic(context.Background(), model, transcript, strictFallbackOptions()).Result()
	var got []any
	for _, payload := range h.bodies() {
		got = append(got, payloadTools(payload)[0]["strict"])
	}
	if want := []any{true, nil, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("strict per request = %v, want %v", got, want)
	}
}

// "preserves prefer through pi-messages so the gateway's Anthropic adapter can
// fall back": the request carries the transcript as is, "prefer" included. The
// engine has no decoder for a transcript's JSON, so the gateway's own Anthropic
// leg is the cases above rather than a round trip here.
func TestPiMessagesPreservesPreferSoTheGatewayCanFallBack(t *testing.T) {
	var request map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &request)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, `data: {"type":"done","reason":"stop","usage":`+piMessagesUsageJSON+"}\n\n")
	}))
	defer server.Close()
	transcript := strictFallbackContext(strictFallbackTool("lookup", ai.ConstrainedSamplingPrefer))
	opts := &PiMessagesOptions{}
	opts.APIKey = "radius-test"
	result := StreamPiMessages(context.Background(), piMessagesTestModel(server.URL), transcript, opts).Result()
	if result.StopReason != ai.StopStop {
		t.Fatalf("stop = %s (%q)", result.StopReason, result.ErrorMessage)
	}
	want, _ := json.Marshal(transcript)
	var got, wantJSON any
	_ = json.Unmarshal(request["context"], &got)
	_ = json.Unmarshal(want, &wantJSON)
	if !reflect.DeepEqual(got, wantJSON) {
		t.Fatalf("request context = %s, want %s", request["context"], want)
	}
	// A normalized context declares its tools on the leading system message.
	leading := jsonItems(got.(map[string]any)["messages"])[0]
	tools := jsonItems(leading["toolsAdded"])
	if len(tools) != 1 {
		t.Fatalf("leading message = %v, want one declared tool", leading)
	}
	expectJSONEqual(t, "toolsAdded[0].constrainedSampling", tools[0]["constrainedSampling"], `{"type":"json_schema","strict":"prefer"}`)
}

// Beyond pi's suite: the limit is matched in the whole APIError.message, as
// pi's `error.message.includes` reads it, not in the copy the engine truncates
// for display at maxProviderErrorBodyChars.
func TestAnthropicStrictFallbackMatchesTheLimitPastTheDisplayTruncation(t *testing.T) {
	long := strings.Repeat("x", maxProviderErrorBodyChars) + " " + strictGrammarError
	h := newStrictFallbackHarness(t, func(attempt int) strictFallbackReply {
		if attempt == 1 {
			return strictFallbackReply{400, strictFallbackErrorBody(long, "invalid_request_error")}
		}
		return strictFallbackReply{body: strictFallbackSuccessSSE}
	})
	result := StreamAnthropic(context.Background(), strictFallbackModel(h.server.URL),
		strictFallbackContext(strictFallbackTool("lookup", ai.ConstrainedSamplingPrefer)), strictFallbackOptions()).Result()
	if result.StopReason != ai.StopStop || len(h.bodies()) != 2 {
		t.Fatalf("stop = %s (%q), %d requests; want stop after 2", result.StopReason, result.ErrorMessage, len(h.bodies()))
	}
}
