package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-inbox.test.ts (snapshot
// 1402973a). The cases that run tools (steers at the postTools boundary, a
// reset or handoff after tools, a terminating round, tool usage) wait for
// slice 2 of the port; the partial converted after reopen waits for the
// generation recovery of slice 1 (d).

// queuedNote is a write that affects model context, so it queues while busy;
// a data-only write would be appended at once.
func queuedNote(text string) SubmissionDraft {
	return SubmissionDraft{Type: durable.SubmissionTypeWrite, Write: &durable.EntryDraft{Kind: "note", Model: []ai.Message{ai.NewUserText(text, 0)}}}
}

// headWrite is a write of kind whose head targets an entry.
func headWrite(kind string, head durable.EntryID) SubmissionDraft {
	return SubmissionDraft{Type: durable.SubmissionTypeWrite, Write: &durable.EntryDraft{Kind: kind, Head: &head}}
}

func followUp(text string) SubmissionDraft {
	draft := inputDraft(text)
	draft.Input.WhenBusy = WhenBusyFollowUp
	return draft
}

func steer(text string) SubmissionDraft {
	draft := inputDraft(text)
	draft.Input.WhenBusy = WhenBusySteer
	return draft
}

// transcript is the kind and text of each entry, skipping system entries.
func transcript(t *testing.T, conversation *Conversation) []string {
	t.Helper()
	var out []string
	for _, entry := range chatEntries(t, conversation) {
		if entry.Kind == durable.SystemEntry.Kind {
			continue
		}
		if text := textOf(entry); text != "" {
			out = append(out, entry.Kind+":"+text)
		} else {
			out = append(out, entry.Kind)
		}
	}
	return out
}

func assistantIDs(t *testing.T, conversation *Conversation) []durable.EntryID {
	t.Helper()
	var out []durable.EntryID
	for _, entry := range chatEntries(t, conversation) {
		if durable.AssistantEntry.Is(&entry) {
			out = append(out, entry.ID)
		}
	}
	return out
}

// inboxItems is [id, mode] of each pi.inbox item.
func inboxView(t *testing.T, h *Harness, conversation *Conversation) []string {
	t.Helper()
	value, err := h.Snapshot(bg, InboxDoc.Conversation(conversation.ID))
	must(t, err)
	items, _ := value["items"].([]any)
	out := []string{}
	for _, item := range items {
		fields := item.(map[string]any)
		out = append(out, fmt.Sprintf("%v:%v", fields["id"], fields["mode"]))
	}
	return out
}

func item(id durable.SubmissionID, mode string) string { return fmt.Sprintf("%d:%s", id, mode) }

func submissionStatus(t *testing.T, submission *Submission) durable.SubmissionRecord {
	t.Helper()
	record, err := submission.Status(bg)
	must(t, err)
	return record
}

func waitFor(t *testing.T, submission *Submission) durable.SubmissionRecord {
	t.Helper()
	record, err := submission.Wait(bg)
	must(t, err)
	return record
}

func expectDone(t *testing.T, name string, record durable.SubmissionRecord, answer *durable.EntryID) {
	t.Helper()
	if record.Status != durable.SubmissionDone {
		t.Fatalf("%s = %+v, want done", name, record)
	}
	if answer != nil && (record.Answer == nil || *record.Answer != *answer) {
		t.Fatalf("%s answer = %v, want %d", name, record.Answer, *answer)
	}
}

func expectUnanswered(t *testing.T, name string, record durable.SubmissionRecord, reason string) {
	t.Helper()
	if record.Status != durable.SubmissionUnanswered || record.Reason != reason {
		t.Fatalf("%s = %+v, want unanswered %s", name, record, reason)
	}
}

func expectStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

func submit(t *testing.T, conversation *Conversation, draft SubmissionDraft) *Submission {
	t.Helper()
	submission, err := conversation.Submit(bg, draft)
	must(t, err)
	return submission
}

func reset(t *testing.T, conversation *Conversation, handoff *string) {
	t.Helper()
	must(t, conversation.Reset(bg, handoff))
}

func appendPlainNote(t *testing.T, conversation *Conversation) durable.EntryRecord {
	t.Helper()
	var appended durable.EntryRecord
	must(t, conversation.Commit(bg, func(tx *durable.Tx) error {
		var err error
		appended, err = tx.AppendEntry(conversation.ID, durable.EntryDraft{Kind: "note"})
		return err
	}))
	return appended
}

func contextOf(t *testing.T, conversation *Conversation) ContextView {
	t.Helper()
	view, err := conversation.Context(bg, nil)
	must(t, err)
	return view
}

func messagesJSON(t *testing.T, messages []ai.Message) string {
	t.Helper()
	raw, err := json.Marshal(messages)
	must(t, err)
	return string(raw)
}

func callCount(setup *chatSetup) int { return setup.faux.State.CallCount }

// installGenerationHooks registers generation hooks in their own extension.
func installGenerationHooks(t *testing.T, setup *chatSetup, hooks *GenerationHooks) {
	t.Helper()
	must(t, setup.registry.Install(&Extension{Name: "test:generation-hooks", Hooks: []HookRegistration{Hook(GenerationTask, hooks)}}))
}

// continueOnce is an OnYield that continues the first answer with "more".
func continueOnce() func(context.Context, ai.AssistantMessage, *TaskRuntime) (*UserInput, error) {
	var yields atomic.Int32
	return func(context.Context, ai.AssistantMessage, *TaskRuntime) (*UserInput, error) {
		if yields.Add(1) == 1 {
			more := TextInput("more")
			return &more, nil
		}
		return nil, nil
	}
}

// inboxOps records the operations of every non-empty pi.inbox change.
type inboxOps struct {
	mu  sync.Mutex
	ops [][]delta.Op
}

func recordInboxOps(t *testing.T, h *Harness) *inboxOps {
	t.Helper()
	recorded := &inboxOps{}
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			if document, ok := change.(durable.DocumentChange); ok && document.Record.Kind == InboxDoc.Definition().Kind && len(document.Ops) > 0 {
				recorded.mu.Lock()
				recorded.ops = append(recorded.ops, append([]delta.Op(nil), document.Ops...))
				recorded.mu.Unlock()
			}
		}
	})
	must(t, err)
	t.Cleanup(unsubscribe)
	return recorded
}

