// Package memory is durable's detached in-memory reference Storage (pi
// packages/durable/src/storage/memory.ts).
//
// Reads and retained writes are copied, as pi copies them, to match the
// ownership boundary of serialization-backed stores: copies go through JSON,
// which is that boundary. This is backend conformance, not validation.
package memory

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math"
	"slices"
	"strconv"
	"sync"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
)

type id = durable.ID

const (
	tableConversation = "conversation"
	tableEntry        = "entry"
	tableTask         = "task"
	tableSubmission   = "submission"
	tableDocument     = "document"
)

type revision struct {
	content durable.DocumentContent
	seq     durable.Seq
}

type storedDocument struct {
	record    durable.DocumentRecord
	revisions []revision
}

type documentAction struct {
	create  *durable.DocumentCreate
	content *durable.DocumentContent
	retire  bool
}

type addressIndex struct {
	ids       []id
	currentID *durable.DocumentID
}

type state struct {
	recordTypes             map[id]string
	conversations           map[durable.ConversationID]durable.ConversationRecord
	conversationIDs         []id
	conversationsByOwnerCnv map[durable.ConversationID][]id
	conversationsByOwnerTsk map[durable.TaskID][]id
	entries                 map[durable.EntryID]durable.EntryRecord
	entryIDs                map[durable.ConversationID][]id
	headEntryIDs            map[durable.ConversationID][]id
	entryCommitSeqs         map[durable.EntryID]durable.Seq
	tasks                   map[durable.TaskID]durable.TaskRecord
	taskIDs                 []id
	taskIDsByStatus         map[string][]id
	submissions             map[durable.SubmissionID]durable.SubmissionRecord
	submissionIDs           []id
	submissionIDsByStatus   map[string][]id
	submissionIDsByRequest  map[durable.ConversationID]map[string]durable.SubmissionID
	documents               map[durable.DocumentID]*storedDocument
	documentAddresses       map[string]*addressIndex
	documentIDsByScope      map[string][]id
}

// Storage is the in-memory Storage. It is safe for concurrent use.
type Storage struct {
	mu      sync.Mutex
	state   state
	nextID  id
	nextSeq durable.Seq
	closed  bool
}

var _ durable.Storage = (*Storage)(nil)

// New makes an empty in-memory Storage.
func New() *Storage {
	s := &Storage{nextID: 2, nextSeq: 1}
	s.state = state{
		recordTypes:             map[id]string{},
		conversations:           map[durable.ConversationID]durable.ConversationRecord{},
		conversationsByOwnerCnv: map[durable.ConversationID][]id{},
		conversationsByOwnerTsk: map[durable.TaskID][]id{},
		entries:                 map[durable.EntryID]durable.EntryRecord{},
		entryIDs:                map[durable.ConversationID][]id{},
		headEntryIDs:            map[durable.ConversationID][]id{},
		entryCommitSeqs:         map[durable.EntryID]durable.Seq{},
		tasks:                   map[durable.TaskID]durable.TaskRecord{},
		taskIDsByStatus:         map[string][]id{},
		submissions:             map[durable.SubmissionID]durable.SubmissionRecord{},
		submissionIDsByStatus:   map[string][]id{},
		submissionIDsByRequest:  map[durable.ConversationID]map[string]durable.SubmissionID{},
		documents:               map[durable.DocumentID]*storedDocument{},
		documentAddresses:       map[string]*addressIndex{},
		documentIDsByScope:      map[string][]id{},
	}
	return s
}

// ErrClosed is the error of every operation after Close.
var ErrClosed = errors.New("MemoryStorage is closed")

func (s *Storage) assertOpen() error {
	if s.closed {
		return ErrClosed
	}
	return nil
}

// clone returns a detached copy of a stored record or document value. Stored
// values passed validation on the way in, so a value that cannot be copied
// is a bug of this storage.
func clone[T any](v T) T {
	var out any
	switch x := any(v).(type) {
	case durable.ConversationRecord:
		out = x.Clone()
	case durable.EntryRecord:
		out = x.Clone()
	case durable.TaskRecord:
		out = x.Clone()
	case durable.SubmissionRecord:
		out = x.Clone()
	case durable.DocumentRecord:
		out = x.Clone()
	case map[string]any:
		c, err := durable.CloneValue(x)
		if err != nil {
			panic(fmt.Sprintf("memory: a stored document value cannot be copied: %v", err))
		}
		out = c
	default:
		panic(fmt.Sprintf("memory: no copy for %T", v))
	}
	return out.(T)
}

func cloneWrite(w durable.StorageWrite) (durable.StorageWrite, error) {
	return durable.CloneWrite(w)
}

