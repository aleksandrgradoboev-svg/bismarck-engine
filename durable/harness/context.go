package harness

import (
	"context"
	"fmt"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

const scanPageSize = 256

// missingResultText is what a synthesized result for a call without one says.
const missingResultText = "Tool result unavailable: history ends before this call completed."

// ContextView is the raw active transcript and the model context derived from
// it.
type ContextView struct {
	// Head is the newest applicable head marker, if any.
	Head *durable.EntryRecord
	// Entries are the raw active entries: the head marker followed by the
	// non-head entries from its head through the tail.
	Entries []durable.EntryRecord
	// Contributions are, per entry of Entries, its model messages after edits
	// and excluded stop reasons, before tool result ordering.
	Contributions [][]ai.Message
	// Messages is the model context for the next provider request.
	Messages []ai.Message
	// contributionOrigins and messageOrigins say where each message of
	// Contributions and Messages came from, standing in for the object
	// identity pi compares; a synthesized result has none. Both are nil in a
	// view built by hand.
	contributionOrigins [][]origin
	messageOrigins      []*origin
}

// contextBounds are the head marker and newest visible entry that fix one
// committed context range.
type contextBounds struct {
	head *durable.EntryRecord
	tail durable.EntryID
}

// captureContextBounds captures the bounds of the current context, or of the
// context cut off at the visible entry at, with two O(1) reads. Run it on the
// Session line; entries at or below the tail are immutable, so deriveContext
// can then scan them off the line. nil means an empty transcript.
func captureContextBounds(ctx context.Context, storage durable.Storage, conversationID durable.ConversationID, at *durable.EntryID) (*contextBounds, error) {
	var tail durable.EntryID
	if at == nil {
		page, err := storage.ScanEntries(ctx, durable.EntryQuery{ConversationID: conversationID}, 1, nil)
		if err != nil {
			return nil, err
		}
		if len(page.Items) == 0 {
			return nil, nil
		}
		tail = page.Items[0].ID
	} else {
		entry, err := storage.VisibleEntry(ctx, conversationID, *at)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			return nil, fmt.Errorf("Entry %d is not visible from conversation %d", *at, conversationID)
		}
		tail = *at
	}
	head, err := storage.FindLatestHeadMarker(ctx, conversationID, &tail)
	if err != nil {
		return nil, err
	}
	return &contextBounds{head: head, tail: tail}, nil
}

// readContext returns the committed context of one conversation: bounds
// captured on the Session line, entries derived off it.
func readContext(ctx context.Context, session *durable.Session, conversationID durable.ConversationID, at *durable.EntryID) (ContextView, error) {
	var bounds *contextBounds
	err := session.ReadOnLine(func() error {
		var err error
		bounds, err = captureContextBounds(ctx, session.Storage(), conversationID, at)
		return err
	})
	if err != nil {
		return ContextView{}, err
	}
	return deriveContext(ctx, session.Storage(), conversationID, bounds)
}

// deriveContext derives the active transcript and model context of one
// conversation within captured bounds.
//
// H = newest visible head marker; the range runs from H.head (or transcript
// start) through the tail. Per target, the newest edit in the range wins.
// Context entries are H followed by the range's non-head entries.
func deriveContext(ctx context.Context, storage durable.Storage, conversationID durable.ConversationID, bounds *contextBounds) (ContextView, error) {
	if bounds == nil {
		return ContextView{}, nil
	}
	entries, err := scanRange(ctx, storage, conversationID, bounds)
	if err != nil {
		return ContextView{}, err
	}
	return deriveRange(bounds, entries), nil
}

// origin identifies one contributed message: the entry it came from, its
// index there, and whether it came from an edit rather than the entry's own
// model. pi tells the baseline from other system messages by object identity;
// Go messages are values, so they carry where they came from instead.
type origin struct {
	entry    durable.EntryID
	index    int
	fromEdit bool
}

type sourced struct {
	message ai.Message
	origin  origin
	// synthetic marks a result synthesized for a call without one.
	synthetic bool
}

// baseline is the baseline's system message: unknown while a later entry may
// be one, none when the range cannot have one, or found at origin.
type baseline struct {
	state  int
	origin origin
}

const (
	baselineUnknown = iota
	baselineNone
	baselineFound
)

