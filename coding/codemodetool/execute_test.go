package codemodetool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/agent"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/coding"
)

// Ports of pi's test/suite/agent-session-codemode.test.ts (v1.2.0), "AgentSession
// codemode tool" and "codemode options and store", minus the models.* cases.

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="

func schema(t *testing.T, text string) *ai.Schema {
	t.Helper()
	var s ai.Schema
	if err := json.Unmarshal([]byte(text), &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

// searchTools ranks a tool first when the query names it, as tool_search does
// (pi ae823bc0, #11331).
func TestSearchToolsRanksTheToolTheQueryNamesFirst(t *testing.T) {
	describe := func(name, description string) coding.ToolDefinition {
		return coding.ToolDefinition{Tool: agent.AgentTool{Name: name, Description: description, Parameters: ai.Object()}, Exposure: coding.ToolExposureCodemode}
	}
	h := setup(t, coding.SessionOptions{},
		describe("mcp__app__queue_read", "Read a queued message of this thread."),
		describe("mcp__dev-app__thread_read", "Read durable state and a paginated timeline from any thread. The default view returns user messages, assistant messages and plans. Continue with afterPosition until hasMore is false."),
	)
	// Scripts see tools by identifier: dev-app becomes dev_app.
	text := resultText(t, h.run(`text((await searchTools("thread_read", {limit: 1})).map(t => t.name).join(","))`))
	if !strings.Contains(text, "mcp__dev_app__thread_read") || strings.Contains(text, "mcp__app__queue_read") {
		t.Fatalf("searchTools:\n%s", text)
	}
}

func testTools(t *testing.T) []coding.ToolDefinition {
	echo := agent.AgentTool{Name: "echo", Label: "Echo", Description: "Echo text back.\n\nSecond paragraph.",
		Parameters: ai.Object(ai.Prop("text", ai.String("Text to echo"))),
		Execute: func(_ context.Context, _ string, params map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "echo: " + params["text"].(string)}}}, nil
		}}
	stats := agent.AgentTool{Name: "stats", Label: "Stats", Description: "Return structured stats",
		Parameters:   ai.Object(),
		OutputSchema: schema(t, `{"type":"object","properties":{"files":{"type":"number"},"names":{"type":"array","items":{"type":"string"}}},"required":["files","names"]}`),
		Execute: func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "2 files"}},
				StructuredContent: map[string]any{"files": 2, "names": []string{"a", "b"}}}, nil
		}}
	screenshot := agent.AgentTool{Name: "screenshot", Label: "Screenshot", Description: "Return a screenshot",
		Parameters: ai.Object(),
		Execute: func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "captured"}, ai.ImageContent{Data: tinyPNG, MimeType: "image/png"}}}, nil
		}}
	return []coding.ToolDefinition{{Tool: echo}, {Tool: stats}, {Tool: screenshot}}
}

// fauxModel answers with queued messages, as pi's harness.setResponses does,
// and records each request's declared tools and system prompt.
type fauxModel struct {
	mu       sync.Mutex
	queue    []*ai.AssistantMessage
	requests [][]string
	prompts  []string
}

func (f *fauxModel) respond(messages ...*ai.AssistantMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, messages...)
}

func (f *fauxModel) stream(_ context.Context, model *ai.Model, c ai.TranscriptContext, _ *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
	var names []string
	for _, tool := range ai.GetCurrentTools(c.Messages) {
		names = append(names, tool.Name)
	}
	f.mu.Lock()
	f.requests = append(f.requests, names)
	f.prompts = append(f.prompts, ai.GetCurrentSystemPrompt(c.Messages))
	msg := &ai.AssistantMessage{StopReason: ai.StopStop, Content: ai.ContentList{ai.TextContent{Text: "done"}}}
	if len(f.queue) > 0 {
		msg, f.queue = f.queue[0], f.queue[1:]
	}
	f.mu.Unlock()
	out := *msg
	out.Api, out.Provider, out.Model = model.Api, model.Provider, model.ID
	s := ai.NewAssistantMessageEventStream()
	s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: out.StopReason, Message: &out})
	s.End()
	return s
}