func lowerBound(ids []id, target id) int {
	i, _ := slices.BinarySearch(ids, target)
	return i
}

func upperBound(ids []id, target id) int {
	lo, hi := 0, len(ids)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if ids[mid] <= target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func insertSorted(ids []id, v id) []id {
	if len(ids) == 0 || ids[len(ids)-1] < v {
		return append(ids, v)
	}
	i := lowerBound(ids, v)
	return slices.Insert(ids, i, v)
}

func removeSorted(ids []id, v id) []id {
	i := lowerBound(ids, v)
	if i < len(ids) && ids[i] == v {
		return slices.Delete(ids, i, i+1)
	}
	return ids
}

func scopeKey(scope durable.DocumentScope) string {
	switch scope.Kind {
	case durable.ScopeConversation:
		return fmt.Sprintf(`["conversation",%d]`, scope.ConversationID)
	case durable.ScopeTask:
		return fmt.Sprintf(`["task",%d]`, scope.TaskID)
	}
	return `["session"]`
}

// addressKey is a document address as a map key. Strings are quoted with
// strconv.Quote rather than JSON: JSON replaces malformed UTF-8, so two
// distinct kinds or keys would share one address.
func addressKey(a durable.DocumentAddress) string {
	member := `["singleton"]`
	if a.Key != nil {
		member = `["family",` + strconv.Quote(*a.Key) + `]`
	}
	return "[" + strconv.Quote(a.Kind) + "," + strconv.Quote(scopeKey(a.Scope)) + "," + member + "]"
}

func createAddressKey(c durable.DocumentCreate) string {
	return addressKey(durable.DocumentAddress{Kind: c.Kind, Scope: c.Scope, Key: c.Key})
}

func isAliveAt(r durable.DocumentRecord, at durable.DocumentPoint) bool {
	if at == durable.Current {
		return r.RetiredAt == nil
	}
	return r.CreatedAt <= at && (r.RetiredAt == nil || at < *r.RetiredAt)
}

// isCurrentOnly reports whether a document keeps no history: every document
// but a rewindable conversation one.
func isCurrentOnly(r durable.DocumentRecord) bool {
	return r.Scope.Kind != durable.ScopeConversation || r.History == durable.HistoryLatest
}

func page[T any](values []T, limit int, order durable.ScanOrder, idOf func(T) id) durable.Page[T] {
	items := values
	if len(items) > limit {
		items = items[:limit]
	}
	out := make([]T, len(items))
	for i, v := range items {
		out[i] = clone(v)
	}
	if len(values) <= limit {
		return durable.Page[T]{Items: out}
	}
	return durable.Page[T]{Items: out, Next: durable.NextCursor(idOf(items[len(items)-1]), order)}
}

// scanIndexes are the indexes of sorted ids in scan order, after the
// cursor's ID when there is one.
func scanIndexes(ids []id, start durable.ScanStart) iter.Seq[int] {
	return func(yield func(int) bool) {
		if start.Order == durable.Ascending {
			first := 0
			if start.HasAfter {
				first = upperBound(ids, start.After)
			}
			for i := first; i < len(ids); i++ {
				if !yield(i) {
					return
				}
			}
			return
		}
		end := len(ids)
		if start.HasAfter {
			end = lowerBound(ids, start.After)
		}
		for i := end - 1; i >= 0; i-- {
			if !yield(i) {
				return
			}
		}
	}
}

// PreparedCommit is a validated, detached state mutation whose application
// performs no fallible preparation (pi PreparedMemoryCommit).
type PreparedCommit struct {
	// Seq is the commit's sequence.
	Seq durable.Seq
	// Writes are the detached writes, for persistence; document copies are
	// resolved into creations.
	Writes  []durable.StorageWrite
	storage *Storage
	actions map[durable.DocumentID]*documentAction
	order   []durable.DocumentID
	applied bool
}

// Apply applies the commit once and returns its sequence; later calls do
// nothing.
func (p *PreparedCommit) Apply() durable.Seq {
	p.storage.mu.Lock()
	defer p.storage.mu.Unlock()
	if !p.applied {
		p.applied = true
		p.storage.applyPreparedCommit(p)
	}
	return p.Seq
}

// Commit atomically persists one batch.
func (s *Storage) Commit(_ context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	prepared, err := s.PrepareCommit(writes, 0)
	if err != nil {
		return 0, err
	}
	return prepared.Apply(), nil
}

// PrepareCommit validates and detaches one commit without changing
// observable state. seq 0 takes the next sequence.
func (s *Storage) PrepareCommit(writes []durable.StorageWrite, seq durable.Seq) (*PreparedCommit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return nil, err
	}
	if seq == 0 {
		seq = s.nextSeq
	}
	if seq < s.nextSeq {
		return nil, fmt.Errorf("Commit sequence %d does not strictly increase", seq)
	}
	detached := make([]durable.StorageWrite, len(writes))
	for i, w := range writes {
		c, err := cloneWrite(w)
		if err != nil {
			return nil, err
		}
		detached[i] = c
	}
	detached, err := s.resolveDocumentCopies(detached)
	if err != nil {
		return nil, err
	}
	if err := s.checkGlobalIDs(detached); err != nil {
		return nil, err
	}
	actions, order, err := prepareDocumentActions(detached)
	if err != nil {
		return nil, err
	}
	if err := s.checkDocumentActions(actions, order); err != nil {
		return nil, err
	}
	return &PreparedCommit{Seq: seq, Writes: detached, storage: s, actions: actions, order: order}, nil
}

