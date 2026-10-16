// Package jsonl is durable's portable JSONL Storage (pi
// packages/durable/src/storage/jsonl).
//
// A directory holds main.jsonl, one commit marker per line, and sidecars
// doc-<id>.jsonl and task-<id>.jsonl with document content and live task
// records. A commit appends its sidecar records first and its marker last;
// the marker publishes them, so a crash leaves at most an unconfirmed tail
// that reopening cuts off. Current-only documents and terminal tasks are
// compacted after their marker is published. State is replayed into an
// in-memory storage, which serves every read.
//
// Unlike pi, a commit holding a string that is not valid UTF-8 is rejected
// before any I/O (see durabletest.StorageOptions.RejectsInvalidUTF8), and a
// line holding a lone surrogate escape, which pi's files can, is refused as
// corruption rather than read lossily.
package jsonl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

const (
	mainFile      = "main.jsonl"
	reclaimSuffix = ".reclaim"
)

var (
	sidecarFileName = regexp.MustCompile(`^(?:doc|task)-(?:0|[1-9]\d*)\.jsonl$`)
	reclaimFileName = regexp.MustCompile(`^(?:doc|task)-(?:0|[1-9]\d*)\.jsonl\.reclaim$`)
)

func sidecarFile(kind string, id int64) string { return fmt.Sprintf("%s-%d.jsonl", kind, id) }

func docFile(id durable.DocumentID) string { return sidecarFile("doc", int64(id)) }

func taskFile(id durable.TaskID) string { return sidecarFile("task", int64(id)) }

func isCurrentOnly(r durable.DocumentCreate) bool {
	return r.Scope.Kind != durable.ScopeConversation || r.History == durable.HistoryLatest
}

// Options configure a JsonlStorage.
type Options struct {
	// Fsync flushes every affected sidecar before appending the main marker.
	Fsync bool
}

// CorruptionError is a directory whose complete, published content is
// inconsistent; opening it fails.
type CorruptionError struct {
	Message string
	Cause   error
}

func (e *CorruptionError) Error() string { return e.Message }
func (e *CorruptionError) Unwrap() error { return e.Cause }

func corruption(cause error, format string, args ...any) *CorruptionError {
	return &CorruptionError{Message: fmt.Sprintf(format, args...), Cause: cause}
}

// PoisonedError is every operation of a storage after a commit's write
// failed part-way: its files may hold an unconfirmed tail, so only reopening
// knows the published state.
type PoisonedError struct{ Cause error }

func (e *PoisonedError) Error() string { return "JSONL storage is poisoned and must be reopened" }
func (e *PoisonedError) Unwrap() error { return e.Cause }

func fileError(action string, err error) error {
	return fmt.Errorf("JSONL %s failed: %w", action, err)
}

// files is an insertion-ordered file → content map: pi's Map, whose order
// fixes the order of file operations.
type files struct {
	names    []string
	contents map[string][]byte
}

func (f *files) set(name string, content []byte) {
	if f.contents == nil {
		f.contents = map[string][]byte{}
	}
	if _, ok := f.contents[name]; !ok {
		f.names = append(f.names, name)
	}
	f.contents[name] = content
}

func (f *files) get(name string) ([]byte, bool) {
	c, ok := f.contents[name]
	return c, ok
}

type encodedCommit struct {
	marker   []byte
	sidecars files
}

// Storage is the JSONL Storage. Reads are safe for concurrent use; commits
// are serialized.
type Storage struct {
	fsys      FS
	directory string
	mainPath  string
	fsync     bool
	memory    *memory.Storage

	commitMu             sync.Mutex
	currentOnlyDocuments map[durable.DocumentID]bool
	liveTaskSidecars     map[durable.TaskID]bool

	mu     sync.Mutex
	closed bool
	poison *PoisonedError
}

var _ durable.Storage = (*Storage)(nil)

