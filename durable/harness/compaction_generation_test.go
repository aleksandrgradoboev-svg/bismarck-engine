package harness

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Compaction with generation (pi harness-compaction.test.ts: the threshold
// and overflow cases of "compaction estimates and interactions", "compaction
// recovery", "compaction and the inbox", "compaction edge cases", "blocking
// and manual compaction together", "compaction pinning…" and "per-model
// compaction overrides"; and the two compaction cases of
// harness-telemetry.test.ts).

func TestCompactionIgnoresUsageMeasuredBeforeASummaryPlacedMidRun(t *testing.T) {
	for _, clock := range []string{"real", "fixed"} {
		t.Run(clock+" clock", func(t *testing.T) {
			setup := newCompactionSetup(t, 2000)
			if clock == "fixed" {
				newMsClock(setup, 1_000)
			}
			faux := scriptCompaction(setup)
			toolReached := make(chan struct{})
			toolGate := make(chan struct{})
			slow := testTool("slow")
			slow.Execute = func(ctx context.Context, _ map[string]any, _ ToolExecutionAPI) (ToolExecutionResult, error) {
				close(toolReached)
				select {
				case <-toolGate:
				case <-ctx.Done():
					return ToolExecutionResult{}, context.Cause(ctx)
				}
				return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: sizedText("result", 200)}}}, nil
			}
			addTool(t, setup.registry, slow)
			chat := openCompactionWith(t, compactionOpen{contextWindow: 2000}, setup, faux)
			must(t, chat.root.Configure(bg, AgentChange{Tools: Set(Exactly(slow))}))
			chat.turn(t, sizedText("u1", 220), sizedText("a1", 220))
			chat.turn(t, sizedText("u2", 220), sizedText("a2", 220))
			chat.turn(t, sizedText("u3", 220), sizedText("a3", 220))
			// Background at 900, blocking at 1500: the tool call's usage plus
			// its result would cross 1500.
			chat.setup.setSettings(compactionPolicyOf(true, 500, 150, 600))
			// The request starts a background compaction; its tool call's usage
			// measures the whole context.
			chat.faux.pushAgent(fauxStep(providers.FauxAssistantMessage(ai.ContentList{providers.FauxToolCall("slow", map[string]any{})}, ai.StopToolUse)), fauxText("done"))
			summary, summaryReached, releaseSummary := gatedStep(fauxAnswer("SUMMARY"))
			chat.faux.pushSummary(summary)
			input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 50)))
			must(t, err)
			awaitSignal(t, toolReached, "the tool")
			awaitSignal(t, summaryReached, "the summary request")
			background := chat.compactionTasks(t)[0]
			releaseSummary()
			// Queued: the run is busy in its tool round.
			submission := chat.summarySubmission(t, background.ID)
			if status := submissionStatus(t, submission).Status; status != durable.SubmissionQueued {
				t.Fatalf("summary = %s, want queued", status)
			}
			close(toolGate)
			if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
				t.Fatalf("input = %s", status)
			}
			// The summary landed at postTools; the successor saw a small
			// context and did not compact again.
			if n := chat.faux.summaryCount(); n != 1 {
				t.Fatalf("summary requests = %d", n)
			}
			if tasks := chat.compactionTasks(t); len(tasks) != 0 {
				t.Fatalf("tasks = %+v", tasks)
			}
			if got := firstUserText(chat.faux.lastAgentRequest(t).messages); !strings.Contains(got, "SUMMARY") {
				t.Fatalf("first user = %q", got)
			}
		})
	}
}

func TestCompactionRecoveryFailsAnOverflowRunWithItsTextWhenItsCompactionFailsAfterReopen(t *testing.T) {
	dir := t.TempDir()
	chat := firstCompaction(t, dir)
	chat.setup.setSettings(overflowEnabled)
	never, reached := unanswered()
	chat.faux.pushSummary(never)
	chat.faux.pushAgent(fauxStep(fauxError(overflowText)))
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
	must(t, err)
	awaitSignal(t, reached, "the summary request")
	generation := runTaskOf(t, chat.h, chat.root)
	state := getTask(t, chat.h, generation).State
	var checkpoint GenerationCheckpoint
	must(t, json.Unmarshal(state.Checkpoint, &checkpoint))
	if state.Status != durable.TaskWaiting || checkpoint.Phase != phasePrepare || checkpoint.Overflow != overflowText {
		t.Fatalf("generation = %s %s", state.Status, state.Checkpoint)
	}
	chat = reopenCompaction(t, dir, chat)
	chat.faux.pushSummary(fauxStep(fauxError("bad request")))
	expectModelError(t, waitSubmission(t, chat.h, input.ID), overflowText)
}

