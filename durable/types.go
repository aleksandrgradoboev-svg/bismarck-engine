package durable

import (
	"context"
	"encoding/json"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/ai"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// Records, queries and the Storage contract (pi packages/durable/src/types.ts,
// the persistence half; the task runtime and the Session are ported with the
// parts of durable that use them).
//
// JSON payloads — entry data, task input, checkpoints, results, memos,
// submission detail — are raw JSON: the bytes keep every shape, a null, and
// pi's key order through storage, as the session record's nested-call
// arguments do. Document values are chord values (chord.CopyJSON's
// representation), since documents change through chord/delta.

// ID is the session-global numeric ID namespace every record kind draws from
// (pi Id<Kind>): one counter, so IDs order records by creation across kinds.
type ID = int64

// The record identities. They share the ID namespace; the types keep them
// apart in Go code, as pi's brands do in TypeScript.
type (
	ConversationID int64
	EntryID        int64
	TaskID         int64
	SubmissionID   int64
	DocumentID     int64
)

// Seq is the strictly increasing sequence of one atomic storage commit; gaps
// are permitted. Commits start at 1.
type Seq int64

// RootConversationID is the reserved ID of the root conversation.
const RootConversationID ConversationID = 1

// ConversationParent is a fork's source: the conversation and the inclusive
// entry through which history is inherited.
type ConversationParent struct {
	ConversationID ConversationID `json:"conversationId"`
	At             EntryID        `json:"at"`
}

// ConversationOwner is the creator edge used for attribution, subtree abort
// and subtree idle waits.
type ConversationOwner struct {
	ConversationID ConversationID `json:"conversationId"`
	TaskID         TaskID         `json:"taskId"`
}

// ConversationRecord is the immutable identity, history ancestry and task
// ownership of a transcript scope.
type ConversationRecord struct {
	ID     ConversationID      `json:"id"`
	Parent *ConversationParent `json:"parent,omitempty"`
	Owner  *ConversationOwner  `json:"owner,omitempty"`
}

// ContextEdit actions.
const (
	ContextEditOmit    = "omit"
	ContextEditReplace = "replace"
)

// ContextEdit is an immutable override of one visible entry's contribution to
// model context: omit its messages, or replace them.
type ContextEdit struct {
	// Target is the entry whose model messages are omitted or replaced.
	Target EntryID `json:"target"`
	Action string  `json:"action"`
	// Messages are contributed instead of the target's, for "replace".
	Messages []ai.Message `json:"messages,omitempty"`
}

// UnmarshalJSON decodes the messages by role.
func (e *ContextEdit) UnmarshalJSON(data []byte) error {
	var raw struct {
		Target   EntryID           `json:"target"`
		Action   string            `json:"action"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	messages, err := unmarshalMessages(raw.Messages)
	if err != nil {
		return err
	}
	*e = ContextEdit{Target: raw.Target, Action: raw.Action, Messages: messages}
	return nil
}

// EntryRecord is an immutable transcript event with separate model-facing and
// application-facing payloads.
type EntryRecord struct {
	ID             EntryID        `json:"id"`
	ConversationID ConversationID `json:"conversationId"`
	// Kind is the application-defined entry discriminator.
	Kind string `json:"kind"`
	// Model are the messages contributed to model context; nil for display
	// or bookkeeping entries.
	Model []ai.Message `json:"model,omitempty"`
	// Data is the JSON payload views, extensions or bookkeeping consume.
	Data json.RawMessage `json:"data,omitempty"`
	// Head is the first entry of the active context this entry selects.
	Head *EntryID `json:"head,omitempty"`
	// Edits are context-only overrides of earlier visible entries.
	Edits []ContextEdit `json:"edits,omitempty"`
	// ByTaskID is the task that appended this entry, when durable work did.
	ByTaskID *TaskID `json:"byTaskId,omitempty"`
}

// UnmarshalJSON decodes the model messages by role.
func (e *EntryRecord) UnmarshalJSON(data []byte) error {
	type plain EntryRecord
	var raw struct {
		plain
		Model []json.RawMessage `json:"model"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	messages, err := unmarshalMessages(raw.Model)
	if err != nil {
		return err
	}
	*e = EntryRecord(raw.plain)
	e.Model = messages
	return nil
}

func unmarshalMessages(raw []json.RawMessage) ([]ai.Message, error) {
	if raw == nil {
		return nil, nil
	}
	messages := make([]ai.Message, len(raw))
	for i, r := range raw {
		m, err := ai.UnmarshalMessage(r)
		if err != nil {
			return nil, err
		}
		messages[i] = m
	}
	return messages, nil
}

// Submission types and statuses.
const (
	SubmissionTypeInput = "input"
	SubmissionTypeWrite = "write"

	// SubmissionQueued: admitted but not yet in the transcript.
	SubmissionQueued = "queued"
	// SubmissionPlaced: an input added to the transcript and owned by an
	// active run.
	SubmissionPlaced = "placed"
	// SubmissionDone: an answered input or an appended write.
	SubmissionDone = "done"
	// SubmissionUnanswered: terminal without an answer or a placement.
	SubmissionUnanswered = "unanswered"
)

// SubmissionRecord is the durable lifecycle of one admitted user input
// ("input") or passive entry write ("write"). Entry is set once it is placed
// ("placed", or "done" for a write), Answer once an input is "done", Reason
// and Detail once it is "unanswered".
type SubmissionRecord struct {
	ID             SubmissionID   `json:"id"`
	ConversationID ConversationID `json:"conversationId"`
	// RequestID is the host's deduplication key, scoped to the conversation.
	RequestID string `json:"requestId,omitempty"`
	// Trace is the adapter-owned context of the admission span; nil without
	// propagation.
	Trace  telemetry.Carrier `json:"trace,omitempty"`
	Type   string            `json:"type"`
	Status string            `json:"status"`
	Entry  *EntryID          `json:"entry,omitempty"`
	Answer *EntryID          `json:"answer,omitempty"`
	Reason string            `json:"reason,omitempty"`
	Detail json.RawMessage   `json:"detail,omitempty"`
}

// Task statuses.
const (
	// TaskPending: eligible for scheduling.
	TaskPending = "pending"
	// TaskRunning: reserved by one in-memory invocation.
	TaskRunning = "running"
	// TaskWaiting: parked until every task in On is terminal.
	TaskWaiting = "waiting"
	// TaskSleeping: parked without an invocation until the Harness clock
	// reaches Until; then it resumes at its Checkpoint.
	TaskSleeping = "sleeping"
	// TaskCompleting: outcome decided; terminal once no ordinary owned work
	// below is live.
	TaskCompleting = "completing"
	// TaskTerminal: permanently settled.
	TaskTerminal = "terminal"
)

// TaskStatuses are the task statuses in pi's order.
var TaskStatuses = []string{TaskPending, TaskRunning, TaskWaiting, TaskSleeping, TaskCompleting, TaskTerminal}

// SubmissionStatuses are the submission statuses in pi's order.
var SubmissionStatuses = []string{SubmissionQueued, SubmissionPlaced, SubmissionDone, SubmissionUnanswered}

// JoinPolicy is how a waiting task treats the tasks it waits on.
type JoinPolicy string

const (
	JoinFailFast   JoinPolicy = "failFast"
	JoinAllSettled JoinPolicy = "allSettled"
)

// Task outcome statuses.
const (
	OutcomeCompleted = "completed"
	// OutcomeFailed: an expected failure the task committed itself.
	OutcomeFailed = "failed"
	// OutcomeAborted: cancellation handled by the task's abort protocol.
	OutcomeAborted = "aborted"
	// OutcomeOrphaned: its definition or migration is unavailable.
	OutcomeOrphaned = "orphaned"
	// OutcomeFaulted: a runtime-detected contract failure.
	OutcomeFaulted = "faulted"
)

// TaskOutcomeError is a JSON-safe error snapshot.
type TaskOutcomeError struct {
	Message string          `json:"message"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// TaskOutcome is the durable result recorded when a task becomes terminal.
type TaskOutcome struct {
	Status string            `json:"status"`
	Result json.RawMessage   `json:"result,omitempty"`
	Error  *TaskOutcomeError `json:"error,omitempty"`
	Reason string            `json:"reason,omitempty"`
}

// TaskState is the complete durable execution state of a task: a Checkpoint
// while pending, running, waiting or sleeping (with On and Policy when
// waiting, Until when sleeping), an Outcome once completing or terminal.
type TaskState struct {
	Status     string          `json:"status"`
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	On         []TaskID        `json:"on,omitempty"`
	Policy     JoinPolicy      `json:"policy,omitempty"`
	// Until is a sleep's deadline, epoch milliseconds on the Harness clock;
	// a pointer, since 0 is a valid time.
	Until   *int64       `json:"until,omitempty"`
	Outcome *TaskOutcome `json:"outcome,omitempty"`
}

// Live reports whether the state can still run: pending, running, waiting or
// sleeping.
func (s TaskState) Live() bool {
	return s.Status == TaskPending || s.Status == TaskRunning || s.Status == TaskWaiting || s.Status == TaskSleeping
}

// SleepingState is the state of a task asleep until the Harness clock
// reaches until, resuming at checkpoint.
func SleepingState(checkpoint json.RawMessage, until int64) TaskState {
	return TaskState{Status: TaskSleeping, Checkpoint: checkpoint, Until: &until}
}

// AbortReasonRestart marks a task abandoned after a restart, or below one.
const AbortReasonRestart = "restart"

// TaskRecord is the complete replacement record of one durable task.
type TaskRecord struct {
	ID             TaskID         `json:"id"`
	ConversationID ConversationID `json:"conversationId"`
	// Kind is the registered task definition name.
	Kind string `json:"kind"`
	// Version is the definition version of the live input and checkpoint.
	Version int             `json:"version"`
	Input   json.RawMessage `json:"input"`
	// Trace is the adapter-owned context that created the task; its phase
	// invocations extract it as their parent. Immutable.
	Trace telemetry.Carrier `json:"trace,omitempty"`
	// Links are creation-time links attached to the spans of the task's
	// first invocation. Immutable.
	Links []telemetry.Carrier `json:"links,omitempty"`
	// Owner is the owning task of a child task; nil for a task its
	// conversation owns. Immutable.
	Owner *TaskID `json:"owner,omitempty"`
	// Background conversation-owned tasks are excluded from ordinary idle
	// waits, conversation aborts and cascades.
	Background bool `json:"background"`
	// AbortRequested is the durable abort mark.
	AbortRequested bool `json:"abortRequested"`
	// AbortReason is "restart" when the mark was set because the task was
	// abandoned after a restart, or below one.
	AbortReason string `json:"abortReason,omitempty"`
	// AbandonOnRestart is set from TaskOptions.AbandonOnRestart. Immutable.
	AbandonOnRestart bool `json:"abandonOnRestart,omitempty"`
	// StartedAt is the wall-clock milliseconds of the first change to
	// running; EndedAt of the change to terminal. Both stamped by the Session.
	StartedAt *int64    `json:"startedAt,omitempty"`
	EndedAt   *int64    `json:"endedAt,omitempty"`
	State     TaskState `json:"state"`
	// Memos are small first-writer-wins values kept while the task can run.
	Memos map[string]json.RawMessage `json:"memos,omitempty"`
	// Restarts counts the times in a row a crash cut the task off without a
	// state change since; a close does not count. Last, as pi appends the key
	// when it first counts one.
	Restarts int `json:"restarts,omitempty"`
}

// Document scope kinds.
const (
	ScopeSession      = "session"
	ScopeConversation = "conversation"
	ScopeTask         = "task"
)

// Document history and fork behaviors.
const (
	HistoryLatest     = "latest"
	HistoryRewindable = "rewindable"

	ForkCurrent = "current"
	ForkInitial = "initial"
	ForkAsOf    = "asOf"
)

// DocumentScope is who owns a document: the Session, a conversation or a
// task.
type DocumentScope struct {
	Kind           string         `json:"kind"`
	ConversationID ConversationID `json:"conversationId,omitempty"`
	TaskID         TaskID         `json:"taskId,omitempty"`
}

// SessionScope, ConversationScope and TaskScope build scopes.
func SessionScope() DocumentScope { return DocumentScope{Kind: ScopeSession} }
func ConversationScope(id ConversationID) DocumentScope {
	return DocumentScope{Kind: ScopeConversation, ConversationID: id}
}
func TaskScope(id TaskID) DocumentScope { return DocumentScope{Kind: ScopeTask, TaskID: id} }

// DocumentCreate is the identity of a new document incarnation; storage
// stamps CreatedAt (pi DocumentCreate). History and Fork are set for
// conversation documents only.
type DocumentCreate struct {
	// ID is the incarnation's; never reused when the same logical document
	// is recreated.
	ID   DocumentID `json:"id"`
	Kind string     `json:"kind"`
	// Key is the family member's key; nil for a singleton.
	Key     *string       `json:"key,omitempty"`
	Scope   DocumentScope `json:"scope"`
	History string        `json:"history,omitempty"`
	Fork    string        `json:"fork,omitempty"`
}

// DocumentRecord is the persisted lifecycle record of one create-to-retire
// document incarnation.
type DocumentRecord struct {
	ID      DocumentID    `json:"id"`
	Kind    string        `json:"kind"`
	Key     *string       `json:"key,omitempty"`
	Scope   DocumentScope `json:"scope"`
	History string        `json:"history,omitempty"`
	Fork    string        `json:"fork,omitempty"`
	// CreatedAt is the commit that created the incarnation.
	CreatedAt Seq `json:"createdAt"`
	// RetiredAt is the commit that retired it; nil while it is current.
	RetiredAt *Seq `json:"retiredAt,omitempty"`
}

// Create is the record's identity without the storage stamps.
func (r DocumentRecord) Create() DocumentCreate {
	return DocumentCreate{ID: r.ID, Kind: r.Kind, Key: r.Key, Scope: r.Scope, History: r.History, Fork: r.Fork}
}

// Address is the record's logical address.
func (r DocumentRecord) Address() DocumentAddress {
	return DocumentAddress{Kind: r.Kind, Scope: r.Scope, Key: r.Key}
}

// Page is one ordered scan result and its continuation: Next is nil on the
// last page.
type Page[T any] struct {
	Items []T
	Next  Cursor
}

// Cursor is backend-owned JSON continuation state that callers only
// round-trip to the same scan. It carries the scan's order: a scan given a
// cursor continues in that order and rejects a different query order.
type Cursor map[string]any

// ScanOrder is the ID order of a scan.
type ScanOrder string

const (
	// Ascending is oldest first, Descending newest first.
	Ascending  ScanOrder = "ascending"
	Descending ScanOrder = "descending"
)

// ConversationQuery filters a conversation scan (default ascending).
type ConversationQuery struct {
	OwnerConversationID *ConversationID
	OwnerTaskID         *TaskID
	Order               ScanOrder
}

// EntryQuery bounds a scan of one conversation's fork-aware history
// (default descending); both bounds are inclusive.
type EntryQuery struct {
	ConversationID ConversationID
	MinEntryID     *EntryID
	MaxEntryID     *EntryID
	Order          ScanOrder
}

// TaskQuery filters a task scan (default ascending); unset fields do not
// filter.
type TaskQuery struct {
	ConversationID *ConversationID
	Kind           string
	Status         string
	AbortRequested *bool
	Background     *bool
	Order          ScanOrder
}

// SubmissionQuery filters a submission scan (default ascending).
type SubmissionQuery struct {
	ConversationID *ConversationID
	Status         string
	Order          ScanOrder
}

// DocumentPoint is the current state or one historical commit sequence, for
// document membership and content reads (pi Seq | "current").
type DocumentPoint = Seq

// Current selects the current state; commit sequences start at 1.
const Current DocumentPoint = 0

// DocumentAddress is the exact logical identity of a singleton (Key nil) or
// one keyed family member.
type DocumentAddress struct {
	Kind  string
	Scope DocumentScope
	Key   *string
}

// DocumentQuery scans the document incarnations alive in one exact scope at
// one point.
type DocumentQuery struct {
	Scope DocumentScope
	At    DocumentPoint
	Kind  string
}

// Document content kinds.
const (
	ContentBase  = "base"
	ContentDelta = "delta"
)

// DocumentContent is a complete checkpoint ("base", Value) or a chord
// operation batch ("delta", Ops) selected by the owning Session.
type DocumentContent struct {
	Version int            `json:"version"`
	Kind    string         `json:"kind"`
	Value   map[string]any `json:"value,omitempty"`
	Ops     []delta.Op     `json:"ops,omitempty"`
}

// UnmarshalJSON decodes the value in chord's representation and the ops with
// delta.UnmarshalOp.
func (c *DocumentContent) UnmarshalJSON(data []byte) error {
	var raw struct {
		Version int               `json:"version"`
		Kind    string            `json:"kind"`
		Value   map[string]any    `json:"value"`
		Ops     []json.RawMessage `json:"ops"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = DocumentContent{Version: raw.Version, Kind: raw.Kind, Value: raw.Value}
	if raw.Ops != nil {
		c.Ops = make([]delta.Op, len(raw.Ops))
		for i, r := range raw.Ops {
			op, err := delta.UnmarshalOp(r)
			if err != nil {
				return err
			}
			c.Ops[i] = op
		}
	}
	return nil
}

// DocumentCopySource is the exact persisted source of a definition-free
// document copy.
type DocumentCopySource struct {
	ID DocumentID    `json:"id"`
	At DocumentPoint `json:"at"`
}

// StoredDocument is a detached materialized value and its stored definition
// version at a selected point.
type StoredDocument struct {
	Record  DocumentRecord
	Version int
	Value   map[string]any
	// DeltasSinceBase is the number of deltas replayed after the selected
	// base.
	DeltasSinceBase int
}

// StoredEntry is an entry and the sequence of the commit that persisted it.
type StoredEntry struct {
	Entry     EntryRecord
	CommitSeq Seq
}

// StorageWrite is one record or document mutation in an atomic storage
// commit: ConversationWrite, EntryWrite, TaskWrite, SubmissionWrite,
// DocumentCreateWrite, DocumentCopyWrite, DocumentChangeWrite or
// DocumentRetireWrite.
type StorageWrite interface {
	// WriteType is pi's discriminator: "conversation", "entry", "task",
	// "submission", "document.create", "document.copy", "document.change" or
	// "document.retire".
	WriteType() string
}

type (
	ConversationWrite struct{ Value ConversationRecord }
	EntryWrite        struct{ Value EntryRecord }
	TaskWrite         struct{ Value TaskRecord }
	SubmissionWrite   struct{ Value SubmissionRecord }
	// DocumentCreateWrite creates an incarnation with a base.
	DocumentCreateWrite struct {
		Record  DocumentCreate
		Content DocumentContent
	}
	// DocumentCopyWrite creates an incarnation from another's stored content.
	DocumentCopyWrite struct {
		Record DocumentCreate
		Source DocumentCopySource
	}
	// DocumentChangeWrite adds a base or a delta to an incarnation.
	DocumentChangeWrite struct {
		ID      DocumentID
		Content DocumentContent
	}
	// DocumentRetireWrite retires an incarnation.
	DocumentRetireWrite struct{ ID DocumentID }
)

func (ConversationWrite) WriteType() string   { return "conversation" }
func (EntryWrite) WriteType() string          { return "entry" }
func (TaskWrite) WriteType() string           { return "task" }
func (SubmissionWrite) WriteType() string     { return "submission" }
func (DocumentCreateWrite) WriteType() string { return "document.create" }
func (DocumentCopyWrite) WriteType() string   { return "document.copy" }
func (DocumentChangeWrite) WriteType() string { return "document.change" }
func (DocumentRetireWrite) WriteType() string { return "document.retire" }

// Storage is the atomic persistence boundary for Session records.
//
// Storage trusts the owning Session to supply semantically valid records,
// references, ancestry and transitions. Implementations enforce atomicity,
// global ID ownership, immutable conversation and entry creation, document
// record consistency and detached values; the Session serializes commits.
//
// An error a method returns is final: it fails the Session, which nothing
// retries, so retry transient failures inside the method. Two errors of a
// read fail only that read: a *StorageRequestError for an invalid request
// (an unknown conversation, a foreign or malformed cursor, history a document
// does not keep), which must have no durable effect, and the error of a
// cancelled ctx. Any error from Commit or MintID is fatal.
//
// Reads return detached values: the caller may change them without changing
// what is stored, and changing a committed write's values afterwards changes
// nothing stored either.
type Storage interface {
	// Commit atomically persists one batch and returns its sequence; later
	// reads observe it.
	Commit(ctx context.Context, writes []StorageWrite) (Seq, error)
	// MintID returns a fresh ID from the Session-global namespace, greater
	// than every ID minted or stored before, also across reopen: scans order
	// by ID as creation order.
	MintID(ctx context.Context) (ID, error)

	// Conversation looks up one conversation; nil when absent.
	Conversation(ctx context.Context, id ConversationID) (*ConversationRecord, error)
	// ScanConversations scans conversations by ID.
	ScanConversations(ctx context.Context, query ConversationQuery, limit int, cursor Cursor) (Page[ConversationRecord], error)

	// Entry looks up one entry globally.
	Entry(ctx context.Context, id EntryID) (*StoredEntry, error)
	// VisibleEntry looks up an entry only when it is visible through the
	// conversation's ancestry (pi entry(conversationId, id)).
	VisibleEntry(ctx context.Context, conversationID ConversationID, id EntryID) (*StoredEntry, error)
	// FindLatestHeadMarker returns the newest visible entry with a Head at
	// or below the optional inclusive cutoff; its Head is the range's actual
	// lower bound.
	FindLatestHeadMarker(ctx context.Context, conversationID ConversationID, atOrBefore *EntryID) (*EntryRecord, error)
	// ScanEntries scans the inclusive visible range, at most limit entries.
	ScanEntries(ctx context.Context, query EntryQuery, limit int, cursor Cursor) (Page[EntryRecord], error)

	// Task looks up the latest record of one task.
	Task(ctx context.Context, id TaskID) (*TaskRecord, error)
	// ScanTasks scans tasks matching every set filter, by ID.
	ScanTasks(ctx context.Context, query TaskQuery, limit int, cursor Cursor) (Page[TaskRecord], error)

	// Submission looks up the latest record of one submission.
	Submission(ctx context.Context, id SubmissionID) (*SubmissionRecord, error)
	// ScanSubmissions scans submissions matching every set filter, by ID.
	ScanSubmissions(ctx context.Context, query SubmissionQuery, limit int, cursor Cursor) (Page[SubmissionRecord], error)
	// SubmissionByRequest finds a submission by its conversation-scoped
	// deduplication key.
	SubmissionByRequest(ctx context.Context, conversationID ConversationID, requestID string) (*SubmissionRecord, error)

	// FindDocument resolves the incarnation at one exact address at the
	// selected point.
	FindDocument(ctx context.Context, address DocumentAddress, at DocumentPoint) (*DocumentRecord, error)
	// Document materializes one incarnation by ID at the selected point,
	// without following a replacement at its address.
	Document(ctx context.Context, id DocumentID, at DocumentPoint) (*StoredDocument, error)
	// ScanDocuments scans the incarnations alive in one exact scope at the
	// selected point.
	ScanDocuments(ctx context.Context, query DocumentQuery, limit int, cursor Cursor) (Page[DocumentRecord], error)

	// Close releases backend resources; later operations fail.
	Close(ctx context.Context) error
}
