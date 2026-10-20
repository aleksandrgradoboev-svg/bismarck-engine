package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	sandbox "github.com/aleksandrgradoboev-svg/bismarck-engine/codemode"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/internal/chattest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// The rest of pi's "durable code mode" cases.

// controlledStorage holds commits while held and fails the next commit on
// request (pi's ControlledStorage, as far as these cases use it).
type controlledStorage struct {
	durable.Storage
	mu       sync.Mutex
	held     chan struct{}
	failNext error
}

func newControlledStorage() *controlledStorage { return &controlledStorage{Storage: memory.New()} }

// holdCommits holds every commit until the returned release is called.
func (s *controlledStorage) holdCommits() (release func()) {
	gate := make(chan struct{})
	s.mu.Lock()
	s.held = gate
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.held == gate {
				s.held = nil
			}
			s.mu.Unlock()
			close(gate)
		})
	}
}

func (s *controlledStorage) failNextCommit(err error) {
	s.mu.Lock()
	s.failNext = err
	s.mu.Unlock()
}

func (s *controlledStorage) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	s.mu.Lock()
	held, fail := s.held, s.failNext
	s.failNext = nil
	s.mu.Unlock()
	if held != nil {
		select {
		case <-held:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if fail != nil {
		return 0, fail
	}
	return s.Storage.Commit(ctx, writes)
}

func failsTool() *harness.ToolRegistration {
	return testTool("fails", harness.ReplayUnsafe, func(context.Context, map[string]any, harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
		return harness.ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "permission denied"}}, IsError: true}, nil
	})
}

func runOnce(t *testing.T, c *testChat, code string) (*harness.Harness, *harness.Conversation) {
	t.Helper()
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{script(code), chattest.Done})
	h, root := c.chat.Open(t, memory.New(), nil)
	chattest.SubmitAndWait(t, root, "go")
	return h, root
}

func detailsOf(t *testing.T, root *harness.Conversation) ToolDetails {
	t.Helper()
	message := chattest.ResultMessage(chattest.ToolResults(chattest.Entries(t, root))[0])
	raw, err := json.Marshal(message.Details)
	must(t, err)
	var details ToolDetails
	must(t, json.Unmarshal(raw, &details))
	return details
}

func TestCodemodeListsTheScriptsToolCallsInItsResultsDetails(t *testing.T) {
	c := newTestChat(t, nil, nil)
	c.chat.Install(t, "tool:fails", failsTool())
	_, root := runOnce(t, c, `await tools.echo({ text: "a" }); try { await tools.fails({}); } catch {} tools.hold({ id: 9 }); return 1;`)
	calls := detailsOf(t, root).Calls
	// A call still running, or not yet launched, when the script ended shows
	// as cancelled.
	if len(calls) != 3 {
		t.Fatalf("calls = %+v", calls)
	}
	want := []NestedCall{
		{ID: 1, Name: "echo", Args: `{"text":"a"}`, Status: "ok"},
		{ID: 2, Name: "fails", Args: "{}", Status: "error", Error: "permission denied"},
		{ID: 3, Name: "hold", Args: `{"id":9}`, Status: "cancelled"},
	}
	for i := range want {
		got := calls[i]
		got.DurationMs = nil
		if got != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, got, want[i])
		}
	}
	if calls[0].DurationMs == nil || *calls[0].DurationMs < 0 {
		t.Fatalf("duration = %v", calls[0].DurationMs)
	}
}

