package harness

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-structured-output.test.ts.
// The bash case needs the tools on env (stage 5).

func schemaOf(t *testing.T, source string) *ai.Schema {
	t.Helper()
	var schema ai.Schema
	must(t, json.Unmarshal([]byte(source), &schema))
	return &schema
}

const countSchema = `{"type":"object","properties":{"count":{"type":"number"}},"required":["count"]}`

// returning is a tool named name that returns result; change may add a
// schema or limits.
func returning(name string, result ToolExecutionResult, change ...func(*ToolRegistration)) *ToolRegistration {
	tool := &ToolRegistration{
		Tool: ai.Tool{Name: name, Description: name, Parameters: objectSchema()},
		Execute: func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
			return result, nil
		},
	}
	for _, c := range change {
		c(tool)
	}
	return tool
}

func withSchema(t *testing.T, source string) func(*ToolRegistration) {
	schema := schemaOf(t, source)
	return func(tool *ToolRegistration) { tool.StructuredOutputSchema = schema }
}

// probeCalling installs a probe tool whose run calls each of names as a
// nested call, in order, and records what it received.
func probeCalling(t *testing.T, setup *chatSetup, names ...string) func() []NestedToolExecutionResult {
	t.Helper()
	var mu sync.Mutex
	var received []NestedToolExecutionResult
	installTool(t, setup.registry, echoTool("probe", func(ctx context.Context, _ map[string]any, api ToolExecutionAPI) (ToolExecutionResult, error) {
		for _, name := range names {
			result, err := api.ExecuteTool(ctx, name, map[string]any{}, NestedCallOptions{})
			if err != nil {
				return ToolExecutionResult{}, err
			}
			mu.Lock()
			received = append(received, result)
			mu.Unlock()
		}
		return ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	return func() []NestedToolExecutionResult {
		mu.Lock()
		defer mu.Unlock()
		return append([]NestedToolExecutionResult(nil), received...)
	}
}

// nestedRun runs name as a nested call of a probe tool and returns what the
// probe received.
func nestedRun(t *testing.T, setup *chatSetup, name string) NestedToolExecutionResult {
	t.Helper()
	received := probeCalling(t, setup, name)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("probe", nil, "c1"))), fauxText("done")})
	_, root := openChat(t, memory.New(), setup)
	waitFor(t, submit(t, root, inputDraft("go")))
	results := received()
	if len(results) != 1 {
		t.Fatalf("received %d results", len(results))
	}
	return results[0]
}

// directRun runs name as a model-issued call and returns its transcript
// message.
func directRun(t *testing.T, setup *chatSetup, name string) ai.ToolResultMessage {
	t.Helper()
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call(name, nil, "c1"))), fauxText("done")})
	_, root := openChat(t, memory.New(), setup)
	waitFor(t, submit(t, root, inputDraft("go")))
	results := toolResults(chatEntries(t, root))
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	return results[0]
}

// normalJSON is raw with object keys sorted: a value read back from a
// document lists its keys in Go's order, so JSON compares by meaning, as
// pi's toEqual.
func normalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var value any
	must(t, json.Unmarshal(raw, &value))
	return jsonOf(t, value)
}

func codes(diagnostics []ToolDiagnostic) []string {
	out := make([]string, len(diagnostics))
	for i, diagnostic := range diagnostics {
		out[i] = diagnostic.Code
	}
	return out
}

func TestStructuredOutputGivesCallersASchemaLessToolsOutputOneTextAsAStringOneImageAsItselfElseTheList(t *testing.T) {
	text := ai.TextContent{Text: "one"}
	image := ai.ImageContent{Data: "AAAA", MimeType: "image/png"}
	const textJSON, imageJSON = `{"type":"text","text":"one"}`, `{"type":"image","data":"AAAA","mimeType":"image/png"}`
	for _, c := range []struct {
		output ai.ContentList
		value  string
	}{
		{ai.ContentList{text}, `"one"`},
		{ai.ContentList{image}, imageJSON},
		{ai.ContentList{}, `""`},
		{ai.ContentList{text, image}, "[" + textJSON + "," + imageJSON + "]"},
		{ai.ContentList{text, text}, "[" + textJSON + "," + textJSON + "]"},
	} {
		setup := newChatSetup(t)
		installTool(t, setup.registry, returning("plain", ToolExecutionResult{Output: c.output}))
		received := nestedRun(t, setup, "plain")
		if normalJSON(t, received.StructuredOutput) != normalJSON(t, []byte(c.value)) || received.IsError || len(received.Diagnostics) != 0 || received.Details != nil || received.Usage != nil || received.DurationMs == nil {
			t.Fatalf("output %s: received %s, want structured output %s", jsonOf(t, c.output), jsonOf(t, received), c.value)
		}
	}
}