func codemodeCall(code string) *ai.AssistantMessage {
	return &ai.AssistantMessage{StopReason: ai.StopToolUse, Content: ai.ContentList{
		ai.ToolCall{ID: fmt.Sprintf("cm%d", time.Now().UnixNano()), Name: ToolName, Arguments: map[string]any{"code": code}},
	}}
}

type harness struct {
	t     *testing.T
	sess  *coding.Session
	model *fauxModel
	mode  Mode
	// budget, when set, is the codemode inline budget.
	budget *int
}

// setup is pi's setup(): a session with codemode active and echo, stats and
// screenshot registered next to it.
func setup(t *testing.T, opts coding.SessionOptions, extra ...coding.ToolDefinition) *harness {
	t.Helper()
	return setupNotices(t, nil, opts, extra...)
}

// setupNotices is setup with the codemode tool's Options.Notices.
func setupNotices(t *testing.T, notices []string, opts coding.SessionOptions, extra ...coding.ToolDefinition) *harness {
	t.Helper()
	h := &harness{t: t, model: &fauxModel{}, mode: ModeOn}
	opts.Model = &ai.Model{ID: "m", Api: "faux", Provider: "faux", ContextWindow: 100000, MaxTokens: 8192, Input: []string{"text", "image"}}
	if opts.Cwd == "" {
		opts.Cwd = t.TempDir()
	}
	opts.NoContextFiles, opts.NoSkills = true, true
	opts.StreamFn = h.model.stream
	h.sess = coding.NewSession(opts)
	h.sess.RegisterTools(Definition(Options{Session: h.sess, Store: &Store{}, Loadout: LoadoutOptions{
		Mode:         func() Mode { return h.mode },
		InlineBudget: func() *int { return h.budget },
	}, Notices: notices}))
	// pi's harness starts with initialActiveToolNames ["codemode"] and
	// activates the extension's tools on registration (includeAllExtensionTools).
	active := []string{ToolName}
	if opts.ToolNames != nil {
		active = opts.ToolNames
	}
	h.sess.SetActiveTools(active)
	h.sess.RegisterTools(append(testTools(t), extra...)...)
	return h
}

// run runs one script through the model and returns the codemode result.
func (h *harness) run(code string) *ai.ToolResultMessage {
	h.t.Helper()
	h.model.respond(codemodeCall(code), &ai.AssistantMessage{StopReason: ai.StopStop, Content: ai.ContentList{ai.TextContent{Text: "done"}}})
	if _, err := h.sess.Run(context.Background(), "go"); err != nil {
		h.t.Fatal(err)
	}
	var result *ai.ToolResultMessage
	for _, m := range h.sess.History() {
		switch r := m.(type) {
		case ai.ToolResultMessage:
			if r.ToolName == ToolName {
				result = &r
			}
		case *ai.ToolResultMessage:
			if r.ToolName == ToolName {
				result = r
			}
		}
	}
	if result == nil {
		h.t.Fatal("no codemode result")
	}
	return result
}

var headerPattern = regexp.MustCompile(`^Script (completed|failed)\nWall time \d+\.\d seconds\nOutput:\n$`)

// resultText is the output after the script header, which is checked on the
// way.
func resultText(t *testing.T, m *ai.ToolResultMessage) string {
	t.Helper()
	if len(m.Content) == 0 {
		t.Fatal("empty result")
	}
	header, ok := m.Content[0].(ai.TextContent)
	if !ok || !headerPattern.MatchString(header.Text) {
		t.Fatalf("header %#v", m.Content[0])
	}
	var parts []string
	for _, c := range m.Content[1:] {
		switch b := c.(type) {
		case ai.TextContent:
			parts = append(parts, b.Text)
		case ai.ImageContent:
			parts = append(parts, "<image>")
		}
	}
	return strings.Join(parts, "\n")
}

var pngLabel = regexp.MustCompile(`^\[Image saved to (\S+\.png) \(image/png, \d+B\)\]$`)

// checkSavedImages replaces the [Image saved to ...] labels with <saved> after
// checking that each file holds the tiny PNG, and removes the files.
func checkSavedImages(t *testing.T, text string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		m := pngLabel.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		data, err := os.ReadFile(m[1])
		os.Remove(m[1])
		if err != nil || base64.StdEncoding.EncodeToString(data) != tinyPNG {
			t.Fatalf("saved image %s: %v", m[1], err)
		}
		lines[i] = "<saved>"
	}
	return strings.Join(lines, "\n")
}

