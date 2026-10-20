package harness

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Manual compaction with the rest of the Harness (pi harness-compaction.test.ts:
// the manual cases of "compaction estimates and interactions", "compaction
// and the inbox", "compaction edge cases", "compaction pinning…", "blocked
// compaction" and "context contributions").

func TestCompactionRebaselinesTheSystemPromptOverKeptSystemDeltas(t *testing.T) {
	setup := newCompactionSetup(t, 0)
	faux := scriptCompaction(setup)
	var mu sync.Mutex
	mood := "cheerful"
	addSection(t, setup.registry, &PromptSection{Key: "mood", Untagged: true, Render: func(context.Context, PromptInput) (string, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return mood, true, nil
	}})
	chat := openCompactionWith(t, compactionOpen{}, setup, faux)
	chat.turn(t, sizedText("u1", 100), sizedText("a1", 100))
	chat.turn(t, sizedText("u2", 100), sizedText("a2", 100))
	mu.Lock()
	mood = "terse"
	mu.Unlock()
	chat.turn(t, sizedText("u3", 100), sizedText("a3", 100))
	chat.setup.setSettings(manualPolicy(250))
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, chat.compact(t, nil))
	// The kept range holds the delta for the terse mood; the next request has
	// one complete baseline.
	kept := false
	for _, record := range chat.context(t).Entries {
		kept = kept || record.Kind == durable.SystemEntry.Kind
	}
	if !kept {
		t.Fatal("the kept range has no pi.system entry")
	}
	chat.turn(t, "next", "ok")
	var systems []ai.SystemMessage
	for _, message := range chat.faux.lastAgentRequest(t).messages {
		if system, ok := message.(ai.SystemMessage); ok {
			systems = append(systems, system)
		}
	}
	if len(systems) != 1 {
		t.Fatalf("system messages = %d", len(systems))
	}
	for name, want := range map[string]string{"preamble": "You are helpful.", "mood": "terse"} {
		if got, ok := systems[0].Sections.Get(name); !ok || got == nil || *got != want {
			t.Fatalf("section %s = %v", name, got)
		}
	}
}

func redactDraft(target durable.EntryID) SubmissionDraft {
	return SubmissionDraft{Type: durable.SubmissionTypeWrite, Write: &durable.EntryDraft{Kind: "app.redact", Edits: []durable.ContextEdit{
		{Target: target, Action: durable.ContextEditReplace, Messages: []ai.Message{ai.NewUserText("REDACTED", 0)}},
	}}}
}

