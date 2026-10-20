package harness

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-input-entries.test.ts: inputs
// placed as application entries, and data-only writes appended at once. The
// two cases that hold a tool are in input_entries_tools_test.go.

// requestLog records the role and text of each request message.
type requestLog struct {
	mu       sync.Mutex
	requests [][]string
}

func (l *requestLog) step(reply *ai.AssistantMessage) providers.FauxResponseStep {
	return func(_ context.Context, req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		lines := make([]string, len(req.Messages))
		for i, message := range req.Messages {
			lines[i] = string(message.MessageRole()) + ":" + textOf(durable.EntryRecord{Model: []ai.Message{message}})
		}
		l.mu.Lock()
		l.requests = append(l.requests, lines)
		l.mu.Unlock()
		return reply
	}
}

func (l *requestLog) all() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.requests)
}

// transcript is each entry's kind, application data and model text, without
// system entries.
func entryTranscript(entries []durable.EntryRecord) [][]string {
	var out [][]string
	for _, entry := range entries {
		if entry.Kind == durable.SystemEntry.Kind {
			continue
		}
		line := []string{entry.Kind}
		if len(entry.Data) > 0 && !strings.HasPrefix(entry.Kind, "pi.") {
			line = append(line, string(entry.Data))
		}
		if len(entry.Model) > 0 {
			if _, result := entry.Model[0].(ai.ToolResultMessage); !result {
				if text := textOf(entry); text != "" {
					line = append(line, text)
				}
			}
		}
		out = append(out, line)
	}
	return out
}

func inboxValues(t *testing.T, h *Harness, conversation *Conversation) []map[string]any {
	t.Helper()
	value, err := h.Snapshot(bg, InboxDoc.Conversation(conversation.ID))
	must(t, err)
	items, _ := value["items"].([]any)
	out := make([]map[string]any, len(items))
	for i, item := range items {
		out[i], _ = item.(map[string]any)
	}
	return out
}

func inboxIDs(t *testing.T, h *Harness, conversation *Conversation) []durable.SubmissionID {
	return names(inboxValues(t, h, conversation), func(item map[string]any) durable.SubmissionID {
		id, _ := item["id"].(float64)
		return durable.SubmissionID(id)
	})
}

const mentionData = `{"author":"ann","text":"%s"}`

func mention(text string) *InputEntry {
	return &InputEntry{Kind: "chat.mention", Data: json.RawMessage(strings.Replace(mentionData, "%s", text, 1))}
}

func inputWith(text string, entry *InputEntry) SubmissionDraft {
	draft := inputDraft(text)
	draft.Input.Entry = entry
	return draft
}

func writeOf(entry durable.EntryDraft) SubmissionDraft {
	return SubmissionDraft{Type: durable.SubmissionTypeWrite, Write: &entry}
}