func (r *inboxOps) all() [][]delta.Op {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]delta.Op(nil), r.ops...)
}

// spliceText is ["p", path, index, remove, items] of a splice, items as JSON.
func spliceText(t *testing.T, op delta.Op) string {
	t.Helper()
	splice, ok := op.(delta.Splice)
	if !ok {
		t.Fatalf("op %T, want a positional splice", op)
	}
	items, err := json.Marshal(splice.Items)
	must(t, err)
	return fmt.Sprintf("p %v %d %d %s", splice.Path, splice.Index, splice.Remove, items)
}

func TestInboxQueuesBusySubmissionsAndPlacesWritesBeforeUserItemsOneFollowUpPerRun(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("second"), fauxText("third")})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	f1 := submit(t, root, inputDraft("f1"))
	write := submit(t, root, queuedNote("w"))
	f2 := submit(t, root, followUp("f2"))
	for _, submission := range []*Submission{f1, write, f2} {
		if got := submissionStatus(t, submission).Status; got != durable.SubmissionQueued {
			t.Fatalf("submission %d = %s, want queued", submission.ID, got)
		}
	}
	expectStrings(t, "inbox", inboxView(t, h, root), []string{item(f1.ID, "followUp"), item(write.ID, "write"), item(f2.ID, "followUp")})
	expectStrings(t, "transcript", transcript(t, root), []string{"pi.user:a"})

	release()
	waitFor(t, f2)
	expectStrings(t, "transcript", transcript(t, root), []string{
		"pi.user:a", "pi.assistant:first", "note:w", "pi.user:f1", "pi.assistant:second", "pi.user:f2", "pi.assistant:third",
	})
	answers := assistantIDs(t, root)
	expectDone(t, "input", submissionStatus(t, input), &answers[0])
	expectDone(t, "write", submissionStatus(t, write), nil)
	expectDone(t, "f1", submissionStatus(t, f1), &answers[1])
	expectDone(t, "f2", submissionStatus(t, f2), &answers[2])
	expectStrings(t, "inbox", inboxView(t, h, root), []string{})
	live, err := h.Snapshot(bg, LiveDoc.Conversation(root.ID))
	must(t, err)
	if len(live) != 0 {
		t.Fatalf("pi.live = %v, want {}", live)
	}
}