func TestCodemodeStoresNoOutcomeWhenTheHostClosesTheSandboxAndReplaysTheScriptAfterARestart(t *testing.T) {
	c := newTestChat(t, nil, nil)
	dir := filepath.Join(t.TempDir(), "store")
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{script(`await tools.echo({ text: "a" }); await tools.hold({}); return "done";`), chattest.Done})
	h, root := c.chat.Open(t, openJSONL(t, dir), nil)
	submission, err := root.Submit(bg, chattest.Input("go"))
	must(t, err)
	id := submission.ID
	c.holds.signal().wait(t, "hold")
	task := scriptTask(t, h)
	// A host shutting down closes the sandbox before the Harness.
	c.sandbox.Close()
	time.Sleep(100 * time.Millisecond)
	run, err := h.Snapshot(bg, RunDoc.Task(task))
	must(t, err)
	if _, ok := run["result"]; ok {
		t.Fatal("an outcome was stored")
	}
	must(t, h.Close(bg))
	// The next process has a new sandbox.
	c.holds.block.Store(false)
	fresh, err := sandbox.New(sandbox.Options{MemoryLimitBytes: 64 * 1024 * 1024})
	must(t, err)
	t.Cleanup(fresh.Close)
	c.chat.Install(t, "tool:codemode", NewTool(Options{Sandbox: fresh}))
	h, root = c.chat.Open(t, openJSONL(t, dir), nil)
	must(t, h.Resume())
	reopened, err := h.Submission(bg, id)
	must(t, err)
	record, err := reopened.Wait(bg)
	must(t, err)
	if record.Status != durable.SubmissionDone {
		t.Fatalf("status = %s", record.Status)
	}
	expectResult(t, resultOf(t, root), "done", false)
	if got := c.echoes(); !reflect.DeepEqual(got, map[string]int{"a": 1}) {
		t.Fatalf("echoes = %v", got)
	}
}

func TestCodemodeAbortsACallStillRunningWhenTheScriptEnds(t *testing.T) {
	c := newTestChat(t, nil, nil)
	// pi's script awaits echo, which its scheduler runs after hold has
	// started; Go runs nested tasks concurrently, so echo waits for hold to
	// run, and hold is still running when the script ends.
	c.chat.Install(t, "tool:afterHold", testTool("afterHold", harness.ReplayUnsafe, func(ctx context.Context, _ map[string]any, _ harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
		select {
		case <-c.holds.signal().ch:
		case <-ctx.Done():
			return harness.ToolExecutionResult{}, context.Cause(ctx)
		}
		return text("after"), nil
	}))
	h, root := runOnce(t, c, `tools.hold({}); await tools.afterHold({}); return 1;`)
	expectResult(t, resultOf(t, root), "1", false)
	var hold *durable.TaskRecord
	for _, task := range tasksOf(t, h) {
		var input harness.ToolTaskInput
		_ = json.Unmarshal(task.Input, &input)
		if input.Kind == "nested" && input.Call != nil && input.Call.Name == "hold" {
			hold = &task
		}
	}
	if hold == nil {
		t.Fatal("no hold task")
	}
	if c.holds.runs.Load() != 1 {
		t.Fatalf("hold ran %d times", c.holds.runs.Load())
	}
	eventually(t, func() bool {
		for _, task := range tasksOf(t, h) {
			if task.ID == hold.ID {
				return task.State.Status == durable.TaskTerminal && task.State.Outcome != nil && task.State.Outcome.Status == "aborted"
			}
		}
		return false
	})
}

func TestCodemodeAdmitsNoCallOnceTheScriptHasEndedEvenOneItMadeBeforeEnding(t *testing.T) {
	c := newTestChat(t, nil, nil)
	c.holds.block.Store(false)
	storage := newControlledStorage()
	// Hold the fingerprint commit, and so the admission of the script's
	// call, until the script has ended.
	c.sandbox.onStart = func(run *sandbox.Run) {
		release := storage.holdCommits()
		go func() {
			run.Wait()
			release()
		}()
	}
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{script(`tools.echo({ text: "late" }); return 1;`), chattest.Done})
	h, root := c.chat.Open(t, storage, nil)
	chattest.SubmitAndWait(t, root, "go")
	expectResult(t, resultOf(t, root), "1", false)
	if len(c.echoes()) != 0 || nestedCount(t, h) != 0 {
		t.Fatalf("echoes %v, nested %d", c.echoes(), nestedCount(t, h))
	}
}

func clearJournal(t *testing.T, h *harness.Harness, task durable.TaskID) {
	t.Helper()
	edit(t, h, JournalDoc.Task(task), func(d *delta.Draft) error { return d.Set("entries", []any{}) })
}

func TestCodemodeChecksARerunsCallsAgainstAnEarlierAttemptsBeforeAdmittingAnyLaterCall(t *testing.T) {
	c := newTestChat(t, nil, nil)
	// With k set, call 1 has other arguments and call 2 is another tool than
	// before; call 3 would be new.
	r := restart(t, c, `const k = load("k");
		tools.echo({ text: k ?? "a" });
		if (k) tools.echo({ text: "new" });
		await tools.hold({});`, func(h *harness.Harness, task durable.TaskID) {
		setStore(t, h, task, map[string]any{"k": "x"})
		// As if the restart came before the first journal commit.
		clearJournal(t, h, task)
	}, nil)
	// The first run already made more calls than before.
	expectError(t, r.result, "its first run made 3 calls, where it had made 2")
	if n := nestedCount(t, r.h); n != 2 {
		t.Fatalf("%d nested calls", n)
	}
	if got := c.echoes(); !reflect.DeepEqual(got, map[string]int{"a": 1}) {
		t.Fatalf("echoes = %v", got)
	}
}

