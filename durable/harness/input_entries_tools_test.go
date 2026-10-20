package harness

import (
	"errors"
	"slices"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// Ported from pi packages/durable/test/harness-input-entries.test.ts: the
// two cases that hold a tool.

// heldResult is what the hold tool of these cases returns.
var heldResult = ToolExecutionResult{Output: ai.ContentList{ai.TextContent{Text: "held"}}}

func TestInputEntriesQueueBusyInputsWithTheirEntriesInTheInboxAndPlaceThemAsThoseEntriesAtBoundaries(t *testing.T) {
	setup := newChatSetup(t)
	gate := make(chan struct{})
	holdTool(t, setup, gate, heldResult)
	setup.faux.SetResponses([]providers.FauxResponseStep{holdCall(), fauxText("after tools"), fauxText("follow-up")})
	h, root := openChat(t, memory.New(), setup)
	input := submit(t, root, inputDraft("a"))
	toolRunning(t, h, root)
	steerDraft := inputWith("s", mention("s"))
	steerDraft.Input.WhenBusy, steerDraft.RequestID = WhenBusySteer, "r-s"
	s := submit(t, root, steerDraft)
	followUpDraft := inputWith("f", &InputEntry{Kind: "p2.input"})
	followUpDraft.Input.WhenBusy = WhenBusyFollowUp
	f := submit(t, root, followUpDraft)
	want := []map[string]any{
		{"id": float64(s.ID), "mode": "steer", "content": "s", "entry": map[string]any{"kind": "chat.mention", "data": map[string]any{"author": "ann", "text": "s"}}},
		{"id": float64(f.ID), "mode": "followUp", "content": "f", "entry": map[string]any{"kind": "p2.input"}},
	}
	if got := inboxValues(t, h, root); jsonOf(t, got) != jsonOf(t, want) {
		t.Fatalf("inbox = %s, want %s", jsonOf(t, got), jsonOf(t, want))
	}
	// The request ID deduplicates a queued input with an entry as any other.
	againDraft := inputWith("other", mention("other"))
	againDraft.Input.WhenBusy, againDraft.RequestID = WhenBusySteer, "r-s"
	if again := submit(t, root, againDraft); again.ID != s.ID {
		t.Fatalf("repeated request = %d, want %d", again.ID, s.ID)
	}
	rejectDraft := inputWith("x", mention("x"))
	rejectDraft.Input.WhenBusy = WhenBusyReject
	var busy *ConversationBusyError
	if _, err := root.Submit(bg, rejectDraft); !errors.As(err, &busy) {
		t.Fatalf("rejected submit = %v, want *ConversationBusyError", err)
	}
	if items := inboxValues(t, h, root); len(items) != 2 {
		t.Fatalf("inbox = %s", jsonOf(t, items))
	}

	close(gate)
	waitFor(t, f)
	expectEntryTranscript(t, root,
		[]string{"pi.user", "a"},
		[]string{"pi.assistant"},
		[]string{"pi.tool-result"},
		[]string{"chat.mention", `{"author":"ann","text":"s"}`, "s"},
		[]string{"pi.assistant", "after tools"},
		[]string{"p2.input", "f"},
		[]string{"pi.assistant", "follow-up"},
	)
	entries := chatEntries(t, root)
	answers := assistantIDs(t, root)
	placed := func(kind string) durable.EntryID {
		return entries[slices.IndexFunc(entries, func(entry durable.EntryRecord) bool { return entry.Kind == kind })].ID
	}
	expectDone(t, "input", submissionStatus(t, input), &answers[1])
	steerStatus := submissionStatus(t, s)
	expectDone(t, "steer", steerStatus, &answers[1])
	if steerStatus.Entry == nil || *steerStatus.Entry != placed("chat.mention") {
		t.Fatalf("steer entry = %v", steerStatus.Entry)
	}
	followUpStatus := submissionStatus(t, f)
	expectDone(t, "follow-up", followUpStatus, &answers[2])
	if followUpStatus.Entry == nil || *followUpStatus.Entry != placed("p2.input") {
		t.Fatalf("follow-up entry = %v", followUpStatus.Entry)
	}
}

func TestDataOnlyWritesAppendMidRunInTimeOrderLeavingTheModelsMessagesUnchanged(t *testing.T) {
	setup := newChatSetup(t)
	gate := make(chan struct{})
	holdTool(t, setup, gate, heldResult)
	log := &requestLog{}
	setup.faux.SetResponses([]providers.FauxResponseStep{log.step(toolCalls(call("hold", nil, "c1"))), log.step(fauxAnswer("done"))})
	h, root := openChat(t, memory.New(), setup)
	_, events := listen(t, root)
	input := submit(t, root, inputDraft("a"))
	toolRunning(t, h, root)
	chat := durable.EntryDraft{Kind: "chat.message", Data: []byte(`{"author":"bob","text":"lunch?"}`)}
	draft := writeOf(chat)
	draft.RequestID = "bob:1"
	write := submit(t, root, draft)
	// Settled and appended in its own commit, between the call and its
	// still-running tool's result.
	settled := submissionStatus(t, write)
	if settled.Status != durable.SubmissionDone || settled.Entry == nil {
		t.Fatalf("write = %+v, want done with its entry", settled)
	}
	if items := inboxValues(t, h, root); len(items) != 0 {
		t.Fatalf("inbox = %s", jsonOf(t, items))
	}
	expectEntryTranscript(t, root,
		[]string{"pi.user", "a"},
		[]string{"pi.assistant"},
		[]string{"chat.message", `{"author":"bob","text":"lunch?"}`},
	)
	eventually(t, func() bool { return slices.Contains(eventTypes(events.events()), "entry_appended") })
	all := events.events()
	appended := all[slices.Index(eventTypes(all), "entry_appended")].(EntryAppendedEvent)
	if appended.Entry.Kind != "chat.message" || appended.Entry.ID != *settled.Entry {
		t.Fatalf("entry_appended = %s", jsonOf(t, appended))
	}
	if slices.Contains(eventTypes(all), "run_end") {
		t.Fatalf("events = %v, want no run_end", eventTypes(all))
	}
	// A repeated request ID returns the same write without a second entry.
	if again := submit(t, root, draft); again.ID != write.ID {
		t.Fatalf("repeated write = %d, want %d", again.ID, write.ID)
	}

	close(gate)
	waitFor(t, input)
	expectEntryTranscript(t, root,
		[]string{"pi.user", "a"},
		[]string{"pi.assistant"},
		[]string{"chat.message", `{"author":"bob","text":"lunch?"}`},
		[]string{"pi.tool-result"},
		[]string{"pi.assistant", "done"},
	)
	requests := log.all()
	if want := [][]string{{"system:", "user:a"}, {"system:", "user:a", "assistant:", "toolResult:held"}}; jsonOf(t, requests) != jsonOf(t, want) {
		t.Fatalf("requests = %q", requests)
	}
}
