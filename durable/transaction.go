package durable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/telemetry"
)

// The transaction of one Session commit callback
// (pi packages/durable/src/session/transaction.ts).
//
// pi's operations are promises, and its single thread makes every stretch
// between two awaits atomic. Here operations are blocking calls the callback
// may make from several goroutines: each such stretch runs under the
// transaction's mutex, and Storage is called outside it. An operation still
// running when the callback returns is counted, so settlement rejects the
// commit and waits for it, as pi drains its unsettled promises.

// errTxSettled is the error of every operation after the callback settled.
var errTxSettled = errors.New("Transaction has settled")

const internalScanPageSize = 256

// loadedDocument is one committed document incarnation owned by the Session
// tracker cache. storedVersion and deltasSinceBase change at adoption, under
// the Session's mutex.
type loadedDocument struct {
	addressID string
	record    DocumentRecord
	// storedVersion is the persisted definition version; older while the
	// tracked value is migrated only in memory.
	storedVersion int
	// valueVersion is the definition version the tracked value has; access
	// with another version reloads from Storage.
	valueVersion int
	// deltasSinceBase are the stored deltas after the newest base.
	deltasSinceBase int
	tracker         *delta.Tracker[map[string]any]
}

// TransactionScope is what a commit binds to: the default Tx.CreateTask
// conversation, and the task attributed to appended entries.
type TransactionScope struct {
	ConversationID ConversationID
	// TaskID is the task whose runtime commit this is; stamped as ByTaskID
	// on appended entries.
	TaskID TaskID
}

// onceResult is a result several goroutines wait for, pi's shared promise.
type onceResult[T any] struct {
	done  chan struct{}
	value T
	err   error
}

func newOnce[T any]() *onceResult[T] { return &onceResult[T]{done: make(chan struct{})} }

func (o *onceResult[T]) settle(value T, err error) {
	o.value, o.err = value, err
	close(o.done)
}

func (o *onceResult[T]) wait() (T, error) {
	<-o.done
	return o.value, o.err
}

// txTask is the committed and candidate state of one task the transaction
// touched.
type txTask struct {
	committed *onceResult[*TaskRecord]
	// write is the candidate record; create tells a new task from a
	// replacement.
	write  *TaskRecord
	create bool
	// publicationConversationID is the conversation that owns the task's
	// documents in publications.
	publicationConversationID ConversationID
}

// submissionChange is a staged settlement, or the placement of a queued
// submission at its entry (place).
type submissionChange struct {
	id         SubmissionID
	settlement SubmissionSettlement
	place      bool
	entry      EntryID
}

type targetKind int

const (
	targetLoaded targetKind = iota + 1
	targetCreated
	targetForkCopy
	targetRetireOnly
)

// documentTarget is the storage and cache provenance of one staged
// incarnation.
type documentTarget struct {
	kind targetKind
	// loaded, for targetLoaded.
	loaded *loadedDocument
	// create and, for targetCreated, version and tracker; for
	// targetForkCopy, source.
	create  DocumentCreate
	version int
	tracker *delta.Tracker[map[string]any]
	source  DocumentCopySource
	// record, for targetRetireOnly.
	record DocumentRecord
}

// documentEntry is one document incarnation acquired, created or retired by
// the transaction.
type documentEntry struct {
	addressID string
	address   DocumentAddress
	// def is nil for definition-free fork copies and for retirements found
	// by a terminal-task scan.
	def *DocDef
	// acquisition is the memoized public acquisition; nil for metadata-only
	// retirement.
	acquisition    *onceResult[*delta.Draft]
	target         *documentTarget
	change         *delta.Change[map[string]any]
	prepared       *delta.Prepared[map[string]any]
	retireOnCommit bool
}

// planChange is the prepared change of a tracked incarnation.
type planChange struct {
	tracker  *delta.Tracker[map[string]any]
	prepared *delta.Prepared[map[string]any]
	version  int
	// loaded is the cached incarnation the change updates; nil when it
	// creates one.
	loaded *loadedDocument
	def    *DocDef
}

// documentPlan is what one staged incarnation writes and publishes, decided
// once before Storage admission so adoption only applies it.
type documentPlan struct {
	addressID string
	// committed incarnations have record; new ones only create.
	committed bool
	record    DocumentRecord
	create    DocumentCreate
	retire    bool
	// content is the create, copy or change write; nil when only
	// retirement is written.
	content StorageWrite
	// change is nil for fork copies and retirement-only entries.
	change *planChange
	// conversationID is resolved before admission so adoption performs no
	// reads.
	conversationID ConversationID
}

func (p *documentPlan) identity() DocumentCreate {
	if p.committed {
		return p.record.Create()
	}
	return p.create
}

// Tx is the transaction of one Session commit callback. Table reads and
// creation results are trusted immutable values and may be shared with the
// commit's state: do not modify them.
type Tx struct {
	session *Session
	ctx     context.Context
	scope   TransactionScope

	mu            sync.Mutex
	idle          *sync.Cond
	pending       int
	sealed        bool
	hasTableWrite bool

	// writes is the atomic batch: conversation and entry writes stage at
	// once, task and document writes at assembly.
	writes                  []StorageWrite
	createdConversations    map[ConversationID]bool
	forkSourceConversations map[ConversationID]bool
	forkSourceDocuments     map[DocumentID]bool
	taskOrder               []TaskID
	tasks                   map[TaskID]*txTask
	submissionOrder         []SubmissionID
	submissions             map[SubmissionID]SubmissionRecord
	submissionChanges       []submissionChange

	plans     []*documentPlan
	documents []*documentEntry
	// latest is the latest transaction-local incarnation or retirement
	// marker at each logical address.
	latest map[string]*documentEntry
}