// expectSameSettlement is pi's toEqual({...settled, id, entry: any}): the
// settlement of one placed together with other.
func expectSameSettlement(t *testing.T, got, other durable.SubmissionRecord) {
	t.Helper()
	if got.Status != other.Status || got.Type != other.Type || got.Reason != other.Reason || !reflect.DeepEqual(got.Answer, other.Answer) || got.Entry == nil {
		t.Fatalf("settlement = %+v, want that of %+v", got, other)
	}
}

func TestInboxPlacesEveryFollowUpInOneSuccessorRunWithFollowUpModeAll(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("both")})
	_, root := openChat(t, memory.New(), setup)
	setup.setSettings(func(s *HarnessSettings) { s.FollowUpMode = QueueAll })
	submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	f1 := submit(t, root, inputDraft("f1"))
	f2 := submit(t, root, inputDraft("f2"))
	release()
	settled := waitFor(t, f2)
	expectSameSettlement(t, submissionStatus(t, f1), settled)
	expectStrings(t, "transcript", transcript(t, root), []string{"pi.user:a", "pi.assistant:first", "pi.user:f1", "pi.user:f2", "pi.assistant:both"})
	if got := callCount(setup); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestInboxReadsQueueModesWhenTheFinalBoundaryCommitRunsOnTheSessionLine(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("both")})
	yielded := make(chan struct{})
	var yieldOnce sync.Once
	installGenerationHooks(t, setup, &GenerationHooks{OnYield: func(context.Context, ai.AssistantMessage, *TaskRuntime) (*UserInput, error) {
		yieldOnce.Do(func() { close(yielded) })
		return nil, nil
	}})
	storage := newControlledStorage()
	_, root := openChat(t, storage, setup)
	submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	f1 := submit(t, root, inputDraft("f1"))
	f2 := submit(t, root, inputDraft("f2"))
	// Occupy the line, let the answer queue its boundary commit behind it,
	// then change the mode.
	held := storage.holdCommits()
	marker := durable.MustDefineDoc(durable.DocDef{Kind: "test.marker", Version: 1, Scope: durable.ScopeSession, Initial: func(any) map[string]any { return map[string]any{"n": float64(0)} }})
	occupying := async(func() (struct{}, error) {
		return struct{}{}, root.Commit(bg, func(tx *durable.Tx) error {
			doc, err := tx.Doc(marker.Session())
			if err != nil {
				return err
			}
			n, _ := doc.Get("n")
			return doc.Set("n", n.(float64)+1)
		})
	})
	held.Entered(t)
	release()
	awaitSignal(t, yielded, "onYield")
	flush()
	setup.setSettings(func(s *HarnessSettings) { s.FollowUpMode = QueueAll })
	storage.releaseHold(held)
	_, err := await(t, occupying)
	must(t, err)
	settled := waitFor(t, f2)
	expectSameSettlement(t, submissionStatus(t, f1), settled)
	if got := callCount(setup); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestInboxPlacesAQueuedResetAfterTheAnswerAtTheFinalBoundary(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	reset(t, root, ptr("handoff"))
	release()
	waitFor(t, input)
	must(t, h.WaitForIdle(bg))
	expectDone(t, "input", submissionStatus(t, input), nil)
	expectStrings(t, "transcript", transcript(t, root), []string{"pi.user:a", "pi.assistant:first", "pi.reset:handoff"})
	view := contextOf(t, root)
	if len(view.Messages) != 1 {
		t.Fatalf("messages = %s, want the handoff alone", messagesJSON(t, view.Messages))
	}
	user, ok := view.Messages[0].(ai.UserMessage)
	if !ok || textOfContent(user.Content) != "handoff" || user.Timestamp == 0 {
		t.Fatalf("message = %s, want the user handoff with a timestamp", messagesJSON(t, view.Messages))
	}
}