func TestCompactionRecoveryKeepsAGenerationWaitingOnItsBlockingCompactionAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	chat := firstCompaction(t, dir)
	never, reached := unanswered()
	chat.faux.pushSummary(never)
	// The faux model window is 128k; this blocking threshold needs a small
	// context window.
	chat.setup.setSettings(compactionPolicyOf(true, 128_000-700, 150, 0))
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 200)))
	must(t, err)
	awaitSignal(t, reached, "the summary request")
	chat = reopenCompaction(t, dir, chat)
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.faux.pushAgent(fauxText("a4"))
	if status := waitSubmission(t, chat.h, input.ID).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	kinds := chat.kinds(t)
	if got := kinds[len(kinds)-3:]; !reflect.DeepEqual(got, []string{"pi.compaction", "pi.system", "pi.assistant"}) {
		t.Fatalf("kinds = %v", got)
	}
}

func TestCompactionKeepsTheFullRetryBudgetAfterAnOverflowCompaction(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	chat.setup.setSettings(overflowEnabled)
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.faux.pushAgent(fauxStep(fauxError("prompt is too long")), fauxStep(fauxError("overloaded")), fauxStep(fauxError("overloaded")), fauxText("finally"))
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
	must(t, err)
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
}

func TestCompactionKeepsTheOneCompactionLimitThroughARetryBackoff(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 1000})
	chat.history(t)
	chat.setup.setSettings(blockingPolicy)
	var asked atomic.Int32
	addCompactionHooks(t, chat.setup.registry, "decline", &CompactionHooks{BeforeCompact: func(context.Context, CompactionRequest, *TaskRuntime) (*CompactionDecision, error) {
		asked.Add(1)
		return &CompactionDecision{Decline: true}, nil
	}})
	chat.faux.pushAgent(fauxStep(fauxError("overloaded")))
	chat.turn(t, sizedText("u4", 200), "a4")
	// One blocking compaction, declined; the retried preparation, still above
	// the threshold, started no other.
	if n := asked.Load(); n != 1 {
		t.Fatalf("hook calls = %d", n)
	}
	if n := chat.agentRequestCount(); n != 5 {
		t.Fatalf("agent requests = %d, want 5", n)
	}
}

func TestCompactionDoesNotStartABackgroundCompactionWhenAManualOneWasAdmittedDuringPreparation(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 2000})
	chat.history(t)
	chat.setup.setSettings(backgroundPolicy)
	rendering := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	addSection(t, chat.setup.registry, Section("slow", func(ctx context.Context, _ PromptInput) (string, bool, error) {
		held := false
		once.Do(func() { held = true })
		if held {
			close(rendering)
			<-release
		}
		return "slow", true, nil
	}))
	never, reached := unanswered()
	chat.faux.pushSummary(never)
	chat.faux.pushAgent(fauxText("a4"))
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
	must(t, err)
	awaitSignal(t, rendering, "the slow section")
	manual := chat.compact(t, nil)
	awaitSignal(t, reached, "the manual summary request")
	close(release)
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	if tasks := chat.compactionTasks(t); len(tasks) != 1 || tasks[0].ID != manual {
		t.Fatalf("tasks = %+v", tasks)
	}
	// A background compaction would have sent its own summarization request.
	if n := chat.faux.summaryCount(); n != 1 {
		t.Fatalf("summary requests = %d", n)
	}
	_, err = chat.h.AbortTask(bg, manual)
	must(t, err)
}

func TestCompactionStartsNoBackgroundCompactionAfterABlockingOneInTheSameGeneration(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 2000})
	chat.history(t)
	// Blocking at 1500, background at 200: the kept part stays above the
	// background threshold.
	chat.setup.setSettings(compactionPolicyOf(true, 500, 400, 1300))
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.turn(t, sizedText("u4", 1000), "a4")
	if n := chat.faux.summaryCount(); n != 1 {
		t.Fatalf("summary requests = %d", n)
	}
	if tasks := chat.compactionTasks(t); len(tasks) != 0 {
		t.Fatalf("tasks = %+v", tasks)
	}
}