func constantGlobal(name string, value any) Global {
	return Global{Name: name, Execute: func(ctxT, rawT, harness.ToolExecutionAPI) (any, error) { return value, nil }}
}

func TestCodemodeEndsTheCallWhenARerunMakesAGlobalCallWhereAnEarlierAttemptMadeAToolCall(t *testing.T) {
	c := newTestChat(t, []Global{constantGlobal("pick", 1)}, nil)
	r := restart(t, c, `if (load("k")) { await pick(); await tools.echo({ text: "new" }); } else { await tools.hold({}); }`, func(h *harness.Harness, task durable.TaskID) {
		setStore(t, h, task, map[string]any{"k": true})
	}, nil)
	expectError(t, r.result, "call 1 is pick(), but was tools.hold({})")
	if len(c.echoes()) != 0 {
		t.Fatalf("echoes = %v", c.echoes())
	}
}

func firstIssued(t *testing.T, h *harness.Harness, task durable.TaskID) int {
	t.Helper()
	value, ok := journalOf(t, h, task)["firstIssued"].(float64)
	if !ok {
		return -1
	}
	return int(value)
}

func TestCodemodeEndsTheCallWhenARerunsFirstRunMakesFewerCallsThanAnEarlierAttemptAdmitted(t *testing.T) {
	// Live, the first run admits hold 1 and hold 2. With k set, it makes only
	// hold 1, and after its result a global and a new tool call: the
	// global's ID is hold 2's key, which no check after the release would
	// see.
	c := newTestChat(t, []Global{constantGlobal("pick", 1)}, nil)
	r := restart(t, c, `const k = load("k");
		const first = tools.hold({ id: 1 });
		if (!k) tools.hold({ id: 2 });
		await first;
		await pick();
		await tools.echo({ text: "new" });`, func(h *harness.Harness, task durable.TaskID) {
		setStore(t, h, task, map[string]any{"k": true})
	}, func(h *harness.Harness, task durable.TaskID) {
		eventually(t, func() bool { return firstIssued(t, h, task) == 2 })
	})
	expectError(t, r.result, "its first run made 1 calls, where it had made 2")
	if len(c.echoes()) != 0 {
		t.Fatalf("echoes = %v", c.echoes())
	}
}

func TestCodemodeEndsTheCallWhenARerunWithoutJournalEntriesEndsInItsFirstRunWhereItWentOnBefore(t *testing.T) {
	c := newTestChat(t, nil, nil)
	r := restart(t, c, `if (load("k")) { store("x", 1); return "early"; } await tools.hold({});`, func(h *harness.Harness, task durable.TaskID) {
		setStore(t, h, task, map[string]any{"k": true})
	}, func(h *harness.Harness, task durable.TaskID) {
		eventually(t, func() bool { return firstIssued(t, h, task) == 1 })
	})
	expectError(t, r.result, "it ended after result 0, where it went on before")
}

func TestCodemodeChecksEveryCallARerunHoldsBeforeLaunchingAnyOfThem(t *testing.T) {
	// Live, call 1 is a global that never finishes and call 2 is hold; with
	// k set, call 1 is a new tool call and call 2 holds with other
	// arguments. Checking call 1 alone would launch it before call 2's
	// conflict shows.
	forever := Global{Name: "forever", Execute: func(ctx ctxT, _ rawT, _ harness.ToolExecutionAPI) (any, error) {
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}}
	c := newTestChat(t, []Global{forever}, nil)
	r := restart(t, c, `const k = load("k");
		if (k) tools.echo({ text: "new" }); else forever();
		await tools.hold({ id: k ? 2 : 1 });`, func(h *harness.Harness, task durable.TaskID) {
		setStore(t, h, task, map[string]any{"k": true})
	}, nil)
	expectError(t, r.result, `call 2 is tools.hold({"id":2}), but was tools.hold({"id":1})`)
	if len(c.echoes()) != 0 || nestedCount(t, r.h) != 1 {
		t.Fatalf("echoes %v, nested %d", c.echoes(), nestedCount(t, r.h))
	}
}