func textOfContent(content ai.ContentList) string {
	for _, block := range content {
		if text, ok := block.(ai.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

func TestInboxResetsAnIdleConversationAtOnceWithOrWithoutHandoffText(t *testing.T) {
	setup := newChatSetup(t)
	setup.setNow(func() time.Time { return time.UnixMilli(7) })
	_, root := openChat(t, memory.New(), setup)
	appendPlainNote(t, root)
	reset(t, root, nil)
	view := contextOf(t, root)
	if view.Head == nil || view.Head.Kind != "pi.reset" || view.Head.Model != nil || len(view.Messages) != 0 {
		t.Fatalf("after a plain reset: head %+v, messages %s", view.Head, messagesJSON(t, view.Messages))
	}
	reset(t, root, ptr("carry on"))
	view = contextOf(t, root)
	if view.Head == nil || view.Head.Head == nil || *view.Head.Head != view.Head.ID {
		t.Fatalf("head = %+v, want a marker heading itself", view.Head)
	}
	if got := messagesJSON(t, view.Messages); got != `[{"role":"user","content":"carry on","timestamp":7}]` {
		t.Fatalf("messages = %s", got)
	}
}

func TestInboxMakesAQueuedHeadWriteStaleWhenItTargetsAnEntryBeforeTheActiveRange(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("second")})
	h, root := openChat(t, memory.New(), setup)
	old := appendPlainNote(t, root)
	reset(t, root, nil)
	resetEntry := contextOf(t, root).Head
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	stale := submit(t, root, headWrite("summary", old.ID))
	fresh := submit(t, root, headWrite("summary", resetEntry.ID))
	release()
	waitFor(t, input)
	expectUnanswered(t, "stale", submissionStatus(t, stale), "stale")
	expectDone(t, "fresh", submissionStatus(t, fresh), nil)
	expectStrings(t, "inbox", inboxView(t, h, root), []string{})

	// The fresh summary's marker starts the range at the reset: a target
	// inside the range is not stale, even when it is older than the marker
	// itself. A reset placed earlier in the same boundary makes it stale.
	var inside durable.EntryID
	for _, entry := range chatEntries(t, root) {
		if durable.UserEntry.Is(&entry) {
			inside = entry.ID
			break
		}
	}
	second := submit(t, root, inputDraft("b"))
	kept := submit(t, root, headWrite("summary", inside))
	waitFor(t, second)
	expectDone(t, "kept", submissionStatus(t, kept), nil)
}

func TestInboxMakesAHeadWriteStaleBehindAResetPlacedEarlierInTheSameBoundary(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first})
	_, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	target := chatEntries(t, root)[0]
	reset(t, root, nil)
	summary := submit(t, root, headWrite("summary", target.ID))
	release()
	waitFor(t, input)
	expectUnanswered(t, "summary", submissionStatus(t, summary), "stale")
}

// Beyond pi: no pi case places two head writes at one boundary. The first
// moves the start of the active range to its head, so the second, targeting
// an entry before that head, is stale, though the newest marker before the
// boundary (none here) would let it in.
func TestInboxMakesAHeadWriteStaleBehindAHeadWritePlacedEarlierInTheSameBoundary(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first})
	_, root := openChat(t, memory.New(), setup)
	older := appendPlainNote(t, root)
	newer := appendPlainNote(t, root)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	advancing := submit(t, root, headWrite("summary", newer.ID))
	behind := submit(t, root, headWrite("summary", older.ID))
	release()
	waitFor(t, input)
	expectDone(t, "advancing", submissionStatus(t, advancing), nil)
	expectUnanswered(t, "behind", submissionStatus(t, behind), "stale")
}

func TestInboxDropsAnOnYieldContinuationWhenTheFinalBoundarySelectsAFollowUp(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("second")})
	installGenerationHooks(t, setup, &GenerationHooks{OnYield: continueOnce()})
	_, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	f := submit(t, root, inputDraft("f"))
	release()
	waitFor(t, f)
	expectDone(t, "input", submissionStatus(t, input), nil)
	expectStrings(t, "transcript", transcript(t, root), []string{"pi.user:a", "pi.assistant:first", "pi.user:f", "pi.assistant:second"})
}