func newTx(session *Session, ctx context.Context, scope TransactionScope) *Tx {
	tx := &Tx{
		session:                 session,
		ctx:                     ctx,
		scope:                   scope,
		createdConversations:    map[ConversationID]bool{},
		forkSourceConversations: map[ConversationID]bool{},
		forkSourceDocuments:     map[DocumentID]bool{},
		tasks:                   map[TaskID]*txTask{},
		submissions:             map[SubmissionID]SubmissionRecord{},
		latest:                  map[string]*documentEntry{},
	}
	tx.idle = sync.NewCond(&tx.mu)
	return tx
}

func (tx *Tx) storage() Storage { return tx.session.storage }

// ─── Operation bookkeeping ───────────────────────────────────────────────

// endOp ends one counted operation.
func (tx *Tx) endOp() {
	tx.mu.Lock()
	tx.pending--
	if tx.pending == 0 {
		tx.idle.Broadcast()
	}
	tx.mu.Unlock()
}

// check fails once the callback settled; operations call it after every
// Storage call, where pi's assertOpen follows an await.
func (tx *Tx) check() error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.sealed {
		return errTxSettled
	}
	return nil
}

// read starts a counted table read: none after the first table write.
func (tx *Tx) read(method string) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.sealed {
		return errTxSettled
	}
	if tx.hasTableWrite {
		return &ReadAfterWriteError{Method: method}
	}
	tx.pending++
	return nil
}

// write starts a counted table write.
func (tx *Tx) write() error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.sealed {
		return errTxSettled
	}
	tx.hasTableWrite = true
	tx.pending++
	return nil
}

// ─── Table reads ─────────────────────────────────────────────────────────

// Conversation reads one committed conversation; nil when absent.
func (tx *Tx) Conversation(id ConversationID) (*ConversationRecord, error) {
	if err := tx.read("conversation"); err != nil {
		return nil, err
	}
	defer tx.endOp()
	return tx.storage().Conversation(tx.ctx, id)
}

// Entry reads one committed entry; nil when absent.
func (tx *Tx) Entry(id EntryID) (*EntryRecord, error) {
	if err := tx.read("entry"); err != nil {
		return nil, err
	}
	defer tx.endOp()
	stored, err := tx.storage().Entry(tx.ctx, id)
	if err != nil || stored == nil {
		return nil, err
	}
	return &stored.Entry, nil
}

// Task reads one committed task; nil when absent. The record is the
// caller's copy: changing it before SetTask leaves the committed record the
// commit validates against untouched.
func (tx *Tx) Task(id TaskID) (*TaskRecord, error) {
	if err := tx.read("task"); err != nil {
		return nil, err
	}
	defer tx.endOp()
	record, err := tx.committedTask(id)
	if record == nil || err != nil {
		return record, err
	}
	copied := record.Clone()
	return &copied, nil
}

// ScanConversations scans committed conversations.
func (tx *Tx) ScanConversations(query ConversationQuery, limit int, cursor Cursor) (Page[ConversationRecord], error) {
	if err := tx.read("scanConversations"); err != nil {
		return Page[ConversationRecord]{}, err
	}
	defer tx.endOp()
	return tx.storage().ScanConversations(tx.ctx, query, limit, cursor)
}

// ScanEntries scans a conversation's committed visible entries.
func (tx *Tx) ScanEntries(query EntryQuery, limit int, cursor Cursor) (Page[EntryRecord], error) {
	if err := tx.read("scanEntries"); err != nil {
		return Page[EntryRecord]{}, err
	}
	defer tx.endOp()
	return tx.storage().ScanEntries(tx.ctx, query, limit, cursor)
}

// LatestHeadMarker is the newest visible entry of the conversation that
// carries a Head.
func (tx *Tx) LatestHeadMarker(conversationID ConversationID) (*EntryRecord, error) {
	if err := tx.read("latestHeadMarker"); err != nil {
		return nil, err
	}
	defer tx.endOp()
	return tx.storage().FindLatestHeadMarker(tx.ctx, conversationID, nil)
}

// ScanTasks scans committed tasks.
func (tx *Tx) ScanTasks(query TaskQuery, limit int, cursor Cursor) (Page[TaskRecord], error) {
	if err := tx.read("scanTasks"); err != nil {
		return Page[TaskRecord]{}, err
	}
	defer tx.endOp()
	return tx.storage().ScanTasks(tx.ctx, query, limit, cursor)
}

// Submission reads one committed submission; for the harness.
func (tx *Tx) Submission(id SubmissionID) (*SubmissionRecord, error) {
	if err := tx.read("submission"); err != nil {
		return nil, err
	}
	defer tx.endOp()
	return tx.storage().Submission(tx.ctx, id)
}

// SubmissionByRequest reads the committed submission with a
// conversation-scoped request ID.
func (tx *Tx) SubmissionByRequest(conversationID ConversationID, requestID string) (*SubmissionRecord, error) {
	if err := tx.read("submissionByRequest"); err != nil {
		return nil, err
	}
	defer tx.endOp()
	return tx.storage().SubmissionByRequest(tx.ctx, conversationID, requestID)
}

// ─── Table writes ────────────────────────────────────────────────────────

// CreateConversation creates a conversation with explicitly selected
// ownership.
func (tx *Tx) CreateConversation(ownership ConversationOwnership) (ConversationRecord, error) {
	if err := tx.write(); err != nil {
		return ConversationRecord{}, err
	}
	defer tx.endOp()
	return tx.stageConversation(nil, ownership, 0)
}

// CreateRootConversation creates the reserved root conversation; for the
// harness's bootstrap.
func (tx *Tx) CreateRootConversation() (ConversationRecord, error) {
	if err := tx.write(); err != nil {
		return ConversationRecord{}, err
	}
	defer tx.endOp()
	return tx.stageConversation(nil, Ownerless, RootConversationID)
}

// ForkConversation creates a history fork at one visible entry with
// explicitly selected ownership.
func (tx *Tx) ForkConversation(parent ConversationID, at EntryID, ownership ConversationOwnership) (ConversationRecord, error) {
	if err := tx.write(); err != nil {
		return ConversationRecord{}, err
	}
	defer tx.endOp()
	return tx.stageConversation(&ConversationParent{ConversationID: parent, At: at}, ownership, 0)
}

