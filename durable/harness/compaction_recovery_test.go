package harness

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Compaction events, live status, and recovery (pi harness-compaction.test.ts,
// "compaction events and live status" and the manual cases of "compaction
// recovery"; pi reopens a SQLite file, here a JSONL directory, as in the
// generation recovery tests).

func TestCompactionReportsStartAndEndAndTheRetryBackoffInALateJoinersSnapshot(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	chat.setup.setSettings(retrySettings(true, 2, time.Minute))
	_, log := listen(t, chat.root)
	chat.faux.pushSummary(fauxStep(fauxError("overloaded")))
	id := chat.compact(t, nil)
	eventually(t, func() bool {
		statuses, _ := chat.live(t)["compactions"].([]any)
		if len(statuses) == 0 {
			return false
		}
		_, retrying := statuses[0].(map[string]any)["retry"]
		return retrying
	})
	late, err := chat.root.WatchEvents(bg)
	must(t, err)
	statuses := late.Snapshot.Compactions
	if len(statuses) != 1 || statuses[0].TaskID != id || statuses[0].Reason != CompactionManual || statuses[0].Blocking || statuses[0].Attempt != 1 ||
		statuses[0].Retry == nil || statuses[0].Retry.Error != "overloaded" || statuses[0].Retry.At == 0 {
		t.Fatalf("late snapshot compactions = %+v", statuses)
	}
	late.Stop()
	_, err = chat.h.AbortTask(bg, id)
	must(t, err)
	eventually(t, func() bool {
		return strings.Contains(strings.Join(eventTypes(log.events()), " "), "compaction_end")
	})
	var compactions []AgentEvent
	for _, event := range log.events() {
		if strings.HasPrefix(event.EventType(), "compaction_") {
			compactions = append(compactions, event)
		}
	}
	want := []AgentEvent{
		CompactionStartEvent{TaskID: id, Reason: CompactionManual},
		CompactionEndEvent{TaskID: id, Reason: CompactionManual},
	}
	if !reflect.DeepEqual(compactions, want) {
		t.Fatalf("compaction events = %+v", compactions)
	}
}

func TestCompactionListsConcurrentCompactionsInTaskIDOrder(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	first, firstReached := unanswered()
	second, secondReached := unanswered()
	chat.faux.pushSummary(first, second)
	a := chat.compact(t, nil)
	b := chat.compact(t, nil)
	awaitSignal(t, firstReached, "the first summary request")
	awaitSignal(t, secondReached, "the second summary request")
	statuses, _ := chat.live(t)["compactions"].([]any)
	var ids []durable.TaskID
	for _, status := range statuses {
		id, _ := status.(map[string]any)["taskId"].(float64)
		ids = append(ids, durable.TaskID(id))
	}
	if !reflect.DeepEqual(ids, []durable.TaskID{a, b}) {
		t.Fatalf("compactions = %v, want [%d %d]", ids, a, b)
	}
	must(t, chat.root.Abort(bg, false))
	if _, ok := chat.live(t)["compactions"]; ok {
		t.Fatal("compactions are still listed")
	}
}

// firstCompaction opens a JSONL Harness with three turns of history.
func firstCompaction(t *testing.T, dir string) *compactionChat {
	t.Helper()
	setup := newChatSetup(t)
	faux := scriptCompaction(setup)
	chat := openCompactionWith(t, compactionOpen{storage: openJSONL(t, dir)}, setup, faux)
	chat.history(t)
	return chat
}

// reopenCompaction closes chat's Harness and opens its directory again.
func reopenCompaction(t *testing.T, dir string, chat *compactionChat) *compactionChat {
	t.Helper()
	must(t, chat.h.Close(bg))
	h, root := openChat(t, openJSONL(t, dir), chat.setup)
	must(t, h.Resume())
	return &compactionChat{h: h, root: root, setup: chat.setup, faux: chat.faux}
}

func TestCompactionRecoveryRepeatsNothingAfterReopenOnceTheSummaryIsPlaced(t *testing.T) {
	dir := t.TempDir()
	chat := firstCompaction(t, dir)
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, chat.compact(t, nil))
	entries := chatEntries(t, chat.root)
	usage, err := chat.h.Snapshot(bg, UsageDoc.Conversation(chat.root.ID))
	must(t, err)
	chat = reopenCompaction(t, dir, chat)
	must(t, chat.h.WaitForIdle(bg))
	if n := chat.faux.summaryCount(); n != 1 {
		t.Fatalf("summary requests = %d", n)
	}
	if got := chatEntries(t, chat.root); !reflect.DeepEqual(got, entries) {
		t.Fatal("entries changed after reopen")
	}
	if got, err := chat.h.Snapshot(bg, UsageDoc.Conversation(chat.root.ID)); err != nil || !reflect.DeepEqual(got, usage) {
		t.Fatalf("usage = %v, want %v (%v)", got, usage, err)
	}
	inspection, err := chat.h.Inspect(bg)
	must(t, err)
	if len(inspection.Tasks) != 0 || len(inspection.Submissions) != 0 {
		t.Fatalf("inspection = %d tasks, %d submissions", len(inspection.Tasks), len(inspection.Submissions))
	}
}