// failingModels is a model access whose CompleteSimple panics.
type failingModels struct{ ai.Models }

func (failingModels) CompleteSimple(context.Context, *ai.Model, ai.Context, *ai.ModelsSimpleStreamOptions) *ai.AssistantMessage {
	panic(errors.New("no credentials"))
}

func TestCompactionFailsTheRunWhenItsBlockingCompactionFaultsAndRecordsTheFault(t *testing.T) {
	setup := newCompactionSetup(t, 1000)
	faux := scriptCompaction(setup)
	var broken atomic.Bool
	setup.modelsFunc = func() ai.Models {
		if broken.Load() {
			return failingModels{setup.models}
		}
		return setup.models
	}
	chat := openCompactionWith(t, compactionOpen{contextWindow: 1000}, setup, faux)
	chat.history(t)
	chat.setup.setSettings(blockingPolicy)
	broken.Store(true)
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 200)))
	must(t, err)
	expectModelError(t, submissionWait(t, input), "Compaction failed: no credentials")
	broken.Store(false)
	if hasKind(chat.kinds(t), "pi.compaction") {
		t.Fatal("a faulted compaction wrote a summary")
	}
	if statuses := chat.liveStatuses(t); statuses != nil {
		t.Fatalf("compactions = %+v", statuses)
	}
	if failure := chat.liveFailure(t); failure == nil || failure.Reason != CompactionThreshold || failure.Error != "no credentials" {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestCompactionFailsAnOverflowRunWithItsTextWhenItsCompactionIsAbortedDirectlyOrItselfOverflows(t *testing.T) {
	for _, how := range []string{"abort", "overflow"} {
		t.Run(how, func(t *testing.T) {
			chat := openCompaction(t, compactionOpen{})
			chat.history(t)
			chat.setup.setSettings(overflowEnabled)
			reached := make(<-chan struct{})
			if how == "abort" {
				var step providers.FauxResponseStep
				step, reached = unanswered()
				chat.faux.pushSummary(step)
			} else {
				chat.faux.pushSummary(fauxStep(fauxError("prompt is too long for the summary")))
			}
			chat.faux.pushAgent(fauxStep(fauxError(overflowText)))
			input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
			must(t, err)
			if how == "abort" {
				awaitSignal(t, reached, "the summary request")
				_, err := chat.h.AbortTask(bg, chat.compactionTasks(t)[0].ID)
				must(t, err)
			}
			expectModelError(t, submissionWait(t, input), overflowText)
			if statuses := chat.liveStatuses(t); statuses != nil {
				t.Fatalf("compactions = %+v", statuses)
			}
		})
	}
}

func TestCompactionTreatsALengthStopAsAnOrdinaryAnswerNotAnOverflow(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	chat.setup.setSettings(overflowEnabled)
	cut := providers.FauxAssistantMessage(ai.ContentList{providers.FauxText("cut short")}, ai.StopLength)
	cut.ErrorMessage = overflowText
	chat.faux.pushAgent(fauxStep(cut))
	input, err := chat.root.Submit(bg, inputDraft("go"))
	must(t, err)
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	if n := chat.faux.summaryCount(); n != 0 {
		t.Fatalf("summary requests = %d", n)
	}
}

func TestCompactionPutsCompactionStartLastInTheBatchOfThePreparationCommit(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 2000})
	chat.history(t)
	chat.setup.setSettings(backgroundPolicy)
	_, log := listen(t, chat.root)
	// A new section makes the preparation commit append a system entry next
	// to the compaction's status.
	addSection(t, chat.setup.registry, constSection("extra", "extra"))
	never, _ := unanswered()
	chat.faux.pushSummary(never)
	chat.turn(t, sizedText("u4", 100), "a4")
	var batch []AgentEvent
	eventually(t, func() bool {
		for _, candidate := range log.all() {
			for _, event := range candidate {
				if event.EventType() == "compaction_start" {
					batch = candidate
					return true
				}
			}
		}
		return false
	})
	if got := eventTypes(batch); !reflect.DeepEqual(got, []string{"message_start", "message_end", "compaction_start"}) {
		t.Fatalf("batch = %v", got)
	}
	_, err := chat.h.AbortTask(bg, chat.compactionTasks(t)[0].ID)
	must(t, err)
}

