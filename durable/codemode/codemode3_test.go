package codemode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	sandbox "github.com/aleksandrgradoboev-svg/bismarck-engine/codemode"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/internal/chattest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// pi's "durable code mode output and tool search", codemode-describe.test,
// and harness-telemetry's code mode replay case.

type scriptRun struct {
	text    string
	message ai.ToolResultMessage
	details ToolDetails
}

// runScript runs code once in a chat with a local environment.
func runScript(t *testing.T, c *testChat, code string) scriptRun {
	t.Helper()
	c.holds.block.Store(false)
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{script(code), chattest.Done})
	_, root := c.chat.Open(t, memory.New(), env.NewLocal(t.TempDir()))
	chattest.SubmitAndWait(t, root, "go")
	message := chattest.ResultMessage(chattest.ToolResults(chattest.Entries(t, root))[0])
	var parts []string
	for _, item := range message.Content {
		switch c := item.(type) {
		case ai.TextContent:
			parts = append(parts, c.Text)
		case ai.ImageContent:
			parts = append(parts, "<image>")
		}
	}
	raw, err := json.Marshal(message.Details)
	must(t, err)
	var details ToolDetails
	must(t, json.Unmarshal(raw, &details))
	return scriptRun{text: strings.Join(parts, ""), message: message, details: details}
}

func TestCodemodeLaysOutTextItemsConsoleLinesAndTheReturnedValueUnderAHeader(t *testing.T) {
	r := runScript(t, newTestChat(t, nil, nil), `text("one"); console.log("log", 1); text({ two: 2 }); return "three";`)
	if !header.MatchString(r.text) {
		t.Fatalf("text = %q", r.text)
	}
	for _, want := range []string{"==> text 1/3 <==\none\n==> text 2/3 <==\n{\"two\":2}\n==> text 3/3 <==\nthree", "<console_output>\nlog 1\n</console_output>"} {
		if !strings.Contains(r.text, want) {
			t.Fatalf("text %q lacks %q", r.text, want)
		}
	}
}

func TestCodemodeCutsTextPastTheTokenBudgetToItsStartAndEndWithTheFullTextInAFile(t *testing.T) {
	r := runScript(t, newTestChat(t, nil, nil), "// @options: {\"max_output_tokens\": 20}\n\t\ttext(\"BEGIN\" + \"x\".repeat(1000) + \"END\");")
	for _, want := range []string{"Warning: truncated output", "BEGIN", "END", "[Full output: " + r.details.FullOutputPath} {
		if !strings.Contains(r.text, want) {
			t.Fatalf("text %q lacks %q", r.text, want)
		}
	}
	b, err := os.ReadFile(r.details.FullOutputPath)
	must(t, err)
	if string(b) != "BEGIN"+strings.Repeat("x", 1000)+"END" {
		t.Fatalf("full output has %d bytes", len(b))
	}
}

func TestCodemodeKeepsAResultInsideTheToolsOutputLimitsWhateverMaxOutputTokensSaysSpillingTheRest(t *testing.T) {
	// 6 MiB of text: over the Harness's 4 MiB limit, which would cut it
	// without a spill file.
	big := runScript(t, newTestChat(t, nil, nil), "// @options: {\"max_output_tokens\": 2000000}\n\t\treturn \"x\".repeat(6 * 1024 * 1024);")
	if !strings.Contains(big.text, "Warning: truncated output") || len(big.text) > 1024*1024+1000 {
		t.Fatalf("big text has %d bytes", len(big.text))
	}
	b, err := os.ReadFile(big.details.FullOutputPath)
	must(t, err)
	if len(b) != 6*1024*1024 {
		t.Fatalf("spill has %d bytes", len(b))
	}
	// 200,000 short lines: within the byte limit, over the 100,000 lines one.
	long := runScript(t, newTestChat(t, nil, nil), "// @options: {\"max_output_tokens\": 2000000}\n\t\treturn Array.from({ length: 200000 }, () => \"a\").join(\"\\n\");")
	if !strings.Contains(long.text, "Warning: truncated output") || strings.Count(long.text, "\n")+1 >= 100_000 || long.details.FullOutputPath == "" {
		t.Fatalf("long text has %d lines, spill %q", strings.Count(long.text, "\n")+1, long.details.FullOutputPath)
	}
}

// crashAfterWrite writes a file, then hangs until the call is cancelled, as
// a process that dies after the write and before the result commits.
type crashAfterWrite struct {
	*env.Local
	written *signal
	path    atomic.Pointer[string]
}

func (e *crashAfterWrite) WriteFile(ctx context.Context, path string, content []byte) error {
	err := e.Local.WriteFile(ctx, path, content)
	e.path.Store(&path)
	e.written.fire()
	<-ctx.Done()
	return err
}