func details(t *testing.T, m *ai.ToolResultMessage) Details {
	t.Helper()
	switch d := m.Details.(type) {
	case Details:
		return d
	case *Details:
		return *d
	}
	t.Fatalf("details %T", m.Details)
	return Details{}
}

func toolDescription(s *coding.Session, name string) string {
	for _, tool := range s.Agent.State().Tools {
		if tool.Name == name {
			return tool.Description
		}
	}
	return ""
}

func TestPresentsCallableToolsPerMode(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	h.sess.SetActiveTools([]string{"read", "echo", ToolName})
	if d := toolDescription(h.sess, "echo"); !strings.Contains(d, "Codemode: `tools.echo(args)` resolves to") || strings.Contains(d, "codemode tool declaration:") {
		t.Fatalf("on: echo:\n%s", d)
	}
	if d := toolDescription(h.sess, ToolName); strings.Contains(d, "### `echo`") {
		t.Fatalf("on: codemode lists echo:\n%s", d)
	}
	h.run("return 1")
	if !slices.Contains(h.model.requests[0], "echo") || !strings.Contains(h.model.prompts[0], "\n- read: ") {
		t.Fatalf("on: request %v", h.model.requests[0])
	}

	// only: codemode lists echo, which stays active but is left out of
	// requests.
	h.mode = ModeOnly
	h.sess.SetActiveTools([]string{"read", "echo", ToolName})
	if d := toolDescription(h.sess, "echo"); strings.Contains(d, "Codemode: `tools.echo") {
		t.Fatalf("only: echo:\n%s", d)
	}
	codemode := toolDescription(h.sess, ToolName)
	if !strings.Contains(codemode, "### `echo`") || strings.Contains(codemode, "### `stats`") {
		t.Fatalf("only: codemode:\n%s", codemode)
	}
	before := len(h.model.requests)
	h.run("return 1")
	request, prompt := h.model.requests[before], h.model.prompts[before]
	if !slices.Contains(request, ToolName) || slices.Contains(request, "echo") || slices.Contains(request, "read") {
		t.Fatalf("only: request %v", request)
	}
	// The prompt's tool list matches the declarations, and hidden tools'
	// guidelines move from the rules to their codemode sections.
	if strings.Contains(prompt, "\n- read: ") || !strings.Contains(prompt, "\n- codemode: ") || strings.Contains(prompt, "Use read to examine files") {
		t.Fatalf("only: prompt:\n%s", prompt)
	}
	if !strings.Contains(codemode, "- Use read to examine files instead of cat or sed.") {
		t.Fatalf("only: codemode lacks read's guideline:\n%s", codemode)
	}

	// Without codemode, tools keep their plain descriptions.
	h.sess.SetActiveTools([]string{"echo"})
	if d := toolDescription(h.sess, "echo"); d != "Echo text back.\n\nSecond paragraph." {
		t.Fatalf("plain echo: %q", d)
	}
}

func TestDescribeToolShowsUnlistedGuidelines(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	h.mode = ModeOnly
	zero := 0
	h.budget = &zero
	h.sess.SetActiveTools([]string{"read", ToolName})
	if strings.Contains(toolDescription(h.sess, ToolName), "### `read`") {
		t.Fatal("read listed with a zero budget")
	}
	if text := resultText(t, h.run(`text(await describeTool("read"))`)); !strings.Contains(text, "- Use read to examine files instead of cat or sed.") {
		t.Fatalf("describeTool:\n%s", text)
	}
}