func TestCompactionKeepsABackgroundSummaryQueuedThroughARetryBackoffWhileABlockingCompactionWins(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 2000})
	chat.history(t)
	chat.setup.setSettings(backgroundPolicy)
	chat.setup.setSettings(retrySettings(true, 2, 300*time.Millisecond))
	summary, summaryReached, releaseSummary := gatedStep(fauxAnswer("BACKGROUND"))
	chat.faux.pushSummary(summary, summaryStep("BLOCKING"))
	failed := make(chan struct{})
	chat.faux.pushAgent(func(context.Context, ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
		close(failed)
		return fauxError("overloaded")
	}, fauxText("a4"))
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
	must(t, err)
	awaitSignal(t, failed, "the failing request")
	awaitSignal(t, summaryReached, "the background summary request")
	// During the backoff: the background summary queues, and the thresholds
	// drop so the retry blocks.
	eventually(t, func() bool {
		generation, _ := chat.live(t)["generation"].(map[string]any)
		_, retrying := generation["retry"]
		return retrying
	})
	background := chat.compactionTasks(t)[0]
	releaseSummary()
	submission := chat.summarySubmission(t, background.ID)
	if status := submissionStatus(t, submission).Status; status != durable.SubmissionQueued {
		t.Fatalf("background summary = %s, want queued", status)
	}
	chat.setup.setSettings(compactionPolicyOf(true, 1500, 50, 1000))
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	if got := firstUserText(chat.faux.lastAgentRequest(t).messages); !strings.Contains(got, "BLOCKING") {
		t.Fatalf("first user = %q", got)
	}
	if record := submissionWait(t, submission); record.Status != durable.SubmissionUnanswered || record.Reason != "stale" {
		t.Fatalf("background summary = %+v", record)
	}
}

// blockingRun is a run whose generation waits on a blocking compaction whose
// summary is held.
func (c *compactionChat) blockingRun(t *testing.T) (input *Submission, release func(), blocking durable.TaskID) {
	t.Helper()
	c.history(t)
	c.setup.setSettings(blockingPolicy)
	step, reached, release := gatedStep(fauxAnswer("BLOCKING"))
	c.faux.pushSummary(step)
	input, err := c.root.Submit(bg, inputDraft(sizedText("u4", 200)))
	must(t, err)
	awaitSignal(t, reached, "the blocking summary request")
	return input, release, c.compactionTasks(t)[0].ID
}

func TestCompactionPlacesAManualSummarySelectedBeforeTheBlockingOneLandedItsEqualCutReplacesIt(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 1000})
	input, release, _ := chat.blockingRun(t)
	// Selected from the same context as the blocking compaction, so it cuts
	// at the same entry.
	chat.faux.pushSummary(summaryStep("MANUAL"))
	submission := chat.summarySubmission(t, chat.compact(t, nil))
	if status := submissionStatus(t, submission).Status; status != durable.SubmissionQueued {
		t.Fatalf("manual summary = %s, want queued", status)
	}
	chat.faux.pushAgent(fauxText("a4"))
	release()
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	// The request after the blocking compaction used its summary; the manual
	// one landed at the final boundary.
	if got := firstUserText(chat.faux.lastAgentRequest(t).messages); !strings.Contains(got, "BLOCKING") {
		t.Fatalf("first user = %q", got)
	}
	if status := submissionWait(t, submission).Status; status != durable.SubmissionDone {
		t.Fatalf("manual summary = %s", status)
	}
	var markers []durable.EntryRecord
	for _, record := range chatEntries(t, chat.root) {
		if record.Kind == "pi.compaction" {
			markers = append(markers, record)
		}
	}
	if len(markers) != 2 || markers[0].Head == nil || markers[1].Head == nil || *markers[1].Head != *markers[0].Head {
		t.Fatalf("markers = %+v", markers)
	}
	messages := chat.context(t).Messages
	if !strings.Contains(firstUserText(messages), "MANUAL") {
		t.Fatalf("first user = %q", firstUserText(messages))
	}
	for _, message := range messages {
		if strings.Contains(msgText(message), "BLOCKING") {
			t.Fatal("the blocking summary is still in the context")
		}
	}
}