func TestCompactionSummarizesAReplacedEntrysReplacementAndShowsItToTheHook(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	u1 := chatEntries(t, chat.root)[0]
	_, err := chat.root.Submit(bg, redactDraft(u1.ID))
	must(t, err)
	var seen []ai.Message
	addCompactionHooks(t, chat.setup.registry, "observe", &CompactionHooks{BeforeCompact: func(_ context.Context, request CompactionRequest, _ *TaskRuntime) (*CompactionDecision, error) {
		seen = request.Messages
		return nil, nil
	}})
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, chat.compact(t, nil))
	if got := firstUserText(seen); got != "REDACTED" {
		t.Fatalf("hook first user = %q", got)
	}
	if prompt := msgText(chat.faux.summaryRequest(t, 0).messages[1]); !strings.Contains(prompt, "[User]: REDACTED") {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestCompactionCompactsAForkWhoseCutFallsOnAParentEntry(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	entries := chatEntries(t, chat.root)
	fork, err := chat.root.Fork(bg, entries[len(entries)-1].ID, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	id, err := fork.Compact(bg, nil)
	must(t, err)
	if status := submissionWait(t, chat.summarySubmission(t, id)).Status; status != durable.SubmissionDone {
		t.Fatalf("summary = %s", status)
	}
	var u3 durable.EntryRecord
	for _, record := range entries {
		if strings.HasPrefix(textOf(record), "u3") {
			u3 = record
		}
	}
	view, err := fork.Context(bg, nil)
	must(t, err)
	if view.Head == nil || view.Head.Kind != "pi.compaction" || view.Head.Head == nil || *view.Head.Head != u3.ID || view.Head.ConversationID != fork.ID {
		t.Fatalf("head = %+v", view.Head)
	}
	var kept []string
	for _, message := range view.Messages[1:] {
		kept = append(kept, msgText(message))
	}
	if want := []string{sizedText("u3", 100), sizedText("a3", 100)}; !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept = %q", kept)
	}
	// The parent is untouched.
	if hasKind(chat.kinds(t), "pi.compaction") {
		t.Fatal("the parent has a summary")
	}
	// The fork's view keeps the parent entries the summary kept.
	state, err := fork.ViewState(bg)
	must(t, err)
	defer state.Dispose()
	decoded, err := DecodeConversationView(state.Value())
	must(t, err)
	if !reflect.DeepEqual(decoded.Entries, view.Entries) {
		t.Fatalf("view entries = %+v, want %+v", decoded.Entries, view.Entries)
	}
}

func TestCompactionSettlesStaleInAForkResetWhileItSummarizes(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	entries := chatEntries(t, chat.root)
	fork, err := chat.root.Fork(bg, entries[len(entries)-1].ID, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	step, reached, release := gatedStep(fauxAnswer("SUMMARY"))
	chat.faux.pushSummary(step)
	id, err := fork.Compact(bg, nil)
	must(t, err)
	awaitSignal(t, reached, "the summary request")
	must(t, fork.Reset(bg, nil))
	release()
	record := submissionStatus(t, chat.summarySubmission(t, id))
	if record.Status != durable.SubmissionUnanswered || record.Reason != "stale" {
		t.Fatalf("summary = %+v", record)
	}
	view, err := fork.Context(bg, nil)
	must(t, err)
	if view.Head == nil || view.Head.Kind != durable.ResetEntry.Kind {
		t.Fatalf("head = %+v", view.Head)
	}
}

func TestCompactionPlacesAnOlderQueuedSummaryAndTheCurrentOneTogetherWhenIdleAdmissionDrainsTheInbox(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	step, reached, release := gatedStep(fauxError("bad request"))
	chat.faux.pushAgent(step)
	failed, err := chat.root.Submit(bg, inputDraft("fails"))
	must(t, err)
	awaitSignal(t, reached, "the failing request")
	chat.faux.pushSummary(summaryStep("OLDER"))
	older := chat.summarySubmission(t, chat.compact(t, nil))
	release()
	submissionWait(t, failed)
	// Idle now, with the older summary still queued; the current one queues
	// behind it and a final boundary runs.
	chat.faux.pushSummary(summaryStep("CURRENT"))
	current := chat.summarySubmission(t, chat.compact(t, nil))
	statuses := []string{submissionStatus(t, older).Status, submissionStatus(t, current).Status}
	if !reflect.DeepEqual(statuses, []string{durable.SubmissionDone, durable.SubmissionDone}) {
		t.Fatalf("statuses = %v", statuses)
	}
	if got := firstUserText(chat.context(t).Messages); !strings.Contains(got, "CURRENT") {
		t.Fatalf("first user = %q", got)
	}
}

func TestCompactionPlacesAHooksSummaryAndHoldsWhileWorkTheHookCreatedRuns(t *testing.T) {
	setup := newCompactionSetup(t, 0)
	faux := scriptCompaction(setup)
	release := make(chan struct{})
	child := gatedTask(t, "test.child", release)
	must(t, setup.registry.Install(&Extension{Name: "task:test.child", Tasks: []Task{child}}))
	chat := openCompactionWith(t, compactionOpen{}, setup, faux)
	addCompactionHooks(t, chat.setup.registry, "child", &CompactionHooks{BeforeCompact: func(ctx context.Context, _ CompactionRequest, rt *TaskRuntime) (*CompactionDecision, error) {
		err := chat.h.Commit(ctx, func(tx *durable.Tx) error {
			_, err := CreateTask[any](tx, child, nil, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipTask, TaskID: rt.TaskID}, ConversationID: chat.root.ID})
			return err
		})
		return &CompactionDecision{Summary: "HOOK"}, err
	}})
	chat.history(t)
	id := chat.compact(t, nil)
	eventually(t, func() bool { return getTask(t, chat.h, id).State.Status == durable.TaskCompleting })
	// The summary and the status removal landed at the hold.
	if kinds := chat.kinds(t); kinds[len(kinds)-1] != "pi.compaction" {
		t.Fatalf("kinds = %v", kinds)
	}
	if _, ok := chat.live(t)["compactions"]; ok {
		t.Fatal("compactions are still listed")
	}
	close(release)
	chat.result(t, id)
}