func TestInboxWithdrawsAQueuedSubmissionAndRemovesItsItem(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	kept := submit(t, root, queuedNote("kept"))
	withdrawn := submit(t, root, inputDraft("f"))
	result, err := withdrawn.Abort(bg)
	must(t, err)
	if result != SubmissionAborted {
		t.Fatalf("abort = %s, want aborted", result)
	}
	expectUnanswered(t, "withdrawn", waitFor(t, withdrawn), "aborted")
	expectStrings(t, "inbox", inboxView(t, h, root), []string{item(kept.ID, "write")})
	release()
	waitFor(t, input)
	expectDone(t, "kept", submissionStatus(t, kept), nil)
	if got := callCount(setup); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestInboxLeavesQueuedItemsAfterAFailedRunUntilTheNextSubmissionPlacesThemInOrder(t *testing.T) {
	setup := newChatSetup(t)
	failing, reached, release := gatedStep(fauxError("invalid request"))
	setup.faux.SetResponses([]providers.FauxResponseStep{failing, fauxText("for f"), fauxText("for g")})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the failing request")
	f := submit(t, root, inputDraft("f"))
	release()
	expectUnanswered(t, "input", waitFor(t, input), "model_error")
	must(t, h.WaitForIdle(bg))
	if got := submissionStatus(t, f).Status; got != durable.SubmissionQueued {
		t.Fatalf("f = %s, want queued", got)
	}
	expectStrings(t, "inbox", inboxView(t, h, root), []string{item(f.ID, "followUp")})

	// Idle with a queued item: the new input queues behind it, and a final
	// boundary places the older one first.
	draft := inputDraft("g")
	draft.Input.WhenBusy = WhenBusyReject
	g := submit(t, root, draft)
	waitFor(t, g)
	expectDone(t, "f", submissionStatus(t, f), nil)
	all := transcript(t, root)
	expectStrings(t, "transcript tail", all[len(all)-4:], []string{"pi.user:f", "pi.assistant:for f", "pi.user:g", "pi.assistant:for g"})
}

// pi reopens SQLite here; Go's root module reopens JSONL (decision 17.10.2026
// for close/reopen cases), which catches the same serialization losses.
func TestInboxKeepsQueuedSubmissionsAcrossReopenAndSettlesThemAfterwards(t *testing.T) {
	directory := t.TempDir()
	setup := newChatSetup(t)
	first, reached, _ := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("first again"), fauxText("f")})
	h, root := openChat(t, openJSONL(t, directory), setup)
	submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	f := submit(t, root, inputDraft("f")).ID
	must(t, h.Close(bg))

	h, root = openChat(t, openJSONL(t, directory), setup)
	submission, err := h.Submission(bg, f)
	must(t, err)
	expectDone(t, "f", waitFor(t, submission), nil)
	all := transcript(t, root)
	expectStrings(t, "transcript tail", all[len(all)-2:], []string{"pi.user:f", "pi.assistant:f"})
}

func TestInboxCommitsInboxChangesAsPositionalChordOperationsAndABaseWhenEmpty(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("second")})
	h, root := openChat(t, memory.New(), setup)
	recorded := recordInboxOps(t, h)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	w1 := submit(t, root, queuedNote("w"))
	f1 := submit(t, root, inputDraft("f1"))
	w2 := submit(t, root, queuedNote("w"))
	f2 := submit(t, root, inputDraft("f2"))
	w3 := submit(t, root, queuedNote("w"))
	// The document value is a map: object keys come out sorted.
	note := `{"entry":{"kind":"note","model":[{"content":"w","role":"user","timestamp":0}]},"id":%d,"mode":"write"}`
	followUpItem := `{"content":"%s","id":%d,"mode":"followUp"}`
	want := []string{
		fmt.Sprintf("p [\"items\"] 0 0 ["+note+"]", w1.ID),
		fmt.Sprintf("p [\"items\"] 1 0 ["+followUpItem+"]", "f1", f1.ID),
		fmt.Sprintf("p [\"items\"] 2 0 ["+note+"]", w2.ID),
		fmt.Sprintf("p [\"items\"] 3 0 ["+followUpItem+"]", "f2", f2.ID),
		fmt.Sprintf("p [\"items\"] 4 0 ["+note+"]", w3.ID),
	}
	ops := recorded.all()
	if len(ops) != len(want) {
		t.Fatalf("inbox changes = %d, want %d", len(ops), len(want))
	}
	for i, change := range ops {
		if len(change) != 1 {
			t.Fatalf("change %d has %d ops, want 1", i, len(change))
		}
		if got := spliceText(t, change[0]); got != want[i] {
			t.Fatalf("change %d = %s, want %s", i, got, want[i])
		}
	}
	release()
	waitFor(t, input)
	// Every write and the first follow-up leave; only f2 at index 3 remains.
	// No retained value is carried. Unlike pi, where the wait resolves in a
	// microtask after every listener, the Session wakes the wait before host
	// listeners (ObserveCommits runs first): wait for the change itself.
	eventually(t, func() bool { return len(recorded.all()) >= 6 })
	removal := recorded.all()[5]
	sawLast := false
	for _, op := range removal {
		splice, ok := op.(delta.Splice)
		if !ok || len(splice.Items) != 0 {
			t.Fatalf("boundary op %s, want removals only", spliceText(t, op))
		}
		if splice.Index == 4 && splice.Remove == 1 {
			sawLast = true
		}
	}
	if !sawLast {
		t.Fatalf("boundary ops %v lack [p [\"items\"] 4 1 []]", removal)
	}
	if raw, _ := json.Marshal(removal); strings.Contains(string(raw), "f2") {
		t.Fatalf("boundary ops carry f2: %s", raw)
	}
	waitFor(t, f2)
}

