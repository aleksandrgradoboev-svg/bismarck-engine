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
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/internal/chattest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/jsonl"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// The cases of pi's harness tests that run the coding tools on an
// environment: harness-tools "coding tools" (2), harness-tools-recovery's
// real bash command (1), and harness-structured-output's bash callers (1).

var bg = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	chattest.Must(t, err)
}

// call is one tool call of a faux answer, written positionally.
type call struct {
	name string
	args map[string]any
	id   string
}

func toolCalls(calls ...call) providers.FauxResponseStep {
	converted := make([]chattest.Call, len(calls))
	for i, c := range calls {
		converted[i] = chattest.Call{Name: c.name, Args: c.args, ID: c.id}
	}
	return chattest.ToolCalls(converted...)
}

func TestCodingToolsAnswerAFailingCommandWithItsRetainedTailAndDiagnosticsInOrder(t *testing.T) {
	c := chattest.New(t)
	c.Install(t, "tool:bash", NewBashTool(BashToolOptions{}))
	command := "i=1; while [ $i -le 3000 ]; do echo line-$i; i=$((i + 1)); done; exit 7"
	c.Faux.SetResponses([]providers.FauxResponseStep{toolCalls(call{"bash", map[string]any{"command": command}, "b"}), chattest.Done})
	_, root := c.Open(t, memory.New(), env.NewLocal(t.TempDir()))
	chattest.SubmitAndWait(t, root, "go")
	entries := chattest.ToolResults(chattest.Entries(t, root))
	if len(entries) != 1 {
		t.Fatalf("%d tool results", len(entries))
	}
	result := chattest.ResultMessage(entries[0])
	if !result.IsError {
		t.Fatal("not an error result")
	}
	text := chattest.ResultText(result)
	if !strings.HasPrefix(text, "line-1001\n") {
		t.Fatalf("text starts %q", text[:min(40, len(text))])
	}
	if got := chattest.DiagnosticCodes(t, entries[0]); !reflect.DeepEqual(got, []string{"full_output", "exit_code", "truncated"}) {
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
	c := chattest.New(t)
	c.Install(t, "coding", NewReadTool(ReadToolOptions{}), NewEditTool(), NewBashTool(BashToolOptions{}))
	c.Faux.SetResponses([]providers.FauxResponseStep{
		toolCalls(call{"read", map[string]any{"path": "notes.txt"}, "r"}),
		toolCalls(call{"edit", map[string]any{"path": "notes.txt", "edits": []any{map[string]any{"oldText": "world", "newText": "durable"}}}, "e"}),
		toolCalls(call{"bash", map[string]any{"command": "cat notes.txt"}, "b"}),
		chattest.Done,
	})
	_, root := c.Open(t, memory.New(), env.NewLocal(directory))
	if settled := chattest.SubmitAndWait(t, root, "go"); settled.Status != durable.SubmissionDone {
		t.Fatalf("status = %s", settled.Status)
	}
	entries := chattest.Entries(t, root)
	var got [][3]any
	for _, entry := range chattest.ToolResults(entries) {
		m := chattest.ResultMessage(entry)
		got = append(got, [3]any{m.ToolName, m.IsError, chattest.ResultText(m)})
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
	c := chattest.New(t)
	c.Install(t, "tool:bash", NewBashTool(BashToolOptions{}))
	c.Faux.SetResponses([]providers.FauxResponseStep{toolCalls(call{"bash", map[string]any{"command": "echo started; sleep 30"}, "b"}), chattest.Done})

	store := filepath.Join(directory, "store")
	openStore := func() durable.Storage {
		storage, err := jsonl.OpenDir(bg, store, jsonl.Options{})
		must(t, err)
		return storage
	}
	h, root := c.Open(t, openStore(), e)
	submission, err := root.Submit(bg, chattest.Input("go"))
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

	h, root = c.Open(t, openStore(), e)
	reopened, err := h.Submission(bg, id)
	must(t, err)
	record, err := reopened.Wait(bg)
	must(t, err)
	if record.Status != durable.SubmissionDone {
		t.Fatalf("status = %s", record.Status)
	}
	results := chattest.ToolResults(chattest.Entries(t, root))
	if len(results) != 1 {
		t.Fatalf("%d tool results", len(results))
	}
	want := "started\n|<harness>\n[error] Tool bash was interrupted and may have partially run\n</harness>"
	if got := chattest.ResultText(chattest.ResultMessage(results[0])); got != want {
		t.Fatalf("text = %q", got)
	}
}

func TestStructuredOutputGivesCallersOfBashItsRetainedOutputAndExitCodeAlsoForANonzeroExit(t *testing.T) {
	c := chattest.New(t)
	c.Install(t, "tool:bash", NewBashTool(BashToolOptions{}))
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
	c.Install(t, "tool:probe", probe)
	c.Faux.SetResponses([]providers.FauxResponseStep{toolCalls(call{"probe", nil, "p"}), chattest.Done})
	h, root := c.Open(t, memory.New(), env.NewLocal(t.TempDir()))
	chattest.SubmitAndWait(t, root, "go")
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