func deriveRange(bounds *contextBounds, entries []durable.EntryRecord) ContextView {
	edits := map[durable.EntryID]durable.ContextEdit{}
	// Edits of every entry in the range count, including older head markers
	// that selectActive drops.
	for _, entry := range entries {
		for _, edit := range entry.Edits {
			edits[edit.Target] = edit
		}
	}
	active := selectActive(bounds.head, entries)
	contributions := make([][]ai.Message, len(active))
	contributionOrigins := make([][]origin, len(active))
	var all []sourced
	for i, entry := range active {
		var edit *durable.ContextEdit
		if e, ok := edits[entry.ID]; ok {
			edit = &e
		}
		contributed := contribute(entry, edit)
		messages := make([]ai.Message, len(contributed))
		origins := make([]origin, len(contributed))
		for j, message := range contributed {
			messages[j] = message.message
			origins[j] = message.origin
		}
		contributions[i] = messages
		contributionOrigins[i] = origins
		all = append(all, contributed...)
	}
	settled, open := settle(nil, all)
	ordered := append(settled, orderToolResults(open)...)
	led := leadWithSystem(ordered, findBaseline(bounds.head, active))
	messages := make([]ai.Message, len(led))
	messageOrigins := make([]*origin, len(led))
	for i, message := range led {
		messages[i] = message.message
		if !message.synthetic {
			o := message.origin
			messageOrigins[i] = &o
		}
	}
	return ContextView{Head: bounds.head, Entries: active, Contributions: contributions, Messages: messages, contributionOrigins: contributionOrigins, messageOrigins: messageOrigins}
}

// findBaseline finds the baseline among entries, oldest first: the system
// message of the first pi.system entry after the head marker, or of the
// transcript without one, when generation wrote it with every section and
// tool in effect. After a head marker it always does, and omits the retained
// pi.system entries. Without one it does when it saw no system message
// before, which holds unless an earlier entry carries one, even if an edit
// omits it later. An edit that targets the baseline contributes other
// messages, or none, so none leads; a later entry does not take its place.
func findBaseline(head *durable.EntryRecord, entries []durable.EntryRecord) baseline {
	for i := range entries {
		entry := &entries[i]
		if head != nil && entry.ID <= head.ID {
			continue
		}
		if durable.SystemEntry.Is(entry) {
			for j, message := range entry.Model {
				if message.MessageRole() == ai.RoleSystem {
					return baseline{state: baselineFound, origin: origin{entry: entry.ID, index: j}}
				}
			}
			return baseline{state: baselineNone}
		}
		if head == nil && carriesSystemMessage(entry) {
			return baseline{state: baselineNone}
		}
	}
	return baseline{state: baselineUnknown}
}

// carriesSystemMessage reports whether an entry's own messages, or the
// replacement messages of its edits, include a system message.
func carriesSystemMessage(entry *durable.EntryRecord) bool {
	for _, message := range entry.Model {
		if message.MessageRole() == ai.RoleSystem {
			return true
		}
	}
	for _, edit := range entry.Edits {
		if edit.Action != durable.ContextEditReplace {
			continue
		}
		for _, message := range edit.Messages {
			if message.MessageRole() == ai.RoleSystem {
				return true
			}
		}
	}
	return false
}

// leadWithSystem moves the first system message to the front when only user
// messages precede it, or when it is the baseline's. A run's input is
// committed before generation renders the system prompt, so a transcript
// starts with the input followed by the baseline, and the range after a
// compaction with the kept history, assistant messages included. Providers
// treat only a leading system message as the initial prompt and tool set;
// without it, a later tool change rewrites the request's tool list and
// invalidates the whole prompt cache. Only the first system message moves, so
// replaying system messages in order gives the same sections and tools. Any
// other system message, before an assistant message, stays where it was
// committed: moving it would change what it means to the model.
func leadWithSystem(messages []sourced, base baseline) []sourced {
	index := -1
	for i, message := range messages {
		if message.message.MessageRole() == ai.RoleSystem {
			index = i
			break
		}
	}
	if index <= 0 {
		return messages
	}
	system := messages[index]
	isBaseline := base.state == baselineFound && !system.synthetic && system.origin == base.origin
	if !isBaseline {
		for _, message := range messages[:index] {
			if message.message.MessageRole() != ai.RoleUser {
				return messages
			}
		}
	}
	out := make([]sourced, 0, len(messages))
	out = append(out, system)
	out = append(out, messages[:index]...)
	return append(out, messages[index+1:]...)
}

// contribute returns one active entry's model messages after its edit and
// excluded stop reasons, before tool result ordering.
func contribute(entry durable.EntryRecord, edit *durable.ContextEdit) []sourced {
	if edit != nil && edit.Action == durable.ContextEditOmit {
		return nil
	}
	contributed, fromEdit := entry.Model, false
	if edit != nil && edit.Action == durable.ContextEditReplace {
		contributed, fromEdit = edit.Messages, true
	}
	var out []sourced
	for i, message := range contributed {
		if assistant, ok := assistantOf(message); ok && excludedStopReason(assistant.StopReason) {
			continue
		}
		out = append(out, sourced{message: message, origin: origin{entry: entry.ID, index: i, fromEdit: fromEdit}})
	}
	return out
}