func TestNestedCallsInParallel(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	result := h.run(`
		const [a, b, stats] = await Promise.all([
			tools.echo({ text: "one" }),
			tools.echo({ text: "two" }),
			tools.stats({}),
		]);
		console.log("files", stats.files);
		text(ALL_TOOLS.map((tool) => tool.name).join(","));
		return { a, b, names: stats.names };
	`)
	if result.IsError {
		t.Fatal("isError")
	}
	want := "==> text 1/2 <==\necho,stats,screenshot\n==> text 2/2 <==\n{\"a\":\"echo: one\",\"b\":\"echo: two\",\"names\":[\"a\",\"b\"]}\n<console_output>\nfiles 2\n</console_output>"
	if got := resultText(t, result); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	var calls []string
	for _, c := range details(t, result).Calls {
		calls = append(calls, c.Name+":"+string(c.Status))
		if !strings.HasPrefix(c.ID, result.ToolCallID+"/") {
			t.Fatalf("call id %s", c.ID)
		}
	}
	slices.Sort(calls)
	if !slices.Equal(calls, []string{"echo:ok", "echo:ok", "stats:ok"}) {
		t.Fatalf("calls %v", calls)
	}
	// Nested calls never become transcript tool results.
	n := 0
	for _, m := range h.sess.History() {
		if m.MessageRole() == ai.RoleToolResult {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d tool results", n)
	}
}

func TestNestedCallsThroughHooks(t *testing.T) {
	h := setup(t, coding.SessionOptions{
		BeforeToolCall: func(_ context.Context, c agent.BeforeToolCallContext) *agent.BeforeToolCallResult {
			if c.ToolCall.Name == "echo" && c.Args["text"] == "forbidden" {
				return &agent.BeforeToolCallResult{Block: true, Reason: "echo of forbidden text is blocked"}
			}
			return nil
		},
		AfterToolCall: func(_ context.Context, c agent.AfterToolCallContext) *agent.AfterToolCallResult {
			if c.ToolCall.Name == "stats" {
				return &agent.AfterToolCallResult{Content: ai.ContentList{ai.TextContent{Text: "redacted"}}, HasContent: true}
			}
			return nil
		},
	})
	result := h.run(`
		let blocked;
		try {
			await tools.echo({ text: "forbidden" });
		} catch (error) {
			blocked = error.message;
		}
		const stats = await tools.stats({});
		return { blocked, stats };
	`)
	// Replacing content without replacing structured content drops the
	// structured result.
	if got := resultText(t, result); got != `{"blocked":"echo of forbidden text is blocked","stats":"redacted"}` {
		t.Fatalf("got %s", got)
	}
	var statuses []string
	for _, c := range details(t, result).Calls {
		statuses = append(statuses, string(c.Status))
	}
	if !slices.Equal(statuses, []string{"error", "ok"}) {
		t.Fatalf("statuses %v", statuses)
	}
}

// pi agent-session-codemode.test.ts "summarizes union and multi-line output
// types instead of inlining them" (f6ee9ce9, #11501); the schemas are the
// JSON TypeBox writes for the test's.
func TestDescriptionSummarizesUnionAndMultiLineOutputTypes(t *testing.T) {
	outputTool := func(name, outputSchema string) coding.ToolDefinition {
		var schema ai.Schema
		if err := json.Unmarshal([]byte(outputSchema), &schema); err != nil {
			t.Fatal(err)
		}
		return coding.ToolDefinition{Tool: agent.AgentTool{Name: name, Label: name, Description: name + " tool", Parameters: ai.Object(), OutputSchema: &schema,
			Execute: func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
				return agent.AgentToolResult{}, nil
			}}}
	}
	process := outputTool("process", `{"anyOf":[`+
		`{"type":"object","properties":{"action":{"const":"start","type":"string"},"process":{"type":"object","properties":{"id":{"description":"Process id","type":"string"},"pid":{"type":"integer"}},"required":["id","pid"]}},"required":["action","process"]},`+
		`{"type":"object","properties":{"action":{"const":"list","type":"string"},"processes":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}},"truncated":{"type":"boolean"}},"required":["action","processes"]}]}`)
	entries := outputTool("entries", `{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"size":{"type":"integer"}},"required":["id","size"]}}`)
	count := outputTool("count", `{"type":"number"}`)
	h := setup(t, coding.SessionOptions{}, process, entries, count)
	h.sess.SetActiveTools([]string{"process", "entries", "count", ToolName})
	for name, want := range map[string]string{
		"process": "process tool\n\nCodemode: `tools.process(args)` resolves to `{ action: \"start\", process } | { action: \"list\", processes, truncated? }`.",
		"entries": "entries tool\n\nCodemode: `tools.entries(args)` resolves to a value; `await describeTool(\"entries\")` shows its type.",
		"count":   "count tool\n\nCodemode: `tools.count(args)` resolves to `number`.",
	} {
		if got := toolDescription(h.sess, name); got != want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, want)
		}
	}
}

