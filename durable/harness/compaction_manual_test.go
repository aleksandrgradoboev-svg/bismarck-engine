package harness

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Manual compaction (pi harness-compaction.test.ts, "manual compaction").

const summaryMarkerText = "The conversation history before this point was compacted into the following summary:\n\n<summary>\nSUMMARY\n</summary>"

func TestCompactionPlacesTheSummaryAtOnceWhenIdleAndKeepsRawHistory(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	before := chatEntries(t, chat.root)
	usageBefore := chat.modelUsage(t)
	chat.faux.pushSummary(summaryStep("SUMMARY"))

	submission := chat.summarySubmission(t, chat.compact(t, ptr("focus on files")))
	placed := submissionWait(t, submission)
	if placed.Status != durable.SubmissionDone {
		t.Fatalf("placed = %+v", placed)
	}

	// Raw history is unchanged; one summary entry heads the first kept
	// entry, u3.
	after := chatEntries(t, chat.root)
	if !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatal("raw history changed")
	}
	marker := after[len(after)-1]
	var u3 durable.EntryRecord
	for _, record := range before {
		if strings.HasPrefix(textOf(record), "u3") {
			u3 = record
		}
	}
	if marker.Kind != "pi.compaction" || marker.Head == nil || *marker.Head != u3.ID || string(marker.Data) != `{"reason":"manual"}` {
		t.Fatalf("marker = %+v", marker)
	}
	if placed.Entry == nil || *placed.Entry != marker.ID {
		t.Fatalf("placed entry = %v, marker %d", placed.Entry, marker.ID)
	}
	if got := textOf(marker); got != summaryMarkerText {
		t.Fatalf("marker text = %q", got)
	}

	// The model context is the summary followed by the kept entries.
	var texts []string
	for _, message := range chat.context(t).Messages {
		texts = append(texts, msgText(message))
	}
	if want := []string{summaryMarkerText, sizedText("u3", 100), sizedText("a3", 100)}; !reflect.DeepEqual(texts, want) {
		t.Fatalf("context = %q", texts)
	}

	// The summarizer saw the serialized prefix, the prompt, and the
	// instructions, without tools or caching.
	request := chat.faux.summaryRequest(t, 0)
	if len(request.messages) != 2 {
		t.Fatalf("summary request has %d messages", len(request.messages))
	}
	prompt := msgText(request.messages[1])
	if !regexp.MustCompile(`^<conversation>\n\[User\]: u1 `).MatchString(prompt) || !strings.Contains(prompt, "[Assistant]: a2 ") ||
		strings.Contains(prompt, "u3 ") || !strings.Contains(prompt, "## Goal") || !strings.HasSuffix(prompt, "\n\nAdditional focus: focus on files") {
		t.Fatalf("prompt = %q", prompt)
	}
	provider, err := chat.h.Snapshot(bg, ProviderDoc.Conversation(chat.root.ID))
	must(t, err)
	options := request.options
	if options.CacheRetention != "none" || options.MaxTokens == nil || *options.MaxTokens != 800 || options.SessionID != provider["sessionId"] || options.Deferred != nil {
		t.Fatalf("options = cache %q, max %v, session %q (want %v), deferred %v", options.CacheRetention, options.MaxTokens, options.SessionID, provider["sessionId"], options.Deferred)
	}

	// The summarizer's spend is in the ledger, and nothing counts it again
	// later.
	if usage := chat.modelUsage(t); usage.Input <= usageBefore.Input {
		t.Fatalf("usage input %d, before %d", usage.Input, usageBefore.Input)
	}
	chat.turn(t, "next", "done")
	agent := chat.faux.lastAgentRequest(t).messages
	// The next request: one complete system baseline leading the summary,
	// the kept turn, and the new input (#10904).
	var roles []ai.Role
	for _, message := range agent {
		roles = append(roles, message.MessageRole())
	}
	if want := []ai.Role{ai.RoleSystem, ai.RoleUser, ai.RoleUser, ai.RoleAssistant, ai.RoleUser}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("roles = %v", roles)
	}
	if text, ok := agent[0].(ai.SystemMessage).Sections.Get("preamble"); !ok || text == nil || *text != "You are helpful." {
		t.Fatalf("baseline sections = %+v", agent[0].(ai.SystemMessage).Sections)
	}
}

