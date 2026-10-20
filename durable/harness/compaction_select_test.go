package harness

import (
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai/providers"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// Range selection and serialization (pi harness-compaction.test.ts).

// sizedText is text of about tokens estimated tokens, starting with label.
func sizedText(label string, tokens int) string {
	return label + " " + strings.Repeat("x", max(0, tokens*4-len(label)-1))
}

type cutEntries struct{ next durable.EntryID }

func (c *cutEntries) entry(kind string, model ...ai.Message) durable.EntryRecord {
	c.next++
	return durable.EntryRecord{ID: c.next, ConversationID: 1, Kind: kind, Model: model}
}

func (c *cutEntries) marker(model ...ai.Message) durable.EntryRecord {
	record := c.entry("pi.compaction", model...)
	head := durable.EntryID(0)
	record.Head = &head
	return record
}

func cutUser(text string) ai.Message { return ai.NewUserText(text, 0) }

func cutAssistant(text string, calls []string, stop ai.StopReason) ai.Message {
	content := ai.ContentList{ai.TextContent{Text: text}}
	for _, id := range calls {
		content = append(content, ai.ToolCall{ID: id, Name: "read", Arguments: map[string]any{}})
	}
	message := *providers.FauxAssistantMessage(content, ai.StopStop)
	if calls != nil {
		message.StopReason = ai.StopToolUse
	}
	if stop != "" {
		message.StopReason = stop
	}
	return message
}

func cutResult(callID, text string) ai.Message {
	return ai.ToolResultMessage{ToolCallID: callID, ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: text}}}
}

// cutView is a view over entries whose contributions are their models, with
// excluded assistants removed (pi's view helper).
func cutView(entries []durable.EntryRecord, head *durable.EntryRecord) ContextView {
	all := entries
	if head != nil {
		all = append([]durable.EntryRecord{*head}, entries...)
	}
	contributions := make([][]ai.Message, len(all))
	for i, record := range all {
		contributions[i] = []ai.Message{}
		for _, message := range record.Model {
			if assistant, ok := assistantOf(message); ok && excludedStopReason(assistant.StopReason) {
				continue
			}
			contributions[i] = append(contributions[i], message)
		}
	}
	view := ContextView{Head: head, Entries: all, Contributions: contributions}
	view.Messages = summarizedMessages(view, len(all))
	return view
}

func wantCut(t *testing.T, view ContextView, keep int, want int) {
	t.Helper()
	cut, ok := selectCut(view, keep)
	if want < 0 {
		if ok {
			t.Fatalf("selectCut(%d) = %d, want none", keep, cut)
		}
		return
	}
	if !ok || cut != want {
		t.Fatalf("selectCut(%d) = %d, %v, want %d", keep, cut, ok, want)
	}
}

func TestSelectCutKeepsAboutKeepRecentTokensAndCutsAtTheFirstCandidateAtOrAfterTheBudget(t *testing.T) {
	var c cutEntries
	entries := []durable.EntryRecord{
		c.entry("pi.user", cutUser(sizedText("1", 10))),
		c.entry("pi.assistant", cutAssistant("2", []string{"c1"}, "")),
		c.entry("pi.tool-result", cutResult("c1", sizedText("3", 3000))),
		c.entry("pi.assistant", cutAssistant(sizedText("4", 10), nil, "")),
		c.entry("pi.user", cutUser(sizedText("5", 10))),
		c.entry("pi.assistant", cutAssistant(sizedText("6", 10), nil, "")),
	}
	wantCut(t, cutView(entries, nil), 2000, 3)
}

func TestSelectCutCutsAtAUserEntry(t *testing.T) {
	var c cutEntries
	entries := []durable.EntryRecord{
		c.entry("pi.user", cutUser(sizedText("u1", 100))),
		c.entry("pi.assistant", cutAssistant(sizedText("a1", 100), nil, "")),
		c.entry("pi.user", cutUser(sizedText("u2", 100))),
		c.entry("pi.assistant", cutAssistant(sizedText("a2", 100), nil, "")),
	}
	wantCut(t, cutView(entries, nil), 150, 2)
}