// brokenModels is a model access whose GetModel panics.
type brokenModels struct{ ai.Models }

func (brokenModels) GetModel(string, string) *ai.Model { panic(errors.New("models broke")) }

func TestCompactionRemovesTheStatusOfAFaultedCompaction(t *testing.T) {
	setup := newCompactionSetup(t, 0)
	faux := scriptCompaction(setup)
	var broken atomic.Bool
	setup.modelsFunc = func() ai.Models {
		if broken.Load() {
			return brokenModels{setup.models}
		}
		return setup.models
	}
	chat := openCompactionWith(t, compactionOpen{}, setup, faux)
	chat.history(t)
	broken.Store(true)
	outcome := chat.outcome(t, chat.compact(t, nil))
	broken.Store(false)
	if outcome.Status != durable.OutcomeFaulted || outcome.Error == nil || outcome.Error.Message != "models broke" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if _, ok := chat.live(t)["compactions"]; ok {
		t.Fatal("compactions are still listed")
	}
}

// queuedSummary is a summary placed through the inbox while busy.
func (c *compactionChat) queuedSummary(t *testing.T, content string) *Submission {
	t.Helper()
	c.faux.pushSummary(summaryStep(content))
	return c.summarySubmission(t, c.compact(t, nil))
}

// busy starts a run whose answer waits for release.
func (c *compactionChat) busy(t *testing.T, reply *ai.AssistantMessage) (*Submission, func()) {
	t.Helper()
	step, reached, release := gatedStep(reply)
	c.faux.pushAgent(step)
	input, err := c.root.Submit(bg, inputDraft("busy"))
	must(t, err)
	awaitSignal(t, reached, "the busy request")
	return input, release
}

func TestCompactionPlacesAResetQueuedAfterTheSummaryLastAndMakesASummaryQueuedAfterAResetStale(t *testing.T) {
	for _, order := range []string{"summary first", "reset first"} {
		t.Run(order, func(t *testing.T) {
			chat := openCompaction(t, compactionOpen{})
			chat.history(t)
			input, release := chat.busy(t, fauxAnswer("done"))
			var submission *Submission
			if order == "summary first" {
				submission = chat.queuedSummary(t, "SUMMARY")
				must(t, chat.root.Reset(bg, nil))
			} else {
				must(t, chat.root.Reset(bg, nil))
				submission = chat.queuedSummary(t, "SUMMARY")
			}
			release()
			submissionWait(t, input)
			want := durable.SubmissionDone
			if order == "reset first" {
				want = durable.SubmissionUnanswered
			}
			if status := submissionWait(t, submission).Status; status != want {
				t.Fatalf("summary = %s, want %s", status, want)
			}
			if kinds := chat.kinds(t); kinds[len(kinds)-1] != durable.ResetEntry.Kind {
				t.Fatalf("kinds = %v", kinds)
			}
			if head := chat.context(t).Head; head == nil || head.Kind != durable.ResetEntry.Kind {
				t.Fatalf("head = %+v", head)
			}
		})
	}
}

func TestCompactionPlacesASummaryLeftQueuedByAFailedRunAtTheNextSubmissionBeforeItsInput(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	input, release := chat.busy(t, fauxError("bad request"))
	submission := chat.queuedSummary(t, "SUMMARY")
	release()
	if status := submissionWait(t, input).Status; status != durable.SubmissionUnanswered {
		t.Fatalf("input = %s", status)
	}
	if status := submissionStatus(t, submission).Status; status != durable.SubmissionQueued {
		t.Fatalf("summary = %s, want queued", status)
	}
	chat.turn(t, "again", "ok")
	if status := submissionStatus(t, submission).Status; status != durable.SubmissionDone {
		t.Fatalf("summary = %s", status)
	}
	request := chat.faux.lastAgentRequest(t).messages
	if !strings.Contains(firstUserText(request), "SUMMARY") {
		t.Fatalf("first user = %q", firstUserText(request))
	}
	found := false
	for _, message := range request {
		found = found || msgText(message) == "again"
	}
	if !found {
		t.Fatal("the input is not in the request")
	}
}

