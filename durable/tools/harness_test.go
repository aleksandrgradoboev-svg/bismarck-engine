package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/env"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/jsonl"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// The cases of pi's harness tests that run the coding tools on an
// environment: harness-tools "coding tools" (2), harness-tools-recovery's
// real bash command (1), and harness-structured-output's bash callers (1).
// The chat setup is pi's chat-support, through the Harness's exported API.

var bg = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type chat struct {
	faux     *providers.FauxProviderRegistration
	models   ai.Models
	registry harness.Registry
}

func newChat(t *testing.T) *chat {
	t.Helper()
	faux := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	t.Cleanup(faux.Unregister)
	models := ai.CreateModels(nil)
	models.SetProvider(ai.CreateProvider(ai.CreateProviderOptions{
		ID:   faux.Models[0].Provider,
		Name: "Faux",
		Auth: ai.ProviderAuth{APIKey: &ai.ApiKeyAuth{Name: "Faux", Resolve: func(context.Context, ai.AuthContext, *ai.Credential) (*ai.AuthResult, error) {
			return &ai.AuthResult{}, nil
		}}},
		Models: faux.Models,
		API: &ai.ProviderStreams{
			StreamSimple: func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				return ai.StreamSimple(ctx, model, ai.Context{Messages: req.Messages}, opts)
			},
		},
	}))
	return &chat{faux: faux, models: models, registry: harness.NewRegistry()}
}

func (c *chat) install(t *testing.T, name string, tools ...*harness.ToolRegistration) {
	t.Helper()
	must(t, c.registry.Install(&harness.Extension{Name: name, Tools: tools}))
}

// open opens a Harness over storage with e as every conversation's
// environment, and returns its root on the faux model; the Harness closes at
// cleanup.
func (c *chat) open(t *testing.T, storage durable.Storage, e env.ExecutionEnv) (*harness.Harness, *harness.Conversation) {
	t.Helper()
	h, err := harness.Open(bg, storage, harness.Options{
		Models:   c.models,
		Registry: c.registry,
		Env:      func(context.Context, harness.EnvTarget) (env.ExecutionEnv, error) { return e, nil },
	})
	must(t, err)
	t.Cleanup(func() { _ = h.Close(bg) })
	root, err := h.Root(bg, &harness.AgentChange{Model: harness.Set(harness.ModelRef{Provider: "faux", ModelID: "faux-1"})}, nil)
	must(t, err)
	return h, root
}

func toolCalls(calls ...[3]any) providers.FauxResponseStep {
	content := ai.ContentList{}
	for _, c := range calls {
		content = append(content, providers.FauxToolCall(c[0].(string), c[1].(map[string]any), c[2].(string)))
	}
	return providers.FauxStatic(providers.FauxAssistantMessage(content, ai.StopToolUse))
}

func callOf(name string, args map[string]any, id string) [3]any { return [3]any{name, args, id} }

var done = providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{providers.FauxText("done")}, ai.StopStop))

func goDraft() harness.SubmissionDraft {
	return harness.SubmissionDraft{Type: durable.SubmissionTypeInput, Input: &harness.InputDraft{Content: harness.TextInput("go")}}
}

func submitAndWait(t *testing.T, root *harness.Conversation) durable.SubmissionRecord {
	t.Helper()
	submission, err := root.Submit(bg, goDraft())
	must(t, err)
	record, err := submission.Wait(bg)
	must(t, err)
	return record
}

func entriesOf(t *testing.T, conversation *harness.Conversation) []durable.EntryRecord {
	t.Helper()
	page, err := conversation.Entries(bg, durable.EntryQuery{}, 1000, nil)
	must(t, err)
	out := make([]durable.EntryRecord, len(page.Items))
	for i, entry := range page.Items {
		out[len(out)-1-i] = entry
	}
	return out
}

func toolResultEntries(entries []durable.EntryRecord) []durable.EntryRecord {
	var out []durable.EntryRecord
	for _, entry := range entries {
		if entry.Kind == durable.ToolResultEntry.Kind {
			out = append(out, entry)
		}
	}
	return out
}

func resultMessage(entry durable.EntryRecord) ai.ToolResultMessage {
	message, _ := entry.Model[0].(ai.ToolResultMessage)
	return message
}