func TestCodemodeKeepsAGlobalsUndefinedResultUndefinedLiveAndReplayed(t *testing.T) {
	c := newTestChat(t, []Global{constantGlobal("nothing", nil)}, nil)
	r := restart(t, c, `const before = await nothing(); await tools.hold({}); return [before === undefined, (await nothing()) === undefined];`, nil, nil)
	expectResult(t, r.result, "[true,true]", false)
}

func TestCodemodeDropsAGlobalsResultThatArrivesAfterTheScriptFinished(t *testing.T) {
	late := make(chan struct{})
	finished := make(chan struct{})
	slowly := Global{Name: "slowly", Execute: func(ctxT, rawT, harness.ToolExecutionAPI) (any, error) {
		<-late
		defer close(finished)
		return "late", nil
	}}
	c := newTestChat(t, []Global{slowly, constantGlobal("quickly", "quick")}, nil)
	h, root := runOnce(t, c, `const pending = slowly(); await quickly(); return 1;`)
	expectResult(t, resultOf(t, root), "1", false)
	// The script's tool call has ended; its pending global resolves now and
	// has nowhere to go.
	close(late)
	<-finished
	time.Sleep(20 * time.Millisecond)
	must(t, h.Close(bg))
}

func TestCodemodeRejectsAFailedToolCallWithItsErrorTextAndStructuredOutput(t *testing.T) {
	c := newTestChat(t, nil, nil)
	c.chat.Install(t, "tool:fails", failsTool())
	c.holds.block.Store(false)
	_, root := runOnce(t, c, `try { await tools.fails({}); } catch (e) { return [e.message, e.structuredOutput]; }`)
	expectResult(t, resultOf(t, root), `["permission denied","permission denied"]`, false)
}

func exitSchema() *ai.Schema {
	return ai.Object(ai.Prop("output", ai.String("")), ai.Prop("exitCode", ai.Number("")))
}

func TestCodemodeResolvesASchemaToolsErrorResultThatCarriesItsValueAndRejectsOneWithoutWithItsText(t *testing.T) {
	c := newTestChat(t, nil, nil)
	run := &harness.ToolRegistration{
		Tool:                   ai.Tool{Name: "run", Description: "run", Parameters: ai.Object()},
		StructuredOutputSchema: exitSchema(),
		// Like bash on a nonzero exit: an error result with its value.
		Execute: func(context.Context, map[string]any, harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			return harness.ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "no matches"}},
				StructuredOutput: map[string]any{"output": "no matches", "exitCode": 1}, IsError: true}, nil
		},
	}
	denied := &harness.ToolRegistration{
		Tool:                   ai.Tool{Name: "denied", Description: "denied", Parameters: ai.Object()},
		StructuredOutputSchema: exitSchema(),
		Execute: func(context.Context, map[string]any, harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			return harness.ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "permission denied"}}, IsError: true}, nil
		},
	}
	c.chat.Install(t, "tool:run", run)
	c.chat.Install(t, "tool:denied", denied)
	_, root := runOnce(t, c, `const r = await tools.run({});
		let e; try { await tools.denied({}); } catch (error) { e = [error.message, error.structuredOutput ?? null]; }
		return [r.exitCode, r.output, e];`)
	expectResult(t, resultOf(t, root), `[1,"no matches",["permission denied",null]]`, false)
}

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func TestCodemodeResolvesASchemaLessToolWithItsTextAlsoWhenItReturnedImages(t *testing.T) {
	c := newTestChat(t, nil, nil)
	picture := &harness.ToolRegistration{
		Tool: ai.Tool{Name: "picture", Description: "picture", Parameters: ai.Object(ai.Prop("caption", ai.Boolean("")))},
		Execute: func(_ context.Context, args map[string]any, _ harness.ToolExecutionAPI) (harness.ToolExecutionResult, error) {
			output := ai.ContentList{}
			if caption, _ := args["caption"].(bool); caption {
				output = append(output, ai.TextContent{Text: "a cat"})
			}
			return harness.ToolExecutionResult{Output: append(output, ai.ImageContent{Data: onePixelPNG, MimeType: "image/png"})}, nil
		},
	}
	c.chat.Install(t, "tool:picture", picture)
	_, root := runOnce(t, c, `return [await tools.picture({ caption: true }), await tools.picture({ caption: false })];`)
	expectResult(t, resultOf(t, root), `["a cat",""]`, false)
}

