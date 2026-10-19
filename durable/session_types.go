package durable

import (
	"encoding/json"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// The Session half of pi packages/durable/src/types.ts: what a commit
// callback stages, and what a commit publishes.

// Ownership kinds.
const (
	OwnershipOwnerless    = "ownerless"
	OwnershipConversation = "conversation"
	OwnershipTask         = "task"
)

// ConversationOwnership is selected explicitly whenever a conversation is
// created: "ownerless", or "task" with TaskID.
type ConversationOwnership struct {
	Kind   string
	TaskID TaskID
}

// Ownerless is the ownership of a conversation no task owns.
var Ownerless = ConversationOwnership{Kind: OwnershipOwnerless}

// TaskOwnership is who owns a task: its conversation ("conversation", a
// top-level task) or another task of the same conversation ("task", a child
// task).
type TaskOwnership struct {
	Kind   string
	TaskID TaskID
}

// TaskOptions are the creation options of a durable task.
type TaskOptions struct {
	// Ownership is required: a task always names its owner.
	Ownership TaskOwnership
	// ConversationID defaults to the owner task's conversation, or to the
	// transaction's bound conversation; it is required for a
	// conversation-owned task of a commit bound to none.
	ConversationID ConversationID
	// Background, for conversation-owned tasks only, excludes the task from
	// ordinary idle waits, conversation aborts and cascades.
	Background bool
	// AbandonOnRestart marks a task its creator awaits only in memory: a
	// later Harness abort-marks it, with reason "restart", before it runs
	// again.
	AbandonOnRestart bool
	// Trace is the task's telemetry parent. Nil inherits the carrier of the
	// commit's span (none without propagation); TraceRoot (pi's "root")
	// stores none, so the task's phases start at the adapter root.
	Trace     telemetry.Carrier
	TraceRoot bool
	// Links are attached to the spans of the task's first invocation.
	Links []telemetry.Carrier
	// TraceSubmissions, for the run a Harness starts with these inputs,
	// replace Trace, TraceRoot and Links (pi's internal submissionIds): the
	// first input's trace is the parent (none: TraceRoot), the others' traces
	// are links. They are read from this commit's own submissions, then from
	// Storage, past the rule that table reads precede the first table write;
	// a trace is immutable, so either is current.
	TraceSubmissions []SubmissionID
}

// TaskDefinition is what creating a task needs of its definition: the
// registered kind, the definition version, and the first checkpoint. The
// task runtime adds the phases.
type TaskDefinition interface {
	Name() string
	Version() int
	// Initial returns the first durable checkpoint for the input.
	Initial(input json.RawMessage) (json.RawMessage, error)
}

// EntryDraft is entry content before the Session assigns its identity and
// task attribution.
type EntryDraft struct {
	Kind  string
	Model []ai.Message
	Data  json.RawMessage
	// Head is the first entry of the active context this entry selects;
	// HeadSelf starts it at the entry itself (pi head: "self").
	Head     *EntryID
	HeadSelf bool
	Edits    []ContextEdit
}

// SubmissionSettlement is the terminal status staged for a submission:
// "done" with Answer, or "unanswered" with Reason and an optional Detail.
type SubmissionSettlement struct {
	Status string
	Answer EntryID
	Reason string
	Detail json.RawMessage
}

// CommitChange is one change of a commit publication: a ConversationWrite,
// EntryWrite, TaskWrite or SubmissionWrite with the complete record, a
// DocumentChange, or a DocumentCopyChange.
type CommitChange interface {
	// ChangeType is pi's discriminator: the write type, "document" or
	// "document.copy".
	ChangeType() string
}

func (w ConversationWrite) ChangeType() string { return w.WriteType() }
func (w EntryWrite) ChangeType() string        { return w.WriteType() }
func (w TaskWrite) ChangeType() string         { return w.WriteType() }
func (w SubmissionWrite) ChangeType() string   { return w.WriteType() }

// DocumentChange is the committed change of one document incarnation.
type DocumentChange struct {
	Record DocumentRecord
	// ConversationID owns the document; a task document's comes from its
	// task. Zero only for Session documents.
	ConversationID ConversationID
	// Version is the definition version of Value; zero when this commit
	// retired the incarnation.
	Version int
	// Value is the exact adopted immutable revision; nil when this commit
	// retired the incarnation.
	Value map[string]any
	// Ops are the exact adopted operations of an ordinary update; empty for
	// creation and retirement.
	Ops []delta.Op
}

// DocumentCopyChange is the definition-free creation of a forked
// conversation's document; consumers hydrate through a state or a watch.
type DocumentCopyChange struct {
	Record         DocumentRecord
	ConversationID ConversationID
	Source         DocumentCopySource
}

func (DocumentChange) ChangeType() string     { return "document" }
func (DocumentCopyChange) ChangeType() string { return "document.copy" }

// CommitPublication is every immutable change of one successful commit, in
// unspecified order.
type CommitPublication struct {
	Seq     Seq
	Changes []CommitChange
}

// Session end reasons.
const (
	SessionClosedReason = "closed"
	SessionFailedReason = "failed"
)

// SessionEnd is why a Session ended: Close, or a failed Storage call, whose
// error it carries.
type SessionEnd struct {
	Reason string
	Err    error
}