func TestCompactionFindsNothingToCompactForAManualCompactionSelectedAfterTheBlockingSummaryLanded(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 1000})
	input, release, _ := chat.blockingRun(t)
	answer, answerReached, releaseAnswer := gatedStep(fauxAnswer("a4"))
	chat.faux.pushAgent(answer)
	release()
	awaitSignal(t, answerReached, "the answer request")
	id := chat.compact(t, nil)
	chat.outcome(t, id)
	if got := outcomeJSON(t, *getTask(t, chat.h, id)); got != `{"status":"completed","result":{}}` {
		t.Fatalf("outcome = %s", got)
	}
	if n := chat.faux.summaryCount(); n != 1 {
		t.Fatalf("summary requests = %d", n)
	}
	releaseAnswer()
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
}

func TestCompactionAbortsTheRunAndBothCompactionsOnEscAndAppendsNothing(t *testing.T) {
	chat := openCompaction(t, compactionOpen{contextWindow: 1000})
	input, _, blocking := chat.blockingRun(t)
	never, reached := unanswered()
	chat.faux.pushSummary(never)
	manual := chat.compact(t, nil)
	awaitSignal(t, reached, "the manual summary request")
	var blockingFlags []bool
	for _, status := range chat.liveStatuses(t) {
		blockingFlags = append(blockingFlags, status.Blocking)
	}
	if !reflect.DeepEqual(blockingFlags, []bool{true, false}) {
		t.Fatalf("blocking flags = %v", blockingFlags)
	}
	must(t, chat.root.Abort(bg, false))
	if settled := submissionWait(t, input); settled.Status != durable.SubmissionUnanswered || settled.Reason != "aborted" {
		t.Fatalf("input = %+v", settled)
	}
	for _, id := range []durable.TaskID{blocking, manual} {
		if outcome := chat.outcome(t, id); outcome.Status != durable.OutcomeAborted {
			t.Fatalf("compaction %d = %+v", id, outcome)
		}
	}
	live := chat.live(t)
	if _, ok := live["compactions"]; ok {
		t.Fatal("compactions are still listed")
	}
	if _, ok := live["run"]; ok {
		t.Fatal("the run is still listed")
	}
	if hasKind(chat.kinds(t), "pi.compaction") {
		t.Fatal("an aborted compaction wrote a summary")
	}
	inspection, err := chat.h.Inspect(bg)
	must(t, err)
	if len(inspection.Submissions) != 0 {
		t.Fatalf("submissions = %+v", inspection.Submissions)
	}
}

func TestCompactionPlacesASummaryThatSurvivedEscAtTheNextSubmissionBeforeItsInput(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	never, reached := unanswered()
	chat.faux.pushAgent(never)
	busy, err := chat.root.Submit(bg, inputDraft("busy"))
	must(t, err)
	awaitSignal(t, reached, "the busy request")
	submission := chat.queuedSummary(t, "SUMMARY")
	must(t, chat.root.Abort(bg, false))
	if settled := submissionWait(t, busy); settled.Status != durable.SubmissionUnanswered || settled.Reason != "aborted" {
		t.Fatalf("busy = %+v", settled)
	}
	if status := submissionStatus(t, submission).Status; status != durable.SubmissionQueued {
		t.Fatalf("summary = %s, want queued", status)
	}
	chat.turn(t, "u5", "a5")
	if status := submissionStatus(t, submission).Status; status != durable.SubmissionDone {
		t.Fatalf("summary = %s", status)
	}
	request := chat.faux.lastAgentRequest(t).messages
	if !strings.Contains(firstUserText(request), "SUMMARY") {
		t.Fatalf("first user = %q", firstUserText(request))
	}
	found := false
	for _, message := range request {
		found = found || msgText(message) == "u5"
	}
	if !found {
		t.Fatal("the input is not in the request")
	}
	entries := chatEntries(t, chat.root)
	var tail []string
	for _, record := range entries[len(entries)-4:] {
		tail = append(tail, record.Kind)
	}
	if !reflect.DeepEqual(tail, []string{"pi.compaction", "pi.user", "pi.system", "pi.assistant"}) {
		t.Fatalf("tail = %v", tail)
	}
}

