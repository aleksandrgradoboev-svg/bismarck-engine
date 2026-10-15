package durable

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
)

// Detached copies of records, for storages that keep values in memory (pi's
// clone in storage/memory.ts). Strings are immutable in Go and are shared;
// everything a caller could change — byte slices, slices, maps, pointers — is
// copied. A copy through JSON would not do: it replaces malformed UTF-8, so
// two distinct string identities could become one.

func ptr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func bytesCopy(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	return slices.Clone(b)
}

// cloneMessages copies messages through their JSON form, which is their
// persisted form.
func cloneMessages(messages []ai.Message) []ai.Message {
	if messages == nil {
		return nil
	}
	out := make([]ai.Message, len(messages))
	for i, m := range messages {
		data, err := json.Marshal(m)
		if err != nil {
			panic(fmt.Sprintf("durable: a message does not encode: %v", err))
		}
		c, err := ai.UnmarshalMessage(data)
		if err != nil {
			panic(fmt.Sprintf("durable: a message does not decode: %v", err))
		}
		out[i] = c
	}
	return out
}

// Clone returns a detached copy.
func (r ConversationRecord) Clone() ConversationRecord {
	return ConversationRecord{ID: r.ID, Parent: ptr(r.Parent), Owner: ptr(r.Owner)}
}

// Clone returns a detached copy.
func (r EntryRecord) Clone() EntryRecord {
	out := r
	out.Model = cloneMessages(r.Model)
	out.Data = bytesCopy(r.Data)
	out.Head = ptr(r.Head)
	out.ByTaskID = ptr(r.ByTaskID)
	if r.Edits != nil {
		out.Edits = make([]ContextEdit, len(r.Edits))
		for i, e := range r.Edits {
			out.Edits[i] = ContextEdit{Target: e.Target, Action: e.Action, Messages: cloneMessages(e.Messages)}
		}
	}
	return out
}

// Clone returns a detached copy.
func (r SubmissionRecord) Clone() SubmissionRecord {
	out := r
	out.Entry = ptr(r.Entry)
	out.Answer = ptr(r.Answer)
	out.Detail = bytesCopy(r.Detail)
	return out
}

// Clone returns a detached copy.
func (o *TaskOutcome) Clone() *TaskOutcome {
	if o == nil {
		return nil
	}
	out := *o
	out.Result = bytesCopy(o.Result)
	if o.Error != nil {
		out.Error = &TaskOutcomeError{Message: o.Error.Message, Detail: bytesCopy(o.Error.Detail)}
	}
	return &out
}

// Clone returns a detached copy.
func (s TaskState) Clone() TaskState {
	return TaskState{
		Status:     s.Status,
		Checkpoint: bytesCopy(s.Checkpoint),
		On:         slices.Clone(s.On),
		Policy:     s.Policy,
		Outcome:    s.Outcome.Clone(),
	}
}

// Clone returns a detached copy.
func (r TaskRecord) Clone() TaskRecord {
	out := r
	out.Input = bytesCopy(r.Input)
	out.Owner = ptr(r.Owner)
	out.StartedAt = ptr(r.StartedAt)
	out.EndedAt = ptr(r.EndedAt)
	out.State = r.State.Clone()
	if r.Memos != nil {
		out.Memos = make(map[string]json.RawMessage, len(r.Memos))
		for k, v := range r.Memos {
			out.Memos[k] = bytesCopy(v)
		}
	}
	return out
}

// Clone returns a detached copy.
func (r DocumentCreate) Clone() DocumentCreate {
	out := r
	out.Key = ptr(r.Key)
	return out
}

// Clone returns a detached copy.
func (r DocumentRecord) Clone() DocumentRecord {
	out := r
	out.Key = ptr(r.Key)
	out.RetiredAt = ptr(r.RetiredAt)
	return out
}

// CloneValue copies a document value. The value must be strict JSON in
// chord's representation.
func CloneValue(v map[string]any) (map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	c, err := chord.CopyJSON(v)
	if err != nil {
		return nil, err
	}
	m, ok := c.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("durable: a document value is not a JSON object")
	}
	return m, nil
}

// cloneOps copies operations through their JSON form.
func cloneOps(ops []delta.Op) ([]delta.Op, error) {
	if ops == nil {
		return nil, nil
	}
	out := make([]delta.Op, len(ops))
	for i, op := range ops {
		data, err := json.Marshal(op)
		if err != nil {
			return nil, err
		}
		if out[i], err = delta.UnmarshalOp(data); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Clone returns a detached copy; it fails for a value that is not strict
// JSON.
func (c DocumentContent) Clone() (DocumentContent, error) {
	out := DocumentContent{Version: c.Version, Kind: c.Kind}
	var err error
	if out.Value, err = CloneValue(c.Value); err != nil {
		return DocumentContent{}, err
	}
	if out.Ops, err = cloneOps(c.Ops); err != nil {
		return DocumentContent{}, err
	}
	return out, nil
}

// CloneWrite returns a detached copy of a storage write.
func CloneWrite(w StorageWrite) (StorageWrite, error) {
	switch w := w.(type) {
	case ConversationWrite:
		return ConversationWrite{Value: w.Value.Clone()}, nil
	case EntryWrite:
		return EntryWrite{Value: w.Value.Clone()}, nil
	case TaskWrite:
		return TaskWrite{Value: w.Value.Clone()}, nil
	case SubmissionWrite:
		return SubmissionWrite{Value: w.Value.Clone()}, nil
	case DocumentCreateWrite:
		content, err := w.Content.Clone()
		if err != nil {
			return nil, err
		}
		return DocumentCreateWrite{Record: w.Record.Clone(), Content: content}, nil
	case DocumentCopyWrite:
		return DocumentCopyWrite{Record: w.Record.Clone(), Source: w.Source}, nil
	case DocumentChangeWrite:
		content, err := w.Content.Clone()
		if err != nil {
			return nil, err
		}
		return DocumentChangeWrite{ID: w.ID, Content: content}, nil
	case DocumentRetireWrite:
		return w, nil
	}
	return nil, fmt.Errorf("Unknown storage write: %T", w)
}
