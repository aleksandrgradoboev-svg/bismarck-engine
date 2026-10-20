package harness

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Threshold compaction (pi harness-compaction.test.ts, "background threshold
// compaction" and "blocking threshold compaction").

func compactionPolicyOf(enabled bool, reserve, keepRecent, background int) func(*HarnessSettings) {
	return func(s *HarnessSettings) {
		s.Compaction.Enabled = ptr(enabled)
		s.Compaction.ReserveTokens = ptr(reserve)
		s.Compaction.KeepRecentTokens = ptr(keepRecent)
		s.Compaction.BackgroundTokens = ptr(background)
		s.Compaction.ModelOverrides = nil
	}
}

// backgroundPolicy: background threshold at 500 and blocking at 1500 tokens
// of a 2000-token window.
var backgroundPolicy = compactionPolicyOf(true, 500, 150, 1000)

// blockingPolicy: blocking threshold at 700 tokens of a 1000-token window,
// no background compaction.
var blockingPolicy = compactionPolicyOf(true, 300, 150, 0)

// compactionTasks are the live compaction tasks.
func (c *compactionChat) compactionTasks(t *testing.T) []durable.TaskRecord {
	t.Helper()
	inspection, err := c.h.Inspect(bg)
	must(t, err)
	var out []durable.TaskRecord
	for _, task := range inspection.Tasks {
		if task.Record.Kind == compactionKind {
			out = append(out, task.Record)
		}
	}
	return out
}

func compactionReason(t *testing.T, record durable.TaskRecord) CompactionReason {
	t.Helper()
	var input CompactionInput
	must(t, json.Unmarshal(record.Input, &input))
	return input.Reason
}

func (c *compactionChat) liveStatuses(t *testing.T) []CompactionStatus {
	t.Helper()
	parts, err := decodeLiveParts(c.live(t))
	must(t, err)
	return parts.Compactions
}

func (c *compactionChat) liveFailure(t *testing.T) *CompactionFailure {
	t.Helper()
	parts, err := decodeLiveParts(c.live(t))
	must(t, err)
	return parts.CompactionFailure
}

func TestCompactionBackgroundStartsAboveTheThresholdWithoutBlockingTheRun(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 2000})
	chat.history(t)
	chat.setup.setSettings(backgroundPolicy)
	step, reached, release := gatedStep(fauxAnswer("SUMMARY"))
	chat.faux.pushSummary(step)
	chat.turn(t, sizedText("u4", 100), sizedText("a4", 100))
	awaitSignal(t, reached, "the summary request")
	tasks := chat.compactionTasks(t)
	if len(tasks) != 1 || !tasks[0].Background || compactionReason(t, tasks[0]) != CompactionThreshold || tasks[0].Owner != nil {
		t.Fatalf("tasks = %+v", tasks)
	}
	task := tasks[0]
	if got, want := chat.liveStatuses(t), []CompactionStatus{{TaskID: task.ID, Reason: CompactionThreshold, Attempt: 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("compactions = %+v", got)
	}
	// Background work: neither conversation idle nor Esc waits for or stops
	// it.
	must(t, chat.root.WaitForIdle(bg))
	must(t, chat.root.Abort(bg, false))
	if status := getTask(t, chat.h, task.ID).State.Status; status != durable.TaskRunning {
		t.Fatalf("status = %s", status)
	}
	release()
	chat.result(t, task.ID)
	if kinds := chat.kinds(t); kinds[len(kinds)-1] != "pi.compaction" {
		t.Fatalf("kinds = %v", kinds)
	}
	if got := firstUserText(chat.context(t).Messages); !strings.Contains(got, "SUMMARY") {
		t.Fatalf("first user = %q", got)
	}
}

func TestCompactionBackgroundDoesNotStart(t *testing.T) {
	for _, c := range []struct {
		name   string
		policy func(*HarnessSettings)
	}{
		{"disabled", compactionPolicyOf(false, 500, 150, 1000)},
		{"backgroundTokens is 0", compactionPolicyOf(true, 500, 150, 0)},
		{"there is no cut", compactionPolicyOf(true, 500, 100_000, 1000)},
	} {
		t.Run(c.name, func(t *testing.T) {
			chat := openCompaction(t, compactionOpen{contextWindow: 2000})
			chat.history(t)
			chat.setup.setSettings(c.policy)
			chat.turn(t, sizedText("u4", 100), sizedText("a4", 100))
			if tasks := chat.compactionTasks(t); len(tasks) != 0 {
				t.Fatalf("tasks = %+v", tasks)
			}
			if n := chat.faux.summaryCount(); n != 0 {
				t.Fatalf("summary requests = %d", n)
			}
		})
	}
}