func documentWriteID(w durable.StorageWrite) (durable.DocumentID, bool) {
	switch w := w.(type) {
	case durable.DocumentCreateWrite:
		return w.Record.ID, true
	case durable.DocumentCopyWrite:
		return w.Record.ID, true
	case durable.DocumentChangeWrite:
		return w.ID, true
	case durable.DocumentRetireWrite:
		return w.ID, true
	}
	return 0, false
}

func (s *Storage) resolveDocumentCopies(writes []durable.StorageWrite) ([]durable.StorageWrite, error) {
	if !slices.ContainsFunc(writes, func(w durable.StorageWrite) bool { _, ok := w.(durable.DocumentCopyWrite); return ok }) {
		return writes, nil
	}
	changed := map[durable.DocumentID]bool{}
	for _, w := range writes {
		if docID, ok := documentWriteID(w); ok {
			changed[docID] = true
		}
	}
	out := make([]durable.StorageWrite, len(writes))
	for i, w := range writes {
		c, ok := w.(durable.DocumentCopyWrite)
		if !ok {
			out[i] = w
			continue
		}
		created, err := s.resolveCopy(c, changed)
		if err != nil {
			return nil, fmt.Errorf("Document copy %d was rejected: %w", c.Record.ID, err)
		}
		out[i] = created
	}
	return out, nil
}

func (s *Storage) resolveCopy(c durable.DocumentCopyWrite, changed map[durable.DocumentID]bool) (durable.StorageWrite, error) {
	if changed[c.Source.ID] {
		return nil, fmt.Errorf("Fork source document %d is changed in the copy batch", c.Source.ID)
	}
	stored, err := s.materializeDocument(c.Source.ID, c.Source.At)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("Fork source document %d cannot be read", c.Source.ID)
	}
	r := stored.Record
	if r.Scope.Kind != durable.ScopeConversation || c.Record.Scope.Kind != durable.ScopeConversation ||
		r.Kind != c.Record.Kind || !sameKey(r.Key, c.Record.Key) || r.History != c.Record.History || r.Fork != c.Record.Fork {
		return nil, fmt.Errorf("Fork source document %d does not match the copied record", c.Source.ID)
	}
	return durable.DocumentCreateWrite{
		Record:  c.Record,
		Content: durable.DocumentContent{Kind: durable.ContentBase, Version: stored.Version, Value: stored.Value},
	}, nil
}

