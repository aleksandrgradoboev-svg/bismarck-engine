package harness

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// pi packages/durable/test/harness-context.test.ts, the cases that derive a
// context from committed entries. The cases of extended and kept reads come
// with the generation stage, which brings those reads.

func assistant(text string, stopReason ai.StopReason, calls ...string) ai.AssistantMessage {
	content := ai.ContentList{ai.TextContent{Text: text}}
	for _, id := range calls {
		content = append(content, ai.ToolCall{ID: id, Name: "tool-" + id, Arguments: map[string]any{}})
	}
	if stopReason == "" {
		stopReason = ai.StopStop
		if len(calls) > 0 {
			stopReason = ai.StopToolUse
		}
	}
	return ai.AssistantMessage{Content: content, Api: "faux", Provider: "faux", Model: "faux", StopReason: stopReason, Timestamp: 2}
}

func toolResult(id string) ai.ToolResultMessage {
	return ai.ToolResultMessage{ToolCallID: id, ToolName: "tool-" + id, Content: ai.ContentList{ai.TextContent{Text: "result " + id}}, Timestamp: 3}
}

// system builds a system message from section name, value pairs; a nil value
// removes the section.
func system(pairs ...any) ai.SystemMessage {
	sections := ai.SystemSections{}
	for i := 0; i < len(pairs); i += 2 {
		section := ai.SystemSection{Name: pairs[i].(string)}
		if value, ok := pairs[i+1].(string); ok {
			section.Value = &value
		}
		sections = append(sections, section)
	}
	return ai.SystemMessage{Sections: sections, Timestamp: 4}
}