// Open opens or creates a JSONL storage directory on fsys.
func Open(ctx context.Context, directory string, fsys FS, options Options) (*Storage, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, fileError("path resolution", err)
	}
	if err := fsys.MkdirAll(ctx, absolute); err != nil {
		return nil, fileError("directory creation", err)
	}
	s := &Storage{
		fsys:                 fsys,
		directory:            absolute,
		mainPath:             filepath.Join(absolute, mainFile),
		fsync:                options.Fsync,
		memory:               memory.New(),
		currentOnlyDocuments: map[durable.DocumentID]bool{},
		liveTaskSidecars:     map[durable.TaskID]bool{},
	}
	if err := s.recover(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenDir opens or creates a JSONL storage directory on the local file
// system (pi openNodeJsonlStorage).
func OpenDir(ctx context.Context, directory string, options Options) (*Storage, error) {
	return Open(ctx, directory, OSFS{}, options)
}

func (s *Storage) path(file string) string { return filepath.Join(s.directory, file) }

// Commit atomically persists one batch.
func (s *Storage) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if err := s.usable(); err != nil {
		return 0, err
	}
	prepared, err := s.memory.PrepareCommit(writes, 0)
	if err != nil {
		return 0, err
	}
	if err := checkUTF8(prepared.Writes); err != nil {
		return 0, err
	}
	encoded, err := s.encodeCommit(prepared.Seq, prepared.Writes)
	if err != nil {
		return 0, err
	}
	reclamations := s.planReclamations(prepared.Writes, &encoded)

	for _, file := range encoded.sidecars.names {
		content, _ := encoded.sidecars.get(file)
		if err := s.fsys.AppendFile(ctx, s.path(file), content); err != nil {
			return 0, s.poisoned(fileError("append to "+file, err))
		}
	}
	if s.fsync {
		for _, file := range encoded.sidecars.names {
			if err := s.fsys.Flush(ctx, s.path(file)); err != nil {
				return 0, s.poisoned(fileError("flush of "+file, err))
			}
		}
	}
	if err := s.fsys.AppendFile(ctx, s.mainPath, encoded.marker); err != nil {
		return 0, s.poisoned(fileError("append to "+mainFile, err))
	}
	seq := prepared.Apply()
	s.adoptSidecarState(prepared.Writes)
	s.reclaimSidecars(ctx, &reclamations)
	return seq, nil
}

func (s *Storage) MintID(ctx context.Context) (durable.ID, error) {
	if err := s.usable(); err != nil {
		return 0, err
	}
	return s.memory.MintID(ctx)
}

func (s *Storage) Conversation(ctx context.Context, id durable.ConversationID) (*durable.ConversationRecord, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.memory.Conversation(ctx, id)
}

func (s *Storage) ScanConversations(ctx context.Context, query durable.ConversationQuery, limit int, cursor durable.Cursor) (durable.Page[durable.ConversationRecord], error) {
	if err := s.usable(); err != nil {
		return durable.Page[durable.ConversationRecord]{}, err
	}
	return s.memory.ScanConversations(ctx, query, limit, cursor)
}

func (s *Storage) Entry(ctx context.Context, id durable.EntryID) (*durable.StoredEntry, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.memory.Entry(ctx, id)
}

func (s *Storage) VisibleEntry(ctx context.Context, conversationID durable.ConversationID, id durable.EntryID) (*durable.StoredEntry, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.memory.VisibleEntry(ctx, conversationID, id)
}

func (s *Storage) FindLatestHeadMarker(ctx context.Context, conversationID durable.ConversationID, atOrBefore *durable.EntryID) (*durable.EntryRecord, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.memory.FindLatestHeadMarker(ctx, conversationID, atOrBefore)
}

func (s *Storage) ScanEntries(ctx context.Context, query durable.EntryQuery, limit int, cursor durable.Cursor) (durable.Page[durable.EntryRecord], error) {
	if err := s.usable(); err != nil {
		return durable.Page[durable.EntryRecord]{}, err
	}
	return s.memory.ScanEntries(ctx, query, limit, cursor)
}

func (s *Storage) Task(ctx context.Context, id durable.TaskID) (*durable.TaskRecord, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.memory.Task(ctx, id)
}

func (s *Storage) ScanTasks(ctx context.Context, query durable.TaskQuery, limit int, cursor durable.Cursor) (durable.Page[durable.TaskRecord], error) {
	if err := s.usable(); err != nil {
		return durable.Page[durable.TaskRecord]{}, err
	}
	return s.memory.ScanTasks(ctx, query, limit, cursor)
}

func (s *Storage) Submission(ctx context.Context, id durable.SubmissionID) (*durable.SubmissionRecord, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.memory.Submission(ctx, id)
}

func (s *Storage) ScanSubmissions(ctx context.Context, query durable.SubmissionQuery, limit int, cursor durable.Cursor) (durable.Page[durable.SubmissionRecord], error) {
	if err := s.usable(); err != nil {
		return durable.Page[durable.SubmissionRecord]{}, err
	}
	return s.memory.ScanSubmissions(ctx, query, limit, cursor)
}