// Regression coverage for #10424.
func TestCompactionCreatesProviderStateBeforeALegacyConversationsSummarizationRequest(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	must(t, chat.root.Commit(bg, func(tx *durable.Tx) error { return tx.RetireDoc(ProviderDoc.Conversation(chat.root.ID)) }))
	if value, err := chat.h.Snapshot(bg, ProviderDoc.Conversation(chat.root.ID)); err != nil || value != nil {
		t.Fatalf("provider = %v, %v", value, err)
	}
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, chat.compact(t, nil))
	stored, err := chat.h.Snapshot(bg, ProviderDoc.Conversation(chat.root.ID))
	must(t, err)
	id, _ := stored["sessionId"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("session ID = %q", id)
	}
	if got := chat.faux.summaryRequest(t, 0).options.SessionID; got != id {
		t.Fatalf("request session ID = %q, want %q", got, id)
	}
}

func TestCompactionKeepsWorkingWhileBusyAndPlacesTheSummaryAtTheNextFinalBoundary(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	step, reached, release := gatedStep(fauxAnswer("late answer"))
	chat.faux.pushAgent(step)
	input, err := chat.root.Submit(bg, inputDraft("busy"))
	must(t, err)
	awaitSignal(t, reached, "the busy request")
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	submission := chat.summarySubmission(t, chat.compact(t, nil))
	if status := submissionStatus(t, submission).Status; status != durable.SubmissionQueued {
		t.Fatalf("summary submission = %s, want queued", status)
	}
	release()
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	if status := submissionWait(t, submission).Status; status != durable.SubmissionDone {
		t.Fatalf("summary = %s", status)
	}
	// The summary follows the answer; the kept range still includes the busy
	// turn.
	kinds := chat.kinds(t)
	if got := kinds[len(kinds)-3:]; !reflect.DeepEqual(got, []string{"pi.user", "pi.assistant", "pi.compaction"}) {
		t.Fatalf("kinds = %v", got)
	}
}

func TestCompactionPlacesAQueuedSummaryAtPostToolsAndTheRunContinuesInTheCompactedContext(t *testing.T) {
	setup := newCompactionSetup(t, 0)
	faux := scriptCompaction(setup)
	wait := testTool("wait")
	wait.Execute = func(context.Context, map[string]any, ToolExecutionAPI) (ToolExecutionResult, error) {
		return ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "waited"}}}, nil
	}
	addTool(t, setup.registry, wait)
	chat := openCompactionWith(t, compactionOpen{}, setup, faux)
	must(t, chat.root.Configure(bg, AgentChange{Tools: Set(Exactly(wait))}))
	chat.history(t)
	step, reached, release := gatedStep(providers.FauxAssistantMessage(ai.ContentList{providers.FauxToolCall("wait", map[string]any{})}, ai.StopToolUse))
	chat.faux.pushAgent(step, fauxText("after tools"))
	input, err := chat.root.Submit(bg, inputDraft("use a tool"))
	must(t, err)
	awaitSignal(t, reached, "the tool-calling request")
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, chat.compact(t, nil))
	release()
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	// The continuation request starts with the new baseline, then the
	// summary, ahead of the kept tool call (#10904).
	continuation := chat.faux.lastAgentRequest(t).messages
	if continuation[0].MessageRole() != ai.RoleSystem || !strings.Contains(msgText(continuation[1]), "<summary>\nSUMMARY\n</summary>") {
		t.Fatalf("continuation = %v", continuation[:2])
	}
	kinds := chat.kinds(t)
	if got := kinds[len(kinds)-4:]; !reflect.DeepEqual(got, []string{"pi.tool-result", "pi.compaction", "pi.system", "pi.assistant"}) {
		t.Fatalf("kinds = %v", got)
	}
}