func (tx *Tx) stageConversation(parent *ConversationParent, ownership ConversationOwnership, reserved ConversationID) (ConversationRecord, error) {
	id := reserved
	if id == 0 {
		minted, err := tx.storage().MintID(tx.ctx)
		if err != nil {
			return ConversationRecord{}, err
		}
		id = ConversationID(minted)
	}
	if err := tx.check(); err != nil {
		return ConversationRecord{}, err
	}
	record := ConversationRecord{ID: id, Parent: parent}
	if ownership.Kind == OwnershipTask {
		task, err := tx.currentTask(ownership.TaskID)
		if err != nil {
			return ConversationRecord{}, err
		}
		if err := tx.check(); err != nil {
			return ConversationRecord{}, err
		}
		if task == nil {
			return ConversationRecord{}, fmt.Errorf("Conversation owner task %d does not exist", ownership.TaskID)
		}
		record.Owner = &ConversationOwner{ConversationID: task.ConversationID, TaskID: ownership.TaskID}
	}
	var copies []forkDocumentCopy
	if parent != nil {
		var err error
		if copies, err = prepareForkDocumentCopies(tx.ctx, tx.storage(), parent.ConversationID, parent.At, id); err != nil {
			return ConversationRecord{}, err
		}
	}
	tx.mu.Lock()
	if tx.sealed {
		tx.mu.Unlock()
		return ConversationRecord{}, errTxSettled
	}
	for _, c := range copies {
		tx.forkSourceDocuments[c.source.ID] = true
		entry := &documentEntry{
			addressID: addressID(createAddress(c.record)),
			address:   createAddress(c.record),
			target:    &documentTarget{kind: targetForkCopy, create: c.record, source: c.source},
		}
		tx.documents = append(tx.documents, entry)
		tx.latest[entry.addressID] = entry
	}
	if parent != nil {
		tx.forkSourceConversations[parent.ConversationID] = true
	}
	tx.createdConversations[id] = true
	tx.writes = append(tx.writes, ConversationWrite{Value: record})
	tx.mu.Unlock()
	if hook := tx.session.hooks.ConversationCreated; hook != nil {
		if err := hook(tx, record); err != nil {
			return ConversationRecord{}, err
		}
	}
	if err := tx.check(); err != nil {
		return ConversationRecord{}, err
	}
	return record, nil
}

// AppendEntry appends an entry. The returned record is Session-owned and
// may be shared with commit listeners.
func (tx *Tx) AppendEntry(conversationID ConversationID, draft EntryDraft) (EntryRecord, error) {
	if err := tx.write(); err != nil {
		return EntryRecord{}, err
	}
	defer tx.endOp()
	if err := tx.requireConversation(conversationID); err != nil {
		return EntryRecord{}, err
	}
	if err := tx.check(); err != nil {
		return EntryRecord{}, err
	}
	minted, err := tx.storage().MintID(tx.ctx)
	if err != nil {
		return EntryRecord{}, err
	}
	id := EntryID(minted)
	if draft.Data != nil && !json.Valid(draft.Data) {
		return EntryRecord{}, fmt.Errorf("Entry data is not strict JSON")
	}
	record := EntryRecord{
		ID:             id,
		ConversationID: conversationID,
		Kind:           draft.Kind,
		Model:          draft.Model,
		Data:           draft.Data,
		Head:           draft.Head,
		Edits:          draft.Edits,
	}.Clone()
	if draft.HeadSelf {
		record.Head = &id
	}
	if tx.scope.TaskID != 0 {
		by := tx.scope.TaskID
		record.ByTaskID = &by
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.sealed {
		return EntryRecord{}, errTxSettled
	}
	tx.writes = append(tx.writes, EntryWrite{Value: record})
	return record, nil
}

// CreateTask creates a durable task and returns its ID.
func (tx *Tx) CreateTask(def TaskDefinition, input json.RawMessage, options TaskOptions) (TaskID, error) {
	if err := tx.write(); err != nil {
		return 0, err
	}
	defer tx.endOp()
	if len(options.TraceSubmissions) > 0 {
		trace, links, err := tx.submissionTraces(options.TraceSubmissions)
		if err != nil {
			return 0, err
		}
		options.Trace, options.TraceRoot, options.Links = trace, trace == nil, links
	}
	var owner *TaskRecord
	if options.Ownership.Kind == OwnershipTask {
		// Validated again against the owner's final candidate at assembly.
		var err error
		if owner, err = tx.currentTask(options.Ownership.TaskID); err != nil {
			return 0, err
		}
		if err := tx.check(); err != nil {
			return 0, err
		}
		if owner == nil {
			return 0, fmt.Errorf("Task owner %d does not exist", options.Ownership.TaskID)
		}
		if options.Background {
			return 0, fmt.Errorf("A child task cannot be background")
		}
		if options.ConversationID != 0 && options.ConversationID != owner.ConversationID {
			return 0, fmt.Errorf("A child task lives in its owner's conversation %d", owner.ConversationID)
		}
	}
	conversationID := options.ConversationID
	if owner != nil {
		conversationID = owner.ConversationID
	}
	if conversationID == 0 {
		conversationID = tx.scope.ConversationID
	}
	if conversationID == 0 {
		return 0, fmt.Errorf("Tx.CreateTask() requires options.ConversationID")
	}
	if err := tx.requireConversation(conversationID); err != nil {
		return 0, err
	}
	if err := tx.check(); err != nil {
		return 0, err
	}
	if !json.Valid(input) {
		return 0, fmt.Errorf("Task %s input is not strict JSON", def.Name())
	}
	checkpoint, err := def.Initial(input)
	if err != nil {
		return 0, err
	}
	if !json.Valid(checkpoint) {
		return 0, fmt.Errorf("Task %s checkpoint is not strict JSON", def.Name())
	}
	minted, err := tx.storage().MintID(tx.ctx)
	if err != nil {
		return 0, err
	}
	id := TaskID(minted)
	record := TaskRecord{
		ID:               id,
		ConversationID:   conversationID,
		Kind:             def.Name(),
		Version:          def.Version(),
		Input:            input,
		Trace:            taskTrace(tx.ctx, options),
		Links:            options.Links,
		Background:       options.Background,
		AbandonOnRestart: options.AbandonOnRestart,
		State:            TaskState{Status: TaskPending, Checkpoint: checkpoint},
	}.Clone()
	if owner != nil {
		ownerID := owner.ID
		record.Owner = &ownerID
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.sealed {
		return 0, errTxSettled
	}
	task := tx.taskEntryLocked(id)
	task.write, task.create = &record, true
	return id, nil
}

// CreateSubmission creates a raw submission record with a fresh ID (create's
// ID is ignored). No admission rules apply: no busy check, no inbox queueing,
// no placement.
func (tx *Tx) CreateSubmission(create SubmissionRecord) (SubmissionRecord, error) {
	if err := tx.write(); err != nil {
		return SubmissionRecord{}, err
	}
	defer tx.endOp()
	if err := tx.requireConversation(create.ConversationID); err != nil {
		return SubmissionRecord{}, err
	}
	if err := tx.check(); err != nil {
		return SubmissionRecord{}, err
	}
	minted, err := tx.storage().MintID(tx.ctx)
	if err != nil {
		return SubmissionRecord{}, err
	}
	record := create.Clone()
	record.ID = SubmissionID(minted)
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.sealed {
		return SubmissionRecord{}, errTxSettled
	}
	tx.putSubmissionLocked(record)
	return record, nil
}

func (tx *Tx) putSubmissionLocked(record SubmissionRecord) {
	if _, ok := tx.submissions[record.ID]; !ok {
		tx.submissionOrder = append(tx.submissionOrder, record.ID)
	}
	tx.submissions[record.ID] = record
}

// SettleSubmission settles a queued or placed submission; only a placed
// input can be answered, and a settled submission stays unchanged. It is
// resolved at assembly against this transaction's latest record of the
// submission, so it works after table writes.
func (tx *Tx) SettleSubmission(id SubmissionID, settlement SubmissionSettlement) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.sealed {
		return errTxSettled
	}
	tx.hasTableWrite = true
	settlement.Detail = bytesCopy(settlement.Detail)
	tx.submissionChanges = append(tx.submissionChanges, submissionChange{id: id, settlement: settlement})
	return nil
}

// PlaceSubmission places a queued submission at entry: an input becomes
// "placed", a write "done". Resolved like SettleSubmission.
func (tx *Tx) PlaceSubmission(id SubmissionID, entry EntryID) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.sealed {
		return errTxSettled
	}
	tx.hasTableWrite = true
	tx.submissionChanges = append(tx.submissionChanges, submissionChange{id: id, place: true, entry: entry})
	return nil
}