func TestStructuredOutputGivesCallersTheBoundedOutputAsTheModelSeesItWithoutTheRenderedDiagnostics(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, returning("long",
		ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "a\nb\nc"}}, Diagnostics: []ToolDiagnostic{{Severity: "info", Message: "note"}}},
		func(tool *ToolRegistration) { tool.OutputLimits = &OutputLimits{MaxLines: 1} }))
	received := nestedRun(t, setup, "long")
	if string(received.StructuredOutput) != `"a\n"` {
		t.Fatalf("structured output = %s", received.StructuredOutput)
	}
	expectStrings(t, "codes", codes(received.Diagnostics), []string{"", "truncated"})
}

func TestStructuredOutputStoresExactlyWhatCallersAndEventsGetWithoutTheModelsOutputAndKeepsAStoredNull(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, returning("plain", ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "a"}}}))
	// Go has no undefined: a stored null is the JSON null.
	installTool(t, setup.registry, returning("nothing", ToolExecutionResult{StructuredOutput: json.RawMessage("null")}, withSchema(t, `{"type":"null"}`)))
	received := probeCalling(t, setup, "plain", "nothing")
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("probe", nil, "c1"))), fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	// The result each nested call's document got, as committed.
	var mu sync.Mutex
	var stored []string
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			document, ok := change.(durable.DocumentChange)
			if !ok || document.Record.Kind != NestedCallDoc.Definition().Kind || len(document.Ops) == 0 || document.Value == nil {
				continue
			}
			if result, ok := document.Value["result"]; ok {
				raw, err := json.Marshal(result)
				must(t, err)
				mu.Lock()
				stored = append(stored, normalJSON(t, raw))
				mu.Unlock()
			}
		}
	})
	must(t, err)
	defer unsubscribe()
	_, log := listen(t, root)
	waitFor(t, submit(t, root, inputDraft("go")))
	must(t, h.WaitForIdle(bg))
	log.quiet(t)
	results := received()
	if len(results) != 2 || string(results[0].StructuredOutput) != `"a"` || results[0].IsError || string(results[1].StructuredOutput) != "null" || results[1].IsError {
		t.Fatalf("received = %s", jsonOf(t, results))
	}
	mu.Lock()
	defer mu.Unlock()
	var want []string
	for _, result := range results {
		want = append(want, normalJSON(t, []byte(jsonOf(t, result))))
	}
	if jsonOf(t, stored) != jsonOf(t, want) {
		t.Fatalf("stored = %v, want %v", stored, want)
	}
	// The ends of the nested calls carry the same results.
	var ends []string
	for _, event := range log.events() {
		if end, ok := event.(ToolExecutionEndEvent); ok && end.ParentToolCallID != "" {
			ends = append(ends, normalJSON(t, []byte(jsonOf(t, end.Result))))
		}
	}
	if jsonOf(t, ends) != jsonOf(t, want) {
		t.Fatalf("ends = %v, want %v", ends, want)
	}
}

func TestStructuredOutputGivesCallersASchemaToolsValidatedStructuredOutputWhichTheTranscriptDoesNotStore(t *testing.T) {
	result := ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "3 things"}}, StructuredOutput: map[string]any{"count": 3}}
	setup := newChatSetup(t)
	installTool(t, setup.registry, returning("counter", result, withSchema(t, countSchema)))
	if received := nestedRun(t, setup, "counter"); string(received.StructuredOutput) != `{"count":3}` || received.IsError {
		t.Fatalf("received = %s", jsonOf(t, received))
	}
	modelSetup := newChatSetup(t)
	installTool(t, modelSetup.registry, returning("counter", result, withSchema(t, countSchema)))
	stored := directRun(t, modelSetup, "counter")
	if stored.IsError || jsonOf(t, stored.Content) != `[{"type":"text","text":"3 things"}]` || strings.Contains(jsonOf(t, stored), "structuredOutput") {
		t.Fatalf("stored = %s", jsonOf(t, stored))
	}
}