func TestInboxSettlesAStaleWriteAtOnceWhileIdleAndAQueuedOneBehindWaitingItems(t *testing.T) {
	setup := newChatSetup(t)
	failing, reached, release := gatedStep(fauxError("invalid request"))
	setup.faux.SetResponses([]providers.FauxResponseStep{failing, fauxText("for f")})
	h, root := openChat(t, memory.New(), setup)
	old := appendPlainNote(t, root)
	reset(t, root, nil)
	before := len(chatEntries(t, root))
	idle := submit(t, root, headWrite("summary", old.ID))
	expectUnanswered(t, "idle", waitFor(t, idle), "stale")
	if got := len(chatEntries(t, root)); got != before {
		t.Fatalf("entries = %d, want %d", got, before)
	}

	// After a failed run, a follow-up waits; a stale write queues behind it
	// and the boundary rejects it.
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the failing request")
	f := submit(t, root, inputDraft("f"))
	release()
	waitFor(t, input)
	must(t, h.WaitForIdle(bg))
	queued := submit(t, root, headWrite("summary", old.ID))
	expectUnanswered(t, "queued", waitFor(t, queued), "stale")
	expectDone(t, "f", waitFor(t, f), nil)
}

func TestInboxKeepsAnOnYieldContinuationAcrossAQueuedPlainWriteWithTheRunsOriginalInput(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("second")})
	installGenerationHooks(t, setup, &GenerationHooks{OnYield: continueOnce()})
	_, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	write := submit(t, root, queuedNote("w"))
	release()
	waitFor(t, input)
	expectStrings(t, "transcript", transcript(t, root), []string{"pi.user:a", "pi.assistant:first", "note:w", "pi.user:more", "pi.assistant:second"})
	entries := chatEntries(t, root)
	expectDone(t, "input", submissionStatus(t, input), &entries[len(entries)-1].ID)
	expectDone(t, "write", submissionStatus(t, write), nil)
}

func TestInboxDropsAnOnYieldContinuationForAQueuedReset(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first})
	installGenerationHooks(t, setup, &GenerationHooks{OnYield: func(context.Context, ai.AssistantMessage, *TaskRuntime) (*UserInput, error) {
		more := TextInput("more")
		return &more, nil
	}})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	reset(t, root, nil)
	release()
	waitFor(t, input)
	must(t, h.WaitForIdle(bg))
	expectDone(t, "input", submissionStatus(t, input), nil)
	expectStrings(t, "transcript", transcript(t, root), []string{"pi.user:a", "pi.assistant:first", "pi.reset"})
	if got := callCount(setup); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestInboxQueuesAnIdleSteerBehindWaitingItemsAndPlacesItWithTheFirstFollowUpInIDOrder(t *testing.T) {
	setup := newChatSetup(t)
	failing, reached, release := gatedStep(fauxError("invalid request"))
	setup.faux.SetResponses([]providers.FauxResponseStep{failing, fauxText("both")})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the failing request")
	f := submit(t, root, inputDraft("f"))
	release()
	waitFor(t, input)
	must(t, h.WaitForIdle(bg))
	steered := submit(t, root, steer("s"))
	settled := waitFor(t, steered)
	expectDone(t, "f", submissionStatus(t, f), settled.Answer)
	all := transcript(t, root)
	expectStrings(t, "transcript tail", all[len(all)-3:], []string{"pi.user:f", "pi.user:s", "pi.assistant:both"})
}