func sameKey(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (s *Storage) applyPreparedCommit(p *PreparedCommit) {
	st := &s.state
	seq := p.Seq
	bump := func(v id) { s.nextID = max(s.nextID, v+1) }
	for _, w := range p.Writes {
		switch w := w.(type) {
		case durable.ConversationWrite:
			v := w.Value
			st.recordTypes[id(v.ID)] = tableConversation
			st.conversations[v.ID] = v
			st.conversationIDs = insertSorted(st.conversationIDs, id(v.ID))
			if v.Owner != nil {
				st.conversationsByOwnerCnv[v.Owner.ConversationID] = insertSorted(st.conversationsByOwnerCnv[v.Owner.ConversationID], id(v.ID))
				st.conversationsByOwnerTsk[v.Owner.TaskID] = insertSorted(st.conversationsByOwnerTsk[v.Owner.TaskID], id(v.ID))
			}
			bump(id(v.ID))
		case durable.EntryWrite:
			v := w.Value
			st.recordTypes[id(v.ID)] = tableEntry
			st.entries[v.ID] = v
			st.entryCommitSeqs[v.ID] = seq
			st.entryIDs[v.ConversationID] = insertSorted(st.entryIDs[v.ConversationID], id(v.ID))
			if v.Head != nil {
				st.headEntryIDs[v.ConversationID] = insertSorted(st.headEntryIDs[v.ConversationID], id(v.ID))
			}
			bump(id(v.ID))
		case durable.TaskWrite:
			v := w.Value
			st.recordTypes[id(v.ID)] = tableTask
			previous, exists := st.tasks[v.ID]
			if !exists {
				st.taskIDs = insertSorted(st.taskIDs, id(v.ID))
				st.taskIDsByStatus[v.State.Status] = insertSorted(st.taskIDsByStatus[v.State.Status], id(v.ID))
			} else if previous.State.Status != v.State.Status {
				st.taskIDsByStatus[previous.State.Status] = removeSorted(st.taskIDsByStatus[previous.State.Status], id(v.ID))
				st.taskIDsByStatus[v.State.Status] = insertSorted(st.taskIDsByStatus[v.State.Status], id(v.ID))
			}
			st.tasks[v.ID] = v
			bump(id(v.ID))
		case durable.SubmissionWrite:
			v := w.Value
			st.recordTypes[id(v.ID)] = tableSubmission
			previous, exists := st.submissions[v.ID]
			if !exists {
				st.submissionIDs = insertSorted(st.submissionIDs, id(v.ID))
				st.submissionIDsByStatus[v.Status] = insertSorted(st.submissionIDsByStatus[v.Status], id(v.ID))
			} else if previous.Status != v.Status {
				st.submissionIDsByStatus[previous.Status] = removeSorted(st.submissionIDsByStatus[previous.Status], id(v.ID))
				st.submissionIDsByStatus[v.Status] = insertSorted(st.submissionIDsByStatus[v.Status], id(v.ID))
			}
			if exists && previous.RequestID != "" {
				if requests := st.submissionIDsByRequest[previous.ConversationID]; requests != nil && requests[previous.RequestID] == v.ID {
					delete(requests, previous.RequestID)
					if len(requests) == 0 {
						delete(st.submissionIDsByRequest, previous.ConversationID)
					}
				}
			}
			st.submissions[v.ID] = v
			if v.RequestID != "" {
				requests := st.submissionIDsByRequest[v.ConversationID]
				if requests == nil {
					requests = map[string]durable.SubmissionID{}
					st.submissionIDsByRequest[v.ConversationID] = requests
				}
				requests[v.RequestID] = v.ID
			}
			bump(id(v.ID))
		}
	}
	s.applyDocumentActions(p.actions, p.order, seq)
	s.nextSeq = seq + 1
}

// MintID returns a fresh ID.
func (s *Storage) MintID(context.Context) (durable.ID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return 0, err
	}
	if s.nextID > 1<<53-1 {
		return 0, errors.New("ID space is exhausted")
	}
	v := s.nextID
	s.nextID++
	return v, nil
}

func (s *Storage) Conversation(_ context.Context, cid durable.ConversationID) (*durable.ConversationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return nil, err
	}
	v, ok := s.state.conversations[cid]
	if !ok {
		return nil, nil
	}
	c := clone(v)
	return &c, nil
}

func (s *Storage) ScanConversations(_ context.Context, q durable.ConversationQuery, limit int, cursor durable.Cursor) (durable.Page[durable.ConversationRecord], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return durable.Page[durable.ConversationRecord]{}, err
	}
	ids := s.state.conversationIDs
	switch {
	case q.OwnerTaskID != nil:
		ids = s.state.conversationsByOwnerTsk[*q.OwnerTaskID]
	case q.OwnerConversationID != nil:
		ids = s.state.conversationsByOwnerCnv[*q.OwnerConversationID]
	}
	start, err := durable.ResolveScanStart(q.Order, cursor, durable.Ascending)
	if err != nil {
		return durable.Page[durable.ConversationRecord]{}, err
	}
	var values []durable.ConversationRecord
	for i := range scanIndexes(ids, start) {
		if len(values) > limit {
			break
		}
		v := s.state.conversations[durable.ConversationID(ids[i])]
		if q.OwnerConversationID != nil && (v.Owner == nil || v.Owner.ConversationID != *q.OwnerConversationID) {
			continue
		}
		values = append(values, v)
	}
	return page(values, limit, start.Order, func(c durable.ConversationRecord) id { return id(c.ID) }), nil
}

func (s *Storage) Entry(_ context.Context, eid durable.EntryID) (*durable.StoredEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return nil, err
	}
	e, ok := s.state.entries[eid]
	if !ok {
		return nil, nil
	}
	return &durable.StoredEntry{Entry: clone(e), CommitSeq: s.state.entryCommitSeqs[eid]}, nil
}