func (s *Storage) SubmissionByRequest(ctx context.Context, conversationID durable.ConversationID, requestID string) (*durable.SubmissionRecord, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.memory.SubmissionByRequest(ctx, conversationID, requestID)
}

func (s *Storage) FindDocument(ctx context.Context, address durable.DocumentAddress, at durable.DocumentPoint) (*durable.DocumentRecord, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.memory.FindDocument(ctx, address, at)
}

func (s *Storage) Document(ctx context.Context, id durable.DocumentID, at durable.DocumentPoint) (*durable.StoredDocument, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.memory.Document(ctx, id, at)
}

func (s *Storage) ScanDocuments(ctx context.Context, query durable.DocumentQuery, limit int, cursor durable.Cursor) (durable.Page[durable.DocumentRecord], error) {
	if err := s.usable(); err != nil {
		return durable.Page[durable.DocumentRecord]{}, err
	}
	return s.memory.ScanDocuments(ctx, query, limit, cursor)
}

// Close releases the storage; later operations fail.
func (s *Storage) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.memory.Close(ctx)
}

func (s *Storage) usable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("JsonlStorage is closed")
	}
	if s.poison != nil {
		return s.poison
	}
	return nil
}

func (s *Storage) poisoned(cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poison == nil {
		s.poison = &PoisonedError{Cause: cause}
	}
	return s.poison
}

func (s *Storage) encodeCommit(seq durable.Seq, writes []durable.StorageWrite) (encodedCommit, error) {
	var mainWrites []any
	var lines files
	nextOrdinal := 0
	var encodeErr error
	addSidecar := func(file string, payload any) int {
		ordinal := nextOrdinal
		nextOrdinal++
		line, err := jsonLine(sidecarLine{Format: formatVersion, Type: "record", Seq: seq, Ordinal: ordinal, Payload: payload})
		if err != nil {
			encodeErr = errors.Join(encodeErr, err)
		}
		previous, _ := lines.get(file)
		lines.set(file, append(previous, line...))
		return ordinal
	}

	for _, w := range writes {
		switch w := w.(type) {
		case durable.ConversationWrite:
			mainWrites = append(mainWrites, valueOp{Type: w.WriteType(), Value: w.Value})
		case durable.EntryWrite:
			mainWrites = append(mainWrites, valueOp{Type: w.WriteType(), Value: w.Value})
		case durable.SubmissionWrite:
			mainWrites = append(mainWrites, valueOp{Type: w.WriteType(), Value: w.Value})
		case durable.DocumentRetireWrite:
			mainWrites = append(mainWrites, idOp{Type: w.WriteType(), ID: int64(w.ID)})
		case durable.TaskWrite:
			if w.Value.State.Status == durable.TaskTerminal {
				mainWrites = append(mainWrites, valueOp{Type: w.WriteType(), Value: w.Value})
			} else {
				ordinal := addSidecar(taskFile(w.Value.ID), taskPayload{Type: "task", Value: w.Value})
				mainWrites = append(mainWrites, ordinalOp{Type: opTaskSidecar, ID: int64(w.Value.ID), Ordinal: ordinal})
			}
		case durable.DocumentCreateWrite:
			ordinal := addSidecar(docFile(w.Record.ID), documentPayload{Type: "document", ID: w.Record.ID, Content: wireContent(w.Content)})
			mainWrites = append(mainWrites, createOp{Type: w.WriteType(), Record: w.Record, Ordinal: ordinal})
		case durable.DocumentChangeWrite:
			ordinal := addSidecar(docFile(w.ID), documentPayload{Type: "document", ID: w.ID, Content: wireContent(w.Content)})
			mainWrites = append(mainWrites, ordinalOp{Type: w.WriteType(), ID: int64(w.ID), Ordinal: ordinal})
		default:
			// Document copies are resolved into creations by PrepareCommit.
			return encodedCommit{}, fmt.Errorf("JsonlStorage cannot persist a %s write", w.WriteType())
		}
	}
	if encodeErr != nil {
		return encodedCommit{}, encodeErr
	}
	if mainWrites == nil {
		mainWrites = []any{}
	}
	marker, err := jsonLine(markerLine{Format: formatVersion, Type: "commit", Seq: seq, Writes: mainWrites})
	if err != nil {
		return encodedCommit{}, err
	}
	return encodedCommit{marker: marker, sidecars: lines}, nil
}