func TestCompactionBackgroundDoesNotStartWhileAnotherCompactionIsListed(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 2000})
	chat.history(t)
	never, reached := unanswered()
	chat.faux.pushSummary(never)
	manual := chat.compact(t, nil)
	awaitSignal(t, reached, "the summary request")
	chat.setup.setSettings(backgroundPolicy)
	chat.turn(t, sizedText("u4", 100), sizedText("a4", 100))
	if tasks := chat.compactionTasks(t); len(tasks) != 1 || tasks[0].ID != manual {
		t.Fatalf("tasks = %+v, want only %d", tasks, manual)
	}
	_, err := chat.h.AbortTask(bg, manual)
	must(t, err)
}

func TestCompactionBackgroundRecordsAFailureUntilASummaryIsProduced(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 2000})
	chat.history(t)
	chat.setup.setSettings(backgroundPolicy)
	_, log := listen(t, chat.root)
	chat.faux.pushSummary(fauxStep(fauxError("bad request")))
	chat.turn(t, sizedText("u4", 100), sizedText("a4", 100))
	endOf := func(id *durable.TaskID) *CompactionEndEvent {
		for _, event := range log.events() {
			if end, ok := event.(CompactionEndEvent); ok && (id == nil || end.TaskID == *id) {
				return &end
			}
		}
		return nil
	}
	eventually(t, func() bool { return endOf(nil) != nil })
	recorded := chat.liveFailure(t)
	if recorded == nil || recorded.Reason != CompactionThreshold || recorded.Error != "Summarization failed: bad request" {
		t.Fatalf("failure = %+v", recorded)
	}
	end := endOf(nil)
	if end.TaskID != recorded.TaskID || end.Reason != CompactionThreshold || end.Error == nil || *end.Error != "Summarization failed: bad request" {
		t.Fatalf("compaction_end = %+v", end)
	}
	late, err := chat.root.WatchEvents(bg)
	must(t, err)
	if !reflect.DeepEqual(late.Snapshot.CompactionFailure, recorded) {
		t.Fatalf("snapshot failure = %+v", late.Snapshot.CompactionFailure)
	}
	late.Stop()
	// A compaction that produces a summary removes the failure, and ends
	// without an error.
	chat.setup.setSettings(manualPolicy(150))
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	manual := chat.compact(t, nil)
	chat.result(t, manual)
	if failure := chat.liveFailure(t); failure != nil {
		t.Fatalf("failure = %+v", failure)
	}
	eventually(t, func() bool { return endOf(&manual) != nil })
	if end := endOf(&manual); end.Error != nil {
		t.Fatalf("compaction_end = %+v", end)
	}
}

func TestCompactionBackgroundStopsThroughAbortTaskAndConversationAbortWithBackground(t *testing.T) {
	for _, stop := range []string{"task", "conversation"} {
		t.Run(stop, func(t *testing.T) {
			chat := openCompaction(t, compactionOpen{contextWindow: 2000})
			chat.history(t)
			chat.setup.setSettings(backgroundPolicy)
			never, reached := unanswered()
			chat.faux.pushSummary(never)
			chat.turn(t, sizedText("u4", 100), sizedText("a4", 100))
			awaitSignal(t, reached, "the summary request")
			task := chat.compactionTasks(t)[0]
			if stop == "task" {
				_, err := chat.h.AbortTask(bg, task.ID)
				must(t, err)
			} else {
				must(t, chat.root.Abort(bg, true))
			}
			if outcome := chat.outcome(t, task.ID); outcome.Status != durable.OutcomeAborted {
				t.Fatalf("outcome = %+v", outcome)
			}
			if statuses := chat.liveStatuses(t); statuses != nil {
				t.Fatalf("compactions = %+v", statuses)
			}
		})
	}
}