func (s *Storage) VisibleEntry(_ context.Context, cid durable.ConversationID, eid durable.EntryID) (*durable.StoredEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return nil, err
	}
	entries, err := s.visibleEntries(cid, id(eid), id(eid))
	if err != nil {
		return nil, err
	}
	for e := range entries {
		return &durable.StoredEntry{Entry: clone(e), CommitSeq: s.state.entryCommitSeqs[eid]}, nil
	}
	return nil, nil
}

func (s *Storage) FindLatestHeadMarker(_ context.Context, cid durable.ConversationID, atOrBefore *durable.EntryID) (*durable.EntryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return nil, err
	}
	if _, ok := s.state.conversations[cid]; !ok {
		return nil, durable.NewStorageRequestError("Unknown conversation: %d", cid)
	}
	current := cid
	upper := id(math.MaxInt64)
	if atOrBefore != nil {
		upper = id(*atOrBefore)
	}
	for {
		ids := s.state.headEntryIDs[current]
		if i := upperBound(ids, upper) - 1; i >= 0 {
			e := clone(s.state.entries[durable.EntryID(ids[i])])
			return &e, nil
		}
		c := s.state.conversations[current]
		if c.Parent == nil {
			return nil, nil
		}
		upper = min(upper, id(c.Parent.At))
		current = c.Parent.ConversationID
	}
}

func (s *Storage) ScanEntries(_ context.Context, q durable.EntryQuery, limit int, cursor durable.Cursor) (durable.Page[durable.EntryRecord], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return durable.Page[durable.EntryRecord]{}, err
	}
	start, err := durable.ResolveScanStart(q.Order, cursor, durable.Descending)
	if err != nil {
		return durable.Page[durable.EntryRecord]{}, err
	}
	// The cursor narrows the bound on the side the scan moves away from.
	minID, maxID := id(math.MinInt64), id(math.MaxInt64)
	if q.MinEntryID != nil {
		minID = id(*q.MinEntryID)
	}
	if q.MaxEntryID != nil {
		maxID = id(*q.MaxEntryID)
	}
	if start.HasAfter && start.Order == durable.Descending {
		maxID = min(maxID, start.After-1)
	}
	if start.HasAfter && start.Order == durable.Ascending {
		minID = max(minID, start.After+1)
	}
	var entries iter.Seq[durable.EntryRecord]
	if start.Order == durable.Descending {
		entries, err = s.visibleEntries(q.ConversationID, minID, maxID)
	} else {
		entries, err = s.visibleEntriesAscending(q.ConversationID, minID, maxID)
	}
	if err != nil {
		return durable.Page[durable.EntryRecord]{}, err
	}
	var visible []durable.EntryRecord
	for e := range entries {
		visible = append(visible, e)
		if len(visible) > limit {
			break
		}
	}
	return page(visible, limit, start.Order, func(e durable.EntryRecord) id { return id(e.ID) }), nil
}

func (s *Storage) Task(_ context.Context, tid durable.TaskID) (*durable.TaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return nil, err
	}
	v, ok := s.state.tasks[tid]
	if !ok {
		return nil, nil
	}
	c := clone(v)
	return &c, nil
}

func (s *Storage) ScanTasks(_ context.Context, q durable.TaskQuery, limit int, cursor durable.Cursor) (durable.Page[durable.TaskRecord], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return durable.Page[durable.TaskRecord]{}, err
	}
	start, err := durable.ResolveScanStart(q.Order, cursor, durable.Ascending)
	if err != nil {
		return durable.Page[durable.TaskRecord]{}, err
	}
	ids := s.state.taskIDs
	if q.Status != "" {
		ids = s.state.taskIDsByStatus[q.Status]
	}
	var values []durable.TaskRecord
	for i := range scanIndexes(ids, start) {
		if len(values) > limit {
			break
		}
		v := s.state.tasks[durable.TaskID(ids[i])]
		if q.ConversationID != nil && v.ConversationID != *q.ConversationID {
			continue
		}
		if q.Kind != "" && v.Kind != q.Kind {
			continue
		}
		if q.AbortRequested != nil && v.AbortRequested != *q.AbortRequested {
			continue
		}
		if q.Background != nil && v.Background != *q.Background {
			continue
		}
		values = append(values, v)
	}
	return page(values, limit, start.Order, func(t durable.TaskRecord) id { return id(t.ID) }), nil
}

func (s *Storage) Submission(_ context.Context, sid durable.SubmissionID) (*durable.SubmissionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return nil, err
	}
	v, ok := s.state.submissions[sid]
	if !ok {
		return nil, nil
	}
	c := clone(v)
	return &c, nil
}