func (s *Storage) planReclamations(writes []durable.StorageWrite, encoded *encodedCommit) files {
	createdCurrentOnly := map[durable.DocumentID]bool{}
	var retired, bases []durable.DocumentID
	var taskOrder []durable.TaskID
	finalTasks := map[durable.TaskID]durable.TaskRecord{}
	for _, w := range writes {
		switch w := w.(type) {
		case durable.DocumentCreateWrite:
			if isCurrentOnly(w.Record) {
				createdCurrentOnly[w.Record.ID] = true
			}
		case durable.DocumentChangeWrite:
			if w.Content.Kind == durable.ContentBase && !slices.Contains(bases, w.ID) {
				bases = append(bases, w.ID)
			}
		case durable.DocumentRetireWrite:
			if !slices.Contains(retired, w.ID) {
				retired = append(retired, w.ID)
			}
		case durable.TaskWrite:
			if _, ok := finalTasks[w.Value.ID]; !ok {
				taskOrder = append(taskOrder, w.Value.ID)
			}
			finalTasks[w.Value.ID] = w.Value
		}
	}

	var replacements files
	currentOnly := func(id durable.DocumentID) bool { return s.currentOnlyDocuments[id] || createdCurrentOnly[id] }
	for _, id := range retired {
		if currentOnly(id) {
			replacements.set(docFile(id), nil)
		}
	}
	for _, id := range bases {
		if !currentOnly(id) || slices.Contains(retired, id) {
			continue
		}
		if content, ok := encoded.sidecars.get(docFile(id)); ok {
			replacements.set(docFile(id), content)
		}
	}
	for _, id := range taskOrder {
		_, encodedHere := encoded.sidecars.get(taskFile(id))
		if finalTasks[id].State.Status == durable.TaskTerminal && (s.liveTaskSidecars[id] || encodedHere) {
			replacements.set(taskFile(id), nil)
		}
	}
	return replacements
}

func (s *Storage) adoptSidecarState(writes []durable.StorageWrite) {
	for _, w := range writes {
		switch w := w.(type) {
		case durable.DocumentCreateWrite:
			if isCurrentOnly(w.Record) {
				s.currentOnlyDocuments[w.Record.ID] = true
			}
		case durable.TaskWrite:
			if w.Value.State.Status == durable.TaskTerminal {
				delete(s.liveTaskSidecars, w.Value.ID)
			} else {
				s.liveTaskSidecars[w.Value.ID] = true
			}
		}
	}
}

// reclaimSidecars compacts or removes sidecars. The marker already published
// the state, so this is retryable best-effort maintenance: a failure leaves
// work for the next reopen.
func (s *Storage) reclaimSidecars(ctx context.Context, replacements *files) {
	if len(replacements.names) == 0 {
		return
	}
	if s.fsync {
		if err := s.fsys.Flush(ctx, s.mainPath); err != nil {
			return
		}
	}
	for _, file := range replacements.names {
		content, _ := replacements.get(file)
		s.replaceSidecar(ctx, file, content)
	}
}

func (s *Storage) replaceSidecar(ctx context.Context, file string, content []byte) {
	path := s.path(file)
	if len(content) == 0 {
		_ = s.fsys.Remove(ctx, path)
		return
	}
	temporary := s.path(file + reclaimSuffix)
	if err := s.fsys.WriteFile(ctx, temporary, content); err != nil {
		return
	}
	if s.fsync {
		if err := s.fsys.Flush(ctx, temporary); err != nil {
			return
		}
	}
	_ = s.fsys.Rename(ctx, temporary, path)
}

type parsedLine[T any] struct {
	value T
	start int64
}

type parsedFile[T any] struct {
	path  string
	lines []parsedLine[T]
}

type recordKey struct {
	file    string
	seq     durable.Seq
	ordinal int
}