func TestNestedUsageOnResult(t *testing.T) {
	billed := agent.AgentTool{Name: "billed", Label: "Billed", Description: "Run a model", Parameters: ai.Object(),
		Execute: func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "ran"}},
				Usage: &ai.Usage{Input: 100, TotalTokens: 100, Cost: ai.CostBreakdown{Input: 0.25, Total: 0.25}}}, nil
		}}
	h := setup(t, coding.SessionOptions{}, coding.ToolDefinition{Tool: billed})
	result := h.run(`await tools.billed({}); await tools.billed({}); await tools.echo({ text: "x" });`)
	if result.Usage == nil || result.Usage.Input != 200 || result.Usage.TotalTokens != 200 || math.Abs(result.Usage.Cost.Total-0.5) > 1e-12 {
		t.Fatalf("usage %+v", result.Usage)
	}
}

func TestReplacedStructuredContent(t *testing.T) {
	h := setup(t, coding.SessionOptions{
		AfterToolCall: func(_ context.Context, c agent.AfterToolCallContext) *agent.AfterToolCallResult {
			if c.ToolCall.Name == "stats" {
				return &agent.AfterToolCallResult{Content: ai.ContentList{ai.TextContent{Text: "0 files"}}, HasContent: true,
					StructuredContent: map[string]any{"files": 0, "names": []string{}}}
			}
			return nil
		},
	})
	if got := resultText(t, h.run("return await tools.stats({});")); got != `{"files":0,"names":[]}` {
		t.Fatalf("got %s", got)
	}
}

func TestImagesInOutputOrder(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	result := h.run(`
		// Tools without an outputSchema resolve to their text; images are not passed on.
		const shot = await tools.screenshot({});
		text(shot);
		image("data:image/png;base64,` + tinyPNG + `");
		image("data:image/png;base64,` + tinyPNG + `");
		text("after");
	`)
	lines := strings.Split(resultText(t, result), "\n")
	want := []string{"==> text 1/2 <==", "captured", lines[2], "<image>", lines[2], "<image>", "==> text 2/2 <==", "after"}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines %q", lines)
	}
	if checkSavedImages(t, lines[2]) != "<saved>" {
		t.Fatalf("label %q", lines[2])
	}
	if img, ok := result.Content[2].(ai.ImageContent); !ok || img.Data != tinyPNG || img.MimeType != "image/png" {
		t.Fatalf("content[2] %#v", result.Content[2])
	}
}