func firstText(content ai.ContentList) string {
	for _, c := range content {
		if text, ok := c.(ai.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

// describeMessage renders a message compactly for assertions.
func describeMessage(message ai.Message) string {
	switch m := message.(type) {
	case ai.UserMessage:
		return "user:" + firstText(m.Content)
	case ai.AssistantMessage:
		return "assistant:" + firstText(m.Content)
	case ai.ToolResultMessage:
		if m.IsError {
			return "result:" + m.ToolCallID + ":error"
		}
		return "result:" + m.ToolCallID + ":" + firstText(m.Content)
	case ai.SystemMessage:
		return "system:" + strings.Join(names(m.Sections.Entries(), func(s ai.SystemSection) string { return s.Name }), ",")
	}
	return "?"
}

func describeAll(messages []ai.Message) []string { return names(messages, describeMessage) }

type contextSetup struct {
	h    *Harness
	root *Conversation
	t    *testing.T
}

func newContextSetup(t *testing.T) contextSetup {
	t.Helper()
	h, _ := openHarness(t, memory.New())
	root, err := h.Root(bg, nil, nil)
	must(t, err)
	return contextSetup{h: h, root: root, t: t}
}

func (s contextSetup) append(draft durable.EntryDraft) durable.EntryRecord {
	s.t.Helper()
	var entry durable.EntryRecord
	must(s.t, s.root.Commit(bg, func(tx *durable.Tx) error {
		var err error
		entry, err = tx.AppendEntry(s.root.ID, draft)
		return err
	}))
	return entry
}

func (s contextSetup) message(model ai.Message, kind ...string) durable.EntryRecord {
	s.t.Helper()
	k := "message"
	if len(kind) > 0 {
		k = kind[0]
	}
	return s.append(durable.EntryDraft{Kind: k, Model: []ai.Message{model}})
}

func (s contextSetup) view(at ...durable.EntryID) ContextView {
	s.t.Helper()
	var point *durable.EntryID
	if len(at) > 0 {
		point = &at[0]
	}
	view, err := s.root.Context(bg, point)
	must(s.t, err)
	return view
}

func entryIDs(entries []durable.EntryRecord) []durable.EntryID {
	return names(entries, func(e durable.EntryRecord) durable.EntryID { return e.ID })
}

func expectMessages(t *testing.T, view ContextView, want ...string) {
	t.Helper()
	if got := describeAll(view.Messages); !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
		t.Fatalf("messages = %q, want %q", got, want)
	}
}

func replaceEdit(target durable.EntryID, messages ...ai.Message) durable.EntryDraft {
	return durable.EntryDraft{Kind: "edit", Edits: []durable.ContextEdit{{Target: target, Action: durable.ContextEditReplace, Messages: messages}}}
}

func omitEdit(target durable.EntryID) durable.EntryDraft {
	return durable.EntryDraft{Kind: "edit", Edits: []durable.ContextEdit{{Target: target, Action: durable.ContextEditOmit}}}
}

func TestContextReturnsTheWholeTranscriptWithoutAHeadAndSkipsModelLessEntries(t *testing.T) {
	s := newContextSetup(t)
	first := s.message(user("hi"))
	note := s.append(durable.EntryDraft{Kind: "note", Data: []byte(`{"text":"display only"}`)})
	answer := s.message(assistant("hello", ""))
	view := s.view()
	if view.Head != nil || !reflect.DeepEqual(entryIDs(view.Entries), []durable.EntryID{first.ID, note.ID, answer.ID}) {
		t.Fatalf("head = %v, entries = %v", view.Head, entryIDs(view.Entries))
	}
	expectMessages(t, view, "user:hi", "assistant:hello")
}

func TestContextExcludesAbortedErrorAndDeferredAnswersButKeepsTheirEntries(t *testing.T) {
	s := newContextSetup(t)
	s.message(user("q"))
	aborted := s.message(assistant("partial", ai.StopAborted))
	s.message(assistant("failed", ai.StopError))
	s.message(assistant("later", ai.StopDeferred))
	s.message(assistant("done", ai.StopLength))
	view := s.view()
	if len(view.Entries) != 5 || view.Entries[1].ID != aborted.ID {
		t.Fatalf("entries = %v", entryIDs(view.Entries))
	}
	expectMessages(t, view, "user:q", "assistant:done")
}

func TestContextResolvesSelfHeadsAndUsesTheNewestHeadMarker(t *testing.T) {
	s := newContextSetup(t)
	s.message(user("old"))
	reset := s.append(durable.EntryDraft{Kind: "reset", HeadSelf: true, Model: []ai.Message{user("fresh start")}})
	if reset.Head == nil || *reset.Head != reset.ID {
		t.Fatalf("reset head = %v", reset.Head)
	}
	after := s.message(assistant("after reset", ""))
	view := s.view()
	if view.Head == nil || view.Head.ID != reset.ID || !reflect.DeepEqual(entryIDs(view.Entries), []durable.EntryID{reset.ID, after.ID}) {
		t.Fatalf("view = %+v", entryIDs(view.Entries))
	}
	expectMessages(t, view, "user:fresh start", "assistant:after reset")

	// A compaction summary heads an earlier kept entry; older head markers in
	// range drop out.
	summary := s.append(durable.EntryDraft{Kind: "summary", Head: &after.ID, Model: []ai.Message{user("summary")}})
	tail := s.message(user("next"))
	view = s.view()
	if view.Head.ID != summary.ID || !reflect.DeepEqual(entryIDs(view.Entries), []durable.EntryID{summary.ID, after.ID, tail.ID}) {
		t.Fatalf("entries = %v", entryIDs(view.Entries))
	}
	expectMessages(t, view, "user:summary", "assistant:after reset", "user:next")
}

func TestContextAppliesTheNewestEditPerTargetWithinTheActiveRange(t *testing.T) {
	s := newContextSetup(t)
	first := s.message(user("first"))
	second := s.message(user("second"))
	s.append(replaceEdit(first.ID, user("first v2")))
	s.append(replaceEdit(first.ID, user("first v3")))
	s.append(omitEdit(second.ID))
	view := s.view()
	if len(view.Entries) != 5 {
		t.Fatalf("entries = %v", entryIDs(view.Entries))
	}
	expectMessages(t, view, "user:first v3")

	// Edits before the active range no longer apply.
	reset := s.append(durable.EntryDraft{Kind: "reset", Head: &second.ID})
	view = s.view()
	if view.Head.ID != reset.ID {
		t.Fatalf("head = %d", view.Head.ID)
	}
	expectMessages(t, view)
	s.append(replaceEdit(second.ID, user("second v2")))
	expectMessages(t, s.view(), "user:second v2")
}

func TestContextKeepsPositionalSystemMessagesAndOrdersToolResultsByCallOrder(t *testing.T) {
	s := newContextSetup(t)
	s.message(system("preamble", "You help."), "pi.system")
	s.message(user("run tools"))
	s.message(assistant("calling", "", "b", "a"))
	s.message(toolResult("a"))
	s.message(system("cwd", "/repo"), "pi.system")
	s.message(toolResult("b"))
	s.message(toolResult("zz"))
	s.message(assistant("done", ""))
	expectMessages(t, s.view(),
		"system:preamble", "user:run tools", "assistant:calling", "result:b:result b", "result:a:result a", "system:cwd", "assistant:done")
}

// pi #10542.
func TestContextLeadsWithASystemMessageOnlyUserMessagesPrecede(t *testing.T) {
	s := newContextSetup(t)
	s.message(user("first"))
	s.message(user("steered"))
	s.message(system("preamble", "You help."), "pi.system")
	s.message(assistant("answer", ""))
	s.message(user("next"))
	s.message(system("cwd", "/repo"), "pi.system")
	view := s.view()
	expectMessages(t, view, "system:preamble", "user:first", "user:steered", "assistant:answer", "user:next", "system:cwd")
	// Stored order and contributions stay as committed.
	var flat []ai.Message
	for _, contribution := range view.Contributions {
		flat = append(flat, contribution...)
	}
	if got := describeAll(flat)[:3]; !reflect.DeepEqual(got, []string{"user:first", "user:steered", "system:preamble"}) {
		t.Fatalf("contributions = %v", got)
	}

	// After a reset, the baseline written after the handoff leads too.
	s.append(durable.EntryDraft{Kind: "reset", HeadSelf: true, Model: []ai.Message{user("handoff")}})
	s.message(system("preamble", "Baseline."), "pi.system")
	expectMessages(t, s.view(), "system:preamble", "user:handoff")
}

// pi #10904.
func TestContextLeadsWithTheBaselineAfterACompactionThatKeepsAnswers(t *testing.T) {
	s := newContextSetup(t)
	s.message(user("old"))
	kept := s.message(user("kept"))
	s.message(assistant("calling", "", "a"))
	s.message(toolResult("a"))
	s.message(assistant("answer", ""))
	s.append(durable.EntryDraft{Kind: "summary", Head: &kept.ID, Model: []ai.Message{user("summary")}})
	s.message(user("next"))
	s.message(system("preamble", "Baseline."), "pi.system")
	expectMessages(t, s.view(),
		"system:preamble", "user:summary", "user:kept", "assistant:calling", "result:a:result a", "assistant:answer", "user:next")

	// Later system messages stay in place.
	s.message(assistant("done", ""))
	s.message(system("cwd", "/repo"), "pi.system")
	got := describeAll(s.view().Messages)
	if !reflect.DeepEqual(got[:2], []string{"system:preamble", "user:summary"}) || !reflect.DeepEqual(got[len(got)-2:], []string{"assistant:done", "system:cwd"}) {
		t.Fatalf("messages = %v", got)
	}
}

// pi #10904.
func TestContextLeadsWithABaselineWrittenAfterTheFirstAnswers(t *testing.T) {
	s := newContextSetup(t)
	s.message(user("first"), "pi.user")
	s.message(assistant("answer", ""), "pi.assistant")
	s.message(user("next"), "pi.user")
	s.message(system("plan", "Plan."), "pi.system")
	expectMessages(t, s.view(), "system:plan", "user:first", "assistant:answer", "user:next")
}

// pi #10904.
func TestContextKeepsOtherSystemMessagesInPlaceOnceAnAnswerPrecedesThem(t *testing.T) {
	s := newContextSetup(t)
	s.message(user("first"))
	s.message(assistant("answer", ""))
	// Another kind's system message is positional; the baseline after it is
	// not the first system message.
	s.message(system("note", "From here on."), "host.note")
	s.message(system("preamble", "Baseline."), "pi.system")
	expectMessages(t, s.view(), "user:first", "assistant:answer", "system:note", "system:preamble")

	// After a compaction that keeps the other kind's system message, the new
	// baseline does not move ahead of it.
	kept := s.message(user("kept"))
	s.message(assistant("kept answer", ""))
	s.message(system("mode", "plan"), "host.note")
	s.append(durable.EntryDraft{Kind: "summary", Head: &kept.ID, Model: []ai.Message{user("summary")}})
	s.message(user("next"))
	s.message(system("preamble", "Baseline."), "pi.system")
	expectMessages(t, s.view(), "user:summary", "user:kept", "assistant:kept answer", "system:mode", "user:next", "system:preamble")
}

// pi #10904.
func TestContextDoesNotLeadWithASystemEntryWrittenAfterAnotherSystemMessage(t *testing.T) {
	s := newContextSetup(t)
	host := s.message(system("legacy", "Use the legacy format."), "host.system")
	s.message(user("first"))
	s.message(assistant("answer", ""))
	// Generation wrote this entry against the host's message: it only removes
	// a section, so it is no baseline.
	s.message(system("legacy", nil), "pi.system")
	s.append(omitEdit(host.ID))
	expectMessages(t, s.view(), "user:first", "assistant:answer", "system:legacy")

	// The same holds for a system message an edit put before it.
	other := newContextSetup(t)
	first := other.message(user("first"))
	other.append(replaceEdit(first.ID, user("first"), system("note", "Note.")))
	other.message(assistant("answer", ""))
	other.message(system("note", nil), "pi.system")
	other.append(replaceEdit(first.ID, user("first")))
	expectMessages(t, other.view(), "user:first", "assistant:answer", "system:note")
}

// pi #10904.
func TestContextDoesNotLeadWithARetainedEditedOrLaterSystemEntryInPlaceOfTheBaseline(t *testing.T) {
	s := newContextSetup(t)
	kept := s.message(user("kept"))
	s.message(assistant("answer", ""))
	retained := s.message(system("cwd", "/repo"), "pi.system")
	// Before generation writes the baseline after the head marker, a retained
	// system entry stays in place.
	s.append(durable.EntryDraft{Kind: "summary", Head: &kept.ID, Model: []ai.Message{user("summary")}})
	s.message(user("next"))
	expectMessages(t, s.view(), "user:summary", "user:kept", "assistant:answer", "system:cwd", "user:next")

	// The baseline omits the retained entry, as generation writes it, and
	// leads.
	baseline := s.append(durable.EntryDraft{
		Kind:  "pi.system",
		Model: []ai.Message{system("preamble", "Baseline.")},
		Edits: []durable.ContextEdit{{Target: retained.ID, Action: durable.ContextEditOmit}},
	})
	s.message(assistant("done", ""))
	s.message(system("preamble", nil), "pi.system")
	expectMessages(t, s.view(), "system:preamble", "user:summary", "user:kept", "assistant:answer", "user:next", "assistant:done", "system:preamble")

	// An omitted baseline leaves no message to lead; the delta after it is not
	// a baseline and stays in place.
	s.append(omitEdit(baseline.ID))
	expectMessages(t, s.view(), "user:summary", "user:kept", "assistant:answer", "user:next", "assistant:done", "system:preamble")

	// A replacement of the baseline is not one either.
	s.append(replaceEdit(baseline.ID, system("other", "Other.")))
	expectMessages(t, s.view(), "user:summary", "user:kept", "assistant:answer", "user:next", "system:other", "assistant:done", "system:preamble")
}

func TestContextSynthesizesMissingToolResultsAfterAForkAndDropsCutResults(t *testing.T) {
	s := newContextSetup(t)
	s.message(user("go"))
	call := s.message(assistant("calling", "", "x", "y"))
	s.message(toolResult("x"))
	second := s.message(toolResult("y"))
	child, err := s.root.Fork(bg, call.ID, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	childView, err := child.Context(bg, nil)
	must(t, err)
	expectMessages(t, childView, "user:go", "assistant:calling", "result:x:error", "result:y:error")
	missing, ok := childView.Messages[2].(ai.ToolResultMessage)
	if !ok || missing.ToolName != "tool-x" || jsonOf(t, missing.Details) != `{"reason":"missing_result"}` {
		t.Fatalf("missing result = %+v", childView.Messages[2])
	}

	// A head between a call and its results leaves stray results that are not
	// sent.
	s.append(durable.EntryDraft{Kind: "reset", Head: &second.ID})
	parentView := s.view()
	expectMessages(t, parentView)
	if got := names(parentView.Entries, func(e durable.EntryRecord) string { return e.Kind }); !reflect.DeepEqual(got, []string{"reset", "message"}) {
		t.Fatalf("entries = %v", got)
	}
}

// pi #10512.
func TestContextReadsAsOfAnEarlierEntryAsAForkThereStarts(t *testing.T) {
	s := newContextSetup(t)
	first := s.message(user("first"))
	call := s.message(assistant("calling", "", "x", "y"))
	result := s.message(toolResult("x"))
	s.message(toolResult("y"))
	edit := s.append(replaceEdit(first.ID, user("first v2")))
	reset := s.append(durable.EntryDraft{Kind: "reset", HeadSelf: true, Model: []ai.Message{user("fresh start")}})
	tail := s.message(assistant("after reset", ""))

	for _, at := range []durable.EntryRecord{first, call, result, edit, reset, tail} {
		fork, err := s.root.Fork(bg, at.ID, ConversationCreateOptions{Ownership: durable.Ownerless})
		must(t, err)
		forkView, err := fork.Context(bg, nil)
		must(t, err)
		if got := s.view(at.ID); !reflect.DeepEqual(got, forkView) {
			t.Fatalf("context at %d = %+v, fork = %+v", at.ID, got, forkView)
		}
	}
	// Stepped back before the edit and the reset: neither applies, and a cut
	// call gets synthesized results.
	expectMessages(t, s.view(call.ID), "user:first", "assistant:calling", "result:x:error", "result:y:error")
	if !reflect.DeepEqual(s.view(tail.ID), s.view()) {
		t.Fatal("the context at the tail differs from the current one")
	}

	other, err := s.root.Fork(bg, first.ID, ConversationCreateOptions{Ownership: durable.Ownerless})
	must(t, err)
	_, err = other.Context(bg, &tail.ID)
	expectError(t, err, "is not visible")
}