func (s *Storage) recover(ctx context.Context) error {
	main, err := readLines(ctx, s.fsys, s.mainPath, mainFile, parseMainMarker)
	if err != nil {
		return err
	}
	var previousSeq durable.Seq
	for _, line := range main.lines {
		if line.value.seq <= previousSeq {
			return corruption(nil, "Commit sequence does not strictly increase in %s", mainFile)
		}
		previousSeq = line.value.seq
	}

	listed, err := s.fsys.ReadDir(ctx, s.directory)
	if err != nil {
		return fileError("directory listing", err)
	}
	var sidecarFiles []string
	for _, info := range listed {
		if info.IsDir() || !info.Type().IsRegular() {
			continue
		}
		if reclaimFileName.MatchString(info.Name()) {
			_ = s.fsys.Remove(ctx, s.path(info.Name()))
		} else if sidecarFileName.MatchString(info.Name()) {
			sidecarFiles = append(sidecarFiles, info.Name())
		}
	}
	slices.Sort(sidecarFiles)

	parsedFiles := make([]parsedFile[sidecarRecord], 0, len(sidecarFiles))
	recordByKey := map[recordKey]sidecarRecord{}
	for _, file := range sidecarFiles {
		parsed, err := readLines(ctx, s.fsys, s.path(file), file, func(text []byte, line int) (sidecarRecord, error) {
			return parseSidecarRecord(text, file, line)
		})
		if err != nil {
			return err
		}
		parsedFiles = append(parsedFiles, parsed)
		for i, line := range parsed.lines {
			if i > 0 {
				previous := parsed.lines[i-1].value
				if line.value.seq < previous.seq || (line.value.seq == previous.seq && line.value.ordinal <= previous.ordinal) {
					return corruption(nil, "Sidecar records are out of order in %s", file)
				}
			}
			recordByKey[recordKey{file, line.value.seq, line.value.ordinal}] = line.value
		}
	}

	currentOnly := map[durable.DocumentID]bool{}
	retired := map[durable.DocumentID]bool{}
	finalTaskIsLive := map[durable.TaskID]bool{}
	for _, line := range main.lines {
		for _, op := range line.value.writes {
			switch op.typ {
			case "document.create":
				if isCurrentOnly(op.record) {
					currentOnly[op.record.ID] = true
				}
			case "document.retire":
				retired[durable.DocumentID(op.id)] = true
			case "task":
				finalTaskIsLive[op.write.(durable.TaskWrite).Value.ID] = false
			case opTaskSidecar:
				finalTaskIsLive[durable.TaskID(op.id)] = true
			}
		}
	}
	retiredCurrentOnly := func(id durable.DocumentID) bool { return retired[id] && currentOnly[id] }

	latestBases := map[durable.DocumentID]sidecarRecord{}
	for _, line := range main.lines {
		marker := line.value
		for _, op := range marker.writes {
			if op.typ != "document.create" && op.typ != "document.change" {
				continue
			}
			id := durable.DocumentID(op.id)
			if !currentOnly[id] {
				continue
			}
			record, ok := recordByKey[recordKey{docFile(id), marker.seq, op.ordinal}]
			if !ok || record.isTask || record.docID != id || record.content.Kind != durable.ContentBase {
				continue
			}
			previous, seen := latestBases[id]
			if !seen || record.seq > previous.seq || (record.seq == previous.seq && record.ordinal > previous.ordinal) {
				latestBases[id] = record
			}
		}
	}
	beforeLatestBase := func(id durable.DocumentID, seq durable.Seq, ordinal int) bool {
		base, ok := latestBases[id]
		return ok && (seq < base.seq || (seq == base.seq && ordinal < base.ordinal))
	}
	terminalTask := func(id durable.TaskID) bool { live, ok := finalTaskIsLive[id]; return ok && !live }

	confirmed := map[recordKey]bool{}
	confirm := func(marker mainMarker, ordinal int, file string, optional bool) (*sidecarRecord, error) {
		key := recordKey{file, marker.seq, ordinal}
		if confirmed[key] {
			return nil, corruption(nil, "Sidecar record is confirmed more than once")
		}
		record, ok := recordByKey[key]
		if !ok {
			if optional {
				return nil, nil
			}
			return nil, corruption(nil, "Missing confirmed sidecar record %s at sequence %d", file, marker.seq)
		}
		confirmed[key] = true
		return &record, nil
	}

	for _, line := range main.lines {
		marker := line.value
		var writes []durable.StorageWrite
		for _, op := range marker.writes {
			switch op.typ {
			case "conversation", "entry", "submission", "task", "document.retire":
				writes = append(writes, op.write)
			case opTaskSidecar:
				id := durable.TaskID(op.id)
				optional := terminalTask(id)
				record, err := confirm(marker, op.ordinal, taskFile(id), optional)
				if err != nil {
					return err
				}
				if record != nil {
					if !record.isTask || record.task.ID != id {
						return corruption(nil, "Confirmed task sidecar data does not match commit %d", marker.seq)
					}
					if !optional {
						writes = append(writes, durable.TaskWrite{Value: record.task})
					}
				}
			case "document.create", "document.change":
				id := durable.DocumentID(op.id)
				reclaimed := retiredCurrentOnly(id) || beforeLatestBase(id, marker.seq, op.ordinal)
				record, err := confirm(marker, op.ordinal, docFile(id), reclaimed)
				if err != nil {
					return err
				}
				var content *durable.DocumentContent
				if record != nil {
					if record.isTask || record.docID != id {
						return corruption(nil, "Confirmed document sidecar data does not match commit %d", marker.seq)
					}
					content = &record.content
				}
				if op.typ == "document.create" {
					if content != nil && content.Kind != durable.ContentBase {
						return corruption(nil, "Document creation lacks a confirmed base in commit %d", marker.seq)
					}
					created := durable.DocumentContent{Kind: durable.ContentBase, Version: 1, Value: map[string]any{}}
					if !reclaimed && content != nil {
						created = *content
					}
					writes = append(writes, durable.DocumentCreateWrite{Record: op.record, Content: created})
				} else if !reclaimed && content != nil {
					writes = append(writes, durable.DocumentChangeWrite{ID: id, Content: *content})
				}
			}
		}
		prepared, err := s.memory.PrepareCommit(writes, marker.seq)
		if err != nil {
			return corruption(err, "Invalid committed state at sequence %d", marker.seq)
		}
		prepared.Apply()
	}

	var reclamations files
	for _, parsed := range parsedFiles {
		file := filepath.Base(parsed.path)
		unconfirmedAt := int64(-1)
		var confirmedLines []parsedLine[sidecarRecord]
		for _, line := range parsed.lines {
			if confirmed[recordKey{file, line.value.seq, line.value.ordinal}] {
				if unconfirmedAt >= 0 {
					return corruption(nil, "Confirmed record follows an unconfirmed tail in %s", file)
				}
				confirmedLines = append(confirmedLines, line)
			} else if unconfirmedAt < 0 {
				unconfirmedAt = line.start
			}
		}
		if unconfirmedAt >= 0 {
			if err := s.fsys.Truncate(ctx, parsed.path, unconfirmedAt); err != nil {
				return fileError("tail truncation of "+file, err)
			}
		}

		numericID, _ := strconv.ParseInt(file[strings.IndexByte(file, '-')+1:len(file)-len(".jsonl")], 10, 64)
		var retained []parsedLine[sidecarRecord]
		compact := false
		switch {
		case strings.HasPrefix(file, "task-") && terminalTask(durable.TaskID(numericID)):
			compact = true
		case strings.HasPrefix(file, "doc-"):
			id := durable.DocumentID(numericID)
			if retiredCurrentOnly(id) {
				compact = true
			} else if _, ok := latestBases[id]; ok {
				compact = true
				for _, line := range confirmedLines {
					if !beforeLatestBase(id, line.value.seq, line.value.ordinal) {
						retained = append(retained, line)
					}
				}
			}
		}
		if compact && (len(retained) < len(confirmedLines) || len(retained) == 0) {
			var content []byte
			for _, line := range retained {
				content = append(append(content, line.value.text...), '\n')
			}
			reclamations.set(file, content)
		}
	}
	s.reclaimSidecars(ctx, &reclamations)

	for id := range currentOnly {
		s.currentOnlyDocuments[id] = true
	}
	for id, live := range finalTaskIsLive {
		if live {
			s.liveTaskSidecars[id] = true
		}
	}
	return nil
}