func TestCodemodeWritesTheResultsFilesToTheSamePathsWhenItRerunsAfterACrash(t *testing.T) {
	c := newTestChat(t, nil, nil)
	c.holds.block.Store(false)
	cwd, dir := t.TempDir(), filepath.Join(t.TempDir(), "store")
	crashing := &crashAfterWrite{Local: env.NewLocal(cwd), written: newSignal()}
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{script("// @options: {\"max_output_tokens\": 20}\nreturn \"x\".repeat(1000);"), chattest.Done})
	h, root := c.chat.Open(t, openJSONL(t, dir), crashing)
	submission, err := root.Submit(bg, chattest.Input("go"))
	must(t, err)
	id := submission.ID
	crashing.written.wait(t, "the full output's write")
	first := *crashing.path.Load()
	must(t, h.Close(bg))
	h, root = c.chat.Open(t, openJSONL(t, dir), env.NewLocal(cwd))
	must(t, h.Resume())
	reopened, err := h.Submission(bg, id)
	must(t, err)
	record, err := reopened.Wait(bg)
	must(t, err)
	if record.Status != durable.SubmissionDone {
		t.Fatalf("status = %s", record.Status)
	}
	message := chattest.ResultMessage(chattest.ToolResults(chattest.Entries(t, root))[0])
	raw, _ := json.Marshal(message.Details)
	var details ToolDetails
	must(t, json.Unmarshal(raw, &details))
	if details.FullOutputPath != first {
		t.Fatalf("full output %q, first written %q", details.FullOutputPath, first)
	}
	files, err := os.ReadDir(filepath.Dir(first))
	must(t, err)
	if len(files) != 1 || files[0].Name() != "output.txt" {
		t.Fatalf("files = %v", files)
	}
}

func TestCodemodeSavesImagesToFilesAndNamesThemNextToTheImage(t *testing.T) {
	r := runScript(t, newTestChat(t, nil, nil), `image("data:image/png;base64,`+onePixelPNG+`");`)
	saved := regexp.MustCompile(`\[Image saved to (\S+) \(image/png, \d+ bytes\)\]`).FindStringSubmatch(r.text)
	if saved == nil {
		t.Fatalf("text = %q", r.text)
	}
	b, err := os.ReadFile(saved[1])
	must(t, err)
	if base64.StdEncoding.EncodeToString(b) != onePixelPNG {
		t.Fatal("saved image differs")
	}
	hasImage := false
	for _, item := range r.message.Content {
		if _, ok := item.(ai.ImageContent); ok {
			hasImage = true
		}
	}
	if !hasImage {
		t.Fatal("no image in the result")
	}
}

func TestCodemodeCallsADeferredToolTheModelIsNotOfferedAndFindsItInAllTools(t *testing.T) {
	c := newTestChat(t, nil, nil)
	later := &harness.ToolRegistration{
		Tool:     ai.Tool{Name: "later", Description: "A deferred tool", Parameters: ai.Object()},
		Deferred: true,
		Execute: func(context.Context, map[string]any, harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			return text("later ran"), nil
		},
	}
	c.chat.Install(t, "tool:later", later)
	r := runScript(t, c, `return [await tools.later({}), ALL_TOOLS.some((tool) => tool.name === "later")];`)
	if !strings.Contains(r.text, `["later ran",true]`) {
		t.Fatalf("text = %q", r.text)
	}
}

func TestCodemodeReportsAFailedScriptWithItsErrorAndTheCallsAlreadyMade(t *testing.T) {
	r := runScript(t, newTestChat(t, nil, nil), `await tools.echo({ text: "a" }); throw new Error("boom");`)
	if !r.message.IsError || !strings.HasPrefix(r.text, "Script failed\n") {
		t.Fatalf("text = %q", r.text)
	}
	for _, want := range []string{"Error: boom", "Tool calls made before the failure (they are not undone): echo (ok)"} {
		if !strings.Contains(r.text, want) {
			t.Fatalf("text %q lacks %q", r.text, want)
		}
	}
}