func expectEntryTranscript(t *testing.T, conversation *Conversation, want ...[]string) {
	t.Helper()
	if got := entryTranscript(chatEntries(t, conversation)); !reflect.DeepEqual(got, want) {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

func TestInputEntriesPlaceAnIdleInputAsItsEntryTheGivenKindAndDataWithTheContentAsItsUserMessage(t *testing.T) {
	setup := newChatSetup(t)
	setup.setNow(func() time.Time { return time.UnixMilli(42) })
	log := &requestLog{}
	setup.faux.SetResponses([]providers.FauxResponseStep{log.step(fauxAnswer("hi ann"))})
	_, root := openChat(t, memory.New(), setup)
	settled := waitFor(t, submit(t, root, inputWith("ann: hi", mention("hi"))))
	expectEntryTranscript(t, root, []string{"chat.mention", `{"author":"ann","text":"hi"}`, "ann: hi"}, []string{"pi.assistant", "hi ann"})
	entries := chatEntries(t, root)
	placed := entries[slices.IndexFunc(entries, func(entry durable.EntryRecord) bool { return entry.Kind == "chat.mention" })]
	if got := jsonOf(t, placed.Model); got != `[{"role":"user","content":"ann: hi","timestamp":42}]` {
		t.Fatalf("placed model = %s", got)
	}
	if settled.Status != durable.SubmissionDone || settled.Entry == nil || *settled.Entry != placed.ID || settled.Answer == nil || *settled.Answer != entries[len(entries)-1].ID {
		t.Fatalf("settled = %+v, want done with the placed entry and the answer", settled)
	}
	if got := log.all(); !reflect.DeepEqual(got, [][]string{{"user:ann: hi"}}) {
		t.Fatalf("requests = %q", got)
	}
	// The context of a fork at the input is the same as for a pi.user entry.
	view, err := root.Context(bg, &placed.ID)
	must(t, err)
	messages := slices.DeleteFunc(slices.Clone(view.Messages), func(message ai.Message) bool {
		_, system := message.(ai.SystemMessage)
		return system
	})
	if jsonOf(t, messages) != jsonOf(t, placed.Model) {
		t.Fatalf("context at the input = %s, want %s", jsonOf(t, messages), jsonOf(t, placed.Model))
	}
}

func TestInputEntriesRejectAnInputEntryWhoseKindIsEmptyOrStartsWithPiWritingNothing(t *testing.T) {
	h, root := openChat(t, memory.New(), newChatSetup(t))
	before := len(chatEntries(t, root))
	// pi's non-string kind (123) cannot be written in Go.
	for _, kind := range []string{"", "pi.user", "pi.compaction"} {
		if _, err := root.Submit(bg, inputWith("x", &InputEntry{Kind: kind})); err == nil || !strings.Contains(err.Error(), "An input entry's kind must be") {
			t.Fatalf("kind %q: submit = %v, want the kind refused", kind, err)
		}
	}
	if after := len(chatEntries(t, root)); after != before {
		t.Fatalf("entries = %d, want %d", after, before)
	}
	expectIdleLive(t, h, root)
	// Writes keep their kinds: Reset writes pi.reset.
	must(t, root.Reset(bg, nil))
	entries := chatEntries(t, root)
	if kind := entries[len(entries)-1].Kind; kind != durable.ResetEntry.Kind {
		t.Fatalf("last entry = %s, want pi.reset", kind)
	}
}

// pi's case passes model, head and edits in an untyped entry and checks that
// only kind and data are taken; InputEntry has no other fields, so here it
// checks how the entry is queued and placed.
func TestInputEntriesQueueAndPlaceOnlyTheKindAndDataOfAnInputEntry(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{fauxText("idle"), first, fauxText("queued")})
	h, root := openChat(t, memory.New(), setup)
	entry := &InputEntry{Kind: "chat.mention"}
	waitFor(t, submit(t, root, inputWith("idle", entry)))
	submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the request")
	queued := submit(t, root, inputWith("later", entry))
	items := inboxValues(t, h, root)
	want := map[string]any{"id": float64(queued.ID), "mode": "followUp", "content": "later", "entry": map[string]any{"kind": "chat.mention"}}
	if got := items[len(items)-1]; jsonOf(t, got) != jsonOf(t, want) {
		t.Fatalf("queued item = %s, want %s", jsonOf(t, got), jsonOf(t, want))
	}
	release()
	waitFor(t, queued)
	var mentions [][]any
	for _, entry := range chatEntries(t, root) {
		if entry.Kind == "chat.mention" {
			mentions = append(mentions, []any{entry.Head == nil, entry.Edits == nil, textOf(entry), len(entry.Model)})
		}
	}
	if want := [][]any{{true, true, "idle", 1}, {true, true, "later", 1}}; !reflect.DeepEqual(mentions, want) {
		t.Fatalf("mentions = %v, want %v", mentions, want)
	}
}

func TestInputEntriesWithdrawQueuedInputsWithEntriesOneByOneAndByConversationAbort(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, _ := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the request")
	withdrawn := submit(t, root, inputWith("w", mention("w")))
	aborted := submit(t, root, inputWith("x", mention("x")))
	result, err := withdrawn.Abort(bg)
	must(t, err)
	if result != "aborted" {
		t.Fatalf("abort = %q, want aborted", result)
	}
	if ids := inboxIDs(t, h, root); !slices.Equal(ids, []durable.SubmissionID{aborted.ID}) {
		t.Fatalf("inbox = %v, want the second", ids)
	}
	must(t, root.Abort(bg, false))
	for _, submission := range []*Submission{withdrawn, aborted, input} {
		expectSettled(t, waitFor(t, submission), durable.SubmissionUnanswered, "aborted")
	}
	if ids := inboxIDs(t, h, root); len(ids) != 0 {
		t.Fatalf("inbox = %v, want empty", ids)
	}
	if slices.ContainsFunc(chatEntries(t, root), func(entry durable.EntryRecord) bool { return entry.Kind == "chat.mention" }) {
		t.Fatal("a withdrawn input was placed")
	}
}

func TestDataOnlyWritesQueueBehindAQueuedResetSoTheyLandAfterTheResetAndStayVisible(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first})
	_, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the request")
	must(t, root.Reset(bg, nil))
	chat := submit(t, root, writeOf(durable.EntryDraft{Kind: "chat.message", Data: json.RawMessage(`"hi"`)}))
	if status := submissionStatus(t, chat).Status; status != durable.SubmissionQueued {
		t.Fatalf("write = %s, want queued", status)
	}
	release()
	waitFor(t, input)
	settled := waitFor(t, chat)
	entries := chatEntries(t, root)
	reset := entries[slices.IndexFunc(entries, func(entry durable.EntryRecord) bool { return entry.Kind == durable.ResetEntry.Kind })]
	if settled.Entry == nil || *settled.Entry <= reset.ID {
		t.Fatalf("write = %+v, want it after the reset %d", settled, reset.ID)
	}
	view, err := root.Context(bg, nil)
	must(t, err)
	if kinds := entryKinds(view.Entries); !slices.Equal(kinds, []string{durable.ResetEntry.Kind, "chat.message"}) {
		t.Fatalf("context entries = %v", kinds)
	}
	// Once the reset is placed, display-only writes go in at once again.
	later := submit(t, root, writeOf(durable.EntryDraft{Kind: "chat.message", Data: json.RawMessage(`"again"`)}))
	if status := submissionStatus(t, later).Status; status != durable.SubmissionDone {
		t.Fatalf("later write = %s, want done", status)
	}
}