// SetTask replaces one task record completely; for the task runtime, through
// which tasks change their own state.
func (tx *Tx) SetTask(value TaskRecord) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.sealed {
		return errTxSettled
	}
	tx.hasTableWrite = true
	task := tx.taskEntryLocked(value.ID)
	candidate := task.write
	if candidate != nil && candidate.State.Status == TaskTerminal {
		return fmt.Errorf("Task %d already has a terminal candidate", value.ID)
	}
	if candidate != nil && candidate.ConversationID != value.ConversationID {
		return fmt.Errorf("Task %d cannot change conversations", value.ID)
	}
	record := tx.stampTimes(value.Clone(), candidate)
	task.write = &record
	return nil
}

// StagedTasks are the candidate records of the tasks this transaction
// created or replaced so far.
func (tx *Tx) StagedTasks() []TaskRecord {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	var records []TaskRecord
	for _, id := range tx.taskOrder {
		if w := tx.tasks[id].write; w != nil {
			records = append(records, *w)
		}
	}
	return records
}

// StagedConversations are the conversations this transaction created or
// forked so far.
func (tx *Tx) StagedConversations() []ConversationRecord {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	var records []ConversationRecord
	for _, w := range tx.writes {
		if c, ok := w.(ConversationWrite); ok {
			records = append(records, c.Value)
		}
	}
	return records
}

// ─── Documents ───────────────────────────────────────────────────────────

// Doc acquires the draft of a document, creating it when absent: a family
// member from its ref's seed. Acquiring one address twice returns the same
// draft. The draft is usable until the callback settles.
func (tx *Tx) Doc(ref DocRef) (*delta.Draft, error) {
	def, address, id, err := ref.resolve()
	if err != nil {
		return nil, err
	}
	tx.mu.Lock()
	if tx.sealed {
		tx.mu.Unlock()
		return nil, errTxSettled
	}
	if address.Scope.Kind == ScopeTask {
		if task := tx.tasks[address.Scope.TaskID]; task != nil && task.write != nil && task.write.State.Status == TaskTerminal {
			tx.mu.Unlock()
			return nil, fmt.Errorf("Task %d is terminal", address.Scope.TaskID)
		}
	}
	latest := tx.latest[id]
	if latest != nil && !latest.retireOnCommit {
		if latest.acquisition != nil {
			acquisition := latest.acquisition
			tx.mu.Unlock()
			return acquisition.wait()
		}
		if latest.target != nil && latest.target.kind == targetForkCopy {
			acquisition := newOnce[*delta.Draft]()
			latest.acquisition = acquisition
			tx.pending++
			tx.mu.Unlock()
			draft, err := tx.acquireForkCopy(latest, def)
			acquisition.settle(draft, err)
			tx.endOp()
			return draft, err
		}
	}
	var seed any
	if def.Family {
		if seed, err = chord.CopyJSON(ref.seed); err != nil {
			tx.mu.Unlock()
			return nil, err
		}
	}
	entry := &documentEntry{addressID: id, address: address, def: &def}
	tx.documents = append(tx.documents, entry)
	tx.latest[id] = entry
	// Retirement is captured now, so a pending old acquisition and its
	// replacement stay distinct.
	skipLoad := latest != nil && latest.retireOnCommit
	acquisition := newOnce[*delta.Draft]()
	entry.acquisition = acquisition
	tx.pending++
	tx.mu.Unlock()
	draft, err := tx.acquire(entry, seed, skipLoad)
	acquisition.settle(draft, err)
	tx.endOp()
	return draft, err
}