func TestInboxLeavesTheInboxAloneWhenTheRunsTaskIsAborted(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, _ := gatedStep(fauxAnswer("never"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	f := submit(t, root, inputDraft("f"))
	run := liveRunOf(t, h, root.ID)
	if run == nil {
		t.Fatal("no live run")
	}
	_, err := h.AbortTask(bg, run.TaskID)
	must(t, err)
	expectUnanswered(t, "input", waitFor(t, input), "aborted")
	must(t, h.WaitForIdle(bg))
	if got := submissionStatus(t, f).Status; got != durable.SubmissionQueued {
		t.Fatalf("f = %s, want queued", got)
	}
	expectStrings(t, "inbox", inboxView(t, h, root), []string{item(f.ID, "followUp")})
}

func TestInboxReturnsAQueuedSubmissionForItsRepeatedRequestIDWithoutASecondItem(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, _ := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first})
	h, root := openChat(t, memory.New(), setup)
	submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	draft := inputDraft("f")
	draft.RequestID = "r"
	queued := submit(t, root, draft)
	again := submit(t, root, draft)
	if again.ID != queued.ID {
		t.Fatalf("again = %d, want %d", again.ID, queued.ID)
	}
	expectStrings(t, "inbox", inboxView(t, h, root), []string{item(queued.ID, "followUp")})
}

func TestInboxWithdrawsAMiddleItemWithOnePositionalRemoval(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, _ := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first})
	h, root := openChat(t, memory.New(), setup)
	submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	var items []*Submission
	for _, text := range []string{"x", "y", "z"} {
		items = append(items, submit(t, root, inputDraft(text)))
	}
	recorded := recordInboxOps(t, h)
	_, err := items[1].Abort(bg)
	must(t, err)
	ops := recorded.all()
	if len(ops) != 1 || len(ops[0]) != 1 || spliceText(t, ops[0][0]) != "p [\"items\"] 1 1 []" {
		t.Fatalf("ops = %v, want one [p [\"items\"] 1 1 []]", ops)
	}
	expectStrings(t, "inbox", inboxView(t, h, root), []string{item(items[0].ID, "followUp"), item(items[2].ID, "followUp")})
}

// inboxWrites records the content kind of every storage write to one inbox
// document: whether it is a base, and an empty one.
type inboxWrites struct {
	durable.Storage
	mu      sync.Mutex
	inboxID atomic.Int64
	written []string
}

func (s *inboxWrites) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	for _, write := range writes {
		change, ok := write.(durable.DocumentChangeWrite)
		if !ok || int64(change.ID) != s.inboxID.Load() {
			continue
		}
		kind := change.Content.Kind
		if kind == durable.ContentBase {
			items, _ := change.Content.Value["items"].([]any)
			if len(items) == 0 {
				kind += " empty"
			}
		}
		s.mu.Lock()
		s.written = append(s.written, kind)
		s.mu.Unlock()
	}
	return s.Storage.Commit(ctx, writes)
}

func TestInboxStoresTheInboxAsABaseExactlyWhenItBecomesEmpty(t *testing.T) {
	storage := &inboxWrites{Storage: memory.New()}
	storage.inboxID.Store(-1)
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("second")})
	h, root := openChat(t, storage, setup)
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			if document, ok := change.(durable.DocumentChange); ok && document.Record.Kind == InboxDoc.Definition().Kind {
				storage.inboxID.Store(int64(document.Record.ID))
			}
		}
	})
	must(t, err)
	defer unsubscribe()
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the first request")
	submit(t, root, inputDraft("f1"))
	f2 := submit(t, root, inputDraft("f2"))
	release()
	waitFor(t, input)
	waitFor(t, f2)
	// The inbox ID is learned from the f1 push; later writes: the f2 push,
	// removing f1, and emptying the inbox.
	storage.mu.Lock()
	written := append([]string(nil), storage.written...)
	storage.mu.Unlock()
	expectStrings(t, "inbox writes", written, []string{durable.ContentDelta, durable.ContentDelta, durable.ContentBase + " empty"})
}