// spec §12.
func TestCompactionLosesAnApplicationEditPlacedWhileACompactionSummarizes(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	u1 := chatEntries(t, chat.root)[0]
	step, reached, release := gatedStep(fauxAnswer("SUMMARY"))
	chat.faux.pushSummary(step)
	id := chat.compact(t, nil)
	awaitSignal(t, reached, "the summary request")
	_, err := chat.root.Submit(bg, redactDraft(u1.ID))
	must(t, err)
	release()
	chat.result(t, id)
	// The summary was made from the unredacted entry, and the edit's target
	// left the range.
	if prompt := msgText(chat.faux.summaryRequest(t, 0).messages[1]); !strings.Contains(prompt, "[User]: u1 ") {
		t.Fatalf("prompt = %q", prompt)
	}
	for _, message := range chat.context(t).Messages {
		if msgText(message) == "REDACTED" {
			t.Fatal("the edit is in the context")
		}
	}
}

func TestCompactionKeepsTheKeptEntriesMountedInTheConversationView(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	state, err := chat.root.ViewState(bg)
	must(t, err)
	defer state.Dispose()
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, chat.compact(t, nil))
	entries := func() []durable.EntryRecord {
		decoded, err := DecodeConversationView(state.Value())
		must(t, err)
		return decoded.Entries
	}
	eventually(t, func() bool {
		current := entries()
		return len(current) > 0 && current[0].Kind == "pi.compaction"
	})
	if got, want := entries(), chat.context(t).Entries; !reflect.DeepEqual(got, want) {
		t.Fatalf("view entries = %+v, want %+v", got, want)
	}
}

func TestCompactionOrdersTheEventsOfASummaryPlacedAtOnce(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	_, log := listen(t, chat.root)
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, chat.compact(t, nil))
	eventually(t, func() bool { return strings.Contains(strings.Join(eventTypes(log.events()), " "), "compaction_end") })
	types := eventTypes(log.events())
	end := -1
	for i, kind := range types {
		if kind == "compaction_end" {
			end = i
			break
		}
	}
	if end < 2 || end+3 > len(types) {
		t.Fatalf("events = %v", types)
	}
	if got, want := types[end-2:end+3], []string{"message_start", "message_end", "compaction_end", "submission", "usage_changed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("batch = %v", got)
	}
}

func TestCompactionSummarizesThePreviousSummaryInASecondCompaction(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	chat.faux.pushSummary(summaryStep("FIRST"))
	chat.result(t, chat.compact(t, nil))
	chat.turn(t, sizedText("u4", 100), sizedText("a4", 100))
	chat.turn(t, sizedText("u5", 100), sizedText("a5", 100))
	chat.faux.pushSummary(summaryStep("SECOND"))
	chat.result(t, chat.compact(t, nil))
	prompt := msgText(chat.faux.summaryRequest(t, 1).messages[1])
	if !strings.HasPrefix(prompt, "<conversation>\n[User]: The conversation history before this point was compacted") || !strings.Contains(prompt, "FIRST") {
		t.Fatalf("prompt = %q", prompt)
	}
	if got := firstUserText(chat.context(t).Messages); !strings.Contains(got, "SECOND") {
		t.Fatalf("first user = %q", got)
	}
}