func resultText(message ai.ToolResultMessage) string {
	parts := make([]string, len(message.Content))
	for i, item := range message.Content {
		if text, ok := item.(ai.TextContent); ok {
			parts[i] = text.Text
		} else {
			parts[i] = "[other]"
		}
	}
	return strings.Join(parts, "|")
}

func entryDiagnosticCodes(t *testing.T, entry durable.EntryRecord) []string {
	t.Helper()
	var data struct {
		Diagnostics []harness.ToolDiagnostic `json:"diagnostics"`
	}
	must(t, json.Unmarshal(entry.Data, &data))
	codes := make([]string, len(data.Diagnostics))
	for i, d := range data.Diagnostics {
		codes[i] = d.Code
	}
	return codes
}

func TestCodingToolsAnswerAFailingCommandWithItsRetainedTailAndDiagnosticsInOrder(t *testing.T) {
	c := newChat(t)
	c.install(t, "tool:bash", NewBashTool(BashToolOptions{}))
	command := "i=1; while [ $i -le 3000 ]; do echo line-$i; i=$((i + 1)); done; exit 7"
	c.faux.SetResponses([]providers.FauxResponseStep{toolCalls(callOf("bash", map[string]any{"command": command}, "b")), done})
	_, root := c.open(t, memory.New(), env.NewLocal(t.TempDir()))
	submitAndWait(t, root)
	entries := toolResultEntries(entriesOf(t, root))
	if len(entries) != 1 {
		t.Fatalf("%d tool results", len(entries))
	}
	result := resultMessage(entries[0])
	if !result.IsError {
		t.Fatal("not an error result")
	}
	text := resultText(result)
	if !strings.HasPrefix(text, "line-1001\n") {
		t.Fatalf("text starts %q", text[:min(40, len(text))])
	}
	if got := entryDiagnosticCodes(t, entries[0]); !reflect.DeepEqual(got, []string{"full_output", "exit_code", "truncated"}) {
		t.Fatalf("codes = %v", got)
	}
	if !strings.Contains(text, "line-3000\n|<harness>\n[info] Full output: ") ||
		!strings.Contains(text, "\n[error] Command exited with code 7\n[warn] Output truncated to its end: 1000 lines, ") {
		t.Fatalf("text ends %q", text[max(0, len(text)-400):])
	}
}