func TestStructuredOutputTurnsBrokenStructuredOutputIntoAnErrorResultForCallersAndAReportForTheModelsCalls(t *testing.T) {
	for _, c := range []struct {
		name    string
		result  ToolExecutionResult
		schema  bool
		message string
	}{
		{"wrong", ToolExecutionResult{StructuredOutput: map[string]any{"count": "three"}}, true, "does not match its schema: count"},
		{"missing", ToolExecutionResult{Output: ai.ContentList{}}, true, "returned no structuredOutput"},
		{"undeclared", ToolExecutionResult{StructuredOutput: map[string]any{"count": 3}}, false, "declares no structuredOutputSchema"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tool := func(setup *chatSetup) {
				var change []func(*ToolRegistration)
				if c.schema {
					change = append(change, withSchema(t, countSchema))
				}
				installTool(t, setup.registry, returning(c.name, c.result, change...))
			}
			setup := newChatSetup(t)
			tool(setup)
			received := nestedRun(t, setup, c.name)
			if !received.IsError {
				t.Fatalf("received = %s, want an error", jsonOf(t, received))
			}
			// The broken value is dropped; a schema-less tool's error result
			// still carries its output, here none.
			if want := map[bool]string{true: "", false: `""`}[c.schema]; string(received.StructuredOutput) != want {
				t.Fatalf("structured output = %s, want %s", received.StructuredOutput, want)
			}
			expectStrings(t, "codes", codes(received.Diagnostics), []string{"invalid_structured_output"})
			if !strings.Contains(received.Diagnostics[0].Message, c.message) {
				t.Fatalf("message = %q", received.Diagnostics[0].Message)
			}

			modelSetup := newChatSetup(t)
			tool(modelSetup)
			// The model never sees structured output: its call stands, and the
			// host gets a report.
			stored := directRun(t, modelSetup, c.name)
			if stored.IsError || strings.Contains(jsonOf(t, stored.Content), c.message) {
				t.Fatalf("stored = %s", jsonOf(t, stored))
			}
			var reports []string
			for _, err := range modelSetup.reports.all() {
				reports = append(reports, err.Error())
			}
			if !strings.Contains(strings.Join(reports, "\n"), c.message) {
				t.Fatalf("reports = %q", reports)
			}
		})
	}
}

func TestStructuredOutputGivesCallersTheOutputOfAnErrorResultOfAToolWithoutASchema(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, returning("bad", ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "bad"}}, IsError: true}))
	if received := nestedRun(t, setup, "bad"); !received.IsError || string(received.StructuredOutput) != `"bad"` {
		t.Fatalf("received = %s", jsonOf(t, received))
	}
}

func TestStructuredOutputGivesCallersNoStructuredOutputForAnErrorResultTheHarnessWroteOnlyDiagnostics(t *testing.T) {
	received := nestedRun(t, newChatSetup(t), "missing")
	if !received.IsError || received.StructuredOutput != nil || len(received.Diagnostics) == 0 || received.Diagnostics[0].Code != "tool_unavailable" {
		t.Fatalf("received = %s", jsonOf(t, received))
	}
}