func TestCompactionLeavesNoUsageSubmissionOrSummaryWhenTheClassifyingCommitFails(t *testing.T) {
	storage := newControlledStorage()
	chat := openCompaction(t, compactionOpen{storage: storage})
	chat.history(t)
	before := chat.modelUsage(t)
	failure := errors.New("disk gone")
	chat.faux.pushSummary(func(ctx context.Context, transcript ai.TranscriptContext, options *ai.SimpleStreamOptions, state *providers.FauxState, model *ai.Model) *ai.AssistantMessage {
		storage.failNextCommit(failure)
		return fauxAnswer("SUMMARY")
	})
	id := chat.compact(t, nil)
	// It fails the Harness.
	expectEnd(t, chat.h, "failed", failure)
	must(t, chat.h.Close(bg))
	// Nothing of the failed commit landed: the task is still live, without
	// usage, entry, or submission.
	reopened := storage.reopen()
	if found, err := reopened.SubmissionByRequest(bg, chat.root.ID, "compaction:"+itoa(id)); err != nil || found != nil {
		t.Fatalf("summary submission = %+v, %v", found, err)
	}
	if record, err := reopened.Task(bg, id); err != nil || record == nil || record.State.Status == durable.TaskTerminal {
		t.Fatalf("task = %+v, %v", record, err)
	}
	h, root := openChat(t, reopened, chat.setup)
	after := &compactionChat{h: h, root: root, setup: chat.setup, faux: chat.faux}
	if usage := after.modelUsage(t); !reflect.DeepEqual(usage, before) {
		t.Fatalf("usage = %+v, want %+v", usage, before)
	}
	if hasKind(after.kinds(t), "pi.compaction") {
		t.Fatal("the failed commit wrote a summary")
	}
}

func TestCompactionKeepsThePinnedModelThroughARetryAndAdvancesTheLiveAttempt(t *testing.T) {
	setup := newChatSetupWith(t, providers.RegisterFauxProviderOptions{Models: []providers.FauxModelDefinition{
		{ID: "faux-1", ContextWindow: 100_000, MaxTokens: 900},
		{ID: "faux-2", ContextWindow: 100_000, MaxTokens: 900},
	}})
	faux := scriptCompaction(setup)
	chat := openCompactionWith(t, compactionOpen{}, setup, faux)
	chat.history(t)
	var attempt any
	chat.faux.pushSummary(func(context.Context, ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
		// Switched during the attempt: the retry still uses the pinned model.
		must(t, chat.root.Configure(bg, AgentChange{Model: Set(ModelRef{Provider: "faux", ModelID: "faux-2"})}))
		return fauxError("overloaded")
	}, func(context.Context, ai.TranscriptContext, *ai.SimpleStreamOptions, *providers.FauxState, *ai.Model) *ai.AssistantMessage {
		if statuses, _ := chat.live(t)["compactions"].([]any); len(statuses) > 0 {
			attempt = statuses[0].(map[string]any)["attempt"]
		}
		return fauxAnswer("SUMMARY")
	})
	chat.result(t, chat.compact(t, nil))
	models := []string{chat.faux.summaryRequest(t, 0).model, chat.faux.summaryRequest(t, 1).model}
	if !reflect.DeepEqual(models, []string{"faux-1", "faux-1"}) {
		t.Fatalf("models = %v", models)
	}
	if attempt != float64(2) {
		t.Fatalf("live attempt = %v, want 2", attempt)
	}
	value, err := chat.h.Snapshot(bg, UsageDoc.Conversation(chat.root.ID))
	must(t, err)
	var usage UsageState
	must(t, fromValue(value, &usage))
	if len(usage.Models) != 1 {
		t.Fatalf("usage models = %v", usage.Models)
	}
	if _, ok := usage.Models["faux/faux-1"]; !ok {
		t.Fatalf("usage models = %v", usage.Models)
	}
}

func TestCompactionShowsALateJoinerACompactionThatIsSummarizing(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	chat.history(t)
	never, reached := unanswered()
	chat.faux.pushSummary(never)
	id := chat.compact(t, nil)
	awaitSignal(t, reached, "the summary request")
	stream, err := chat.root.WatchEvents(bg)
	must(t, err)
	if want := []CompactionStatus{{TaskID: id, Reason: CompactionManual, Attempt: 1}}; !reflect.DeepEqual(stream.Snapshot.Compactions, want) {
		t.Fatalf("compactions = %+v", stream.Snapshot.Compactions)
	}
	stream.Stop()
	_, err = chat.h.AbortTask(bg, id)
	must(t, err)
}