func TestCompactionBlockingWaitsForItsCompactionWhichAppendsTheSummaryBeforeTheRequest(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 1000})
	chat.history(t)
	chat.setup.setSettings(blockingPolicy)
	// A new section: preparation has a system entry to append, but must not
	// append it before the wait.
	addSection(t, chat.setup.registry, constSection("extra", "EXTRA"))
	step, reached, release := gatedStep(fauxAnswer("SUMMARY"))
	chat.faux.pushSummary(step)
	chat.faux.pushAgent(fauxText("a4"))
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 200)))
	must(t, err)
	awaitSignal(t, reached, "the summary request")
	child := chat.compactionTasks(t)[0]
	generation := runTaskOf(t, chat.h, chat.root)
	if child.Owner == nil || *child.Owner != generation || child.Background || compactionReason(t, child) != CompactionThreshold {
		t.Fatalf("child = %+v", child)
	}
	state := getTask(t, chat.h, generation).State
	if state.Status != durable.TaskWaiting || !reflect.DeepEqual(state.On, []durable.TaskID{child.ID}) ||
		!strings.HasPrefix(string(state.Checkpoint), `{"phase":"prepare","attempt":1,"compacted":`+itoa(child.ID)) {
		t.Fatalf("generation state = %s %v %s", state.Status, state.On, state.Checkpoint)
	}
	if got, want := chat.liveStatuses(t), []CompactionStatus{{TaskID: child.ID, Reason: CompactionThreshold, Blocking: true, Attempt: 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("compactions = %+v", got)
	}
	// Nothing was appended before the wait.
	if kinds := chat.kinds(t); kinds[len(kinds)-1] != "pi.user" {
		t.Fatalf("kinds = %v", kinds)
	}
	release()
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	chat.result(t, child.ID)
	kinds := chat.kinds(t)
	if got := kinds[len(kinds)-3:]; !reflect.DeepEqual(got, []string{"pi.compaction", "pi.system", "pi.assistant"}) {
		t.Fatalf("kinds = %v", got)
	}
	request := chat.faux.lastAgentRequest(t).messages
	if !strings.Contains(firstUserText(request), "SUMMARY") {
		t.Fatalf("first user = %q", firstUserText(request))
	}
	var systems []ai.SystemMessage
	for _, message := range request {
		if system, ok := message.(ai.SystemMessage); ok {
			systems = append(systems, system)
		}
	}
	if len(systems) != 1 {
		t.Fatalf("system messages = %d", len(systems))
	}
	for name, want := range map[string]string{"preamble": "You are helpful.", "extra": "<extra>\nEXTRA\n</extra>"} {
		if got, ok := systems[0].Sections.Get(name); !ok || got == nil || *got != want {
			t.Fatalf("section %s = %v", name, got)
		}
	}
}

func TestCompactionBlockingSendsTheRequestOnceWithoutASecondCompactionWhenTheKeptPartIsStillAboveTheThreshold(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 1000})
	chat.history(t)
	chat.setup.setSettings(compactionPolicyOf(true, 300, 700, 0))
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.turn(t, sizedText("u4", 400), "a4")
	if n := chat.faux.summaryCount(); n != 1 {
		t.Fatalf("summary requests = %d", n)
	}
	count := 0
	for _, kind := range chat.kinds(t) {
		if kind == "pi.compaction" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("summaries = %d", count)
	}
}

func TestCompactionBlockingSendsTheRequestAnywayWhenItsCompactionDeclines(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 1000})
	chat.history(t)
	chat.setup.setSettings(blockingPolicy)
	addCompactionHooks(t, chat.setup.registry, "decline", decide(&CompactionDecision{Decline: true}))
	chat.turn(t, sizedText("u4", 200), "a4")
	if hasKind(chat.kinds(t), "pi.compaction") {
		t.Fatal("a declined compaction wrote a summary")
	}
	if got := firstUserText(chat.faux.lastAgentRequest(t).messages); got != sizedText("u1", 100) {
		t.Fatalf("first user = %q", got)
	}
	if failure := chat.liveFailure(t); failure != nil {
		t.Fatalf("failure = %+v", failure)
	}
}

func (c *compactionChat) agentRequestCount() int {
	c.faux.mu.Lock()
	defer c.faux.mu.Unlock()
	return len(c.faux.agentRequests)
}

