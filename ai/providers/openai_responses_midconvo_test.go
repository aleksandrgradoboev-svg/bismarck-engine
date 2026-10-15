package providers

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
)

// openai-responses models with compat.supportsMidConvoSystemMessages receive
// later system messages in place (upstream 9e05370b2): each is sent as an
// instruction-role message carrying its rendered update, preceded by its tool
// additions as an additional_tools item or a synthetic client tool search when
// the model supports either and the tool history is purely additive. The first
// three tests transliterate the openai-responses cases of
// packages/ai/test/transcript-tool-changes.test.ts at the sha, same fixtures
// and expected values, through ai.StreamSimple as the suite goes through
// compat.ts streamSimple.

// gpt54 is the suite's native openai-responses model.
func gpt54(compat string) *ai.Model {
	m := foldModel("gpt-5.4", ai.APIOpenAIResponses, "openai", compat)
	m.Name = "GPT-5.4"
	return m
}

// toolNames lists the name of each tool object in a payload array.
func toolNames(tools []map[string]any) []string {
	names := []string{}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	return names
}

// nestedTools reads an input item's tools array.
func nestedTools(t *testing.T, item map[string]any) []map[string]any {
	t.Helper()
	return payloadItems(t, item, "tools")
}

func TestAnchorsOpenAIAdditionsAtTheirDeveloperMessage(t *testing.T) {
	model := gpt54(`{"supportsMidConvoSystemMessages":true,"supportsAdditionalTools":true}`)
	body := captureStreamSimplePayload(t, model, transcriptAdditionContext())

	if got := toolNames(payloadItems(t, body, "tools")); !slices.Equal(got, []string{"base_tool"}) {
		t.Fatalf("tools = %v, want [base_tool]", got)
	}
	input := payloadItems(t, body, "input")
	var additional []string
	for _, item := range input {
		if item["type"] == "additional_tools" {
			additional = toolNames(nestedTools(t, item))
			break
		}
	}
	if !slices.Equal(additional, []string{"late_tool"}) {
		t.Fatalf("additional_tools tools = %v, want [late_tool]", additional)
	}
	var developer []any
	for _, item := range input {
		if _, typed := item["type"]; item["role"] == "developer" && !typed {
			developer = append(developer, item["content"])
		}
	}
	if !slices.Equal(developer, []any{"base prompt", "updated guidance"}) {
		t.Fatalf("developer contents = %v, want [base prompt, updated guidance]", developer)
	}
}

func TestMapsSystemMessageAdditionsIntoSyntheticToolSearch(t *testing.T) {
	model := gpt54(`{"supportsMidConvoSystemMessages":true,"supportsToolSearch":true}`)
	body := captureStreamSimplePayload(t, model, transcriptAdditionContext())

	if got := toolNames(payloadItems(t, body, "tools")); !slices.Equal(got, []string{"base_tool"}) {
		t.Fatalf("tools = %v, want [base_tool]", got)
	}
	input := payloadItems(t, body, "input")
	var types []any
	var searched []string
	found := false
	for _, item := range input {
		types = append(types, item["type"])
		if item["type"] == "tool_search_output" && !found {
			searched = toolNames(nestedTools(t, item))
			found = true
		}
	}
	if !slices.Contains(types, any("tool_search_call")) {
		t.Fatalf("input types = %v, want a tool_search_call", types)
	}
	if !slices.Equal(searched, []string{"late_tool"}) {
		t.Fatalf("tool_search_output tools = %v, want [late_tool]", searched)
	}
}

func TestFallsBackToTheCompleteCurrentToolStateWhenRemovalsAreUnsupported(t *testing.T) {
	model := gpt54(`{"supportsMidConvoSystemMessages":true,"supportsAdditionalTools":true}`)
	body := captureStreamSimplePayload(t, model, transcriptChangesContext())

	if got := toolNames(payloadItems(t, body, "tools")); !slices.Equal(got, []string{"late_tool"}) {
		t.Fatalf("tools = %v, want [late_tool]", got)
	}
	input := payloadItems(t, body, "input")
	developer := 0
	for _, item := range input {
		if item["type"] == "additional_tools" {
			t.Fatalf("input carries an additional_tools item: %v", item)
		}
		if item["role"] == "developer" {
			developer++
		}
	}
	if developer != 2 {
		t.Fatalf("developer items = %d, want 2 (input %v)", developer, input)
	}
}