func TestCodemodeFindsToolsWithSearchToolsDescribeToolAndDescribeNamespaceAndListsThemInAllTools(t *testing.T) {
	c := newTestChat(t, nil, []Namespace{{Name: "github", Description: "GitHub MCP server", Prefix: "mcp__github__"}})
	c.chat.Install(t, "tool:mcp__github__list_issues", testTool("mcp__github__list_issues", harness.ReplayUnsafe, func(context.Context, map[string]any, harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
		return harness.ToolExecutionResult{Output: ai.ContentList{}}, nil
	}))
	r := runScript(t, c, `const found = await searchTools("issues");
		const inNamespace = await searchTools("echo", { namespace: "github" });
		return {
			found: found.map((tool) => tool.name),
			inNamespace,
			described: (await describeTool("echo")).includes("echo(args:"),
			missing: (await describeTool("nope")) ?? null,
			namespace: await describeNamespace("github"),
			all: ALL_TOOLS.map((tool) => tool.name).includes("mcp__github__list_issues"),
		};`)
	var got any
	must(t, json.Unmarshal([]byte(header.ReplaceAllString(r.text, "")), &got))
	want := map[string]any{
		"found":       []any{"mcp__github__list_issues"},
		"inNamespace": []any{},
		"described":   true,
		"missing":     nil,
		"namespace":   map[string]any{"name": "github", "description": "GitHub MCP server", "tools": []any{"mcp__github__list_issues"}},
		"all":         true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

// codemode-describe.test.

func describeTool(name, description string) *harness.ToolRegistration {
	return &harness.ToolRegistration{
		Tool: ai.Tool{Name: name, Description: description, Parameters: ai.Object(ai.Prop("path", ai.String("File path")))},
		Execute: func(context.Context, map[string]any, harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			return harness.ToolExecutionResult{Output: ai.ContentList{}}, nil
		},
	}
}

var github = Namespace{Name: "github", Description: "GitHub MCP server", Prefix: "mcp__github__"}

func TestCodemodeKeepsTheToolDescriptionTheSameForEveryConversation(t *testing.T) {
	text := description()
	for _, want := range []string{"`await tools.<name>({ ...args })`", "searchTools(query", "The `codemode` section of the system prompt lists the tools scripts can call"} {
		if !strings.Contains(text, want) {
			t.Fatalf("description lacks %q", want)
		}
	}
	// No models.* globals in the engine: pi's codemodeDescription(false).
	if strings.Contains(text, "`models`") {
		t.Fatal("description mentions models")
	}
}

func TestCodemodeListsCallableToolsAsDeclarationsGroupedByNamespaceWithNamespaceDescriptions(t *testing.T) {
	text, ok := section([]*harness.ToolRegistration{describeTool("read", "Read a file."), describeTool("mcp__github__list_issues", "List issues.")}, []Namespace{github}, 3000)
	if !ok {
		t.Fatal("no section")
	}
	for _, want := range []string{"### `read`\nRead a file.", "read(args: {", "## github\nGitHub MCP server", "Promise<string>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("section lacks %q", want)
		}
	}
	// Tools before namespaces; a namespace's tools after its heading.
	if !(strings.Index(text, "### `read`") < strings.Index(text, "## github") && strings.Index(text, "## github") < strings.Index(text, "### `mcp__github__list_issues`")) {
		t.Fatalf("section order: %q", text)
	}
}

func TestCodemodeListsWhatFitsTheBudgetEveryNamespaceBeforeAnyIsCompleteAndSaysWhatIsLeftOut(t *testing.T) {
	var githubTools []*harness.ToolRegistration
	for i := range 30 {
		githubTools = append(githubTools, describeTool("mcp__github__tool"+strings.TrimSpace(jsonNumber(float64(i))), strings.Repeat("x", 200)))
	}
	text, _ := section(append([]*harness.ToolRegistration{describeTool("read", "Read a file.")}, githubTools...), []Namespace{github}, 400)
	if !strings.Contains(text, "### `read`") || !strings.Contains(text, "## github (some tools not listed)") || strings.Contains(text, "tool29") {
		t.Fatalf("section = %q", text)
	}
	if text, _ := section(githubTools, []Namespace{github}, 10); !strings.Contains(text, "## github (tools not listed)") {
		t.Fatalf("tight section = %q", text)
	}
	if _, ok := section(nil, []Namespace{github}, 3000); ok {
		t.Fatal("a section without tools")
	}
}

func TestCodemodeUsesTheLongestMatchingNamespacePrefix(t *testing.T) {
	text, _ := section([]*harness.ToolRegistration{describeTool("mcp__github__x", "x")}, []Namespace{{Name: "git", Prefix: "mcp__git"}, github}, 3000)
	if !strings.Contains(text, "## github") || strings.Contains(text, "## git\n") {
		t.Fatalf("section = %q", text)
	}
}

func TestCodemodeRendersTheSectionFromTheConversationsCallableToolsIntoTheSystemPrompt(t *testing.T) {
	c := chattest.New(t)
	s, err := sandbox.New(sandbox.Options{})
	must(t, err)
	t.Cleanup(s.Close)
	must(t, c.Registry.Install(NewExtension(Options{Sandbox: s, Namespaces: []Namespace{github}})))
	tools := []*harness.ToolRegistration{describeTool("read", "Read a file."), describeTool("mcp__github__list_issues", "List issues."), describeTool("mcp__github__get_issue", "Get an issue.")}
	tools[1].Callers = []string{harness.CallerTools}
	tools[2].Deferred = true
	must(t, c.Registry.Install(&harness.Extension{Name: "tools", Tools: tools}))
	c.Faux.SetResponses([]providers.FauxResponseStep{providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{providers.FauxText("hi")}, ai.StopStop))})
	_, root := c.Open(t, memory.New(), nil)
	chattest.SubmitAndWait(t, root, "go")
	var system ai.SystemMessage
	for _, entry := range chattest.Entries(t, root) {
		if entry.Kind == durable.SystemEntry.Kind {
			system, _ = entry.Model[0].(ai.SystemMessage)
			break
		}
	}
	text, _ := system.Sections.Get("codemode")
	if text == nil {
		t.Fatalf("sections = %v", system.Sections)
	}
	for _, want := range []string{"### `read`", "### `mcp__github__list_issues`"} {
		if !strings.Contains(*text, want) {
			t.Fatalf("section lacks %q", want)
		}
	}
	// Deferred tools are found with searchTools(), not listed; the code mode
	// tool itself is not callable from scripts.
	if strings.Contains(*text, "mcp__github__get_issue") || strings.Contains(*text, "### `codemode`") {
		t.Fatalf("section = %q", *text)
	}
	// The tool declaration carries the script grammar; ai applies it only
	// for models that support it.
	for _, tool := range system.ToolsAdded {
		if tool.Name == ToolName {
			want := &ai.ConstrainedSamplingConfig{Type: ai.ConstrainedSamplingGrammar, Variants: ai.GrammarVariants{OpenAILark: sandbox.SourceGrammar}}
			if !reflect.DeepEqual(tool.ConstrainedSampling, want) {
				t.Fatalf("constrained sampling = %+v", tool.ConstrainedSampling)
			}
			return
		}
	}
	t.Fatal("codemode not declared")
}