func TestCompactionRecoveryRerunsSelectAndItsHookAfterACrashInSelect(t *testing.T) {
	dir := t.TempDir()
	chat := firstCompaction(t, dir)
	calls := 0
	reached := make(chan struct{})
	addCompactionHooks(t, chat.setup.registry, "crash", &CompactionHooks{BeforeCompact: func(ctx context.Context, _ CompactionRequest, _ *TaskRuntime) (*CompactionDecision, error) {
		calls++
		if calls == 1 {
			close(reached)
			<-ctx.Done()
			return nil, context.Cause(ctx)
		}
		return nil, nil
	}})
	id := chat.compact(t, nil)
	awaitSignal(t, reached, "the first hook call")
	chat = reopenCompaction(t, dir, chat)
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, id)
	if calls != 2 {
		t.Fatalf("hook calls = %d, want 2", calls)
	}
}

// withoutTimestamps clears the timestamps of system and user messages.
func withoutTimestamps(messages []ai.Message) []ai.Message {
	out := make([]ai.Message, len(messages))
	for i, message := range messages {
		switch m := message.(type) {
		case ai.SystemMessage:
			m.Timestamp = 0
			out[i] = m
		case ai.UserMessage:
			m.Timestamp = 0
			out[i] = m
		default:
			out[i] = message
		}
	}
	return out
}

func TestCompactionRecoveryResendsAnInterruptedSummarizationOnceAndCountsOnlyTheAnsweredAttempt(t *testing.T) {
	dir := t.TempDir()
	chat := firstCompaction(t, dir)
	never, reached := unanswered()
	chat.faux.pushSummary(never)
	id := chat.compact(t, nil)
	awaitSignal(t, reached, "the summary request")
	usageBefore := chat.modelUsage(t)
	if phase, _ := CheckpointPhase(getTask(t, chat.h, id).State.Checkpoint); phase != phaseSummarize {
		t.Fatalf("phase = %s, want summarize", phase)
	}
	chat = reopenCompaction(t, dir, chat)
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, id)
	if n := chat.faux.summaryCount(); n != 2 {
		t.Fatalf("summary requests = %d", n)
	}
	sent, resent := chat.faux.summaryRequest(t, 0), chat.faux.summaryRequest(t, 1)
	if !reflect.DeepEqual(withoutTimestamps(resent.messages), withoutTimestamps(sent.messages)) {
		t.Fatal("the resent request differs from the interrupted one")
	}
	usage := chat.modelUsage(t)
	if usage.Input <= usageBefore.Input || usage.Output-usageBefore.Output != 2 {
		t.Fatalf("usage = %+v, before %+v", usage, usageBefore)
	}
}

func TestCompactionRecoveryResumesARetryBackoffAfterReopen(t *testing.T) {
	dir := t.TempDir()
	chat := firstCompaction(t, dir)
	now := newMsClock(chat.setup, 1_000)
	chat.setup.setSettings(retrySettings(true, 2, time.Minute))
	chat.faux.pushSummary(fauxStep(fauxError("overloaded")))
	id := chat.compact(t, nil)
	eventually(t, func() bool { return getTask(t, chat.h, id).State.Status == durable.TaskSleeping })
	state := getTask(t, chat.h, id).State
	if state.Until == nil || *state.Until != 61_000 || !strings.HasPrefix(string(state.Checkpoint), `{"phase":"retry","attempt":1,`) || !strings.HasSuffix(string(state.Checkpoint), `,"until":61000}`) {
		t.Fatalf("state = %s %v %s", state.Status, state.Until, state.Checkpoint)
	}
	must(t, chat.h.Close(bg))
	// Earlier versions slept inside a running retry phase: reopened before
	// its time, it sleeps again.
	storage := openJSONL(t, dir)
	record, err := storage.Task(bg, id)
	must(t, err)
	record.State.Status = durable.TaskRunning
	record.State.Until = nil
	_, err = storage.Commit(bg, []durable.StorageWrite{durable.TaskWrite{Value: *record}})
	must(t, err)
	must(t, storage.Close(bg))
	h, root := openChat(t, openJSONL(t, dir), chat.setup)
	must(t, h.Resume())
	chat = &compactionChat{h: h, root: root, setup: chat.setup, faux: chat.faux}
	eventually(t, func() bool { return getTask(t, chat.h, id).State.Status == durable.TaskSleeping })
	if n := chat.faux.summaryCount(); n != 1 {
		t.Fatalf("summary requests = %d", n)
	}
	now.set(121_000)
	chat = reopenCompaction(t, dir, chat)
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, id)
	if n := chat.faux.summaryCount(); n != 2 {
		t.Fatalf("summary requests = %d", n)
	}
}

func TestCompactionRecoveryKeepsAQueuedSummaryAcrossReopenAndPlacesItAtTheNextBoundary(t *testing.T) {
	dir := t.TempDir()
	chat := firstCompaction(t, dir)
	never, reached := unanswered()
	chat.faux.pushAgent(never)
	input, err := chat.root.Submit(bg, inputDraft("busy"))
	must(t, err)
	awaitSignal(t, reached, "the busy request")
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	result := chat.result(t, chat.compact(t, nil))
	chat = reopenCompaction(t, dir, chat)
	chat.faux.pushAgent(fauxText("answered"))
	if status := waitSubmission(t, chat.h, input.ID).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	if result.SubmissionID == nil {
		t.Fatal("no summary submission")
	}
	if status := waitSubmission(t, chat.h, *result.SubmissionID).Status; status != durable.SubmissionDone {
		t.Fatalf("summary = %s", status)
	}
}