func TestCompactionRunsFollowUpsLeftByAFailedRunAfterPlacingTheSummary(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	step, reached, release := gatedStep(fauxError("bad request"))
	chat.faux.pushAgent(step)
	failed, err := chat.root.Submit(bg, inputDraft("fails"))
	must(t, err)
	awaitSignal(t, reached, "the failing request")
	followUp, err := chat.root.Submit(bg, inputDraft("follow-up"))
	must(t, err)
	release()
	if status := submissionWait(t, failed).Status; status != durable.SubmissionUnanswered {
		t.Fatalf("failed = %s", status)
	}
	if status := submissionStatus(t, followUp).Status; status != durable.SubmissionQueued {
		t.Fatalf("follow-up = %s", status)
	}

	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.faux.pushAgent(fauxText("followed"))
	chat.result(t, chat.compact(t, nil))
	if status := submissionWait(t, followUp).Status; status != durable.SubmissionDone {
		t.Fatalf("follow-up = %s", status)
	}
	request := chat.faux.lastAgentRequest(t).messages
	if !strings.Contains(firstUserText(request), "SUMMARY") {
		t.Fatalf("first user = %q", firstUserText(request))
	}
	found := false
	for _, message := range request {
		if msgText(message) == "follow-up" {
			found = true
		}
	}
	if !found {
		t.Fatal("the follow-up is not in the request")
	}
}

func TestCompactionSettlesStaleWhenAResetLandsWhileItSummarizes(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	step, reached, release := gatedStep(fauxAnswer("SUMMARY"))
	chat.faux.pushSummary(step)
	id := chat.compact(t, nil)
	awaitSignal(t, reached, "the summary request")
	must(t, chat.root.Reset(bg, nil))
	release()
	record := submissionStatus(t, chat.summarySubmission(t, id))
	if record.Status != durable.SubmissionUnanswered || record.Reason != "stale" {
		t.Fatalf("summary = %+v", record)
	}
	if kinds := chat.kinds(t); kinds[len(kinds)-1] != "pi.reset" {
		t.Fatalf("kinds = %v", kinds)
	}
}

func TestCompactionDoesNotMakeTheConversationBusy(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	summary, summaryReached, releaseSummary := gatedStep(fauxAnswer("SUMMARY"))
	chat.faux.pushSummary(summary)
	id := chat.compact(t, nil)
	awaitSignal(t, summaryReached, "the summary request")
	answer, answerReached, releaseAnswer := gatedStep(fauxAnswer("a4"))
	chat.faux.pushAgent(answer)
	input, err := chat.root.Submit(bg, inputDraft("u4"))
	must(t, err)
	// Placed and answered immediately with the uncompacted context, not
	// queued behind the compaction.
	if status := submissionStatus(t, input).Status; status != durable.SubmissionPlaced {
		t.Fatalf("input = %s, want placed", status)
	}
	awaitSignal(t, answerReached, "the answer request")
	if got := firstUserText(chat.faux.lastAgentRequest(t).messages); got != sizedText("u1", 100) {
		t.Fatalf("first user = %q", got)
	}
	// The summary is ready while the run is busy, so it queues and lands
	// after the answer.
	releaseSummary()
	submission := chat.summarySubmission(t, id)
	if status := submissionStatus(t, submission).Status; status != durable.SubmissionQueued {
		t.Fatalf("summary = %s, want queued", status)
	}
	releaseAnswer()
	if status := submissionWait(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s", status)
	}
	if status := submissionWait(t, submission).Status; status != durable.SubmissionDone {
		t.Fatalf("summary = %s", status)
	}
	kinds := chat.kinds(t)
	if got := kinds[len(kinds)-3:]; !reflect.DeepEqual(got, []string{"pi.user", "pi.assistant", "pi.compaction"}) {
		t.Fatalf("kinds = %v", got)
	}
}