func TestCodemodeDeliversAResultOnlyAfterItsJournalCommit(t *testing.T) {
	storage := newControlledStorage()
	var mu sync.Mutex
	var seen []string
	record := func(name string) {
		mu.Lock()
		seen = append(seen, name)
		mu.Unlock()
	}
	first := Global{Name: "first", Execute: func(ctxT, rawT, harness.ToolExecutionAPI) (any, error) {
		record("first")
		// The next commit is this result's journal commit: it never lands.
		storage.failNextCommit(errors.New("lost"))
		return 1, nil
	}}
	second := Global{Name: "second", Execute: func(ctxT, rawT, harness.ToolExecutionAPI) (any, error) {
		record("second")
		return 2, nil
	}}
	c := newTestChat(t, []Global{first, second}, nil)
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{script(`await first(); await second();`), chattest.Done})
	_, root := c.chat.Open(t, storage, nil)
	_, err := root.Submit(bg, chattest.Input("go"))
	must(t, err)
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) > 0
	})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(seen, []string{"first"}) {
		t.Fatalf("seen = %v", seen)
	}
}

func TestCodemodeRefusesToResumeWhenTheSandboxsFingerprintOrTheCodeModeVersionChanged(t *testing.T) {
	for _, field := range []string{"fingerprint", "version"} {
		t.Run(field, func(t *testing.T) {
			c := newTestChat(t, nil, nil)
			r := restart(t, c, `await tools.hold({}); await tools.echo({ text: "late" });`, func(h *harness.Harness, task durable.TaskID) {
				edit(t, h, RunDoc.Task(task), func(d *delta.Draft) error {
					if field == "fingerprint" {
						return d.At("start").Set("fingerprint", "another sandbox")
					}
					return d.At("start").Set("version", 0.0)
				})
			}, nil)
			expectError(t, r.result, "cannot resume")
			if len(c.echoes()) != 0 {
				t.Fatalf("echoes = %v", c.echoes())
			}
		})
	}
}

func TestCodemodeTimesAScriptOutAcrossARestartWithoutAdmittingNewCalls(t *testing.T) {
	c := newTestChat(t, nil, nil)
	// The Harness's clock moves past the deadline while the call is
	// interrupted; the live run has the whole minute to reach hold.
	var offset atomic.Int64
	base := time.Now()
	c.chat.Now = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	r := restart(t, c, "// @options: {\"timeout_ms\": 60000}\n\t\tawait tools.hold({}); await tools.echo({ text: \"late\" });", func(*harness.Harness, durable.TaskID) {
		offset.Add(int64(61 * time.Second))
	}, nil)
	expectError(t, r.result, "deadline passed")
	if len(c.echoes()) != 0 || c.sandbox.starts.Load() != 1 {
		t.Fatalf("echoes %v, starts %d", c.echoes(), c.sandbox.starts.Load())
	}
}

func TestCodemodeReturnsADecidedOutcomeAfterARestartWithoutRunningTheScriptAgain(t *testing.T) {
	c := newTestChat(t, nil, nil)
	c.holds.block.Store(false)
	dir := filepath.Join(t.TempDir(), "store")
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{script(`return "first"`), chattest.Done})
	// Stop after execute returned, so after the outcome is stored, and
	// before the result commits.
	var block atomic.Bool
	block.Store(true)
	reached := newSignal()
	must(t, c.chat.Registry.Install(&harness.Extension{Name: "hooks", Hooks: []harness.HookRegistration{harness.Hook(harness.ToolTask, &harness.ToolHooks{
		AfterTool: func(ctx context.Context, call harness.ToolHookCall, _ harness.ToolExecutionResult, _ *harness.TaskRuntime) (*harness.ToolExecutionResult, error) {
			if call.Name != ToolName || !block.Load() {
				return nil, nil
			}
			reached.fire()
			<-ctx.Done()
			return nil, context.Cause(ctx)
		},
	})}}))
	h, root := c.chat.Open(t, openJSONL(t, dir), nil)
	submission, err := root.Submit(bg, chattest.Input("go"))
	must(t, err)
	id := submission.ID
	reached.wait(t, "afterTool")
	run, err := h.Snapshot(bg, RunDoc.Task(scriptTask(t, h)))
	must(t, err)
	if _, ok := run["result"]; !ok {
		t.Fatal("no stored outcome")
	}
	must(t, h.Close(bg))
	block.Store(false)
	h, root = c.chat.Open(t, openJSONL(t, dir), nil)
	must(t, h.Resume())
	reopened, err := h.Submission(bg, id)
	must(t, err)
	record, err := reopened.Wait(bg)
	must(t, err)
	if record.Status != durable.SubmissionDone {
		t.Fatalf("status = %s", record.Status)
	}
	expectResult(t, resultOf(t, root), "first", false)
	if c.sandbox.starts.Load() != 1 {
		t.Fatalf("starts = %d", c.sandbox.starts.Load())
	}
}

