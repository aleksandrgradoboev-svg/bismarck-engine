package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

// UserInput is the content of an input's user message, pi's
// UserMessage["content"]: a plain string or content blocks. The zero value is
// empty content blocks.
type UserInput struct {
	message ai.UserMessage
}

// TextInput is input as a plain string, stored as pi stores `content: "hi"`.
func TextInput(text string) UserInput { return UserInput{message: ai.NewUserText(text, 0)} }

// ContentInput is input as content blocks (text and images).
func ContentInput(content ai.ContentList) UserInput {
	return UserInput{message: ai.UserMessage{Content: append(ai.ContentList(nil), content...)}}
}

// Content returns the content blocks; a plain string is one text block.
func (u UserInput) Content() ai.ContentList { return append(ai.ContentList(nil), u.message.Content...) }

// userMessage is the user message of this input at timestamp.
func (u UserInput) userMessage(timestamp int64) ai.UserMessage {
	m := u.message
	m.Content = append(ai.ContentList(nil), m.Content...)
	m.Timestamp = timestamp
	return m
}

// MarshalJSON writes the content as pi does: a string or a block array.
func (u UserInput) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(u.message)
	if err != nil {
		return nil, err
	}
	var fields struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if len(fields.Content) == 0 || string(fields.Content) == "null" {
		return []byte("[]"), nil
	}
	return fields.Content, nil
}

// UnmarshalJSON reads a string or a block array.
func (u *UserInput) UnmarshalJSON(data []byte) error {
	wrapped := append(append([]byte(`{"role":"user","content":`), data...), '}')
	var m ai.UserMessage
	if err := json.Unmarshal(wrapped, &m); err != nil {
		return err
	}
	m.Timestamp = 0
	u.message = m
	return nil
}

// InputEntry is the entry an input is placed as instead of pi.user: an
// application kind, and data the transcript shows, such as its author. Its
// model message is still the input's user message.
type InputEntry struct {
	Kind string
	Data json.RawMessage
}

// inboxItem is a queued submission: user input for a run (mode steer or
// followUp, with the entry it is placed as when not pi.user), or an entry
// write (mode write).
type inboxItem struct {
	id      durable.SubmissionID
	mode    string
	content UserInput
	input   *InputEntry
	write   durable.EntryDraft
}

const (
	modeSteer    = "steer"
	modeFollowUp = "followUp"
	modeWrite    = "write"
)

// value is the item as pi.inbox stores it.
func (item inboxItem) value() (map[string]any, error) {
	out := map[string]any{"id": float64(item.id), "mode": item.mode}
	if item.mode == modeWrite {
		entry, err := entryDraftValue(item.write)
		if err != nil {
			return nil, err
		}
		out["entry"] = entry
		return out, nil
	}
	content, err := toValue(item.content)
	if err != nil {
		return nil, err
	}
	out["content"] = content
	if item.input != nil {
		entry, err := inputEntryValue(*item.input)
		if err != nil {
			return nil, err
		}
		out["entry"] = entry
	}
	return out, nil
}

func decodeInboxItem(value any) (inboxItem, error) {
	fields, ok := value.(map[string]any)
	if !ok {
		return inboxItem{}, errors.New("pi.inbox item is not an object")
	}
	id, _ := fields["id"].(float64)
	mode, _ := fields["mode"].(string)
	item := inboxItem{id: durable.SubmissionID(id), mode: mode}
	if mode == modeWrite {
		entry, _ := fields["entry"].(map[string]any)
		draft, err := decodeEntryDraft(entry)
		if err != nil {
			return inboxItem{}, err
		}
		item.write = draft
		return item, nil
	}
	if err := decodeValue(fields["content"], &item.content); err != nil {
		return inboxItem{}, err
	}
	if entry, ok := fields["entry"].(map[string]any); ok {
		kind, _ := entry["kind"].(string)
		input := InputEntry{Kind: kind}
		if data, present := entry["data"]; present {
			raw, err := json.Marshal(data)
			if err != nil {
				return inboxItem{}, err
			}
			input.Data = raw
		}
		item.input = &input
	}
	return item, nil
}

