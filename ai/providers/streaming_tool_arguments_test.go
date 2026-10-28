package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/internal/jstext"
)

// streamingToolArgumentsCaptureFile is written by
// testdata/streaming-tool-arguments/capture.mts, which streams tool calls
// through pi's providers from source at the named sha and records the partial
// arguments of every toolcall_delta (pi f715a15c, #9265).
const streamingToolArgumentsCaptureFile = "testdata/streaming-tool-arguments/streaming-tool-arguments-f715a15c.json"

type streamingToolArgumentsCapture struct {
	SHA   string `json:"sha"`
	Cases []struct {
		Name      string                                 `json:"name"`
		Chunk     int                                    `json:"chunk"`
		Text      string                                 `json:"text"`
		Providers map[string]streamingToolArgumentsTrace `json:"providers"`
	} `json:"cases"`
}

type streamingToolArgumentsTrace struct {
	Body string `json:"body"`
	// Starts are the arguments each toolcall_start showed.
	Starts []string `json:"starts"`
	Deltas []struct {
		Delta string `json:"delta"`
		Args  string `json:"args"`
	} `json:"deltas"`
	End          json.RawMessage `json:"end"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage *string         `json:"errorMessage"`
}

// streamingToolArgumentsModel is the capture's model for provider: only its
// api and ids matter, the stream is the canned body.
func streamingToolArgumentsModel(provider string) *ai.Model {
	model := &ai.Model{Input: []string{"text"}, ContextWindow: 200_000, MaxTokens: 64_000}
	switch provider {
	case "anthropic":
		model.ID, model.Name, model.Api, model.Provider, model.BaseURL = "claude-test", "c", ai.APIAnthropicMessages, "anthropic", "https://api.anthropic.com"
	case "completions":
		model.ID, model.Name, model.Api, model.Provider, model.BaseURL = "gpt-test", "g", ai.APIOpenAICompletions, "openai", "https://api.openai.com/v1"
	case "responses":
		model.ID, model.Name, model.Api, model.Provider, model.BaseURL = "gpt-test", "g", ai.APIOpenAIResponses, "openai", "https://api.openai.com/v1"
	case "piMessages":
		model.ID, model.Name, model.Api, model.Provider, model.BaseURL = "auto", "a", ai.APIPiMessages, "radius", "https://example.test/v1"
	}
	return model
}

// streamToolArguments streams body (faux: the arguments text) through the
// port's provider and returns what the capture records of it, when record
// is set; otherwise only the final message, so that what the stream costs
// can be measured without the cost of reading every partial.
func streamToolArguments(t *testing.T, provider, text, body string, record bool) (got streamedToolArguments) {
	t.Helper()
	req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("write", 0)}})
	var opts ai.StreamOptions
	opts.APIKey = "k"
	opts.HTTPClient = cannedDoer{status: http.StatusOK, body: body}
	var stream *ai.AssistantMessageEventStream
	switch provider {
	case "anthropic":
		stream = StreamAnthropic(context.Background(), streamingToolArgumentsModel(provider), req, &AnthropicOptions{StreamOptions: opts})
	case "completions":
		stream = StreamSimpleOpenAICompletions(context.Background(), streamingToolArgumentsModel(provider), req, &ai.SimpleStreamOptions{StreamOptions: opts})
	case "responses":
		stream = StreamSimpleOpenAIResponses(context.Background(), streamingToolArgumentsModel(provider), req, &ai.SimpleStreamOptions{StreamOptions: opts})
	case "piMessages":
		stream = StreamSimplePiMessages(context.Background(), streamingToolArgumentsModel(provider), req, &ai.SimpleStreamOptions{StreamOptions: opts})
	case "faux":
		reg := RegisterFauxProvider(RegisterFauxProviderOptions{MinTokenSize: 1, MaxTokenSize: 1})
		defer reg.Unregister()
		args, order, err := ai.DecodeOrderedObject(json.RawMessage(text))
		if err != nil {
			t.Fatalf("faux arguments %q: %v", text, err)
		}
		call := ai.ToolCall{ID: "call_1", Name: "write", Arguments: args, ArgumentsOrder: order}
		reg.SetResponses([]FauxResponseStep{FauxStatic(FauxAssistantMessage(ai.ContentList{call}, ai.StopToolUse))})
		stream = ai.StreamSimple(context.Background(), reg.GetModel(), ai.Context{Messages: []ai.Message{ai.NewUserText("write", 0)}}, nil)
	default:
		t.Fatalf("unknown provider %q", provider)
	}
	stringify := func(v any) string {
		s, err := jstext.Stringify(v)
		if err != nil {
			t.Fatalf("stringify %#v: %v", v, err)
		}
		return s
	}
	for ev := range stream.Events() {
		switch {
		case !record:
		case ev.Type == ai.EventToolCallStart:
			call, _ := ev.Partial.Content[ev.ContentIndex].(ai.ToolCall)
			got.starts = append(got.starts, stringify(call.OrderedArguments()))
		case ev.Type == ai.EventToolCallDelta:
			call, _ := ev.Partial.Content[ev.ContentIndex].(ai.ToolCall)
			got.deltas = append(got.deltas, [2]string{ev.Delta, stringify(call.OrderedArguments())})
		case ev.Type == ai.EventToolCallEnd:
			fields := ai.OrderedObject{{Key: "arguments", Value: ev.ToolCall.OrderedArguments()}}
			if ev.ToolCall.ArgumentsError != "" {
				fields = append(fields, ai.OrderedField{Key: "argumentsError", Value: ev.ToolCall.ArgumentsError})
			}
			got.end = stringify(fields)
		}
	}
	got.final = stream.Result()
	return got
}

// streamedToolArguments is what streamToolArguments records: the arguments
// each toolcall_start showed, each toolcall_delta's delta and arguments, the
// toolcall_end's arguments (and argumentsError), and the final message.
type streamedToolArguments struct {
	starts []string
	deltas [][2]string
	end    string
	final  *ai.AssistantMessage
}

// piLoneSurrogate is a lone surrogate as JSON.stringify writes one: the only
// surrogates it escapes. The port holds a string's characters as UTF-8, so a
// lone surrogate is U+FFFD, and an escaped high surrogate is held back while
// it may still pair, where pi shows it alone (see ai.StreamingJSONParser).
var piLoneSurrogate = regexp.MustCompile(`\\ud[89ab][0-9a-f]{2}"|\\ud[89a-f][0-9a-f]{2}`)

func portSurrogates(piJSON string) string {
	return piLoneSurrogate.ReplaceAllStringFunc(piJSON, func(m string) string {
		if m[len(m)-1] == '"' {
			return `"`
		}
		return "�"
	})
}

func TestStreamingToolArgumentsMatchPi(t *testing.T) {
	raw, err := os.ReadFile(streamingToolArgumentsCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture streamingToolArgumentsCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Cases) < 70 {
		t.Fatalf("capture has %d cases", len(capture.Cases))
	}
	for _, c := range capture.Cases {
		for provider, want := range c.Providers {
			t.Run(c.Name+"/"+provider+"/"+jsNumberText(c.Chunk), func(t *testing.T) {
				got := streamToolArguments(t, provider, c.Text, want.Body, true)
				starts, deltas, end, final := got.starts, got.deltas, got.end, got.final
				if fmt.Sprint(starts) != fmt.Sprint(want.Starts) {
					t.Fatalf("toolcall_start arguments %q, pi %q", starts, want.Starts)
				}
				if len(deltas) != len(want.Deltas) {
					t.Fatalf("%d toolcall_delta events, pi pushed %d", len(deltas), len(want.Deltas))
				}
				for i, got := range deltas {
					w := want.Deltas[i]
					if got[0] != w.Delta || got[1] != portSurrogates(w.Args) {
						t.Fatalf("toolcall_delta %d: delta %q, arguments %s; pi: delta %q, arguments %s", i, got[0], got[1], w.Delta, w.Args)
					}
				}
				if wantEnd := portSurrogates(string(want.End)); end != wantEnd {
					t.Errorf("toolcall_end %s, pi %s", end, wantEnd)
				}
				if string(final.StopReason) != want.StopReason {
					t.Errorf("stopReason %q (%s), pi %q", final.StopReason, final.ErrorMessage, want.StopReason)
				}
				if want.ErrorMessage != nil && final.ErrorMessage != *want.ErrorMessage {
					t.Errorf("errorMessage %q, pi %q", final.ErrorMessage, *want.ErrorMessage)
				}
			})
		}
	}
}

func jsNumberText(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// The rest is pi's streaming-tool-arguments.test.ts (f715a15c), but for its
// collector case (pi checks that dropped streams and snapshots are released,
// which Go's collector does not let a test observe) and its stream-state keys
// (a port's ToolCall has no room for them), on the port's providers.

// streamingContent is pi's test's file content (as ai's streamingjson_test.go
// has it).
var streamingContent = func() string {
	var b strings.Builder
	for line := range 20_000 {
		b.WriteString(`const line` + strconv.Itoa(line) + ` = "value ` + strconv.Itoa(line) + `"; // \ done` + "\n")
	}
	return b.String()
}()

// allocatedBytes is pi's memory-helpers allocatedBytes: what fn allocates,
// counted by the runtime.
func allocatedBytes(fn func()) float64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return float64(after.TotalAlloc - before.TotalAlloc)
}

// streamingWriteCall is pi's writeCall: `write` arguments with size
// characters of file content, and their JSON text.
func streamingWriteCall(size int) (map[string]any, string) {
	content := streamingContent[:size]
	args := map[string]any{"path": "/src/file.ts", "content": content}
	text, _ := jstext.Stringify(ai.OrderedObject{{Key: "path", Value: "/src/file.ts"}, {Key: "content", Value: content}})
	return args, text
}

func streamingDeltasOf(text string, size int) []string {
	var deltas []string
	for i := 0; i < len(text); i += size {
		deltas = append(deltas, text[i:min(i+size, len(text))])
	}
	return deltas
}

// streamingSSE is one server-sent event of data, named when event is set.
func streamingSSE(b *strings.Builder, event string, data any) {
	text, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	if event != "" {
		b.WriteString("event: " + event + "\n")
	}
	b.WriteString("data: ")
	b.Write(text)
	b.WriteString("\n\n")
}

type obj = map[string]any

// streamingToolArgumentsBody is the response body that streams one `write`
// call whose argument text arrives as deltas, as pi's test's generators
// write it.
func streamingToolArgumentsBody(provider string, deltas []string, text string) string {
	var b strings.Builder
	switch provider {
	case "anthropic":
		streamingSSE(&b, "message_start", obj{"type": "message_start", "message": obj{"id": "msg_1", "model": "claude-test", "usage": obj{"input_tokens": 1, "output_tokens": 1}}})
		streamingSSE(&b, "content_block_start", obj{"type": "content_block_start", "index": 0, "content_block": obj{"type": "tool_use", "id": "toolu_1", "name": "write", "input": obj{}}})
		for _, d := range deltas {
			streamingSSE(&b, "content_block_delta", obj{"type": "content_block_delta", "index": 0, "delta": obj{"type": "input_json_delta", "partial_json": d}})
		}
		streamingSSE(&b, "content_block_stop", obj{"type": "content_block_stop", "index": 0})
		streamingSSE(&b, "message_delta", obj{"type": "message_delta", "delta": obj{"stop_reason": "tool_use"}, "usage": obj{"output_tokens": 10}})
		streamingSSE(&b, "message_stop", obj{"type": "message_stop"})
	case "completions":
		call := func(fields obj) obj {
			fields["index"] = 0
			return obj{"id": "chat_1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-test", "choices": []any{obj{"index": 0, "delta": obj{"tool_calls": []any{fields}}}}}
		}
		streamingSSE(&b, "", call(obj{"id": "call_1", "type": "function", "function": obj{"name": "write", "arguments": ""}}))
		for _, d := range deltas {
			streamingSSE(&b, "", call(obj{"function": obj{"arguments": d}}))
		}
		streamingSSE(&b, "", obj{"id": "chat_1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-test", "choices": []any{obj{"index": 0, "delta": obj{}, "finish_reason": "tool_calls"}}, "usage": obj{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
		b.WriteString("data: [DONE]\n\n")
	case "responses":
		item := obj{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "write", "arguments": "", "status": "in_progress"}
		streamingSSE(&b, "", obj{"type": "response.output_item.added", "output_index": 0, "item": item})
		for _, d := range deltas {
			streamingSSE(&b, "", obj{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_1", "delta": d})
		}
		streamingSSE(&b, "", obj{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": "fc_1", "arguments": text})
		done := obj{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "write", "arguments": text, "status": "completed"}
		streamingSSE(&b, "", obj{"type": "response.output_item.done", "output_index": 0, "item": done})
		streamingSSE(&b, "", obj{"type": "response.completed", "response": obj{"id": "resp_1", "status": "completed", "output": []any{}, "usage": obj{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
	case "piMessages":
		streamingSSE(&b, "", obj{"type": "start"})
		streamingSSE(&b, "", obj{"type": "toolcall_start", "contentIndex": 0, "id": "call_1", "toolName": "write"})
		for _, d := range deltas {
			streamingSSE(&b, "", obj{"type": "toolcall_delta", "contentIndex": 0, "delta": d})
		}
		streamingSSE(&b, "", obj{"type": "toolcall_end", "contentIndex": 0, "toolCall": obj{"type": "toolCall", "id": "call_1", "name": "write", "arguments": json.RawMessage(text)}})
		streamingSSE(&b, "", obj{"type": "done", "reason": "toolUse", "usage": obj{"input": 1, "output": 1, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 2, "cost": obj{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}}})
	}
	return b.String()
}

var streamingToolArgumentsProviders = []string{"anthropic", "completions", "responses", "piMessages", "faux"}

// pi: "shows growing partial arguments ... and finishes with the exact
// arguments" — every partial content is a prefix of the content, never
// shorter than the one before, and they are many.
func TestStreamingToolArgumentsGrowToTheExactArguments(t *testing.T) {
	args, text := streamingWriteCall(5_000)
	for _, provider := range streamingToolArgumentsProviders {
		t.Run(provider, func(t *testing.T) {
			body := streamingToolArgumentsBody(provider, streamingDeltasOf(text, 24), text)
			got := streamToolArguments(t, provider, text, body, true)
			deltas, end, final := got.deltas, got.end, got.final
			shown, partials := -1, 0
			for i, d := range deltas {
				var partial map[string]any
				if err := json.Unmarshal([]byte(d[1]), &partial); err != nil {
					t.Fatalf("delta %d: arguments %s: %v", i, d[1], err)
				}
				content, isString := partial["content"].(string)
				if !isString {
					continue
				}
				partials++
				if !strings.HasPrefix(args["content"].(string), content) || len(content) < shown {
					t.Fatalf("delta %d shows %d characters of content after %d, or not a prefix", i, len(content), shown)
				}
				shown = len(content)
			}
			if final.StopReason != ai.StopToolUse {
				t.Fatalf("stopReason %q: %s", final.StopReason, final.ErrorMessage)
			}
			if partials <= 100 {
				t.Fatalf("%d partials show content", partials)
			}
			if want := `{"arguments":` + text + `}`; end != want {
				t.Fatalf("toolcall_end %.200s…, want %.200s…", end, want)
			}
		})
	}
}

// pi: "allocates linearly in the argument text" — ten times the text costs
// about ten times the allocation; re-parsing the text so far on every delta
// costs about a hundred times.
func TestStreamingToolArgumentsAllocateLinearly(t *testing.T) {
	for _, provider := range streamingToolArgumentsProviders {
		t.Run(provider, func(t *testing.T) {
			run := func(size int) float64 {
				_, text := streamingWriteCall(size)
				body := streamingToolArgumentsBody(provider, streamingDeltasOf(text, 32), text)
				return allocatedBytes(func() { streamToolArguments(t, provider, text, body, false) })
			}
			small, large := run(25_000), run(250_000)
			_, text := streamingWriteCall(250_000)
			if large/small >= 25 || large/float64(len(text)) >= 5_000 {
				t.Fatalf("allocated %.0f bytes for 25k characters, %.0f for 250k (%.1fx, %.0f per character)", small, large, large/small, large/float64(len(text)))
			}
		})
	}
}

// pi constrained-sampling.test.ts: "streams custom tool input in time and
// memory linear in its length" — through the Responses provider, whose call
// to a tool it was not told about keeps its input under "input".
func TestResponsesCustomToolInputStreamsLinearly(t *testing.T) {
	run := func(size int) float64 {
		var input strings.Builder
		deltas := make([]string, size/32)
		for i := range deltas {
			delta := `chunk ` + strconv.Itoa(i) + ` "quoted" \ ` + "\n"
			deltas[i] = delta + strings.Repeat("x", 32-len(delta))
			input.WriteString(deltas[i])
		}
		item := obj{"type": "custom_tool_call", "call_id": "call_1", "id": "ctc_1", "name": "sample_tool", "input": ""}
		var b strings.Builder
		streamingSSE(&b, "", obj{"type": "response.output_item.added", "output_index": 0, "item": item})
		for _, d := range deltas {
			streamingSSE(&b, "", obj{"type": "response.custom_tool_call_input.delta", "output_index": 0, "item_id": "ctc_1", "delta": d})
		}
		streamingSSE(&b, "", obj{"type": "response.custom_tool_call_input.done", "output_index": 0, "item_id": "ctc_1", "input": input.String()})
		item["input"] = input.String()
		streamingSSE(&b, "", obj{"type": "response.output_item.done", "output_index": 0, "item": item})
		streamingSSE(&b, "", obj{"type": "response.completed", "response": obj{"status": "completed", "usage": obj{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
		body := b.String()

		var jsonDeltas strings.Builder
		bytes := allocatedBytes(func() {
			req := ai.NormalizeContext(ai.Context{Messages: []ai.Message{ai.NewUserText("write", 0)}})
			opts := &ai.SimpleStreamOptions{}
			opts.APIKey = "k"
			opts.HTTPClient = cannedDoer{status: http.StatusOK, body: body}
			stream := StreamSimpleOpenAIResponses(context.Background(), streamingToolArgumentsModel("responses"), req, opts)
			for ev := range stream.Events() {
				if ev.Type == ai.EventToolCallDelta {
					jsonDeltas.WriteString(ev.Delta)
				}
			}
			if final := stream.Result(); final.StopReason != ai.StopToolUse {
				t.Fatalf("stopReason %q: %s", final.StopReason, final.ErrorMessage)
			}
		})
		var got map[string]any
		if err := json.Unmarshal([]byte(jsonDeltas.String()), &got); err != nil || len(got) != 1 || got["input"] != input.String() {
			t.Fatalf("the JSON deltas make %.200s (%v)", jsonDeltas.String(), err)
		}
		return bytes
	}
	small, large := run(25_600), run(256_000)
	if large/small >= 25 {
		t.Fatalf("allocated %.0f bytes for 25.6k characters, %.0f for 256k (%.1fx)", small, large, large/small)
	}
}

// pi: "leaves no stream state on the error message of a stream cut off
// mid-call" — the message keeps the partial arguments read so far.
func TestStreamingToolArgumentsCutOffKeepsThePartialArguments(t *testing.T) {
	_, text := streamingWriteCall(2_000)
	body := streamingToolArgumentsBody("anthropic", streamingDeltasOf(text, 16)[:20], text)
	// Drop the block stop and the message end, as a dropped connection does.
	events := strings.SplitAfter(body, "\n\n")
	body = strings.Join(events[:len(events)-4], "")
	final := streamToolArguments(t, "anthropic", text, body, true).final
	if final.StopReason != ai.StopError {
		t.Fatalf("stopReason %q", final.StopReason)
	}
	call, _ := final.Content[0].(ai.ToolCall)
	// pi: toMatchObject({ type: "toolCall", arguments: { path } }).
	if call.Arguments["path"] != "/src/file.ts" || call.ArgumentsError != "" {
		t.Fatalf("content[0] = %#v", final.Content[0])
	}
}