func TestSelectCutCutsAtAnAssistantInTheMiddleOfOneLongRunAndNeverAtAToolResult(t *testing.T) {
	var c cutEntries
	entries := []durable.EntryRecord{c.entry("pi.user", cutUser("do it"))}
	for index := range 5 {
		id := "c" + string(rune('0'+index))
		entries = append(entries, c.entry("pi.assistant", cutAssistant("step "+string(rune('0'+index)), []string{id}, "")))
		entries = append(entries, c.entry("pi.tool-result", cutResult(id, sizedText("r"+string(rune('0'+index)), 100))))
	}
	cut, ok := selectCut(cutView(entries, nil), 150)
	// The budget is reached at the fourth result; the cut is the last call,
	// whose result it keeps.
	if !ok || entries[cut].Kind != "pi.assistant" || cut != 9 {
		t.Fatalf("cut = %d (%v)", cut, ok)
	}
}

// #10407.
func TestSelectCutNeverCutsAtADisplayOnlyEntryWhereTheBudgetIsReached(t *testing.T) {
	var c cutEntries
	entries := []durable.EntryRecord{
		c.entry("pi.user", cutUser(sizedText("u1", 100))),
		c.entry("pi.assistant", cutAssistant(sizedText("a1", 100), nil, "")),
		c.entry("chat.message"),
		c.entry("pi.assistant", cutAssistant(sizedText("a2", 100), nil, "")),
	}
	// The budget is reached at a1; the display-only entry after it is skipped
	// for the assistant a2.
	wantCut(t, cutView(entries, nil), 150, 1)
	wantCut(t, cutView(entries, nil), 50, 3)
}

func TestSelectCutKeepsAHugeLastToolResultTogetherWithItsAssistant(t *testing.T) {
	var c cutEntries
	entries := []durable.EntryRecord{
		c.entry("pi.user", cutUser("u")),
		c.entry("pi.assistant", cutAssistant("a", []string{"c"}, "")),
		c.entry("pi.tool-result", cutResult("c", sizedText("big", 5000))),
	}
	wantCut(t, cutView(entries, nil), 100, 1)
}

func TestSelectCutNeverCutsAtASystemEntryOrAnExcludedErrorOrAbortedAnswer(t *testing.T) {
	var c cutEntries
	system := ai.SystemMessage{Sections: ai.SystemSections{{Name: "s", Value: ptr(sizedText("s", 100))}}}
	entries := []durable.EntryRecord{
		c.entry("pi.user", cutUser(sizedText("u1", 100))),
		c.entry("pi.assistant", cutAssistant(sizedText("a1", 100), nil, "")),
		c.entry("pi.system", system),
		c.entry("pi.assistant", cutAssistant(sizedText("err", 100), nil, ai.StopError)),
		c.entry("pi.assistant", cutAssistant(sizedText("stopped", 100), nil, ai.StopAborted)),
		c.entry("pi.assistant", cutAssistant(sizedText("a2", 100), nil, "")),
	}
	// The walk reaches 150 at the system entry; the excluded answers after it
	// contribute nothing.
	wantCut(t, cutView(entries, nil), 150, 5)
}

func TestSelectCutFollowsEditedContributions(t *testing.T) {
	var c cutEntries
	entries := []durable.EntryRecord{
		c.entry("pi.user", cutUser(sizedText("u1", 100))),
		c.entry("pi.assistant", cutAssistant(sizedText("a1", 100), nil, "")),
		c.entry("pi.user", cutUser(sizedText("u2", 100))),
		c.entry("pi.assistant", cutAssistant(sizedText("a2", 100), nil, "")),
	}
	plain := cutView(entries, nil)
	wantCut(t, plain, 150, 2)
	// An omitted entry adds nothing and is no candidate.
	omitted := plain
	omitted.Contributions = append([][]ai.Message(nil), plain.Contributions...)
	omitted.Contributions[2] = []ai.Message{}
	omitted.Messages = summarizedMessages(omitted, len(omitted.Entries))
	wantCut(t, omitted, 150, 1)
}