func TestDataOnlyWritesLeaveTheRequestUnchangedByAWriteThatLandsWhileTheGenerationPrepares(t *testing.T) {
	setup := newChatSetup(t)
	rendering := make(chan struct{})
	render := make(chan struct{})
	var renders atomic.Int64
	addSection(t, setup.registry, Section("slow", func(context.Context, PromptInput) (string, bool, error) {
		if renders.Add(1) == 2 {
			close(rendering)
			<-render
		}
		return "SLOW", true, nil
	}))
	log := &requestLog{}
	setup.faux.SetResponses([]providers.FauxResponseStep{log.step(fauxAnswer("one")), log.step(fauxAnswer("two"))})
	_, root := openChat(t, memory.New(), setup)
	waitFor(t, submit(t, root, inputDraft("a")))
	second := submit(t, root, inputDraft("b"))
	awaitSignal(t, rendering, "the second preparation")
	submit(t, root, writeOf(durable.EntryDraft{Kind: "chat.message", Data: json.RawMessage(`"hi"`)}))
	close(render)
	waitFor(t, second)
	if got := log.all()[1]; !slices.Equal(got, []string{"system:", "user:a", "assistant:one", "user:b"}) {
		t.Fatalf("second request = %q", got)
	}
	// The unchanged prompt adds no second pi.system entry.
	expectKinds(t, root, "pi.user", "pi.system", "pi.assistant", "pi.user", "chat.message", "pi.assistant")
}