func TestCodingToolsReadEditAndRunACommandInOneRunThenAnswer(t *testing.T) {
	directory := t.TempDir()
	must(t, os.WriteFile(filepath.Join(directory, "notes.txt"), []byte("hello world\n"), 0o644))
	c := newChat(t)
	c.install(t, "coding", NewReadTool(ReadToolOptions{}), NewEditTool(), NewBashTool(BashToolOptions{}))
	c.faux.SetResponses([]providers.FauxResponseStep{
		toolCalls(callOf("read", map[string]any{"path": "notes.txt"}, "r")),
		toolCalls(callOf("edit", map[string]any{"path": "notes.txt", "edits": []any{map[string]any{"oldText": "world", "newText": "durable"}}}, "e")),
		toolCalls(callOf("bash", map[string]any{"command": "cat notes.txt"}, "b")),
		done,
	})
	_, root := c.open(t, memory.New(), env.NewLocal(directory))
	if settled := submitAndWait(t, root); settled.Status != durable.SubmissionDone {
		t.Fatalf("status = %s", settled.Status)
	}
	entries := entriesOf(t, root)
	var got [][3]any
	for _, entry := range toolResultEntries(entries) {
		m := resultMessage(entry)
		got = append(got, [3]any{m.ToolName, m.IsError, resultText(m)})
	}
	want := [][3]any{
		{"read", false, "hello world\n"},
		{"edit", false, "Successfully replaced 1 block(s) in notes.txt."},
		{"bash", false, "hello durable\n"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("results = %v", got)
	}
	if last := entries[len(entries)-1]; last.Kind != "pi.assistant" {
		t.Fatalf("last entry = %s", last.Kind)
	}
	b, err := os.ReadFile(filepath.Join(directory, "notes.txt"))
	must(t, err)
	if string(b) != "hello durable\n" {
		t.Fatalf("file = %q", b)
	}
}

func liveToolOutput(t *testing.T, h *harness.Harness, id durable.ConversationID) string {
	t.Helper()
	value, err := h.Snapshot(bg, harness.LiveDoc.Conversation(id))
	must(t, err)
	b, err := json.Marshal(value)
	must(t, err)
	var live struct {
		Tools []harness.ToolSlot `json:"tools"`
	}
	must(t, json.Unmarshal(b, &live))
	if len(live.Tools) == 0 || live.Tools[0].Output == nil {
		return ""
	}
	return *live.Tools[0].Output
}

func TestToolsRecoveryAnswersARealBashCommandInterruptedByCloseAndReopenThenFinishesTheRun(t *testing.T) {
	directory := t.TempDir()
	e := env.NewLocal(directory)
	c := newChat(t)
	c.install(t, "tool:bash", NewBashTool(BashToolOptions{}))
	c.faux.SetResponses([]providers.FauxResponseStep{toolCalls(callOf("bash", map[string]any{"command": "echo started; sleep 30"}, "b")), done})

	store := filepath.Join(directory, "store")
	openStore := func() durable.Storage {
		storage, err := jsonl.OpenDir(bg, store, jsonl.Options{})
		must(t, err)
		return storage
	}
	h, root := c.open(t, openStore(), e)
	submission, err := root.Submit(bg, goDraft())
	must(t, err)
	id := submission.ID
	deadline := time.Now().Add(20 * time.Second)
	for liveToolOutput(t, h, root.ID) != "started\n" {
		if time.Now().After(deadline) {
			t.Fatal("the command's output never reached the slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	must(t, h.Close(bg))

	h, root = c.open(t, openStore(), e)
	reopened, err := h.Submission(bg, id)
	must(t, err)
	record, err := reopened.Wait(bg)
	must(t, err)
	if record.Status != durable.SubmissionDone {
		t.Fatalf("status = %s", record.Status)
	}
	results := toolResultEntries(entriesOf(t, root))
	if len(results) != 1 {
		t.Fatalf("%d tool results", len(results))
	}
	want := "started\n|<harness>\n[error] Tool bash was interrupted and may have partially run\n</harness>"
	if got := resultText(resultMessage(results[0])); got != want {
		t.Fatalf("text = %q", got)
	}
}

func TestStructuredOutputGivesCallersOfBashItsRetainedOutputAndExitCodeAlsoForANonzeroExit(t *testing.T) {
	c := newChat(t)
	c.install(t, "tool:bash", NewBashTool(BashToolOptions{}))
	var received []harness.NestedToolExecutionResult
	probe := &harness.ToolRegistration{
		Tool: ai.Tool{Name: "probe", Description: "probe", Parameters: ai.Object()},
		Execute: func(ctx context.Context, _ map[string]any, api harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			for _, command := range []string{"printf ok", "printf bad; exit 4"} {
				result, err := api.ExecuteTool(ctx, "bash", map[string]any{"command": command}, harness.NestedCallOptions{})
				if err != nil {
					return harness.ToolExecutionResult{}, err
				}
				received = append(received, result)
			}
			return harness.ToolExecutionResult{}, nil
		},
	}
	c.install(t, "tool:probe", probe)
	c.faux.SetResponses([]providers.FauxResponseStep{toolCalls(callOf("probe", map[string]any{}, "p")), done})
	h, root := c.open(t, memory.New(), env.NewLocal(t.TempDir()))
	submitAndWait(t, root)
	must(t, h.Close(bg))
	if len(received) != 2 {
		t.Fatalf("%d results", len(received))
	}
	for i, want := range []struct {
		isError bool
		output  ShellStructuredOutput
	}{{false, ShellStructuredOutput{Output: "ok", ExitCode: 0}}, {true, ShellStructuredOutput{Output: "bad", ExitCode: 4}}} {
		var got ShellStructuredOutput
		must(t, json.Unmarshal(received[i].StructuredOutput, &got))
		if received[i].IsError != want.isError || got != want.output {
			t.Errorf("result %d = isError %v, %+v", i, received[i].IsError, got)
		}
	}
}