func TestTextItemsAndConsole(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	result := h.run("text(\"one\\ntwo\");\nconsole.log(\"a\");\nconsole.log(\"b\");\ntext(\"three\\n\");\nreturn 4;")
	if len(result.Content) != 2 {
		t.Fatalf("%d blocks", len(result.Content))
	}
	want := "==> text 1/3 <==\none\ntwo\n==> text 2/3 <==\nthree\n==> text 3/3 <==\n4\n<console_output>\na\nb\n</console_output>"
	if got := resultText(t, result); got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestScriptFailureKeepsPartialOutput(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	result := h.run("text(\"partial\");\nconsole.log(\"log\");\nawait tools.echo({ text: \"x\" });\nthrow new Error(\"boom\");")
	if !result.IsError {
		t.Fatal("not an error")
	}
	if header := result.Content[0].(ai.TextContent).Text; !strings.HasPrefix(header, "Script failed\n") {
		t.Fatalf("header %q", header)
	}
	text := resultText(t, result)
	if !regexp.MustCompile(`^partial\n<console_output>\nlog\n</console_output>\nScript error:\nError: boom\n`).MatchString(text) ||
		!strings.Contains(text, "codemode.js:4") ||
		!strings.Contains(text, "Tool calls made before the failure (they are not undone): echo (ok)") {
		t.Fatalf("text:\n%s", text)
	}
}

func TestOptionsTimeoutAndInvalid(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	timedOut := h.run("// @options: {\"timeout_ms\": 200}\nwhile (true) {}")
	if !timedOut.IsError || !strings.Contains(resultText(t, timedOut), "Script error:\nScript timed out") {
		t.Fatalf("timeout: %v", timedOut.Content)
	}
	for code, want := range map[string]string{
		"// @options: {\"yield\": 1}\ntext(1)":                          "@options only supports `max_output_tokens` and `timeout_ms`; got `yield`",
		"// count\n// @options: {\"timeout_ms\": 200}\nwhile (true) {}": "The // @options: line must be the first line of the input; found one on line 2",
	} {
		result := h.run(code)
		if !result.IsError || len(result.Content) != 1 || result.Content[0].(ai.TextContent).Text != want {
			t.Errorf("%q: %#v", code, result.Content)
		}
	}
}

func TestMemoryLimit(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	result := h.run("// @options: {\"timeout_ms\": 30000}\nlet a = [];\ntry { while (true) a.push(\"x\".repeat(1 << 20) + a.length); } catch (error) { const n = a.length; a = null; return { n, error: String(error) }; }")
	if result.IsError {
		t.Fatalf("isError: %s", resultText(t, result))
	}
	var got struct {
		N     int    `json:"n"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(resultText(t, result)), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Error, "out of memory") || got.N >= 512 {
		t.Fatalf("%+v", got)
	}
}

func TestTruncatesAndSpills(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	result := h.run("// @options: {\"max_output_tokens\": 30}\nfor (let i = 0; i < 100; i++) text(\"row \" + i);\nimage(\"data:image/png;base64," + tinyPNG + "\");")
	path := details(t, result).FullOutputPath
	if path == "" {
		t.Fatal("no spill file")
	}
	defer os.Remove(path)
	text := resultText(t, result)
	for _, want := range []string{"row 0\n", "tokens truncated", "row 99\n", "[Full output: " + path + " (read with offset/limit)]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if !strings.HasPrefix(text, "Warning: truncated output") || strings.Contains(text, "row 50\n") {
		t.Fatalf("text:\n%s", text)
	}
	if img, ok := result.Content[len(result.Content)-1].(ai.ImageContent); !ok || img.Data != tinyPNG {
		t.Fatal("image not last")
	}
	lines := strings.Split(text, "\n")
	if checkSavedImages(t, lines[len(lines)-2]) != "<saved>" {
		t.Fatalf("label %q", lines[len(lines)-2])
	}
	full, _ := os.ReadFile(path)
	var want []string
	for i := range 100 {
		want = append(want, fmt.Sprintf("==> text %d/100 <==\nrow %d", i+1, i))
	}
	if string(full) != strings.Join(want, "\n") {
		t.Fatalf("spill file:\n%s", full)
	}
	if details(t, h.run("return { ok: true };")).FullOutputPath != "" {
		t.Fatal("small output spilled")
	}
}

func TestOutputAtItemLimitIsLinear(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	started := time.Now()
	result := h.run(`for (let i = 0; i < 100000; i++) text("");`)
	elapsed := time.Since(started)
	os.Remove(details(t, result).FullOutputPath)
	if result.IsError || !strings.Contains(resultText(t, result), "==> text 100000/100000 <==") {
		t.Fatal("output")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %v", elapsed)
	}
}

func TestBashResolvesToStructuredResult(t *testing.T) {
	h := setup(t, coding.SessionOptions{ToolNames: []string{ToolName, "bash"}})
	result := h.run("const r = await tools.bash({ command: \"echo out; exit 3\" });\ntext(JSON.stringify([r.output, r.exit_code, typeof r.wall_time_seconds]));")
	if result.IsError {
		t.Fatal(resultText(t, result))
	}
	if got := resultText(t, result); got != `["out\n",3,"number"]` {
		t.Fatalf("got %s", got)
	}
}

func TestReadResolvesToTextAndImageBlocks(t *testing.T) {
	dir := t.TempDir()
	png, _ := base64.StdEncoding.DecodeString(tinyPNG)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "pixel.png"), png, 0o644)
	h := setup(t, coding.SessionOptions{Cwd: dir, ToolNames: []string{ToolName, "read"}})
	result := h.run("text(await tools.read({ path: \"notes.txt\" }));\nconst shot = await tools.read({ path: \"pixel.png\" });\ntext(shot.note);\nimage(shot);")
	if result.IsError {
		t.Fatal(resultText(t, result))
	}
	if got := checkSavedImages(t, resultText(t, result)); got != "==> text 1/2 <==\nhello\n==> text 2/2 <==\nRead image file [image/png]\n<saved>\n<image>" {
		t.Fatalf("got:\n%s", got)
	}
	if img, ok := result.Content[len(result.Content)-1].(ai.ImageContent); !ok || img.Data != tinyPNG {
		t.Fatalf("last %#v", result.Content[len(result.Content)-1])
	}
}

const increment = "const next = (load(\"count\") ?? 0) + 1;\nstore(\"count\", next);\nreturn next;"

func TestStoreAcrossCalls(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	for _, want := range []string{"1", "2"} {
		if got := resultText(t, h.run(increment)); got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	if got := resultText(t, h.run("store(\"count\", undefined);\nreturn load(\"count\") === undefined;")); got != "true" {
		t.Fatalf("delete: %s", got)
	}
	if got := resultText(t, h.run(increment)); got != "1" {
		t.Fatalf("after delete: %s", got)
	}
}

func TestStoreKeepsNothingFromFailedScripts(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	if !h.run("store(\"count\", 5);\nthrow new Error(\"boom\");").IsError {
		t.Fatal("not an error")
	}
	if got := resultText(t, h.run("return load(\"count\") ?? \"missing\";")); got != "missing" {
		t.Fatalf("got %s", got)
	}
}

// setupRecorded is setup with the session recorded to a file, as pi's
// harness records through its session manager. The session directory lives
// under a temporary home.
func setupRecorded(t *testing.T) (*harness, *coding.SessionRecorder) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	h := setup(t, coding.SessionOptions{})
	rec, err := coding.StartSession(h.sess.Cwd, h.sess.Model)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h.sess.Record(rec)
	return h, rec
}

// storeEntries is pi's storeEntries(): the codemode-store data on the branch.
func storeEntries(t *testing.T, rec *coding.SessionRecorder) []string {
	t.Helper()
	var out []string
	for _, raw := range rec.CustomEntries(StoreEntryType) {
		out = append(out, string(raw))
	}
	return out
}

// fileStoreEntries is the codemode-store data as the session file holds it.
func fileStoreEntries(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry struct {
			Type       string          `json:"type"`
			CustomType string          `json:"customType"`
			Data       json.RawMessage `json:"data"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Type == "custom" && entry.CustomType == StoreEntryType {
			out = append(out, string(entry.Data))
		}
	}
	return out
}

// pi "persists store() writes as custom entries for later calls"; the file
// check is Go-only (pi's test reads the session manager, which is the file).
// pi also counts entry_appended events; the port has no such event.
func TestStorePersistsAsCustomEntries(t *testing.T) {
	h, rec := setupRecorded(t)
	for _, want := range []string{"1", "2"} {
		if got := resultText(t, h.run(increment)); got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	want := []string{`{"set":{"count":1},"delete":[]}`, `{"set":{"count":2},"delete":[]}`}
	if got := storeEntries(t, rec); !slices.Equal(got, want) {
		t.Fatalf("entries %q, want %q", got, want)
	}
	if got := resultText(t, h.run("store(\"count\", undefined);\nreturn load(\"count\") === undefined;")); got != "true" {
		t.Fatalf("delete: %s", got)
	}
	want = append(want, `{"set":{},"delete":["count"]}`)
	if got := storeEntries(t, rec); !slices.Equal(got, want) {
		t.Fatalf("entries after delete %q, want %q", got, want)
	}
	if got := fileStoreEntries(t, rec.Path()); !slices.Equal(got, want) {
		t.Fatalf("file holds %q, want %q", got, want)
	}
}

// pi "appends nothing for failed scripts or scripts without writes".
func TestStoreAppendsNothingForFailedOrReadOnlyScripts(t *testing.T) {
	h, rec := setupRecorded(t)
	if !h.run("store(\"count\", 5);\nthrow new Error(\"boom\");").IsError {
		t.Fatal("not an error")
	}
	if result := h.run("return load(\"count\") ?? \"missing\";"); result.IsError || resultText(t, result) != "missing" {
		t.Fatalf("read-only script: %#v", result)
	}
	if got := storeEntries(t, rec); len(got) != 0 {
		t.Fatalf("entries %q, want none", got)
	}
}

// pi "loads the values written on the current branch".
func TestStoreLoadsTheCurrentBranch(t *testing.T) {
	h, rec := setupRecorded(t)
	h.run(increment)
	tree, err := coding.LoadSessionTree(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	var firstPrompt string
	for _, e := range tree.Entries {
		if e.Type == "message" {
			firstPrompt = e.ID
			break
		}
	}
	if firstPrompt == "" {
		t.Fatal("no first prompt entry")
	}
	if got := resultText(t, h.run(increment)); got != "2" {
		t.Fatalf("got %s, want 2", got)
	}
	// Branch from the first prompt: the store entries written after it are
	// on another path.
	rec.ForkFrom(firstPrompt)
	if got := resultText(t, h.run(increment)); got != "1" {
		t.Fatalf("after the fork got %s, want 1", got)
	}
}

// pi "folds store entries from the root, ignoring malformed data"; the
// other-extension entry is left out by CustomEntries, which filters by type,
// and is checked in the recorder's tests. The array set and the non-string
// delete are Go-only cases of isStoreEntryData.
func TestReadStoreFolds(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	got := readStore([]json.RawMessage{
		raw(`{"set":{"a":1,"b":{"c":2}},"delete":[]}`),
		raw(`{"set":{"a":3},"delete":["b"]}`),
		raw(`{"set":{"z":1}}`),
		raw(`{"set":null,"delete":[]}`),
		raw(`{"set":{"y":1},"delete":[1]}`),
		raw(`[{"set":{"x":1},"delete":[]}]`),
		nil,
	})
	if len(got) != 1 || string(got["a"].(json.RawMessage)) != "3" {
		t.Fatalf("store %v, want {a: 3}", got)
	}
	got = readStore([]json.RawMessage{raw(`{"set":["p","q"],"delete":[]}`)})
	if len(got) != 2 || string(got["0"].(json.RawMessage)) != `"p"` || string(got["1"].(json.RawMessage)) != `"q"` {
		t.Fatalf("array set %v, want its indices as keys", got)
	}
}

// Go-only: the values survive reopening the session file, as pi's do when it
// loads a session.
func TestStoreSurvivesResume(t *testing.T) {
	h, rec := setupRecorded(t)
	h.run(increment)
	h.run(increment)
	path := rec.Path()
	rec.Close()

	resumed := setup(t, coding.SessionOptions{})
	rec2, err := coding.ResumeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec2.Close() })
	resumed.sess.Record(rec2)
	if got := resultText(t, resumed.run(increment)); got != "3" {
		t.Fatalf("after resume got %s, want 3", got)
	}
}

func TestRunsWithoutSession(t *testing.T) {
	def := Definition(Options{})
	result, err := def.Tool.Execute(context.Background(), "direct", map[string]any{"code": increment}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Content[1].(ai.TextContent).Text; got != "1" {
		t.Fatalf("got %q", got)
	}
}

// TestNonObjectArguments is Go-only: a script that passes a value that is not
// an object gets the validator's error, as pi's would, without the call
// reaching the tool.
func TestNonObjectArguments(t *testing.T) {
	h := setup(t, coding.SessionOptions{})
	text := resultText(t, h.run(`try { await tools.echo(5); } catch (e) { return e.message; }`))
	want := "Validation failed for tool \"echo\":\n  - root: must be object\n\nReceived arguments:\n5"
	if text != want {
		t.Fatalf("got:\n%s\nwant:\n%s", text, want)
	}
	if got := resultText(t, h.run(`return await tools.echo();`)); !strings.Contains(got, "Validation failed for tool \"echo\"") || !strings.Contains(got, "text") {
		t.Fatalf("undefined args: %s", got)
	}
}

func TestScriptReach(t *testing.T) {
	if all, _ := ScriptReach(`const r = await searchTools("docs")`); !all {
		t.Fatal("discovery global not seen")
	}
	all, words := ScriptReach(`await tools["mcp__docs__" + name]({})`)
	if all || !slices.Contains(words, "mcp__docs__") || !slices.Contains(words, "tools") {
		t.Fatalf("%v %v", all, words)
	}
}