const responsesMidConvoCaptureFile = "testdata/transcript/responses-midconvo-9e05370b2.json"

// responsesMidConvoCapture is what capture-responses-midconvo.mts records.
type responsesMidConvoCapture struct {
	SHA            string                                 `json:"sha"`
	Bodies         map[string]responsesMidConvoCase       `json:"bodies"`
	LoneSurrogates responsesMidConvoCase                  `json:"loneSurrogates"`
	Streams        map[string]responsesMidConvoStreamCase `json:"streams"`
}

// responsesMidConvoCase is one body case of the capture: the model and context
// pi was given and the request body it built.
type responsesMidConvoCase struct {
	Model   ai.Model        `json:"model"`
	Context json.RawMessage `json:"context"`
	Body    json.RawMessage `json:"body"`
}

// responsesMidConvoStreamCase is one stream case of the capture: the model and
// context pi was given, the SSE response it read, and what it parsed from it.
type responsesMidConvoStreamCase struct {
	Model      ai.Model        `json:"model"`
	Context    json.RawMessage `json:"context"`
	SSE        string          `json:"sse"`
	StopReason ai.StopReason   `json:"stopReason"`
	ToolCalls  json.RawMessage `json:"toolCalls"`
}

// loadResponsesMidConvoCapture reads the capture, checking it holds the cases
// the tests expect.
func loadResponsesMidConvoCapture(t *testing.T) responsesMidConvoCapture {
	t.Helper()
	data, err := os.ReadFile(responsesMidConvoCaptureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture responsesMidConvoCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("%s: %v", responsesMidConvoCaptureFile, err)
	}
	if len(capture.Bodies) != 13 || len(capture.Streams) != 2 || capture.LoneSurrogates.Body == nil {
		t.Fatalf("%s has %d body cases, %d stream cases and loneSurrogates %t, want 13, 2 and true; rerun capture-responses-midconvo.mts",
			responsesMidConvoCaptureFile, len(capture.Bodies), len(capture.Streams), capture.LoneSurrogates.Body != nil)
	}
	return capture
}

// decodeTranscriptFixture decodes a recorded pi context ({messages}), whose
// messages are role-discriminated.
func decodeTranscriptFixture(t *testing.T, raw json.RawMessage) ai.Context {
	t.Helper()
	var recorded struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	var req ai.Context
	for i, m := range recorded.Messages {
		message, err := ai.UnmarshalMessage(m)
		if err != nil {
			t.Fatalf("messages[%d]: %v", i, err)
		}
		req.Messages = append(req.Messages, message)
	}
	return req
}

// The complete request bodies pi builds for native models, captured under node
// at upstream 9e05370b2 by testdata/transcript/capture-responses-midconvo.mts:
// the suite's three cases, then the tool_search call_id seed
// (pi_tool_load_<shortHash("system:<msgIndex>:<names>")>, where the leading
// system message does not advance msgIndex and a system message held behind a
// pending tool call takes its transformed position), rendered section updates,
// the system instruction role (additional_tools items stay developer under a
// system instruction role), defer_loading on custom and strict tool search
// results only, additional_tools over tool search, later system messages that
// add no tools (no additional_tools item, no tool_search pair), a transcript
// without a leading system message, one whose leading system message is the
// first transformed message because an aborted assistant message before it is
// dropped, and a removed grammar tool whose call still replays as a custom tool
// call.
func TestResponsesMidConvoRequestBodies(t *testing.T) {
	capture := loadResponsesMidConvoCapture(t)
	for _, name := range slices.Sorted(maps.Keys(capture.Bodies)) {
		t.Run(name, func(t *testing.T) {
			c := capture.Bodies[name]
			body := captureStreamSimplePayload(t, &c.Model, decodeTranscriptFixture(t, c.Context))
			assertRequestBody(t, body, c.Body)
		})
	}
}