func TestCompactionBlockedSurvivesReopenBlockedAndIsOrphanedOnAbortWithItsStatusRemoved(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	h, root := openChat(t, openJSONL(t, dir), setup)
	// A compaction stored by a newer version than this process registers, for
	// example after a downgrade.
	newer := versioned(t, "pi.compaction", 2, nil, nil)
	var id durable.TaskID
	must(t, root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		id, err = CreateTask(tx, newer, CompactionInput{Reason: CompactionManual}, durable.TaskOptions{Ownership: durable.TaskOwnership{Kind: durable.OwnershipConversation}})
		if err != nil {
			return err
		}
		live, err := liveDraft(tx, root.ID)
		if err != nil {
			return err
		}
		return setDraftField(live, "compactions", []CompactionStatus{{TaskID: id, Reason: CompactionManual, Attempt: 1}})
	}))
	must(t, h.Close(bg))
	h, root = openChat(t, openJSONL(t, dir), setup)
	must(t, h.Resume())
	scriptCompaction(setup)
	_, log := listen(t, root)
	inspection, err := h.Inspect(bg)
	must(t, err)
	expectInspection(t, inspectionOf(inspection, id), "blocked", false, blockedTaskTooOld, "")
	_, err = h.AbortTask(bg, id)
	must(t, err)
	record, err := h.WaitForTask(bg, id)
	must(t, err)
	if outcome := record.State.Outcome; outcome == nil || outcome.Status != durable.OutcomeOrphaned || outcome.Reason != "task_too_old" {
		t.Fatalf("outcome = %+v", outcome)
	}
	live, err := h.Snapshot(bg, LiveDoc.Conversation(root.ID))
	must(t, err)
	if _, ok := live["compactions"]; ok {
		t.Fatal("compactions are still listed")
	}
	eventually(t, func() bool { return strings.Contains(strings.Join(eventTypes(log.events()), " "), "compaction_end") })
}

func TestCompactionContextContributionsApplyEditsCarriedByAnOlderHeadMarkerInTheRange(t *testing.T) {
	chat := openCompaction(t, compactionOpen{})
	var a, b, c durable.EntryID
	must(t, chat.root.Commit(bg, func(tx *durable.Tx) error {
		note := func(text string) (durable.EntryID, error) {
			record, err := tx.AppendEntry(chat.root.ID, durable.EntryDraft{Kind: "app.note", Model: []ai.Message{user(text)}})
			return record.ID, err
		}
		var err error
		if a, err = note("a"); err != nil {
			return err
		}
		if b, err = note("b"); err != nil {
			return err
		}
		// An older marker that omits b, then a newer one whose range still
		// contains the older marker.
		if _, err = tx.AppendEntry(chat.root.ID, durable.EntryDraft{Kind: "app.head", Head: &a, Edits: []durable.ContextEdit{{Target: b, Action: durable.ContextEditOmit}}}); err != nil {
			return err
		}
		if c, err = note("c"); err != nil {
			return err
		}
		_, err = tx.AppendEntry(chat.root.ID, durable.EntryDraft{Kind: "app.head", Head: &a, Model: []ai.Message{user("H")}})
		return err
	}))
	view := chat.context(t)
	var ids []durable.EntryID
	for _, record := range view.Entries[1:] {
		ids = append(ids, record.ID)
	}
	if !reflect.DeepEqual(ids, []durable.EntryID{a, b, c}) {
		t.Fatalf("entries = %v", ids)
	}
	var contributions [][]string
	for _, messages := range view.Contributions {
		texts := []string{}
		for _, message := range messages {
			texts = append(texts, msgText(message))
		}
		contributions = append(contributions, texts)
	}
	if want := [][]string{{"H"}, {"a"}, {}, {"c"}}; !reflect.DeepEqual(contributions, want) {
		t.Fatalf("contributions = %q", contributions)
	}
	// The summarizer sees the same contributions: the omitted entry stays out.
	chat.setup.setSettings(manualPolicy(1))
	chat.faux.pushSummary(summaryStep("SUMMARY"))
	chat.result(t, chat.compact(t, nil))
	if prompt := msgText(chat.faux.summaryRequest(t, 0).messages[1]); !strings.Contains(prompt, "<conversation>\n[User]: H\n\n[User]: a\n</conversation>") {
		t.Fatalf("prompt = %q", prompt)
	}
}

func itoa(id durable.TaskID) string { return strconv.FormatInt(int64(id), 10) }
