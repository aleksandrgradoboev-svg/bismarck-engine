package codemode

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/harness"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/internal/chattest"
)

// Ported from pi packages/durable/test/codemode.test.ts, "durable code mode".
// pi reopens node SQLite; here a restart reopens JSONL.

func expectResult(t *testing.T, got scriptResult, text string, isError bool) {
	t.Helper()
	if got.text != text || got.isError != isError {
		t.Fatalf("result = %q (error %v), want %q (error %v)", got.text, got.isError, text, isError)
	}
}

func expectError(t *testing.T, got scriptResult, contains ...string) {
	t.Helper()
	if !got.isError {
		t.Fatalf("result %q is not an error", got.text)
	}
	for _, part := range contains {
		if !strings.Contains(got.text, part) {
			t.Fatalf("result %q does not contain %q", got.text, part)
		}
	}
}

func TestCodemodeRunsAScriptThatCallsToolsAsNestedCallsAndJournalsEachResultBeforeDeliveringIt(t *testing.T) {
	c := newTestChat(t, nil, nil)
	c.holds.block.Store(false)
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{
		script(`const a = await tools.echo({ text: "a" }); const b = await tools.echo({ text: "b" }); return [a, b];`),
		chattest.Done,
	})
	h, root := c.chat.Open(t, openJSONL(t, filepath.Join(t.TempDir(), "store")), nil)
	chattest.SubmitAndWait(t, root, "go")
	expectResult(t, resultOf(t, root), `["echo a","echo b"]`, false)
	if n := nestedCount(t, h); n != 2 {
		t.Fatalf("%d nested calls", n)
	}
}

func TestCodemodeResumesAScriptAfterARestartWithoutRunningItsFinishedCallsAgain(t *testing.T) {
	c := newTestChat(t, nil, nil)
	r := restart(t, c, `const a = await tools.echo({ text: "a" });
		const held = await tools.hold({ id: 1 });
		const b = await tools.echo({ text: "b" });
		return [a, held, b, Date.now() > 0];`, nil, nil)
	expectResult(t, r.result, `["echo a","held 1","echo b",true]`, false)
	if got := c.echoes(); !reflect.DeepEqual(got, map[string]int{"a": 1, "b": 1}) {
		t.Fatalf("echoes = %v", got)
	}
	// The VM ran twice; hold, replay-safe, reran once after the restart.
	if c.sandbox.starts.Load() != 2 || c.holds.runs.Load() != 2 {
		t.Fatalf("starts %d, hold runs %d", c.sandbox.starts.Load(), c.holds.runs.Load())
	}
}

func TestCodemodeReplaysResultsInTheOrderTheyArrivedNotTheOrderTheCallsWereMade(t *testing.T) {
	c := newTestChat(t, nil, nil)
	// 2 finishes first, then 1.
	c.slows.release(2)
	go func() {
		<-c.slows.finishedCall(2)
		c.slows.release(1)
	}()
	r := restart(t, c, `const order = [];
		await Promise.all([tools.slow({ id: 1 }).then(() => order.push(1)), tools.slow({ id: 2 }).then(() => order.push(2))]);
		await tools.echo({ text: order.join(",") });
		await tools.hold({});
		return order;`, nil, nil)
	expectResult(t, r.result, "[2,1]", false)
	if got := c.echoes(); !reflect.DeepEqual(got, map[string]int{"2,1": 1}) {
		t.Fatalf("echoes = %v", got)
	}
}

func TestCodemodeJournalsGlobalsWithTheirResultsAndHandsAReplayTheRecordedOnes(t *testing.T) {
	counter := 0
	globals := []Global{{Name: "next", Execute: func(_ ctxT, _ rawT, _ harness.ToolExecutionAPI) (any, error) {
		counter++
		return counter, nil
	}}}
	c := newTestChat(t, globals, nil)
	r := restart(t, c, `const n = await next(); await tools.hold({}); return [n, await next()];`, nil, nil)
	// The first next() ran once and was replayed from the journal; the
	// second ran after the restart.
	expectResult(t, r.result, "[1,2]", false)
	if counter != 2 {
		t.Fatalf("counter = %d", counter)
	}
}