func (s *Storage) ScanSubmissions(_ context.Context, q durable.SubmissionQuery, limit int, cursor durable.Cursor) (durable.Page[durable.SubmissionRecord], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return durable.Page[durable.SubmissionRecord]{}, err
	}
	start, err := durable.ResolveScanStart(q.Order, cursor, durable.Ascending)
	if err != nil {
		return durable.Page[durable.SubmissionRecord]{}, err
	}
	ids := s.state.submissionIDs
	if q.Status != "" {
		ids = s.state.submissionIDsByStatus[q.Status]
	}
	var values []durable.SubmissionRecord
	for i := range scanIndexes(ids, start) {
		if len(values) > limit {
			break
		}
		v := s.state.submissions[durable.SubmissionID(ids[i])]
		if q.ConversationID != nil && v.ConversationID != *q.ConversationID {
			continue
		}
		values = append(values, v)
	}
	return page(values, limit, start.Order, func(r durable.SubmissionRecord) id { return id(r.ID) }), nil
}

func (s *Storage) SubmissionByRequest(_ context.Context, cid durable.ConversationID, requestID string) (*durable.SubmissionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return nil, err
	}
	sid, ok := s.state.submissionIDsByRequest[cid][requestID]
	if !ok {
		return nil, nil
	}
	c := clone(s.state.submissions[sid])
	return &c, nil
}

func (s *Storage) FindDocument(_ context.Context, address durable.DocumentAddress, at durable.DocumentPoint) (*durable.DocumentRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return nil, err
	}
	index := s.state.documentAddresses[addressKey(address)]
	if at == durable.Current {
		if index == nil || index.currentID == nil {
			return nil, nil
		}
		r := clone(s.state.documents[*index.currentID].record)
		return &r, nil
	}
	if index == nil {
		return nil, nil
	}
	for _, did := range index.ids {
		r := s.state.documents[durable.DocumentID(did)].record
		if isAliveAt(r, at) {
			c := clone(r)
			return &c, nil
		}
	}
	return nil, nil
}

func (s *Storage) Document(_ context.Context, did durable.DocumentID, at durable.DocumentPoint) (*durable.StoredDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return nil, err
	}
	stored, err := s.materializeDocument(did, at)
	if err != nil || stored == nil {
		return nil, err
	}
	return &durable.StoredDocument{
		Record:          clone(stored.Record),
		Version:         stored.Version,
		Value:           clone(stored.Value),
		DeltasSinceBase: stored.DeltasSinceBase,
	}, nil
}

func (s *Storage) ScanDocuments(_ context.Context, q durable.DocumentQuery, limit int, cursor durable.Cursor) (durable.Page[durable.DocumentRecord], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.assertOpen(); err != nil {
		return durable.Page[durable.DocumentRecord]{}, err
	}
	ids := s.state.documentIDsByScope[scopeKey(q.Scope)]
	first := 0
	if cursor != nil {
		after, ok := cursor["after"]
		if ok {
			n, isNumber := cursorID(after)
			if !isNumber {
				return durable.Page[durable.DocumentRecord]{}, durable.NewStorageRequestError("Invalid storage cursor")
			}
			first = upperBound(ids, n)
		}
	}
	var values []durable.DocumentRecord
	for i := first; i < len(ids) && len(values) <= limit; i++ {
		r := s.state.documents[durable.DocumentID(ids[i])].record
		if q.Kind != "" && r.Kind != q.Kind {
			continue
		}
		if isAliveAt(r, q.At) {
			values = append(values, r)
		}
	}
	return page(values, limit, durable.Ascending, func(r durable.DocumentRecord) id { return id(r.ID) }), nil
}

func cursorID(v any) (id, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return id(n), true
	case float64:
		if n != math.Trunc(n) || math.Abs(n) > 1<<53-1 {
			return 0, false
		}
		return id(n), true
	}
	return 0, false
}

// Close releases the storage; later operations fail.
func (s *Storage) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *Storage) materializeDocument(did durable.DocumentID, at durable.DocumentPoint) (*durable.StoredDocument, error) {
	stored := s.state.documents[did]
	if stored == nil {
		return nil, nil
	}
	if at != durable.Current && isCurrentOnly(stored.record) {
		return nil, durable.NewStorageRequestError("Document %d does not retain historical content", did)
	}
	if !isAliveAt(stored.record, at) {
		return nil, nil
	}
	revisions := stored.revisions
	if at != durable.Current {
		revisions = slices.DeleteFunc(slices.Clone(revisions), func(r revision) bool { return r.seq > at })
	}
	base := len(revisions) - 1
	for base >= 0 && revisions[base].content.Kind != durable.ContentBase {
		base--
	}
	if base < 0 {
		return nil, fmt.Errorf("Document %d is missing a required base", did)
	}
	version := revisions[base].content.Version
	for _, r := range revisions[base+1:] {
		if r.content.Kind != durable.ContentDelta || r.content.Version != version {
			return nil, fmt.Errorf("Document %d crosses a stored version boundary without a base", did)
		}
	}
	batches := func(yield func([]delta.Op) bool) {
		for _, r := range revisions[base+1:] {
			if !yield(r.content.Ops) {
				return
			}
		}
	}
	value, err := delta.ApplyImmutableBatches(revisions[base].content.Value, batches)
	if err != nil {
		return nil, err
	}
	return &durable.StoredDocument{Record: stored.record, Version: version, Value: value, DeltasSinceBase: len(revisions) - base - 1}, nil
}