func TestSelectCutDoesNotCutAtAUserEntryThatAResultOfThePrecedingCallStillFollows(t *testing.T) {
	var c cutEntries
	entries := []durable.EntryRecord{
		c.entry("pi.user", cutUser(sizedText("u1", 100))),
		c.entry("pi.assistant", cutAssistant("a", []string{"c"}, "")),
		c.entry("pi.user", cutUser(sizedText("steer", 100))),
		c.entry("pi.tool-result", cutResult("c", sizedText("r", 100))),
		c.entry("pi.assistant", cutAssistant(sizedText("a2", 100), nil, "")),
	}
	// The budget is reached at the steer; its result follows it, so the cut
	// moves to the next assistant.
	wantCut(t, cutView(entries, nil), 250, 4)
}

func TestSelectCutFindsNothingWhenTheBudgetIsNeverReachedOrOnlyTheMarkerPrecedesTheCut(t *testing.T) {
	var c cutEntries
	small := []durable.EntryRecord{c.entry("pi.user", cutUser("hi")), c.entry("pi.assistant", cutAssistant("hello", nil, ""))}
	wantCut(t, cutView(small, nil), 150, -1)
	marker := c.marker(cutUser("summary"))
	// The budget is reached at the only entry after the marker, so the marker
	// alone would be summarized.
	wantCut(t, cutView([]durable.EntryRecord{c.entry("pi.user", cutUser(sizedText("u", 200)))}, &marker), 150, -1)
}

func TestSelectCutSummarizesAnEarlierSummaryMarkerFirst(t *testing.T) {
	var c cutEntries
	marker := c.marker(cutUser("EARLIER"))
	kept := []durable.EntryRecord{
		c.entry("pi.user", cutUser(sizedText("u1", 100))),
		c.entry("pi.assistant", cutAssistant(sizedText("a1", 100), nil, "")),
		c.entry("pi.user", cutUser(sizedText("u2", 100))),
		c.entry("pi.assistant", cutAssistant(sizedText("a2", 100), nil, "")),
	}
	selected := cutView(kept, &marker)
	wantCut(t, selected, 150, 3)
	var flat []ai.Message
	for _, messages := range selected.Contributions[:3] {
		flat = append(flat, messages...)
	}
	if got := serializeConversation(flat); !strings.HasPrefix(got, "[User]: EARLIER") {
		t.Fatalf("serialized = %q", got)
	}
}

func TestSerializeConversationWritesATranscriptTruncatesToolResultsAndOmitsSystemMessagesAndThinking(t *testing.T) {
	call := ai.ToolCall{ID: "c", Name: "read", Arguments: map[string]any{"path": "a.ts"}}
	messages := []ai.Message{
		ai.SystemMessage{Sections: ai.SystemSections{{Name: "s", Value: ptr("hidden")}}},
		cutUser("hello"),
		*providers.FauxAssistantMessage(ai.ContentList{ai.ThinkingContent{Thinking: "hmm"}, ai.TextContent{Text: "sure"}, call}, ai.StopToolUse),
		cutResult("c", strings.Repeat("y", 2500)),
	}
	serialized := serializeConversation(messages)
	for _, absent := range []string{"hidden", "hmm", "[Assistant thinking]"} {
		if strings.Contains(serialized, absent) {
			t.Fatalf("serialized contains %q: %q", absent, serialized)
		}
	}
	for _, present := range []string{"[User]: hello", "[Assistant]: sure", `[Assistant tool calls]: read(path="a.ts")`, "[Tool result]: " + strings.Repeat("y", 2000) + "\n\n[... 500 more characters truncated]"} {
		if !strings.Contains(serialized, present) {
			t.Fatalf("serialized lacks %q: %q", present, serialized)
		}
	}
}