// resultTexts are the second content item of each code mode result: the
// script's output after the header.
func resultTexts(t *testing.T, conversation *harness.Conversation) []string {
	t.Helper()
	var texts []string
	for _, entry := range chattest.ToolResults(chattest.Entries(t, conversation)) {
		content := chattest.ResultMessage(entry).Content
		if len(content) > 1 {
			texts = append(texts, content[1].(ai.TextContent).Text)
		}
	}
	return texts
}

func TestCodemodeKeepsStoreValuesPerConversationAsOfAForksEntry(t *testing.T) {
	c := newTestChat(t, nil, nil)
	c.holds.block.Store(false)
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{
		script(`store("k", 1); return load("k")`), chattest.Done,
		script(`return [load("k") ?? null]`), chattest.Done,
	})
	_, root := c.chat.Open(t, memory.New(), nil)
	chattest.SubmitAndWait(t, root, "first")
	var firstUser durable.EntryID
	for _, entry := range chattest.Entries(t, root) {
		if entry.Kind == "pi.user" {
			firstUser = entry.ID
			break
		}
	}
	chattest.SubmitAndWait(t, root, "second")
	if got := resultTexts(t, root); !reflect.DeepEqual(got, []string{"1", "[1]"}) {
		t.Fatalf("results = %q", got)
	}
	// A fork at the first input is from before the first script stored
	// anything.
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{script(`return [load("k") ?? null]`), chattest.Done})
	fork, err := root.Fork(bg, firstUser, harness.ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	chattest.SubmitAndWait(t, fork, "fork")
	texts := resultTexts(t, fork)
	if len(texts) == 0 || texts[len(texts)-1] != "[null]" {
		t.Fatalf("fork results = %q", texts)
	}
}

// In pi the 50 results finish in one tick; Go has no tick, so "together" is
// what the pump batches: results that arrive while a journal commit is in
// flight go into the next one. The first result's commit is held until all
// 50 globals have returned.
func TestCodemodeJournalsResultsThatFinishTogetherInOneCommit(t *testing.T) {
	release := make(chan struct{})
	var waiting, returned atomic.Int32
	wait := Global{Name: "wait", Execute: func(ctxT, rawT, harness.ToolExecutionAPI) (any, error) {
		defer returned.Add(1)
		if waiting.Add(1) == 50 {
			close(release)
		}
		<-release
		return nil, nil
	}}
	c := newTestChat(t, []Global{wait}, nil)
	c.holds.block.Store(false)
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{script(`return (await Promise.all(Array.from({ length: 50 }, () => wait()))).length`), chattest.Done})
	storage := newHoldingStorage(`"global":{"name":"wait"`)
	h, root := c.chat.Open(t, storage, nil)
	t.Cleanup(storage.open)
	go func() {
		// The first result's commit, or the cleanup's release.
		select {
		case <-storage.reached.ch:
		case <-storage.release:
			return
		}
		for returned.Load() < 50 {
			time.Sleep(time.Millisecond)
		}
		// Each returned global queues its result right after.
		time.Sleep(100 * time.Millisecond)
		storage.open()
	}()
	var journalCommits atomic.Int32
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			if document, ok := change.(durable.DocumentChange); ok && document.Record.Kind == JournalDoc.Definition().Kind {
				journalCommits.Add(1)
			}
		}
	})
	must(t, err)
	defer unsubscribe()
	chattest.SubmitAndWait(t, root, "go")
	expectResult(t, resultOf(t, root), "50", false)
	// The journal's creation and a few groups, not one commit per result
	// (51).
	if n := journalCommits.Load(); n >= 10 {
		t.Fatalf("%d journal commits", n)
	}
}