func TestCompactionCountsTheSpendOfASummaryThatEndsStaleAndWritesNoEntryForIt(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	before := chat.modelUsage(t).Input
	step, reached, release := gatedStep(fauxAnswer("SUMMARY"))
	chat.faux.pushSummary(step)
	id := chat.compact(t, nil)
	awaitSignal(t, reached, "the summary request")
	must(t, chat.root.Reset(bg, nil))
	release()
	record := submissionStatus(t, chat.summarySubmission(t, id))
	if record.Status != durable.SubmissionUnanswered || record.Reason != "stale" {
		t.Fatalf("summary = %+v", record)
	}
	if after := chat.modelUsage(t).Input; after <= before {
		t.Fatalf("usage input %d, before %d", after, before)
	}
	if hasKind(chat.kinds(t), "pi.compaction") {
		t.Fatal("a stale summary wrote an entry")
	}
}

func TestCompactionLetsTheCompactionThatCutsFurthestWinWhateverFinishesFirst(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	first, firstReached, releaseFirst := gatedStep(fauxAnswer("FIRST"))
	chat.faux.pushSummary(first)
	early := chat.compact(t, nil)
	awaitSignal(t, firstReached, "the first summary request")
	chat.turn(t, sizedText("u4", 100), sizedText("a4", 100))
	chat.faux.pushSummary(summaryStep("SECOND"))
	chat.result(t, chat.compact(t, nil))
	releaseFirst()
	// The early compaction cut at u3, before the later cut at u4.
	record := submissionStatus(t, chat.summarySubmission(t, early))
	if record.Status != durable.SubmissionUnanswered || record.Reason != "stale" {
		t.Fatalf("early summary = %+v", record)
	}
	if got := firstUserText(chat.context(t).Messages); !strings.Contains(got, "SECOND") {
		t.Fatalf("first user = %q", got)
	}
}

func TestCompactionPlacesAnOlderSelectedSummaryThatCutsLaterThanTheNewerOne(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	// A: small budget, late cut; selected first, finishes last.
	step, reached, release := gatedStep(fauxAnswer("A"))
	chat.faux.pushSummary(step)
	a := chat.compact(t, nil)
	awaitSignal(t, reached, "the summary request of A")
	// B: larger budget, earlier cut; placed first.
	chat.setup.setSettings(manualPolicy(350))
	chat.faux.pushSummary(summaryStep("B"))
	chat.result(t, chat.compact(t, nil))
	if got := firstUserText(chat.context(t).Messages); !strings.Contains(got, "B") {
		t.Fatalf("first user = %q", got)
	}
	release()
	if status := submissionStatus(t, chat.summarySubmission(t, a)).Status; status != durable.SubmissionDone {
		t.Fatalf("A = %s", status)
	}
	messages := chat.context(t).Messages
	if got := firstUserText(messages); !strings.Contains(got, "<summary>\nA\n</summary>") {
		t.Fatalf("first user = %q", got)
	}
	var rest []string
	for _, message := range messages[1:] {
		rest = append(rest, msgText(message))
	}
	if want := []string{sizedText("u3", 100), sizedText("a3", 100)}; !reflect.DeepEqual(rest, want) {
		t.Fatalf("kept = %q", rest)
	}
}

func TestCompactionPlacesTwoQueuedSummariesInOneBoundary(t *testing.T) {
	for _, c := range []struct {
		order string
		keeps [2]int
		stale bool
	}{{"before", [2]int{150, 350}, true}, {"at", [2]int{150, 150}, false}, {"after", [2]int{350, 150}, false}} {
		t.Run("the second cuts "+c.order+" the first", func(t *testing.T) {
			chat := openCompaction(t, compactionOpen{})
			chat.history(t)
			step, reached, release := gatedStep(fauxAnswer("done"))
			chat.faux.pushAgent(step)
			input, err := chat.root.Submit(bg, inputDraft("busy"))
			must(t, err)
			awaitSignal(t, reached, "the busy request")
			var submissions []*Submission
			for index, keep := range c.keeps {
				chat.setup.setSettings(manualPolicy(keep))
				chat.faux.pushSummary(summaryStep("S" + string(rune('0'+index))))
				submissions = append(submissions, chat.summarySubmission(t, chat.compact(t, nil)))
			}
			release()
			submissionWait(t, input)
			var statuses []string
			for _, submission := range submissions {
				statuses = append(statuses, submissionWait(t, submission).Status)
			}
			want := []string{durable.SubmissionDone, durable.SubmissionDone}
			if c.stale {
				want[1] = durable.SubmissionUnanswered
			}
			if !reflect.DeepEqual(statuses, want) {
				t.Fatalf("statuses = %v, want %v", statuses, want)
			}
		})
	}
}