func (s *Storage) visibleEntries(cid durable.ConversationID, minID, maxID id) (iter.Seq[durable.EntryRecord], error) {
	if _, ok := s.state.conversations[cid]; !ok {
		return nil, durable.NewStorageRequestError("Unknown conversation: %d", cid)
	}
	return func(yield func(durable.EntryRecord) bool) {
		current, upper := cid, maxID
		for {
			ids := s.state.entryIDs[current]
			for i := upperBound(ids, upper) - 1; i >= 0; i-- {
				if ids[i] < minID {
					break
				}
				if !yield(s.state.entries[durable.EntryID(ids[i])]) {
					return
				}
			}
			c := s.state.conversations[current]
			if c.Parent == nil {
				return
			}
			upper = min(upper, id(c.Parent.At))
			if upper < minID {
				return
			}
			current = c.Parent.ConversationID
		}
	}, nil
}

// visibleEntriesAscending is visible entries oldest first: the fork chain's
// segments from the root conversation forward.
func (s *Storage) visibleEntriesAscending(cid durable.ConversationID, minID, maxID id) (iter.Seq[durable.EntryRecord], error) {
	if _, ok := s.state.conversations[cid]; !ok {
		return nil, durable.NewStorageRequestError("Unknown conversation: %d", cid)
	}
	type segment struct {
		cid   durable.ConversationID
		upper id
	}
	var segments []segment
	current, upper := cid, maxID
	for {
		segments = append(segments, segment{current, upper})
		c := s.state.conversations[current]
		if c.Parent == nil {
			break
		}
		upper = min(upper, id(c.Parent.At))
		if upper < minID {
			break
		}
		current = c.Parent.ConversationID
	}
	slices.Reverse(segments)
	return func(yield func(durable.EntryRecord) bool) {
		for _, seg := range segments {
			ids := s.state.entryIDs[seg.cid]
			for i := lowerBound(ids, minID); i < len(ids); i++ {
				if ids[i] > seg.upper {
					break
				}
				if !yield(s.state.entries[durable.EntryID(ids[i])]) {
					return
				}
			}
		}
	}, nil
}

func (s *Storage) checkGlobalIDs(writes []durable.StorageWrite) error {
	claimed := map[id]string{}
	for _, w := range writes {
		var table string
		var wid id
		switch w := w.(type) {
		case durable.DocumentChangeWrite, durable.DocumentRetireWrite:
			continue
		case durable.ConversationWrite:
			table, wid = tableConversation, id(w.Value.ID)
		case durable.EntryWrite:
			table, wid = tableEntry, id(w.Value.ID)
		case durable.TaskWrite:
			table, wid = tableTask, id(w.Value.ID)
		case durable.SubmissionWrite:
			table, wid = tableSubmission, id(w.Value.ID)
		case durable.DocumentCreateWrite:
			table, wid = tableDocument, id(w.Record.ID)
		case durable.DocumentCopyWrite:
			table, wid = tableDocument, id(w.Record.ID)
		}
		existing, has := s.state.recordTypes[wid]
		earlier, claimedBefore := claimed[wid]
		if table == tableConversation || table == tableEntry || table == tableDocument {
			if has {
				return fmt.Errorf("ID %d already belongs to %s", wid, existing)
			}
			if claimedBefore {
				return fmt.Errorf("ID %d is written more than once", wid)
			}
		} else {
			if has && existing != table {
				return fmt.Errorf("ID %d already belongs to %s", wid, existing)
			}
			if claimedBefore && earlier != table {
				return fmt.Errorf("ID %d is written as two record types", wid)
			}
		}
		claimed[wid] = table
	}
	return nil
}