func TestStructuredOutputGivesCallersASchemaToolsErrorTextAsADiagnosticNeverAsStructuredOutput(t *testing.T) {
	failing := ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "nope"}}, IsError: true}
	setup := newChatSetup(t)
	installTool(t, setup.registry, returning("fails", failing, withSchema(t, countSchema)))
	// Its text reaches the caller as an error diagnostic: structured output
	// is only ever the tool's own value.
	received := nestedRun(t, setup, "fails")
	if !received.IsError || received.StructuredOutput != nil || jsonOf(t, received.Diagnostics) != `[{"severity":"error","message":"nope","code":"tool_error"}]` {
		t.Fatalf("received = %s", jsonOf(t, received))
	}

	// The model's call keeps the text in its output, without the extra
	// diagnostic.
	modelSetup := newChatSetup(t)
	installTool(t, modelSetup.registry, returning("fails", failing, withSchema(t, countSchema)))
	if stored := directRun(t, modelSetup, "fails"); jsonOf(t, stored.Content) != `[{"type":"text","text":"nope"}]` {
		t.Fatalf("stored = %s", jsonOf(t, stored.Content))
	}

	// A thrown error's message is in its tool_error diagnostic, not in the
	// output: one diagnostic, not a second one from its (empty) output.
	thrown := newChatSetup(t)
	throws := returning("throws", ToolExecutionResult{}, withSchema(t, countSchema))
	throws.Execute = func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{}, errorString("permission denied")
	}
	installTool(t, thrown.registry, throws)
	thrownResult := nestedRun(t, thrown, "throws")
	if !thrownResult.IsError || thrownResult.StructuredOutput != nil || len(thrownResult.Diagnostics) != 1 || thrownResult.Diagnostics[0].Code != "tool_error" || thrownResult.Diagnostics[0].Message != "permission denied" {
		t.Fatalf("thrown = %s", jsonOf(t, thrownResult))
	}

	// An error result whose own diagnostics say what went wrong gets no
	// second one from its output.
	explained := newChatSetup(t)
	installTool(t, explained.registry, returning("fails", ToolExecutionResult{
		Output:      ai.ContentList{ai.TextContent{Text: "partial log"}},
		IsError:     true,
		Diagnostics: []ToolDiagnostic{{Severity: "error", Code: "quota", Message: "quota exceeded"}},
	}, withSchema(t, countSchema)))
	if got := jsonOf(t, nestedRun(t, explained, "fails").Diagnostics); got != `[{"severity":"error","message":"quota exceeded","code":"quota"}]` {
		t.Fatalf("explained diagnostics = %s", got)
	}

	// An error result that has structured output keeps it.
	kept := newChatSetup(t)
	installTool(t, kept.registry, returning("fails", ToolExecutionResult{
		Output:           ai.ContentList{ai.TextContent{Text: "nope"}},
		StructuredOutput: map[string]any{"count": 0},
		IsError:          true,
	}, withSchema(t, countSchema)))
	if result := nestedRun(t, kept, "fails"); !result.IsError || string(result.StructuredOutput) != `{"count":0}` {
		t.Fatalf("kept = %s", jsonOf(t, result))
	}
}

func TestStructuredOutputKeepsStructuredOutputWhenAfterToolReplacesTheOutputAndValidatesOneAfterToolReplaces(t *testing.T) {
	setup := newChatSetup(t)
	installTool(t, setup.registry, returning("plain", ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "secret"}}}))
	installTool(t, setup.registry, returning("counter", ToolExecutionResult{StructuredOutput: map[string]any{"count": 1}}, withSchema(t, countSchema)))
	installTool(t, setup.registry, returning("other", ToolExecutionResult{StructuredOutput: map[string]any{"count": 1}}, withSchema(t, countSchema)))
	installHooks(t, setup.registry, "hooks", ToolTask, &ToolHooks{AfterTool: func(_ context.Context, call ToolHookCall, result ToolExecutionResult, _ *TaskRuntime) (*ToolExecutionResult, error) {
		switch call.Name {
		case "other":
			result.StructuredOutput = map[string]any{"count": "broken"}
		case "probe":
		default:
			result.Output = ai.ContentList{ai.TextContent{Text: "rewritten"}}
		}
		return &result, nil
	}})
	received := probeCalling(t, setup, "plain", "counter", "other")
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxStep(toolCalls(call("probe", nil, "c1"))), fauxText("done")})
	_, root := openChat(t, memory.New(), setup)
	waitFor(t, submit(t, root, inputDraft("go")))
	results := received()
	if len(results) != 3 {
		t.Fatalf("received = %s", jsonOf(t, results))
	}
	// A schema-less tool's value is its output, so redacting the output
	// redacts what programs get.
	if string(results[0].StructuredOutput) != `"rewritten"` {
		t.Fatalf("plain = %s", jsonOf(t, results[0]))
	}
	if string(results[1].StructuredOutput) != `{"count":1}` {
		t.Fatalf("counter = %s", jsonOf(t, results[1]))
	}
	if !results[2].IsError {
		t.Fatalf("other = %s", jsonOf(t, results[2]))
	}
	expectStrings(t, "other codes", codes(results[2].Diagnostics), []string{"invalid_structured_output"})
}