func excludedStopReason(reason ai.StopReason) bool {
	return reason == ai.StopAborted || reason == ai.StopError || reason == ai.StopDeferred
}

// settle moves the ordered messages before the last assistant message of open
// to settled. orderToolResults of a sequence equals the concatenation over its
// parts when each later part starts with an assistant message.
func settle(settled, open []sourced) (newSettled, newOpen []sourced) {
	last := -1
	for i := len(open) - 1; i >= 0; i-- {
		if open[i].message.MessageRole() == ai.RoleAssistant {
			last = i
			break
		}
	}
	if last <= 0 {
		return settled, open
	}
	return append(append([]sourced(nil), settled...), orderToolResults(open[:last])...), open[last:]
}

// scanRange returns the visible entries within bounds, oldest first: from the
// head marker's head, or transcript start, through the tail.
func scanRange(ctx context.Context, storage durable.Storage, conversationID durable.ConversationID, bounds *contextBounds) ([]durable.EntryRecord, error) {
	query := durable.EntryQuery{ConversationID: conversationID, MaxEntryID: &bounds.tail}
	if bounds.head != nil {
		query.MinEntryID = bounds.head.Head
	}
	entries, err := scanAll(func(cursor durable.Cursor) (durable.Page[durable.EntryRecord], error) {
		return storage.ScanEntries(ctx, query, scanPageSize, cursor)
	})
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

// selectActive returns the head marker followed by the range's non-head
// entries, or the whole range without a marker.
func selectActive(head *durable.EntryRecord, entries []durable.EntryRecord) []durable.EntryRecord {
	if head == nil {
		return entries
	}
	active := []durable.EntryRecord{*head}
	for _, entry := range entries {
		if entry.Head == nil {
			active = append(active, entry)
		}
	}
	return active
}

// orderToolResults places each assistant's tool results directly after it in
// call order. Results are taken from the messages before the next assistant;
// a missing result is synthesized and unmatched results are dropped.
func orderToolResults(messages []sourced) []sourced {
	var ordered []sourced
	for index, message := range messages {
		if message.message.MessageRole() == ai.RoleToolResult {
			continue
		}
		ordered = append(ordered, message)
		assistant, ok := assistantOf(message.message)
		if !ok {
			continue
		}
		var calls []ai.ToolCall
		for _, content := range assistant.Content {
			if call, ok := toolCallOf(content); ok {
				calls = append(calls, call)
			}
		}
		if len(calls) == 0 {
			continue
		}
		results := map[string]int{}
		for next := index + 1; next < len(messages) && messages[next].message.MessageRole() != ai.RoleAssistant; next++ {
			if result, ok := toolResultOf(messages[next].message); ok {
				if _, seen := results[result.ToolCallID]; !seen {
					results[result.ToolCallID] = next
				}
			}
		}
		for _, call := range calls {
			if at, ok := results[call.ID]; ok {
				ordered = append(ordered, messages[at])
			} else {
				ordered = append(ordered, sourced{message: missingResult(call, assistant.Timestamp), synthetic: true})
			}
		}
	}
	return ordered
}

func missingResult(call ai.ToolCall, timestamp int64) ai.ToolResultMessage {
	return ai.ToolResultMessage{
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Content:    ai.ContentList{ai.TextContent{Text: missingResultText}},
		IsError:    true,
		Details:    map[string]any{"reason": "missing_result"},
		Timestamp:  timestamp,
	}
}

func assistantOf(message ai.Message) (ai.AssistantMessage, bool) {
	switch m := message.(type) {
	case ai.AssistantMessage:
		return m, true
	case *ai.AssistantMessage:
		if m != nil {
			return *m, true
		}
	}
	return ai.AssistantMessage{}, false
}

func toolResultOf(message ai.Message) (ai.ToolResultMessage, bool) {
	switch m := message.(type) {
	case ai.ToolResultMessage:
		return m, true
	case *ai.ToolResultMessage:
		if m != nil {
			return *m, true
		}
	}
	return ai.ToolResultMessage{}, false
}

func toolCallOf(content ai.Content) (ai.ToolCall, bool) {
	switch c := content.(type) {
	case ai.ToolCall:
		return c, true
	case *ai.ToolCall:
		if c != nil {
			return *c, true
		}
	}
	return ai.ToolCall{}, false
}