func TestCodemodeKeepsTheScriptsClockAndRandomnessAcrossARestart(t *testing.T) {
	c := newTestChat(t, nil, nil)
	r := restart(t, c, `const before = [Date.now(), Math.random()];
		await tools.hold({});
		await tools.echo({ text: String(before[0]) + "/" + String(before[1]) });
		return "ok";`, nil, nil)
	if r.result.text != "ok" {
		t.Fatalf("result = %q", r.result.text)
	}
	// One echo: the same arguments, so the rerun admitted no second call.
	echoes := c.echoes()
	if len(echoes) != 1 {
		t.Fatalf("echoes = %v", echoes)
	}
	for _, n := range echoes {
		if n != 1 {
			t.Fatalf("echoes = %v", echoes)
		}
	}
}

func entryAt(draft *delta.Draft, i int) *delta.Draft { return draft.At("entries").At(i) }

func TestCodemodeEndsTheCallWhenAResumedScriptMadeADifferentNumberOfCallsThanBefore(t *testing.T) {
	c := newTestChat(t, nil, nil)
	r := restart(t, c, `await tools.echo({ text: "a" }); await tools.hold({});`, func(h *harness.Harness, task durable.TaskID) {
		edit(t, h, JournalDoc.Task(task), func(d *delta.Draft) error { return entryAt(d, 0).Set("issuedAfter", 7.0) })
	}, nil)
	expectError(t, r.result, "after result 1 it had made 2 calls, where it had made 7")
}

func TestCodemodeEndsTheCallAtOnceWhenAResumedScriptWaitsWithoutMakingACallItMadeBefore(t *testing.T) {
	c := newTestChat(t, nil, nil)
	r := restart(t, c, `await tools.echo({ text: "a" }); await tools.hold({});`, func(h *harness.Harness, task durable.TaskID) {
		edit(t, h, JournalDoc.Task(task), func(d *delta.Draft) error { return entryAt(d, 0).Set("id", 99.0) })
	}, nil)
	expectError(t, r.result, "it waits without making call 99")
	// hold, admitted before the restart, resumed on its own; no call was
	// admitted by the replay.
	if n := nestedCount(t, r.h); n != 2 {
		t.Fatalf("%d nested calls", n)
	}
}

func TestCodemodeEndsTheCallWhenAResumedScriptCallsAGlobalWithOtherArguments(t *testing.T) {
	c := newTestChat(t, []Global{{Name: "next", Execute: func(ctxT, rawT, harness.ToolExecutionAPI) (any, error) { return 1, nil }}}, nil)
	r := restart(t, c, `await next(); await tools.hold({});`, func(h *harness.Harness, task durable.TaskID) {
		edit(t, h, JournalDoc.Task(task), func(d *delta.Draft) error { return entryAt(d, 0).At("global").Set("args", "other") })
	}, nil)
	expectError(t, r.result, "call 1 is next")
}

func setStore(t *testing.T, h *harness.Harness, task durable.TaskID, store map[string]any) {
	t.Helper()
	edit(t, h, RunDoc.Task(task), func(d *delta.Draft) error { return d.At("start").Set("store", store) })
}

func TestCodemodeAdmitsNoCallOfADivergingReplayEvenOneTheScriptNeverMadeBefore(t *testing.T) {
	c := newTestChat(t, nil, nil)
	// With k set, the replay makes three extra calls after result 1, which
	// the live run did not make; the count in result 1's acknowledgment
	// shows the divergence only after they were issued.
	r := restart(t, c, `await tools.echo({ text: "a" });
		const extra = load("k") ?? 0;
		for (let i = 0; i < extra; i++) tools.echo({ text: "extra" + i });
		await tools.hold({});`, func(h *harness.Harness, task durable.TaskID) {
		setStore(t, h, task, map[string]any{"k": 3.0})
	}, func(h *harness.Harness, task durable.TaskID) {
		eventually(t, func() bool { return issuedAfter(t, h, task, 0) == 2 })
	})
	expectError(t, r.result, "after result 1 it had made 5 calls, where it had made 2")
	if got := c.echoes(); !reflect.DeepEqual(got, map[string]int{"a": 1}) {
		t.Fatalf("echoes = %v", got)
	}
	if n := nestedCount(t, r.h); n != 2 {
		t.Fatalf("%d nested calls", n)
	}
}