func TestCompactionTreatsSilentOverflowAsAnOrdinaryAnswer(t *testing.T) {
	length := providers.FauxAssistantMessage(nil, ai.StopLength)
	for _, c := range []struct {
		name     string
		response *ai.AssistantMessage
	}{
		{"a stop whose input exceeds the window", fauxAnswer("fine")},
		{"a length stop that fills the window without output", length},
	} {
		t.Run(c.name, func(t *testing.T) {
			chat := openCompaction(t, compactionOpen{contextWindow: 300})
			chat.history(t)
			// Thresholds out of reach, so only overflow classification could
			// compact.
			chat.setup.setSettings(compactionPolicyOf(true, -100_000, 150, 0))
			chat.faux.pushAgent(providers.FauxStatic(c.response))
			input, err := chat.root.Submit(bg, inputDraft("go"))
			must(t, err)
			if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
				t.Fatalf("input = %s", status)
			}
			entries := chatEntries(t, chat.root)
			last, ok := entries[len(entries)-1].Model[0].(ai.AssistantMessage)
			if !ok || last.Usage.Input < 300 {
				t.Fatalf("last = %+v", entries[len(entries)-1])
			}
			if n := chat.faux.summaryCount(); n != 0 {
				t.Fatalf("summary requests = %d", n)
			}
		})
	}
}

func TestCompactionSendsTheRequestWhenABlockingCompactionFindsNothingUnderAPolicyChangedAfterPreparation(t *testing.T) {
	setup := newCompactionSetup(t, 1000)
	faux := scriptCompaction(setup)
	var changed atomic.Bool
	addSection(t, setup.registry, Section("policy", func(context.Context, PromptInput) (string, bool, error) {
		// Rendering runs after preparation read the policy; the compaction
		// reads this one.
		if changed.CompareAndSwap(false, true) {
			setup.setSettings(compactionPolicyOf(true, 300, 100_000, 0))
		}
		return "p", true, nil
	}))
	chat := openCompactionWith(t, compactionOpen{contextWindow: 1000}, setup, faux)
	chat.history(t)
	changed.Store(false)
	chat.setup.setSettings(blockingPolicy)
	chat.turn(t, sizedText("u4", 200), "a4")
	if tasks := chat.compactionTasks(t); len(tasks) != 0 {
		t.Fatalf("tasks = %+v", tasks)
	}
	if n := chat.faux.summaryCount(); n != 0 {
		t.Fatalf("summary requests = %d", n)
	}
	if hasKind(chat.kinds(t), "pi.compaction") {
		t.Fatal("a summary was written")
	}
}

func TestCompactionAppliesTheCurrentModelsOverrideToTheBlockingThreshold(t *testing.T) {
	for _, key := range []string{"faux/faux-1", "faux/other"} {
		t.Run(key, func(t *testing.T) {
			chat := openCompaction(t, compactionOpen{contextWindow: 1000})
			chat.history(t)
			// Harness-wide: blocking only above the full window; the override
			// for faux-1 blocks above 700.
			chat.setup.setSettings(func(s *HarnessSettings) {
				compactionPolicyOf(true, 0, 150, 0)(s)
				s.Compaction.ModelOverrides = map[string]CompactionOverride{key: {ReserveTokens: ptr(300)}}
			})
			chat.faux.pushSummary(summaryStep("SUMMARY"))
			chat.turn(t, sizedText("u4", 200), "a4")
			want := 0
			if key == "faux/faux-1" {
				want = 1
			}
			if n := chat.faux.summaryCount(); n != want {
				t.Fatalf("summary requests = %d, want %d", n, want)
			}
		})
	}
}

func TestCompactionAppliesTheRequestModelsOverrideOnAContextOverflow(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	chat.setup.setSettings(func(s *HarnessSettings) {
		manualPolicy(150)(s)
		s.Compaction.ModelOverrides = map[string]CompactionOverride{"faux/faux-1": {Enabled: ptr(true)}}
	})
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.faux.pushAgent(fauxStep(fauxError(overflowText)), fauxText("fits"))
	input, err := chat.root.Submit(bg, inputDraft(sizedText("u4", 100)))
	must(t, err)
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	if n := chat.faux.summaryCount(); n != 1 {
		t.Fatalf("summary requests = %d", n)
	}
}