// A failed compaction leaves a context it judged too large; sending it once
// filled a 1M window and got an empty length answer on every turn while the
// failure stayed invisible.
func TestCompactionBlockingFailsTheRunWithTheCompactionsErrorWithoutARequestWhenItsCompactionFails(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 1000})
	chat.history(t)
	chat.setup.setSettings(blockingPolicy)
	chat.faux.pushSummary(fauxStep(fauxError("bad request")))
	requests := chat.agentRequestCount()
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 200)))
	must(t, err)
	settled := submissionWait(t, input)
	if settled.Status != durable.SubmissionUnanswered || settled.Reason != "model_error" || string(settled.Detail) != `"Compaction failed: Summarization failed: bad request"` {
		t.Fatalf("input = %+v (%s)", settled, settled.Detail)
	}
	if n := chat.agentRequestCount(); n != requests {
		t.Fatalf("agent requests = %d, want %d", n, requests)
	}
	if kinds := chat.kinds(t); kinds[len(kinds)-1] != "pi.user" {
		t.Fatalf("kinds = %v", kinds)
	}
	if statuses := chat.liveStatuses(t); statuses != nil {
		t.Fatalf("compactions = %+v", statuses)
	}
	if failure := chat.liveFailure(t); failure == nil || failure.Reason != CompactionThreshold || failure.Error != "Summarization failed: bad request" {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestCompactionBlockingFailsTheRunWhenItsCompactionIsAbortedDirectly(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 1000})
	chat.history(t)
	chat.setup.setSettings(blockingPolicy)
	never, reached := unanswered()
	chat.faux.pushSummary(never)
	requests := chat.agentRequestCount()
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 200)))
	must(t, err)
	awaitSignal(t, reached, "the summary request")
	child := chat.compactionTasks(t)[0]
	_, err = chat.h.AbortTask(bg, child.ID)
	must(t, err)
	settled := submissionWait(t, input)
	if settled.Status != durable.SubmissionUnanswered || settled.Reason != "model_error" || string(settled.Detail) != `"Compaction was aborted"` {
		t.Fatalf("input = %+v (%s)", settled, settled.Detail)
	}
	if n := chat.agentRequestCount(); n != requests {
		t.Fatalf("agent requests = %d, want %d", n, requests)
	}
	if hasKind(chat.kinds(t), "pi.compaction") {
		t.Fatal("an aborted compaction wrote a summary")
	}
	// Aborting is not a failure.
	if failure := chat.liveFailure(t); failure != nil {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestCompactionBlockingIsAbortedWithItsGenerationByEscBeforeTheGenerationsAbortHandler(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 1000})
	chat.history(t)
	chat.setup.setSettings(blockingPolicy)
	never, reached := unanswered()
	chat.faux.pushSummary(never)
	_, log := listen(t, chat.root)
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 200)))
	must(t, err)
	awaitSignal(t, reached, "the summary request")
	child := chat.compactionTasks(t)[0]
	must(t, chat.root.Abort(bg, false))
	if settled := submissionWait(t, input); settled.Status != durable.SubmissionUnanswered || settled.Reason != "aborted" {
		t.Fatalf("input = %+v", settled)
	}
	if outcome := getTask(t, chat.h, child.ID).State.Outcome; outcome == nil || outcome.Status != durable.OutcomeAborted {
		t.Fatalf("child outcome = %+v", outcome)
	}
	eventually(t, func() bool { return strings.Contains(strings.Join(eventTypes(log.events()), " "), "run_end") })
	types := eventTypes(log.events())
	end, runEnd := -1, -1
	for i, kind := range types {
		if kind == "compaction_end" && end < 0 {
			end = i
		}
		if kind == "run_end" && runEnd < 0 {
			runEnd = i
		}
	}
	if end < 0 || end >= runEnd {
		t.Fatalf("events = %v", types)
	}
}

func TestCompactionBlockingWinsOverABackgroundCompactionStillInFlightWhichThenSettlesStale(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 2000})
	chat.history(t)
	chat.setup.setSettings(backgroundPolicy)
	step, reached, release := gatedStep(fauxAnswer("BACKGROUND"))
	chat.faux.pushSummary(step)
	chat.turn(t, sizedText("u4", 100), sizedText("a4", 100))
	awaitSignal(t, reached, "the background summary request")
	background := chat.compactionTasks(t)[0]
	chat.faux.pushSummary(summaryStep("BLOCKING"))
	chat.turn(t, sizedText("u5", 1000), "a5")
	if got := firstUserText(chat.context(t).Messages); !strings.Contains(got, "BLOCKING") {
		t.Fatalf("first user = %q", got)
	}
	release()
	record := submissionStatus(t, chat.summarySubmission(t, background.ID))
	if record.Status != durable.SubmissionUnanswered || record.Reason != "stale" {
		t.Fatalf("background summary = %+v", record)
	}
}