// Leading and later system text goes out with lone UTF-16 surrogates removed
// and paired ones kept, as pi's sanitizeSurrogates does (captured under node at
// 9e05370b2, "loneSurrogates"). encoding/json turns a lone surrogate escape into
// U+FFFD, so the context is built here with the surrogates as WTF-8 triples,
// the form a Go string carries them in; each string is checked against the
// recorded context's JSON.stringify form.
func TestResponsesMidConvoSanitizesLoneSurrogatesInSystemText(t *testing.T) {
	const (
		highD83D = "\xed\xa0\xbd"
		lowDE48  = "\xed\xb9\x88"
	)
	c := loadResponsesMidConvoCapture(t).LoneSurrogates

	leadingText := "base " + highD83D + " prompt " + highD83D + lowDE48
	updateText := "updated " + lowDE48 + " guidance"
	rules := "<rules>\nr" + highD83D + "\n</rules>"
	for _, s := range []string{leadingText, updateText, rules} {
		if !strings.Contains(string(c.Context), jsQuote(s)) {
			t.Fatalf("recorded context does not carry %s; keep this fixture in step with capture-responses-midconvo.mts", jsQuote(s))
		}
	}
	leading := ai.NewSystemText(leadingText, 0)
	leading.ToolsAdded = []ai.Tool{transcriptTool("base_tool")}
	update := ai.NewSystemText(updateText, 2)
	update.Sections = ai.SystemSections{{Name: "rules", Value: &rules}}
	req := ai.Context{Messages: []ai.Message{leading, ai.NewUserText("before", 1), update}}

	body := captureStreamSimplePayload(t, &c.Model, req)
	assertRequestBody(t, body, c.Body)
}

// The stream parses grammar tool calls with the input properties of every tool
// the resolved transcript declared, as pi's stream does: a grammar tool that a
// later system message removed still parses as {query} on a native model, whose
// transcript keeps the declaring message, and as {input} on a collapsed one,
// whose single system message declares only the current tools. Captured under
// node at 9e05370b2 by feeding pi the recorded SSE through its fetch option.
func TestResponsesMidConvoStreamedGrammarToolCalls(t *testing.T) {
	capture := loadResponsesMidConvoCapture(t)
	for _, name := range slices.Sorted(maps.Keys(capture.Streams)) {
		t.Run(name, func(t *testing.T) {
			c := capture.Streams[name]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				io.WriteString(w, c.SSE)
			}))
			defer server.Close()
			model := c.Model
			model.BaseURL = server.URL
			opts := &ai.SimpleStreamOptions{}
			opts.APIKey = "test-key"
			final := ai.StreamSimple(context.Background(), &model, decodeTranscriptFixture(t, c.Context), opts).Result()
			if final.StopReason != c.StopReason {
				t.Fatalf("stop reason = %s (%q), want %s", final.StopReason, final.ErrorMessage, c.StopReason)
			}
			type parsedToolCall struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			calls := []parsedToolCall{}
			for _, block := range final.Content {
				if call, ok := block.(ai.ToolCall); ok {
					calls = append(calls, parsedToolCall{Name: call.Name, Arguments: call.Arguments})
				}
			}
			gotJSON, err := json.Marshal(calls)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(gotJSON, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c.ToolCalls, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("parsed tool calls = %s, want pi's %s", gotJSON, c.ToolCalls)
			}
		})
	}
}

// Copilot dynamic headers read the transcript the adapter resolved: a native
// model keeps a trailing system message in place, so the request is
// agent-initiated (pi at 9e05370b2 under node: X-Initiator "agent" with
// supportsMidConvoSystemMessages, "user" without).
func TestCopilotInitiatorReadsTheResolvedResponsesTranscript(t *testing.T) {
	for _, tc := range []struct {
		compat string
		want   string
	}{
		{`{"supportsMidConvoSystemMessages":true}`, "agent"},
		{"", "user"},
	} {
		var initiator string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			initiator = r.Header.Get("X-Initiator")
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, responsesSSE)
		}))
		model := &ai.Model{ID: "gpt-5", Api: ai.APIOpenAIResponses, Provider: "github-copilot", BaseURL: server.URL, Input: []string{"text"}}
		if tc.compat != "" {
			model.Compat = json.RawMessage(tc.compat)
		}
		req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1), ai.NewSystemText("later guidance", 2)}}
		opts := &ai.SimpleStreamOptions{}
		opts.APIKey = "k"
		ai.StreamSimple(context.Background(), model, req, opts).Result()
		server.Close()
		if initiator != tc.want {
			t.Fatalf("compat %s: X-Initiator = %q, want %q", tc.compat, initiator, tc.want)
		}
	}
}