// harness-telemetry: "emits replay for a codemode journal result without
// rerunning its nested tool".
func TestCodemodeEmitsReplayForAJournalResultWithoutRerunningItsNestedTool(t *testing.T) {
	c := chattest.New(t)
	recorder := telemetry.NewInMemoryContext()
	c.Telemetry = recorder
	s, err := sandbox.New(sandbox.Options{MemoryLimitBytes: 64 * 1024 * 1024})
	must(t, err)
	t.Cleanup(s.Close)
	var runs atomic.Int32
	var recover atomic.Bool
	reached := newSignal()
	c.Install(t, "tool:codemode", NewTool(Options{Sandbox: s}))
	c.Install(t, "tool:once", &harness.ToolRegistration{Tool: ai.Tool{Name: "once", Description: "once", Parameters: ai.Object()},
		Execute: func(context.Context, map[string]any, harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			runs.Add(1)
			return harness.ToolExecutionResult{Output: ai.ContentList{}}, nil
		}})
	c.Install(t, "tool:hold", &harness.ToolRegistration{Tool: ai.Tool{Name: "hold", Description: "hold", Parameters: ai.Object()}, Replay: harness.ReplaySafe,
		Execute: func(ctx context.Context, _ map[string]any, _ harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			if !recover.Load() {
				reached.fire()
				<-ctx.Done()
				return harness.ToolExecutionResult{}, context.Cause(ctx)
			}
			return harness.ToolExecutionResult{Output: ai.ContentList{}}, nil
		}})
	c.Faux.SetResponses([]providers.FauxResponseStep{script("await tools.once({}); await tools.hold({});"), chattest.Done})
	dir := filepath.Join(t.TempDir(), "store")
	h, root := c.Open(t, openJSONL(t, dir), nil)
	submission, err := root.Submit(bg, chattest.Input("run"))
	must(t, err)
	reached.wait(t, "hold")
	must(t, h.Close(bg))
	recover.Store(true)
	h, _ = c.Open(t, openJSONL(t, dir), nil)
	reopened, err := h.Submission(bg, submission.ID)
	must(t, err)
	_, err = reopened.Wait(bg)
	must(t, err)
	must(t, h.WaitForIdle(bg))
	eventually(t, func() bool {
		for _, span := range recorder.Spans() {
			if !span.Settled {
				return false
			}
		}
		return true
	})
	if runs.Load() != 1 {
		t.Fatalf("once ran %d times", runs.Load())
	}
	for _, span := range recorder.Spans() {
		for _, event := range span.Events {
			if event.Name == "pi.durable.replay" && event.Attributes["tool.name"] == "once" {
				return
			}
		}
	}
	t.Fatal("no replay event for once")
}