func TestDataOnlyWritesAppendWhileIdleAfterAFailedRunWithoutPlacingTheQueuedItems(t *testing.T) {
	setup := newChatSetup(t)
	failing, reached, release := gatedStep(fauxError("invalid request"))
	setup.faux.SetResponses([]providers.FauxResponseStep{failing, fauxText("for f")})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the request")
	f := submit(t, root, inputWith("f", mention("f")))
	release()
	expectSettled(t, waitFor(t, input), durable.SubmissionUnanswered, "model_error")
	must(t, h.WaitForIdle(bg))
	note := submit(t, root, writeOf(durable.EntryDraft{Kind: "note", Data: json.RawMessage(`"n"`)}))
	if status := submissionStatus(t, note).Status; status != durable.SubmissionDone {
		t.Fatalf("note = %s, want done", status)
	}
	must(t, h.WaitForIdle(bg))
	if status := submissionStatus(t, f).Status; status != durable.SubmissionQueued {
		t.Fatalf("f = %s, want queued", status)
	}
	if ids := inboxIDs(t, h, root); !slices.Equal(ids, []durable.SubmissionID{f.ID}) {
		t.Fatalf("inbox = %v", ids)
	}
	expectIdleLive(t, h, root)
	if calls := setup.faux.State.CallCount; calls != 1 {
		t.Fatalf("model calls = %d, want 1", calls)
	}
	// The next input places the waiting mention first.
	waitFor(t, submit(t, root, inputDraft("g")))
	got := slices.DeleteFunc(entryTranscript(chatEntries(t, root)), func(line []string) bool { return line[0] == durable.AssistantEntry.Kind })
	want := [][]string{{"pi.user", "a"}, {"note", `"n"`}, {"chat.mention", `{"author":"ann","text":"f"}`, "f"}, {"pi.user", "g"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

func TestDataOnlyWritesKeepAQueuedInputsEntryAcrossReopenAndPlaceItAsThatEntry(t *testing.T) {
	dir := t.TempDir()
	setup := newChatSetup(t)
	first, reached, _ := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("first again"), fauxText("for f")})
	h, root := openChat(t, openJSONL(t, dir), setup)
	submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the request")
	f := submit(t, root, inputWith("f", mention("f"))).ID
	must(t, h.Close(bg))

	h, root = openChat(t, openJSONL(t, dir), setup)
	items := inboxValues(t, h, root)
	index := slices.IndexFunc(items, func(item map[string]any) bool { return item["id"] == float64(f) })
	if index < 0 || jsonOf(t, items[index]["entry"]) != jsonOf(t, map[string]any{"kind": "chat.mention", "data": map[string]any{"author": "ann", "text": "f"}}) {
		t.Fatalf("inbox = %s, want f with its entry", jsonOf(t, items))
	}
	settled := waitSubmission(t, h, f)
	entries := chatEntries(t, root)
	placed := entries[slices.IndexFunc(entries, func(entry durable.EntryRecord) bool { return entry.Kind == "chat.mention" })]
	if settled.Status != durable.SubmissionDone || settled.Entry == nil || *settled.Entry != placed.ID {
		t.Fatalf("settled = %+v, want done with the placed entry", settled)
	}
	if string(placed.Data) != `{"author":"ann","text":"f"}` || len(placed.Model) != 1 || textOf(placed) != "f" {
		t.Fatalf("placed = %+v", placed)
	}
	if _, user := placed.Model[0].(ai.UserMessage); !user {
		t.Fatalf("placed model = %T, want a user message", placed.Model[0])
	}
}

func TestDataOnlyWritesAppendAtOnceBehindANonEmptyInboxAndStillQueueWritesWithModelOrEdits(t *testing.T) {
	setup := newChatSetup(t)
	first, reached, release := gatedStep(fauxAnswer("first"))
	setup.faux.SetResponses([]providers.FauxResponseStep{first, fauxText("second")})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	awaitSignal(t, reached, "the request")
	followUp := submit(t, root, inputDraft("f"))
	model := submit(t, root, writeOf(durable.EntryDraft{Kind: "note", Model: []ai.Message{ai.NewUserText("m", 0)}}))
	entries := chatEntries(t, root)
	target := entries[slices.IndexFunc(entries, func(entry durable.EntryRecord) bool { return entry.Kind == durable.UserEntry.Kind })]
	edit := submit(t, root, writeOf(durable.EntryDraft{Kind: "redact", Edits: []durable.ContextEdit{{Target: target.ID, Action: "omit"}}}))
	note := submit(t, root, writeOf(durable.EntryDraft{Kind: "note", Data: json.RawMessage(`"n"`)}))
	if status := submissionStatus(t, note).Status; status != durable.SubmissionDone {
		t.Fatalf("note = %s, want done", status)
	}
	items := names(inboxValues(t, h, root), func(item map[string]any) string { return jsonOf(t, []any{item["id"], item["mode"]}) })
	want := []string{jsonOf(t, []any{followUp.ID, "followUp"}), jsonOf(t, []any{model.ID, "write"}), jsonOf(t, []any{edit.ID, "write"})}
	if !slices.Equal(items, want) {
		t.Fatalf("inbox = %v, want %v", items, want)
	}
	release()
	waitFor(t, followUp)
	expectEntryTranscript(t, root,
		[]string{"pi.user", "a"}, []string{"note", `"n"`}, []string{"pi.assistant", "first"}, []string{"note", "m"},
		[]string{"redact"}, []string{"pi.user", "f"}, []string{"pi.assistant", "second"})
	if status := submissionStatus(t, input).Status; status != durable.SubmissionDone {
		t.Fatalf("input = %s, want done", status)
	}
}