func TestCompactionIsAbortedByConversationAbortAndAnAlreadyQueuedSummarySurvivesIt(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	never, reached := unanswered()
	chat.faux.pushSummary(never)
	id := chat.compact(t, nil)
	awaitSignal(t, reached, "the summary request")
	if statuses, _ := chat.live(t)["compactions"].([]any); len(statuses) != 1 {
		t.Fatalf("compactions = %v", chat.live(t)["compactions"])
	}
	must(t, chat.root.Abort(bg, false))
	if outcome := chat.outcome(t, id); outcome.Status != durable.OutcomeAborted {
		t.Fatalf("outcome = %+v", outcome)
	}
	if _, ok := chat.live(t)["compactions"]; ok {
		t.Fatal("compactions are still listed")
	}
	if hasKind(chat.kinds(t), "pi.compaction") {
		t.Fatal("an aborted compaction wrote an entry")
	}

	// Queued while busy, then Esc: the queued write stays and lands with the
	// next run's boundary.
	busy, busyReached := unanswered()
	chat.faux.pushAgent(busy)
	_, err := chat.root.Submit(bg, inputDraft("busy"))
	must(t, err)
	awaitSignal(t, busyReached, "the busy request")
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	queued := chat.compact(t, nil)
	submission := chat.summarySubmission(t, queued)
	must(t, chat.root.Abort(bg, false))
	if status := submissionStatus(t, submission).Status; status != durable.SubmissionQueued {
		t.Fatalf("summary = %s, want queued", status)
	}
	result, err := submission.Abort(bg)
	must(t, err)
	if result != SubmissionAborted {
		t.Fatalf("abort = %s", result)
	}
	chat.turn(t, "next", "ok")
	record := submissionStatus(t, submission)
	if record.Status != durable.SubmissionUnanswered || record.Reason != "aborted" {
		t.Fatalf("summary = %+v", record)
	}
	if hasKind(chat.kinds(t), "pi.compaction") {
		t.Fatal("a withdrawn summary wrote an entry")
	}
}

func TestCompactionIsOrdinaryWorkIdleWaitsIncludeIt(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	step, reached, release := gatedStep(fauxAnswer("SUMMARY"))
	chat.faux.pushSummary(step)
	id := chat.compact(t, nil)
	awaitSignal(t, reached, "the summary request")
	idle := make(chan error, 1)
	go func() { idle <- chat.root.WaitForIdle(bg) }()
	select {
	case <-idle:
		t.Fatal("idle while the compaction summarizes")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	must(t, <-idle)
	if record := getTask(t, chat.h, id); record == nil || record.State.Status != durable.TaskTerminal {
		t.Fatalf("task = %+v", record)
	}
}

func TestCompactionEnablesSchedulingRightAfterOpen(t *testing.T) {
	setup := newChatSetup(t)
	faux := scriptCompaction(setup)
	h, root := openChat(t, memory.New(), setup)
	setup.setSettings(manualPolicy(10))
	id, err := root.Compact(bg, nil)
	must(t, err)
	// GetTask only reads, so progress here comes from Compact itself.
	eventually(t, func() bool {
		record, err := h.GetTask(bg, id)
		return err == nil && record != nil && record.State.Status == durable.TaskTerminal
	})
	record := getTask(t, h, id)
	if outcomeJSON(t, *record) != `{"status":"completed","result":{}}` {
		t.Fatalf("outcome = %s", outcomeJSON(t, *record))
	}
	if n := faux.summaryCount(); n != 0 {
		t.Fatalf("summary requests = %d", n)
	}
}