func TestCodemodeEndsTheCallWhenAResumedScriptEndsWhereItWentOnBefore(t *testing.T) {
	c := newTestChat(t, nil, nil)
	r := restart(t, c, `await tools.echo({ text: "a" }); if (load("k")) return "early"; await tools.hold({});`, func(h *harness.Harness, task durable.TaskID) {
		setStore(t, h, task, map[string]any{"k": true})
	}, func(h *harness.Harness, task durable.TaskID) {
		// The live run acknowledged result 1 before the restart: it went on
		// after it.
		eventually(t, func() bool { return issuedAfter(t, h, task, 0) == 2 })
	})
	expectError(t, r.result, "it ended after result 1, where it went on before")
}

func TestCodemodeEndsTheCallWhenAResumedScriptEndsBeforeCallsTheLiveRunMadeEvenWithoutARecordedCount(t *testing.T) {
	c := newTestChat(t, nil, nil)
	r := restart(t, c, `await tools.echo({ text: "a" }); if (load("k")) return "early"; await tools.echo({ text: "b" }); await tools.hold({});`, func(h *harness.Harness, task durable.TaskID) {
		setStore(t, h, task, map[string]any{"k": true})
		// As if the live run crashed before recording result 1's count.
		edit(t, h, JournalDoc.Task(task), func(d *delta.Draft) error {
			if entry := entryAt(d, 0); entry.Has("issuedAfter") {
				return entry.Delete("issuedAfter")
			}
			return nil
		})
	}, nil)
	expectError(t, r.result, "it ended after result 1, where it went on before")
}

func TestCodemodeEndsTheCallWhenAResumedScriptMakesACallAnEarlierAttemptAdmittedWithOtherArguments(t *testing.T) {
	c := newTestChat(t, nil, nil)
	r := restart(t, c, `await tools.echo({ text: load("k") ?? "a" }); await tools.hold({});`, func(h *harness.Harness, task durable.TaskID) {
		setStore(t, h, task, map[string]any{"k": "other"})
		// As if the restart came before the first journal commit: the rerun
		// runs live from the start.
		edit(t, h, JournalDoc.Task(task), func(d *delta.Draft) error { return d.Set("entries", []any{}) })
	}, nil)
	expectError(t, r.result, "different path", "call 1 is tools.echo")
	if got := c.echoes(); !reflect.DeepEqual(got, map[string]int{"a": 1}) {
		t.Fatalf("echoes = %v", got)
	}
}

func TestCodemodeRecordsACountTheLiveRunNeverRecordedAsSoonAsAReplayAcknowledgesIt(t *testing.T) {
	c := newTestChat(t, nil, nil)
	dir := filepath.Join(t.TempDir(), "store")
	c.chat.Faux.SetResponses([]providers.FauxResponseStep{script(`await tools.echo({ text: "a" }); await tools.hold({});`), chattest.Done})
	h, root := c.chat.Open(t, openJSONL(t, dir), nil)
	submission, err := root.Submit(bg, chattest.Input("go"))
	must(t, err)
	id := submission.ID
	c.holds.signal().wait(t, "hold")
	must(t, h.Close(bg))
	// As if result 1's count was never recorded; hold blocks again after
	// the restart.
	c.holds.reset()
	h, _ = c.chat.Open(t, openJSONL(t, dir), nil)
	task := scriptTask(t, h)
	edit(t, h, JournalDoc.Task(task), func(d *delta.Draft) error {
		if entry := entryAt(d, 0); entry.Has("issuedAfter") {
			return entry.Delete("issuedAfter")
		}
		return nil
	})
	must(t, h.Resume())
	// The replay acknowledges result 1 and goes live; the script waits on
	// hold, and the count is already recorded.
	c.holds.signal().wait(t, "hold after the restart")
	eventually(t, func() bool { return issuedAfter(t, h, task, 0) == 2 })
	must(t, h.Close(bg))
	c.holds.block.Store(false)
	h, _ = c.chat.Open(t, openJSONL(t, dir), nil)
	must(t, h.Resume())
	reopened, err := h.Submission(bg, id)
	must(t, err)
	record, err := reopened.Wait(bg)
	must(t, err)
	if record.Status != durable.SubmissionDone {
		t.Fatalf("status = %s", record.Status)
	}
}