func prepareDocumentActions(writes []durable.StorageWrite) (map[durable.DocumentID]*documentAction, []durable.DocumentID, error) {
	actions := map[durable.DocumentID]*documentAction{}
	var order []durable.DocumentID
	get := func(did durable.DocumentID) *documentAction {
		a := actions[did]
		if a == nil {
			a = &documentAction{}
			actions[did] = a
			order = append(order, did)
		}
		return a
	}
	for _, w := range writes {
		switch w := w.(type) {
		case durable.DocumentCreateWrite:
			a := get(w.Record.ID)
			if a.create != nil || a.content != nil {
				return nil, nil, fmt.Errorf("Document %d has more than one content command", w.Record.ID)
			}
			record, content := w.Record, w.Content
			a.create, a.content = &record, &content
		case durable.DocumentChangeWrite:
			a := get(w.ID)
			if a.content != nil {
				return nil, nil, fmt.Errorf("Document %d has more than one content command", w.ID)
			}
			content := w.Content
			a.content = &content
		case durable.DocumentRetireWrite:
			a := get(w.ID)
			if a.retire {
				return nil, nil, fmt.Errorf("Document %d is retired more than once", w.ID)
			}
			a.retire = true
		}
	}
	return actions, order, nil
}

func (s *Storage) checkDocumentActions(actions map[durable.DocumentID]*documentAction, order []durable.DocumentID) error {
	liveCounts := map[string]int{}
	var keys []string
	for _, did := range order {
		a := actions[did]
		existing := s.state.documents[did]
		if a.create == nil && existing == nil {
			return fmt.Errorf("Unknown document: %d", did)
		}
		if a.create != nil && existing != nil {
			return fmt.Errorf("Document %d already exists", did)
		}
		if existing != nil && existing.record.RetiredAt != nil {
			return fmt.Errorf("Document %d is retired", did)
		}
		if a.content != nil && a.content.Kind == durable.ContentDelta {
			if existing == nil || len(existing.revisions) == 0 {
				return fmt.Errorf("Document %d delta has no base", did)
			}
			if existing.revisions[len(existing.revisions)-1].content.Version != a.content.Version {
				return fmt.Errorf("Document %d version transition requires a base", did)
			}
		}
		var key string
		if a.create == nil {
			key = addressKey(existing.record.Address())
		} else {
			key = createAddressKey(*a.create)
		}
		var currentID *durable.DocumentID
		if index := s.state.documentAddresses[key]; index != nil {
			currentID = index.currentID
		}
		live, seen := liveCounts[key]
		if !seen {
			keys = append(keys, key)
			if currentID != nil {
				live = 1
			}
		}
		if a.retire && currentID != nil && *currentID == did {
			live--
		}
		if a.create != nil && !a.retire {
			live++
		}
		liveCounts[key] = live
	}
	for _, key := range keys {
		if liveCounts[key] > 1 {
			return errors.New("Document address already has a current incarnation")
		}
	}
	return nil
}

func (s *Storage) applyDocumentActions(actions map[durable.DocumentID]*documentAction, order []durable.DocumentID, seq durable.Seq) {
	st := &s.state
	for _, did := range order {
		a := actions[did]
		stored := st.documents[did]
		if a.create != nil {
			record := durable.DocumentRecord{
				ID: a.create.ID, Kind: a.create.Kind, Key: a.create.Key, Scope: a.create.Scope,
				History: a.create.History, Fork: a.create.Fork, CreatedAt: seq,
			}
			if a.retire {
				retired := seq
				record.RetiredAt = &retired
			}
			stored = &storedDocument{record: record, revisions: []revision{{content: *a.content, seq: seq}}}
			st.recordTypes[id(did)] = tableDocument
			st.documents[did] = stored
			key := addressKey(record.Address())
			index := st.documentAddresses[key]
			if index == nil {
				index = &addressIndex{}
				st.documentAddresses[key] = index
			}
			index.ids = insertSorted(index.ids, id(did))
			sk := scopeKey(record.Scope)
			st.documentIDsByScope[sk] = insertSorted(st.documentIDsByScope[sk], id(did))
			s.nextID = max(s.nextID, id(did)+1)
		} else if a.content != nil {
			r := revision{content: *a.content, seq: seq}
			if r.content.Kind == durable.ContentBase && isCurrentOnly(stored.record) {
				stored.revisions = []revision{r}
			} else {
				stored.revisions = append(stored.revisions, r)
			}
		}
		if a.retire && a.create == nil {
			retired := seq
			stored.record.RetiredAt = &retired
		}
		if a.retire && isCurrentOnly(stored.record) {
			stored.revisions = nil
		}
		if a.create != nil || a.retire {
			index := st.documentAddresses[addressKey(stored.record.Address())]
			if a.retire && index.currentID != nil && *index.currentID == did {
				index.currentID = nil
			}
			if a.create != nil && !a.retire {
				current := did
				index.currentID = &current
			}
		}
	}
}