// decodeValue decodes a document value into out.
func decodeValue(value any, out any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// inputEntryValue is the kind and data of an input's entry as strict JSON,
// without anything else.
func inputEntryValue(entry InputEntry) (map[string]any, error) {
	out := map[string]any{"kind": entry.Kind}
	if entry.Data != nil {
		var data any
		if err := json.Unmarshal(entry.Data, &data); err != nil {
			return nil, fmt.Errorf("input entry data: %w", err)
		}
		out["data"] = data
	}
	return out, nil
}

// entryDraftValue is an EntryDraft as pi stores it in an inbox item: present
// fields are written even when empty, as they count for isDisplayOnly, and
// head is "self" or an entry ID.
func entryDraftValue(draft durable.EntryDraft) (map[string]any, error) {
	out := map[string]any{"kind": draft.Kind}
	if draft.Model != nil {
		model, err := toValue(draft.Model)
		if err != nil {
			return nil, err
		}
		out["model"] = model
	}
	if draft.Data != nil {
		var data any
		if err := json.Unmarshal(draft.Data, &data); err != nil {
			return nil, fmt.Errorf("entry data: %w", err)
		}
		out["data"] = data
	}
	if draft.HeadSelf {
		out["head"] = "self"
	} else if draft.Head != nil {
		out["head"] = float64(*draft.Head)
	}
	if draft.Edits != nil {
		edits, err := toValue(draft.Edits)
		if err != nil {
			return nil, err
		}
		out["edits"] = edits
	}
	return out, nil
}

func decodeEntryDraft(value map[string]any) (durable.EntryDraft, error) {
	if value == nil {
		return durable.EntryDraft{}, errors.New("pi.inbox write item has no entry")
	}
	kind, _ := value["kind"].(string)
	draft := durable.EntryDraft{Kind: kind}
	if model, present := value["model"]; present {
		var raw []json.RawMessage
		if err := decodeValue(model, &raw); err != nil {
			return durable.EntryDraft{}, err
		}
		draft.Model = make([]ai.Message, 0, len(raw))
		for _, r := range raw {
			message, err := ai.UnmarshalMessage(r)
			if err != nil {
				return durable.EntryDraft{}, err
			}
			draft.Model = append(draft.Model, message)
		}
	}
	if data, present := value["data"]; present {
		raw, err := json.Marshal(data)
		if err != nil {
			return durable.EntryDraft{}, err
		}
		draft.Data = raw
	}
	switch head := value["head"].(type) {
	case string:
		draft.HeadSelf = head == "self"
	case float64:
		id := durable.EntryID(head)
		draft.Head = &id
	}
	if edits, present := value["edits"]; present {
		draft.Edits = []durable.ContextEdit{}
		if err := decodeValue(edits, &draft.Edits); err != nil {
			return durable.EntryDraft{}, err
		}
	}
	return draft, nil
}

// boundary is what a boundary reads before the commit's first table write,
// and the newest head it has seen so far.
type boundary struct {
	conversationID durable.ConversationID
	// inbox is the pi.inbox draft; its items are read when applied, so items
	// queued after preparation count.
	inbox        *delta.Draft
	steeringMode QueueMode
	followUpMode QueueMode
	// head starts the active range: the newest head marker's head, advanced
	// by heads written in this commit.
	head *durable.EntryID
}

// boundaryResult is the selected user items, in ID order, and whether a reset
// (head "self" write) was placed.
type boundaryResult struct {
	users []durable.SubmissionID
	reset bool
}

// inboxDraft returns the pi.inbox draft of a conversation.
func inboxDraft(tx *durable.Tx, conversationID durable.ConversationID) (*delta.Draft, error) {
	return tx.Doc(InboxDoc.Conversation(conversationID))
}

// inboxItems decodes the queued items of a pi.inbox draft.
func inboxItems(inbox *delta.Draft) ([]inboxItem, error) {
	itemsDraft := inbox.At("items")
	if itemsDraft == nil {
		return nil, errors.New("pi.inbox has no items")
	}
	snapshot, err := itemsDraft.Snapshot()
	if err != nil {
		return nil, err
	}
	values, _ := snapshot.([]any)
	items := make([]inboxItem, 0, len(values))
	for _, value := range values {
		item, err := decodeInboxItem(value)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// pushInboxItem queues item at the end of a pi.inbox draft.
func pushInboxItem(inbox *delta.Draft, item inboxItem) error {
	value, err := item.value()
	if err != nil {
		return err
	}
	_, err = inbox.At("items").Push(value)
	return err
}

// prepareBoundary reads what a boundary needs. Table reads must precede the
// commit's first table write, so callers prepare the boundary at the start of
// their commit.
func prepareBoundary(tx *durable.Tx, conversationID durable.ConversationID, settings Settings) (*boundary, error) {
	marker, err := tx.LatestHeadMarker(conversationID)
	if err != nil {
		return nil, err
	}
	inbox, err := inboxDraft(tx, conversationID)
	if err != nil {
		return nil, err
	}
	b := &boundary{conversationID: conversationID, inbox: inbox, steeringMode: settings.SteeringMode, followUpMode: settings.FollowUpMode}
	if marker != nil && marker.Head != nil {
		head := *marker.Head
		b.head = &head
	}
	return b, nil
}

// applyBoundary places the queued items a boundary selects (spec §6): every
// write, the first or all steers, and at final the first or all follow-ups. A
// selected reset turns a postTools boundary into final. Writes are placed
// first and user items after them, each in ID order, so user items queued
// before a reset run in the new context. A write whose head targets an entry
// before the active range, including a range started earlier in this commit,
// is stale. Selected and stale items are removed positionally.
func applyBoundary(tx *durable.Tx, b *boundary, final bool, now int64) (boundaryResult, error) {
	items, err := inboxItems(b.inbox)
	if err != nil {
		return boundaryResult{}, err
	}
	reset := hasQueuedReset(items)
	final = final || reset
	pick := func(mode string, queueMode QueueMode) []int {
		var indexes []int
		for index, item := range items {
			if item.mode == mode {
				indexes = append(indexes, index)
			}
		}
		if queueMode != QueueAll && len(indexes) > 1 {
			indexes = indexes[:1]
		}
		return indexes
	}
	var writes []int
	for index, item := range items {
		if item.mode == modeWrite {
			writes = append(writes, index)
		}
	}
	users := pick(modeSteer, b.steeringMode)
	if final {
		users = append(users, pick(modeFollowUp, b.followUpMode)...)
	}
	sort.Ints(users)

	for _, index := range writes {
		item := items[index]
		if isStale(b, item.write) {
			if err := tx.SettleSubmission(item.id, durable.SubmissionSettlement{Status: durable.SubmissionUnanswered, Reason: "stale"}); err != nil {
				return boundaryResult{}, err
			}
			continue
		}
		entry, err := tx.AppendEntry(b.conversationID, item.write)
		if err != nil {
			return boundaryResult{}, err
		}
		if item.write.HeadSelf {
			head := entry.ID
			b.head = &head
		} else if item.write.Head != nil {
			head := *item.write.Head
			b.head = &head
		}
		if err := tx.PlaceSubmission(item.id, entry.ID); err != nil {
			return boundaryResult{}, err
		}
	}
	placed := []durable.SubmissionID{}
	for _, index := range users {
		item := items[index]
		entry, err := appendInput(tx, b.conversationID, item.content, item.input, now)
		if err != nil {
			return boundaryResult{}, err
		}
		if err := tx.PlaceSubmission(item.id, entry.ID); err != nil {
			return boundaryResult{}, err
		}
		placed = append(placed, item.id)
	}
	removed := append(append([]int(nil), writes...), users...)
	sort.Ints(removed)
	itemsDraft := b.inbox.At("items")
	for i := len(removed) - 1; i >= 0; i-- {
		if _, err := itemsDraft.Splice(removed[i], 1); err != nil {
			return boundaryResult{}, err
		}
	}
	return boundaryResult{users: placed, reset: reset}, nil
}

// appendInput appends the entry an input is placed as: entry's kind and data,
// or pi.user, with the user message of content.
func appendInput(tx *durable.Tx, conversationID durable.ConversationID, content UserInput, entry *InputEntry, now int64) (durable.EntryRecord, error) {
	draft := durable.EntryDraft{Kind: durable.UserEntry.Kind, Model: []ai.Message{content.userMessage(now)}}
	if entry != nil {
		picked := pickInputEntry(*entry)
		draft.Kind, draft.Data = picked.Kind, picked.Data
	}
	return tx.AppendEntry(conversationID, draft)
}

// pickInputEntry is the kind and data of an input's entry, copied.
func pickInputEntry(entry InputEntry) InputEntry {
	return InputEntry{Kind: entry.Kind, Data: append(json.RawMessage(nil), entry.Data...)}
}

// checkInputEntry rejects an input entry whose kind is empty or starts with
// "pi.".
func checkInputEntry(entry InputEntry) error {
	if entry.Kind == "" || strings.HasPrefix(entry.Kind, "pi.") {
		return fmt.Errorf(`An input entry's kind must be a non-empty string not starting with "pi.", got %s`, entry.Kind)
	}
	return nil
}

// isDisplayOnly reports whether a write changes nothing the model sees: no
// model, head, or edits (present counts, even if empty), so it needs no
// boundary.
func isDisplayOnly(entry durable.EntryDraft) bool {
	return entry.Model == nil && entry.Head == nil && !entry.HeadSelf && entry.Edits == nil
}

// hasQueuedReset reports whether a reset (head "self" write) is queued;
// entries appended before it leave the active range when it lands.
func hasQueuedReset(items []inboxItem) bool {
	for _, item := range items {
		if item.mode == modeWrite && item.write.HeadSelf {
			return true
		}
	}
	return false
}

// isStale reports whether a head write targets an entry before the active
// range, so placing it would bring back cut history.
func isStale(b *boundary, entry durable.EntryDraft) bool {
	return entry.Head != nil && b.head != nil && *entry.Head < *b.head
}

// removeInboxItem removes a withdrawn submission's item; the caller settles
// the submission.
func removeInboxItem(tx *durable.Tx, conversationID durable.ConversationID, id durable.SubmissionID) error {
	inbox, err := inboxDraft(tx, conversationID)
	if err != nil {
		return err
	}
	items, err := inboxItems(inbox)
	if err != nil {
		return err
	}
	for index, item := range items {
		if item.id == id {
			_, err := inbox.At("items").Splice(index, 1)
			return err
		}
	}
	return nil
}

// withdrawQueuedInputs withdraws every queued input of a conversation, as
// Conversation.Abort and abort cascades do: each settles unanswered with
// reason and leaves the inbox; queued writes stay for later placement.
func withdrawQueuedInputs(tx *durable.Tx, conversationID durable.ConversationID, reason string) error {
	inbox, err := inboxDraft(tx, conversationID)
	if err != nil {
		return err
	}
	items, err := inboxItems(inbox)
	if err != nil {
		return err
	}
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		if item.mode == modeWrite {
			continue
		}
		if err := tx.SettleSubmission(item.id, durable.SubmissionSettlement{Status: durable.SubmissionUnanswered, Reason: reason}); err != nil {
			return err
		}
		if _, err := inbox.At("items").Splice(index, 1); err != nil {
			return err
		}
	}
	return nil
}