// readLines reads a file's complete lines, cutting off a torn last line
// (one without its newline). A missing file has no lines.
func readLines[T any](ctx context.Context, fsys FS, path, name string, parse func(text []byte, line int) (T, error)) (parsedFile[T], error) {
	data, err := fsys.ReadFile(ctx, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return parsedFile[T]{path: path}, nil
		}
		return parsedFile[T]{}, fileError("read of "+name, err)
	}
	complete := len(data)
	if complete > 0 && data[complete-1] != '\n' {
		complete = bytes.LastIndexByte(data, '\n') + 1
		if err := fsys.Truncate(ctx, path, int64(complete)); err != nil {
			return parsedFile[T]{}, fileError("torn-line truncation of "+name, err)
		}
	}
	out := parsedFile[T]{path: path}
	start, number := 0, 1
	for end := 0; end < complete; end++ {
		if data[end] != '\n' {
			continue
		}
		text := data[start:end]
		if !utf8.Valid(text) {
			return parsedFile[T]{}, corruption(nil, "Invalid UTF-8 in complete %s line %d", name, number)
		}
		value, err := parse(text, number)
		if err != nil {
			return parsedFile[T]{}, err
		}
		out.lines = append(out.lines, parsedLine[T]{value: value, start: int64(start)})
		start = end + 1
		number++
	}
	return out, nil
}