// harness-telemetry.test.ts.
func TestTelemetryParentsBackgroundCompactionToThePreparePhaseThatCreatesIt(t *testing.T) {
	setup := newCompactionSetup(t, 1000)
	recorder := telemetry.NewInMemoryContext()
	setup.telemetry = recorder
	setup.setSettings(compactionPolicyOf(true, 300, 150, 1000))
	addCompactionHooks(t, setup.registry, "decline", decide(&CompactionDecision{Decline: true}))
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("done")})
	h, root := openChat(t, memory.New(), setup)
	must(t, h.Resume())
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		if _, err := tx.AppendEntry(root.ID, durable.EntryDraft{Kind: durable.UserEntry.Kind, Model: []ai.Message{ai.NewUserText(strings.Repeat("x", 400), 1)}}); err != nil {
			return err
		}
		_, err := tx.AppendEntry(root.ID, durable.EntryDraft{Kind: durable.AssistantEntry.Kind, Model: []ai.Message{*fauxAnswer(strings.Repeat("y", 400))}})
		return err
	}))
	submission, err := root.Submit(bg, inputDraft(strings.Repeat("z", 400)))
	must(t, err)
	submissionWait(t, submission)
	must(t, h.WaitForIdle(bg))
	phaseOf := func(name string) *telemetry.RecordedSpan {
		for _, span := range recorder.Spans() {
			if span.Attributes["task.name"] == name && (name != GenerationTask.Name() || span.Attributes["phase"] == phasePrepare) {
				return &span
			}
		}
		return nil
	}
	eventually(t, func() bool { span := phaseOf(compactionKind); return span != nil && span.Settled })
	prepare, compaction := phaseOf(GenerationTask.Name()), phaseOf(compactionKind)
	if prepare == nil || !reflect.DeepEqual(compaction.ParentContext, prepare.Context) {
		t.Fatalf("compaction parent = %v, prepare = %+v", compaction.ParentContext, prepare)
	}
	var records []durable.TaskRecord
	must(t, h.ReadOnLine(func() error {
		page, err := h.Storage().ScanTasks(bg, durable.TaskQuery{Kind: compactionKind}, 10, nil)
		records = page.Items
		return err
	}))
	if len(records) != 1 || !records[0].Background || !reflect.DeepEqual(records[0].Trace, prepare.Context) {
		t.Fatalf("records = %+v", records)
	}
}

func TestTelemetryRecordsInternalCompactionAdmissionsAsPhaseEventsWithoutASubmitSpan(t *testing.T) {
	setup := newChatSetup(t)
	recorder := telemetry.NewInMemoryContext()
	setup.telemetry = recorder
	setup.setSettings(func(s *HarnessSettings) { s.Compaction.KeepRecentTokens = ptr(1) })
	storage := newControlledStorage()
	var held atomic.Pointer[gate]
	addCompactionHooks(t, setup.registry, "summary", &CompactionHooks{BeforeCompact: func(context.Context, CompactionRequest, *TaskRuntime) (*CompactionDecision, error) {
		held.Store(storage.holdCommits())
		return &CompactionDecision{Summary: "private-summary"}, nil
	}})
	h, root := openChat(t, storage, setup)
	must(t, h.Resume())
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		if _, err := tx.AppendEntry(root.ID, durable.EntryDraft{Kind: durable.UserEntry.Kind, Model: []ai.Message{ai.NewUserText("old", 1)}}); err != nil {
			return err
		}
		_, err := tx.AppendEntry(root.ID, durable.EntryDraft{Kind: durable.AssistantEntry.Kind, Model: []ai.Message{*fauxAnswer("answer")}})
		return err
	}))
	id, err := root.Compact(bg, nil)
	must(t, err)
	eventually(t, func() bool { return held.Load() != nil })
	held.Load().Entered(t)
	if spans := spansNamed(recorder, SpanSubmit); len(spans) != 0 {
		t.Fatalf("submit spans = %d", len(spans))
	}
	phase := func() *telemetry.RecordedSpan {
		for _, span := range recorder.Spans() {
			if span.Attributes["task.name"] == compactionKind {
				return &span
			}
		}
		return nil
	}
	if span := phase(); span == nil || span.Settled || len(span.Events) != 0 {
		t.Fatalf("compaction phase = %+v", span)
	}
	storage.releaseHold(held.Load())
	_, err = h.WaitForTask(bg, id)
	must(t, err)
	allSettled(t, recorder)
	if spans := spansNamed(recorder, SpanSubmit); len(spans) != 0 {
		t.Fatalf("submit spans = %d", len(spans))
	}
	entry, done := false, false
	for _, event := range phase().Events {
		entry = entry || event.Name == EventEntry
		done = done || event.Name == EventSubmission && event.Attributes["status"] == durable.SubmissionDone
	}
	if !entry || !done {
		t.Fatalf("compaction phase events = %+v", phase().Events)
	}
	mustNotContain(t, "spans", recorder.Spans(), "private-summary")
}