// RetireDoc retires a document at commit. Retiring an acquired draft
// persists its final content first; retiring an absent address does nothing.
func (tx *Tx) RetireDoc(ref DocRef) error {
	def, address, id, err := ref.resolve()
	if err != nil {
		return err
	}
	tx.mu.Lock()
	if tx.sealed {
		tx.mu.Unlock()
		return errTxSettled
	}
	latest := tx.latest[id]
	if latest != nil && latest.retireOnCommit {
		tx.mu.Unlock()
		return nil
	}
	if latest != nil && latest.target != nil && latest.target.kind == targetForkCopy {
		defer tx.mu.Unlock()
		if err := checkRecordScope(def, latest.target.create); err != nil {
			return err
		}
		latest.retireOnCommit = true
		return nil
	}
	if latest != nil && latest.acquisition != nil {
		latest.retireOnCommit = true
		acquisition := latest.acquisition
		tx.pending++
		tx.mu.Unlock()
		defer tx.endOp()
		_, err := acquisition.wait()
		return err
	}
	entry := &documentEntry{addressID: id, address: address, def: &def, retireOnCommit: true}
	tx.documents = append(tx.documents, entry)
	tx.latest[id] = entry
	tx.pending++
	tx.mu.Unlock()
	defer tx.endOp()
	return tx.findRetirement(entry)
}