func TestInboxKeepsACompleteInboxBaseExactlyWhileItIsEmptyAndAUsageBaseOnEveryChange(t *testing.T) {
	info := durable.CheckpointInfo{DeltasSinceBase: 1000}
	inbox := InboxDoc.Definition().CheckpointWhen
	if !inbox(map[string]any{"items": []any{}}, nil, info) {
		t.Fatal("an empty inbox is not a base")
	}
	if inbox(map[string]any{"items": []any{map[string]any{"id": float64(1), "mode": "followUp", "content": "x"}}}, nil, info) {
		t.Fatal("a non-empty inbox is a base")
	}
	if !UsageDoc.Definition().CheckpointWhen(map[string]any{"models": map[string]any{}, "tools": map[string]any{}}, nil, info) {
		t.Fatal("a usage change is not a base")
	}
}

// pi's case keeps tools named like Object.prototype keys; a Go map has no
// prototype, so what applies is that each name keeps its own exact total in
// the ledger and the Session total. Key order is n/a: Go maps are unordered.
func TestUsageKeepsToolsNamedLikeObjectPrototypeKeysInTheLedgerAndTheSessionTotal(t *testing.T) {
	h, root := openChat(t, memory.New(), newChatSetup(t))
	usage := ai.Usage{Input: 1, Output: 1, TotalTokens: 2}
	names := []string{"constructor", "__proto__", "toString"}
	for _, name := range names {
		for range 2 {
			must(t, root.Commit(bg, func(tx *durable.Tx) error { return recordUsage(tx, root.ID, "tools", name, usage) }))
		}
	}
	total, err := h.Usage(bg)
	must(t, err)
	var keys []string
	for key := range total.Tools {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := append([]string(nil), names...)
	sort.Strings(want)
	expectStrings(t, "tools", keys, want)
	for _, name := range names {
		if got := total.Tools[name].Output; got != 2 {
			t.Fatalf("%s output = %d, want 2", name, got)
		}
	}
}

func TestUsageRecordsUsageAsNumericSetsOnTheLedgerInTheEntrysCommit(t *testing.T) {
	setup := newChatSetup(t)
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("one"), fauxText("two")})
	h, root := openChat(t, memory.New(), setup)
	type usageCommit struct {
		entries int
		ops     []delta.Op
	}
	var mu sync.Mutex
	var commits []usageCommit
	unsubscribe, err := h.SubscribeCommits(func(publication durable.CommitPublication, _ context.Context) {
		for _, change := range publication.Changes {
			document, ok := change.(durable.DocumentChange)
			if !ok || document.Record.Kind != UsageDoc.Definition().Kind {
				continue
			}
			entries := 0
			for _, other := range publication.Changes {
				if other.ChangeType() == "entry" {
					entries++
				}
			}
			mu.Lock()
			commits = append(commits, usageCommit{entries: entries, ops: document.Ops})
			mu.Unlock()
		}
	})
	must(t, err)
	defer unsubscribe()
	waitFor(t, submit(t, root, inputDraft("a")))
	waitFor(t, submit(t, root, inputDraft("b")))
	// The wait wakes before host listeners see the answer's commit.
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(commits) >= 2
	})
	flush()
	mu.Lock()
	defer mu.Unlock()
	if len(commits) != 2 {
		t.Fatalf("usage commits = %d, want 2", len(commits))
	}
	isModelSet := func(op delta.Op) bool {
		set, ok := op.(delta.Set)
		return ok && len(set.Path) == 2 && set.Path[0] == delta.Key("models") && set.Path[1] == delta.Key("faux/faux-1")
	}
	if len(commits[0].ops) != 1 || !isModelSet(commits[0].ops[0]) {
		t.Fatalf("first usage ops = %v, want one set of models/faux/faux-1", commits[0].ops)
	}
	if _, ok := commits[0].ops[0].(delta.Set).Value.(map[string]any); !ok {
		t.Fatalf("first set value = %T, want an object", commits[0].ops[0].(delta.Set).Value)
	}
	for _, op := range commits[1].ops {
		if !isModelSet(op) {
			t.Fatalf("second usage op %v, want sets under models/faux/faux-1", op)
		}
	}
	for i, commit := range commits {
		if commit.entries < 1 {
			t.Fatalf("usage commit %d has no entry", i)
		}
	}
}