func (tx *Tx) acquire(entry *documentEntry, seed any, skipLoad bool) (*delta.Draft, error) {
	def := *entry.def
	var loaded *loadedDocument
	if !skipLoad {
		var err error
		if loaded, err = tx.session.loadDocument(tx.ctx, def, entry.addressID, entry.address); err != nil {
			return nil, err
		}
	}
	if err := tx.check(); err != nil {
		return nil, err
	}
	if loaded != nil {
		if err := checkRecordScope(def, loaded.record.Create()); err != nil {
			return nil, err
		}
		if err := checkRecordVersion(def, loaded.record.Create(), tx.session.storedVersion(loaded)); err != nil {
			return nil, err
		}
		tx.mu.Lock()
		defer tx.mu.Unlock()
		if tx.sealed {
			return nil, errTxSettled
		}
		change, err := loaded.tracker.BeginChange()
		if err != nil {
			return nil, err
		}
		entry.target = &documentTarget{kind: targetLoaded, loaded: loaded}
		entry.change = change
		return change.State(), nil
	}
	switch scope := entry.address.Scope; scope.Kind {
	case ScopeConversation:
		if err := tx.requireConversation(scope.ConversationID); err != nil {
			return nil, err
		}
	case ScopeTask:
		task, err := tx.currentTask(scope.TaskID)
		if err != nil {
			return nil, err
		}
		if task == nil {
			return nil, fmt.Errorf("Task %d does not exist", scope.TaskID)
		}
		if task.State.Status == TaskTerminal {
			return nil, fmt.Errorf("Task %d is terminal", scope.TaskID)
		}
	}
	if err := tx.check(); err != nil {
		return nil, err
	}
	value, err := initialValue(def, seed)
	if err != nil {
		return nil, err
	}
	minted, err := tx.storage().MintID(tx.ctx)
	if err != nil {
		return nil, err
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.sealed {
		return nil, errTxSettled
	}
	tracker := delta.Track(value)
	change, err := tracker.BeginChange()
	if err != nil {
		return nil, err
	}
	entry.target = &documentTarget{
		kind:    targetCreated,
		create:  documentCreate(def, entry.address, DocumentID(minted)),
		version: def.Version,
		tracker: tracker,
	}
	entry.change = change
	return change.State(), nil
}

func (tx *Tx) acquireForkCopy(entry *documentEntry, def DocDef) (*delta.Draft, error) {
	target := entry.target
	stored, err := tx.storage().Document(tx.ctx, target.source.ID, target.source.At)
	if err != nil {
		return nil, err
	}
	if err := tx.check(); err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("Fork source document %d cannot be read", target.source.ID)
	}
	r, c := stored.Record, target.create
	if r.Scope.Kind != ScopeConversation || r.Kind != c.Kind || !sameKey(r.Key, c.Key) || r.History != c.History || r.Fork != c.Fork {
		return nil, fmt.Errorf("Fork source document %d does not match the copied record", target.source.ID)
	}
	value, err := materializeDocumentValue(def, c, stored.Version, stored.Value)
	if err != nil {
		return nil, err
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.sealed {
		return nil, errTxSettled
	}
	tracker := delta.Track(value)
	change, err := tracker.BeginChange()
	if err != nil {
		return nil, err
	}
	entry.def = &def
	entry.target = &documentTarget{kind: targetCreated, create: c, version: def.Version, tracker: tracker}
	entry.change = change
	return change.State(), nil
}

func sameKey(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (tx *Tx) findRetirement(entry *documentEntry) error {
	var record *DocumentRecord
	if cached := tx.session.cachedDocument(entry.addressID); cached != nil {
		r := cached.record
		record = &r
	} else {
		var err error
		if record, err = tx.storage().FindDocument(tx.ctx, entry.address, Current); err != nil {
			return err
		}
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.sealed {
		return errTxSettled
	}
	if record == nil {
		return nil
	}
	if err := checkRecordScope(*entry.def, record.Create()); err != nil {
		return err
	}
	entry.target = &documentTarget{kind: targetRetireOnly, record: *record}
	return nil
}

// ─── Settlement ──────────────────────────────────────────────────────────

func (tx *Tx) waitIdleLocked() {
	for tx.pending > 0 {
		tx.idle.Wait()
	}
}

// settleFailure seals after a callback failure: abort every change and wait
// for every pending operation.
func (tx *Tx) settleFailure() {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.sealed = true
	tx.abortChangesLocked()
	tx.waitIdleLocked()
	// An operation that finished in the meantime may have opened a change.
	tx.abortChangesLocked()
}

// settleSuccess seals after a callback success, prepares every open change
// and assembles the atomic batch. Any failure aborts every change before
// Storage admission.
func (tx *Tx) settleSuccess() ([]StorageWrite, error) {
	tx.mu.Lock()
	tx.sealed = true
	if tx.pending > 0 {
		tx.abortChangesLocked()
		tx.waitIdleLocked()
		tx.abortChangesLocked()
		tx.mu.Unlock()
		return nil, errors.New("Session commit callback settled before its pending Tx operations")
	}
	// Prepare every open change; this revokes every draft.
	for _, d := range tx.documents {
		if d.change == nil {
			continue
		}
		prepared, err := d.change.Prepare()
		if err != nil {
			tx.abortChangesLocked()
			tx.mu.Unlock()
			return nil, err
		}
		d.prepared = prepared
	}
	tx.mu.Unlock()
	writes, err := tx.assemble()
	if err != nil {
		tx.discard()
		return nil, err
	}
	return writes, nil
}

// discard aborts every prepared change after a Storage failure or when no
// write is required.
func (tx *Tx) discard() {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.abortChangesLocked()
}

func (tx *Tx) abortChangesLocked() {
	for _, d := range tx.documents {
		if d.change != nil {
			d.change.Abort()
		}
	}
}

// adopt adopts every prepared change after Storage success and describes the
// publication. The Session's mutex is held.
func (tx *Tx) adopt(seq Seq) []CommitChange {
	var publications []CommitChange
	for _, plan := range tx.plans {
		record := plan.record
		if !plan.committed {
			c := plan.create
			record = DocumentRecord{ID: c.ID, Kind: c.Kind, Key: c.Key, Scope: c.Scope, History: c.History, Fork: c.Fork, CreatedAt: seq}
		}
		if plan.retire {
			retiredAt := seq
			record.RetiredAt = &retiredAt
		}
		change := plan.change
		if change != nil {
			// A new incarnation is adopted unless it retires in the same
			// commit; a loaded one only when it changed.
			adopt := len(change.prepared.Ops()) > 0
			if change.loaded == nil {
				adopt = !plan.retire
			}
			if adopt {
				if err := change.tracker.Adopt(change.prepared); err != nil {
					panic(fmt.Errorf("durable: a prepared document change could not be adopted: %w", err))
				}
			} else {
				change.prepared.Abort()
			}
			if loaded := change.loaded; loaded != nil {
				if loaded.storedVersion < change.version {
					loaded.storedVersion = change.version
				}
				if w, ok := plan.content.(DocumentChangeWrite); ok {
					if w.Content.Kind == ContentBase {
						loaded.deltasSinceBase = 0
					} else {
						loaded.deltasSinceBase++
					}
				}
			} else if !plan.retire {
				tx.session.installLocked(&loadedDocument{
					addressID:     plan.addressID,
					record:        record,
					storedVersion: change.version,
					valueVersion:  change.version,
					tracker:       change.tracker,
				})
			}
		}
		if plan.retire {
			if plan.committed {
				tx.session.evictLocked(plan.addressID, record.ID)
			}
			publications = append(publications, DocumentChange{Record: record, ConversationID: plan.conversationID, Ops: []delta.Op{}})
		} else if copyWrite, ok := plan.content.(DocumentCopyWrite); ok {
			publications = append(publications, DocumentCopyChange{Record: record, ConversationID: plan.conversationID, Source: copyWrite.Source})
		} else if change != nil && plan.publishes() {
			ops := change.prepared.Ops()
			if change.loaded == nil {
				ops = []delta.Op{}
			}
			publications = append(publications, DocumentChange{
				Record:         record,
				ConversationID: plan.conversationID,
				Version:        change.version,
				Value:          change.prepared.Value(),
				Ops:            ops,
			})
		}
	}
	return publications
}

func (tx *Tx) assemble() ([]StorageWrite, error) {
	storage := tx.storage()
	for _, d := range tx.documents {
		if plan := planDocument(d); plan != nil {
			tx.plans = append(tx.plans, plan)
		}
	}
	if err := tx.rejectForkSourceWrites(); err != nil {
		return nil, err
	}
	if err := tx.validateOwners(); err != nil {
		return nil, err
	}
	for _, id := range tx.taskOrder {
		task := tx.tasks[id]
		if task.write == nil || task.create {
			continue
		}
		committed, err := tx.committedTask(id)
		if err != nil {
			return nil, err
		}
		if committed == nil {
			return nil, fmt.Errorf("Task %d does not exist", id)
		}
		if committed.State.Status == TaskTerminal {
			return nil, fmt.Errorf("Task %d is already terminal", id)
		}
		if committed.ConversationID != task.write.ConversationID {
			return nil, fmt.Errorf("Task %d cannot change conversations", id)
		}
	}

	// Terminal settlement retires every task document, including ones this
	// transaction created.
	var terminal []TaskID
	terminalSet := map[TaskID]bool{}
	for _, id := range tx.taskOrder {
		if w := tx.tasks[id].write; w != nil && w.State.Status == TaskTerminal {
			terminal = append(terminal, id)
			terminalSet[id] = true
		}
	}
	if len(terminal) > 0 {
		retiring := map[DocumentID]bool{}
		for _, plan := range tx.plans {
			identity := plan.identity()
			if identity.Scope.Kind != ScopeTask || !terminalSet[identity.Scope.TaskID] {
				continue
			}
			plan.retire = true
			retiring[identity.ID] = true
		}
		for _, taskID := range terminal {
			if tx.tasks[taskID].create {
				continue
			}
			var cursor Cursor
			for {
				page, err := storage.ScanDocuments(tx.ctx, DocumentQuery{Scope: TaskScope(taskID), At: Current}, internalScanPageSize, cursor)
				if err != nil {
					return nil, err
				}
				for _, record := range page.Items {
					if retiring[record.ID] {
						continue
					}
					tx.plans = append(tx.plans, &documentPlan{addressID: addressID(record.Address()), committed: true, record: record, retire: true})
					retiring[record.ID] = true
				}
				if page.Next == nil {
					break
				}
				cursor = page.Next
			}
		}
	}

	// Resolve publication ownership before admission, so adoption stays
	// synchronous.
	for _, plan := range tx.plans {
		if !plan.publishes() {
			continue
		}
		scope := plan.identity().Scope
		switch scope.Kind {
		case ScopeConversation:
			plan.conversationID = scope.ConversationID
		case ScopeTask:
			tx.mu.Lock()
			task := tx.taskEntryLocked(scope.TaskID)
			tx.mu.Unlock()
			if task.publicationConversationID == 0 {
				current, err := tx.currentTask(scope.TaskID)
				if err != nil {
					return nil, err
				}
				if current != nil {
					task.publicationConversationID = current.ConversationID
				}
			}
			plan.conversationID = task.publicationConversationID
		}
	}

	for _, c := range tx.submissionChanges {
		current, ok := tx.submissions[c.id]
		if !ok {
			stored, err := storage.Submission(tx.ctx, c.id)
			if err != nil {
				return nil, err
			}
			if stored == nil {
				return nil, fmt.Errorf("Submission %d does not exist", c.id)
			}
			current = *stored
		}
		next, changed, err := applySubmissionChange(current, c)
		if err != nil {
			return nil, err
		}
		if changed {
			tx.putSubmissionLocked(next)
		}
	}

	writes := tx.writes
	for _, id := range tx.submissionOrder {
		writes = append(writes, SubmissionWrite{Value: tx.submissions[id]})
	}
	for _, id := range tx.taskOrder {
		if w := tx.tasks[id].write; w != nil {
			writes = append(writes, TaskWrite{Value: *w})
		}
	}
	for _, plan := range tx.plans {
		// Checkpoint predicates run last, after every validation.
		if w, ok := plan.content.(DocumentChangeWrite); ok && w.Content.Kind == ContentDelta && plan.change != nil && plan.change.loaded != nil {
			change := plan.change
			info := CheckpointInfo{DeltasSinceBase: change.loaded.deltasSinceBase}
			if change.def != nil && change.def.CheckpointWhen != nil {
				base, err := callHook(func() bool { return change.def.CheckpointWhen(change.prepared.Value(), change.prepared.Ops(), info) })
				if err != nil {
					return nil, err
				}
				if base {
					plan.content = DocumentChangeWrite{ID: w.ID, Content: DocumentContent{Version: change.version, Kind: ContentBase, Value: change.prepared.Value()}}
				}
			}
		}
		if plan.content != nil {
			writes = append(writes, plan.content)
		}
		if plan.retire {
			writes = append(writes, DocumentRetireWrite{ID: plan.identity().ID})
		}
	}
	tx.writes = writes
	return writes, nil
}

// applySubmissionChange returns the complete record after one change.
// Placement turns a queued input "placed" and a queued write "done"; only a
// placed input can be answered; a settled record stays.
func applySubmissionChange(current SubmissionRecord, c submissionChange) (SubmissionRecord, bool, error) {
	if current.Status == SubmissionDone || current.Status == SubmissionUnanswered {
		return current, false, nil
	}
	next := current.Clone()
	if c.place {
		if current.Status != SubmissionQueued {
			return current, false, fmt.Errorf("Submission %d is not queued", current.ID)
		}
		next.Status = SubmissionDone
		if current.Type == SubmissionTypeInput {
			next.Status = SubmissionPlaced
		}
		entry := c.entry
		next.Entry = &entry
		return next, true, nil
	}
	switch c.settlement.Status {
	case SubmissionDone:
		if current.Status != SubmissionPlaced {
			return current, false, fmt.Errorf("Submission %d is not a placed input", current.ID)
		}
		answer := c.settlement.Answer
		next.Status, next.Answer = SubmissionDone, &answer
	case SubmissionUnanswered:
		next.Status, next.Reason, next.Detail = SubmissionUnanswered, c.settlement.Reason, bytesCopy(c.settlement.Detail)
	default:
		return current, false, fmt.Errorf("Invalid submission settlement status: %s", c.settlement.Status)
	}
	return next, true, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────

// validateOwners requires a live owner of new owned work, judged on the
// owner's final candidate: not completing, terminal or abort-marked. A task
// therefore cannot create owned work in the commit that finishes it.
func (tx *Tx) validateOwners() error {
	type owner struct {
		what   string
		taskID TaskID
	}
	var owners []owner
	for _, w := range tx.writes {
		if c, ok := w.(ConversationWrite); ok && c.Value.Owner != nil {
			owners = append(owners, owner{"Conversation owner task", c.Value.Owner.TaskID})
		}
	}
	for _, id := range tx.taskOrder {
		if t := tx.tasks[id]; t.create && t.write.Owner != nil {
			owners = append(owners, owner{"Task owner", *t.write.Owner})
		}
	}
	for _, o := range owners {
		task, err := tx.currentTask(o.taskID)
		if err != nil {
			return err
		}
		if task == nil {
			return fmt.Errorf("%s %d does not exist", o.what, o.taskID)
		}
		if task.State.Status == TaskTerminal || task.State.Status == TaskCompleting {
			return fmt.Errorf("%s %d is %s", o.what, o.taskID, task.State.Status)
		}
		if task.AbortRequested {
			return fmt.Errorf("%s %d is abort-marked", o.what, o.taskID)
		}
	}
	return nil
}

func (tx *Tx) rejectForkSourceWrites() error {
	for _, plan := range tx.plans {
		if plan.content == nil && !plan.retire {
			continue
		}
		identity := plan.identity()
		if tx.forkSourceDocuments[identity.ID] {
			return fmt.Errorf("Cannot change fork source document %d in the fork transaction", identity.ID)
		}
		scope := identity.Scope
		if scope.Kind == ScopeConversation && tx.forkSourceConversations[scope.ConversationID] && identity.Fork == ForkCurrent {
			return fmt.Errorf("Cannot fork conversation %d while changing its current-policy documents", scope.ConversationID)
		}
	}
	return nil
}

func (tx *Tx) requireConversation(id ConversationID) error {
	tx.mu.Lock()
	created := tx.createdConversations[id]
	tx.mu.Unlock()
	if created {
		return nil
	}
	record, err := tx.storage().Conversation(tx.ctx, id)
	if err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("Conversation %d does not exist", id)
	}
	return nil
}

// stampTimes sets StartedAt on the first change to running and EndedAt on
// the change to terminal. Once set they carry over from the replaced
// record; records written before they existed lack them.
func (tx *Tx) stampTimes(value TaskRecord, candidate *TaskRecord) TaskRecord {
	now := func(stamps bool) *int64 {
		if !stamps {
			return nil
		}
		ms := tx.session.now().UnixMilli()
		return &ms
	}
	startedAt := value.StartedAt
	endedAt := value.EndedAt
	if candidate != nil && candidate.StartedAt != nil {
		startedAt = ptr(candidate.StartedAt)
	}
	if candidate != nil && candidate.EndedAt != nil {
		endedAt = ptr(candidate.EndedAt)
	}
	if startedAt == nil {
		startedAt = now(value.State.Status == TaskRunning)
	}
	if endedAt == nil {
		endedAt = now(value.State.Status == TaskTerminal)
	}
	value.StartedAt, value.EndedAt = startedAt, endedAt
	return value
}

func (tx *Tx) taskEntryLocked(id TaskID) *txTask {
	task := tx.tasks[id]
	if task == nil {
		task = &txTask{}
		tx.tasks[id] = task
		tx.taskOrder = append(tx.taskOrder, id)
	}
	return task
}

// currentTask is the latest candidate record, else the committed one; not a
// caller table read.
// submissionTraces resolves the telemetry of a run started with ids (pi
// createTask with submissionIds): the first submission's trace, and the
// others' as links, from this commit's submissions or else Storage.
func (tx *Tx) submissionTraces(ids []SubmissionID) (telemetry.Carrier, []telemetry.Carrier, error) {
	var trace telemetry.Carrier
	var links []telemetry.Carrier
	for i, id := range ids {
		tx.mu.Lock()
		record, staged := tx.submissions[id]
		tx.mu.Unlock()
		var carrier telemetry.Carrier
		if staged {
			carrier = record.Trace
		} else {
			stored, err := tx.storage().Submission(tx.ctx, id)
			if err != nil {
				return nil, nil, err
			}
			if stored != nil {
				carrier = stored.Trace
			}
		}
		if i == 0 {
			trace = carrierCopy(carrier)
		} else if carrier != nil {
			links = append(links, carrierCopy(carrier))
		}
	}
	return trace, links, nil
}

func (tx *Tx) currentTask(id TaskID) (*TaskRecord, error) {
	tx.mu.Lock()
	if task := tx.tasks[id]; task != nil && task.write != nil {
		w := task.write
		tx.mu.Unlock()
		return w, nil
	}
	tx.mu.Unlock()
	return tx.committedTask(id)
}

// committedTask reads a task once per transaction.
func (tx *Tx) committedTask(id TaskID) (*TaskRecord, error) {
	tx.mu.Lock()
	task := tx.taskEntryLocked(id)
	read := task.committed
	start := read == nil
	if start {
		read = newOnce[*TaskRecord]()
		task.committed = read
	}
	tx.mu.Unlock()
	if start {
		record, err := tx.storage().Task(tx.ctx, id)
		read.settle(record, err)
	}
	return read.wait()
}

// planDocument plans one staged document: its record, content write and
// prepared change. Retirement is decided later.
func planDocument(d *documentEntry) *documentPlan {
	target := d.target
	if target == nil {
		return nil
	}
	plan := &documentPlan{addressID: d.addressID, retire: d.retireOnCommit}
	switch target.kind {
	case targetCreated:
		prepared := d.prepared
		plan.create = target.create
		plan.content = DocumentCreateWrite{Record: target.create, Content: DocumentContent{Version: target.version, Kind: ContentBase, Value: prepared.Value()}}
		plan.change = &planChange{tracker: target.tracker, prepared: prepared, version: target.version}
	case targetForkCopy:
		plan.create = target.create
		plan.content = DocumentCopyWrite{Record: target.create, Source: target.source}
	case targetRetireOnly:
		plan.committed, plan.record = true, target.record
	case targetLoaded:
		loaded := target.loaded
		prepared := d.prepared
		version := d.def.Version
		plan.committed, plan.record = true, loaded.record
		// A version change stores a base even without operations; otherwise
		// only a change stores a delta.
		if loaded.storedVersion < version {
			plan.content = DocumentChangeWrite{ID: loaded.record.ID, Content: DocumentContent{Version: version, Kind: ContentBase, Value: prepared.Value()}}
		} else if len(prepared.Ops()) > 0 {
			plan.content = DocumentChangeWrite{ID: loaded.record.ID, Content: DocumentContent{Version: version, Kind: ContentDelta, Ops: prepared.Ops()}}
		}
		plan.change = &planChange{tracker: loaded.tracker, prepared: prepared, version: version, loaded: loaded, def: d.def}
	}
	return plan
}

// publishes reports whether adoption publishes the plan: every creation,
// copy and retirement, and a loaded incarnation that writes content, which
// includes a migration-only base so observers of the older shape receive the
// new value.
func (p *documentPlan) publishes() bool {
	return p.retire || p.change == nil || p.change.loaded == nil || p.content != nil
}
