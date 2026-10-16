// Package sqlite is durable's portable SQLite Storage (pi
// packages/durable/src/storage/sqlite).
//
// The storage runs on a database/sql database of any SQLite driver, which
// takes the place of pi's SqliteDatabase facade; the file adapter on
// modernc.org/sqlite lives in the durable/storage/sqlite/modernc module, so
// this package needs nothing beyond the standard library. The schema and
// its migrations are pi's, so a database either side created reads the
// same.
//
// The database must run transactions as BEGIN IMMEDIATE and serialize them,
// as pi's adapter does; the file adapter keeps one connection. Unlike pi, a
// commit holding a string that is not valid UTF-8 is rejected (see
// durabletest.StorageOptions.RejectsInvalidUTF8).
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/internal/utf8check"
)

const maxSafeInteger = 1<<53 - 1

// Options configure a SqliteStorage.
type Options struct {
	// Prefix is put before every table and index name; "" is none. It must
	// be a SQLite identifier outside SQLite's reserved "sqlite_" prefix.
	// Existing data is not migrated when it changes.
	Prefix string
	// BeforeClose runs on the database after the storage's last operation
	// and before the database closes: an adapter's final maintenance, such
	// as the file adapter's WAL truncation. Close returns its error and
	// closes the database anyway.
	BeforeClose func(ctx context.Context, db *sql.DB) error
}

// querier is a database or one of its transactions.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Storage is the SQLite Storage. It is safe for concurrent use.
type Storage struct {
	db          *sql.DB
	schema      Schema
	beforeClose func(ctx context.Context, db *sql.DB) error

	idMu   sync.Mutex
	nextID int64

	// life is held shared by every operation and exclusively by Close, so
	// Close waits for the operations it admitted, and their later queries
	// never reach a closed database.
	life      sync.RWMutex
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

var _ durable.Storage = (*Storage)(nil)

var errClosed = errors.New("SqliteStorage is closed")

// Open initializes storage over a database it takes ownership of: it
// applies pending migrations, and closes the database when it fails and on
// Close.
func Open(ctx context.Context, db *sql.DB, options Options) (*Storage, error) {
	s, err := open(ctx, db, options)
	if err != nil {
		_ = db.Close() // Preserve the initialization failure.
		return nil, err
	}
	return s, nil
}

func open(ctx context.Context, db *sql.DB, options Options) (*Storage, error) {
	schema, err := NewSchema(options.Prefix)
	if err != nil {
		return nil, err
	}
	if err := ApplyMigrationsForSchema(ctx, db, schema, nil); err != nil {
		return nil, err
	}
	var nextID string
	var nextSeq int64
	err = db.QueryRowContext(ctx, `SELECT next_id, next_seq FROM `+schema.Tables.DurableMetadata+` WHERE singleton = 1`).Scan(&nextID, &nextSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("Durable SQLite metadata is missing")
	}
	if err != nil {
		return nil, err
	}
	n, err := strconv.ParseInt(nextID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("Durable SQLite metadata has an invalid next ID %q", nextID)
	}
	return &Storage{db: db, schema: schema, beforeClose: options.BeforeClose, nextID: n}, nil
}

// admit admits one operation; the caller must call s.life.RUnlock after it.
func (s *Storage) admit() error {
	s.life.RLock()
	if s.closed {
		s.life.RUnlock()
		return errClosed
	}
	return nil
}

// Close waits for admitted operations, then closes the database; later
// operations fail. Every call returns the first call's result once the
// database is closed.
func (s *Storage) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.life.Lock()
		s.closed = true
		s.life.Unlock()
		var hookErr error
		if s.beforeClose != nil {
			hookErr = s.beforeClose(ctx, s.db)
		}
		s.closeErr = errors.Join(hookErr, s.db.Close())
	})
	return s.closeErr
}

// encodeJSON is JSON.stringify for records: no HTML escaping, no newline.
func encodeJSON(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// indexedString is pi's encodeIndexedString, JSON.stringify of a string,
// byte for byte, so indexed columns match a database pi wrote. Strings are
// valid UTF-8 here (commits check it), so no surrogate needs escaping.
func indexedString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

type addressParts struct {
	kind      string
	scopeKind string
	ownerID   int64
	family    int
	keyValue  string
}

func scopeColumns(scope durable.DocumentScope) (string, int64) {
	switch scope.Kind {
	case durable.ScopeConversation:
		return durable.ScopeConversation, int64(scope.ConversationID)
	case durable.ScopeTask:
		return durable.ScopeTask, int64(scope.TaskID)
	}
	return durable.ScopeSession, 0
}

func newAddressParts(kind string, key *string, scope durable.DocumentScope) addressParts {
	scopeKind, ownerID := scopeColumns(scope)
	p := addressParts{kind: indexedString(kind), scopeKind: scopeKind, ownerID: ownerID, keyValue: indexedString("")}
	if key != nil {
		p.family, p.keyValue = 1, indexedString(*key)
	}
	return p
}

func (p addressParts) key() string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s", p.kind, p.scopeKind, p.ownerID, p.family, p.keyValue)
}

func isAliveAt(r durable.DocumentRecord, at durable.DocumentPoint) bool {
	if at == durable.Current {
		return r.RetiredAt == nil
	}
	return r.CreatedAt <= at && (r.RetiredAt == nil || at < *r.RetiredAt)
}

func isCurrentOnly(r durable.DocumentRecord) bool {
	return r.Scope.Kind != durable.ScopeConversation || r.History == durable.HistoryLatest
}

func record[T any](ctx context.Context, q querier, query string, args ...any) (*T, error) {
	var text string
	err := q.QueryRowContext(ctx, query, args...).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func records[T any](ctx context.Context, q querier, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		var v T
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func page[T any](values []T, limit int, order durable.ScanOrder, idOf func(T) int64) durable.Page[T] {
	if values == nil {
		values = []T{}
	}
	if len(values) <= limit {
		return durable.Page[T]{Items: values}
	}
	items := values[:limit]
	return durable.Page[T]{Items: items, Next: durable.NextCursor(idOf(items[len(items)-1]), order)}
}

// scanSQL is the ID condition, its parameter and the ORDER BY direction of a
// table scan.
func scanSQL(start durable.ScanStart) (clause string, param int64, direction string) {
	if start.Order == durable.Ascending {
		param = -1
		if start.HasAfter {
			param = start.After
		}
		return "id > ?", param, "ASC"
	}
	param = maxSafeInteger
	if start.HasAfter {
		param = start.After
	}
	return "id < ?", param, "DESC"
}

func cursorID(cursor durable.Cursor) (int64, bool, error) {
	after, ok := cursor["after"]
	if !ok {
		return 0, false, nil
	}
	switch n := after.(type) {
	case int64:
		return n, true, nil
	case int:
		return int64(n), true, nil
	case float64:
		if n == math.Trunc(n) && math.Abs(n) <= maxSafeInteger {
			return int64(n), true, nil
		}
	}
	return 0, false, durable.NewStorageRequestError("Invalid storage cursor")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// MintID returns a fresh ID.
func (s *Storage) MintID(context.Context) (durable.ID, error) {
	if err := s.admit(); err != nil {
		return 0, err
	}
	defer s.life.RUnlock()
	s.idMu.Lock()
	defer s.idMu.Unlock()
	if s.nextID > maxSafeInteger {
		return 0, errors.New("ID space is exhausted")
	}
	v := s.nextID
	s.nextID++
	return v, nil
}

// Commit atomically persists one batch.
func (s *Storage) Commit(ctx context.Context, writes []durable.StorageWrite) (durable.Seq, error) {
	if err := s.admit(); err != nil {
		return 0, err
	}
	defer s.life.RUnlock()
	if err := utf8check.Writes("SqliteStorage", writes); err != nil {
		return 0, err
	}
	actions, err := prepareDocumentActions(writes)
	if err != nil {
		return 0, err
	}
	candidate, err := s.candidateNextID(writes)
	if err != nil {
		return 0, err
	}
	t := s.schema.Tables
	var seq durable.Seq
	err = transact(ctx, s.db, func(tx *sql.Tx) error {
		var storedNextID string
		var nextSeq int64
		err := tx.QueryRowContext(ctx, `SELECT next_id, next_seq FROM `+t.DurableMetadata+` WHERE singleton = 1`).Scan(&storedNextID, &nextSeq)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("Durable SQLite metadata is missing")
		}
		if err != nil {
			return err
		}
		stored, err := strconv.ParseInt(storedNextID, 10, 64)
		if err != nil {
			return fmt.Errorf("Durable SQLite metadata has an invalid next ID %q", storedNextID)
		}
		committed := durable.Seq(nextSeq)
		if err := s.checkGlobalIDs(ctx, tx, writes); err != nil {
			return err
		}
		if err := s.checkDocumentActions(ctx, tx, actions); err != nil {
			return err
		}
		for _, w := range writes {
			if err := s.applyTableWrite(ctx, tx, w, committed); err != nil {
				return err
			}
		}
		if err := s.applyDocumentActions(ctx, tx, actions, committed); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+t.DurableMetadata+` SET next_id = ?, next_seq = ? WHERE singleton = 1`,
			strconv.FormatInt(max(stored, candidate), 10), int64(committed)+1); err != nil {
			return err
		}
		seq = committed
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.idMu.Lock()
	s.nextID = max(s.nextID, candidate)
	s.idMu.Unlock()
	return seq, nil
}

func writeID(w durable.StorageWrite) (int64, bool, error) {
	switch w := w.(type) {
	case durable.ConversationWrite:
		return int64(w.Value.ID), true, nil
	case durable.EntryWrite:
		return int64(w.Value.ID), true, nil
	case durable.TaskWrite:
		return int64(w.Value.ID), true, nil
	case durable.SubmissionWrite:
		return int64(w.Value.ID), true, nil
	case durable.DocumentCreateWrite:
		return int64(w.Record.ID), true, nil
	case durable.DocumentCopyWrite:
		return int64(w.Record.ID), true, nil
	case durable.DocumentChangeWrite, durable.DocumentRetireWrite:
		return 0, false, nil
	}
	return 0, false, fmt.Errorf("Unknown storage write: %T", w)
}

func (s *Storage) candidateNextID(writes []durable.StorageWrite) (int64, error) {
	s.idMu.Lock()
	next := s.nextID
	s.idMu.Unlock()
	for _, w := range writes {
		id, ok, err := writeID(w)
		if err != nil {
			return 0, err
		}
		if ok {
			next = max(next, id+1)
		}
	}
	return next, nil
}

func (s *Storage) checkGlobalIDs(ctx context.Context, q querier, writes []durable.StorageWrite) error {
	claimed := map[int64]string{}
	for _, w := range writes {
		id, ok, _ := writeID(w)
		if !ok {
			continue
		}
		table := w.WriteType()
		if table == "document.create" || table == "document.copy" {
			table = "document"
		}
		var existing string
		err := q.QueryRowContext(ctx, `SELECT record_type FROM `+s.schema.Tables.RecordIDs+` WHERE id = ?`, id).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		earlier := claimed[id]
		if table == "conversation" || table == "entry" || table == "document" {
			if existing != "" {
				return fmt.Errorf("ID %d already belongs to %s", id, existing)
			}
			if earlier != "" {
				return fmt.Errorf("ID %d is written more than once", id)
			}
		} else {
			if existing != "" && existing != table {
				return fmt.Errorf("ID %d already belongs to %s", id, existing)
			}
			if earlier != "" && earlier != table {
				return fmt.Errorf("ID %d is written as two record types", id)
			}
		}
		claimed[id] = table
	}
	return nil
}

type documentAction struct {
	id      durable.DocumentID
	create  *durable.DocumentCreate
	copy    *durable.DocumentCopySource
	content *durable.DocumentContent
	retire  bool
}

// prepareDocumentActions groups a batch's document writes per document, in
// the order each document first appears.
func prepareDocumentActions(writes []durable.StorageWrite) ([]*documentAction, error) {
	var actions []*documentAction
	byID := map[durable.DocumentID]*documentAction{}
	get := func(id durable.DocumentID) *documentAction {
		a := byID[id]
		if a == nil {
			a = &documentAction{id: id}
			byID[id] = a
			actions = append(actions, a)
		}
		return a
	}
	for _, w := range writes {
		switch w := w.(type) {
		case durable.DocumentCreateWrite:
			a := get(w.Record.ID)
			if a.create != nil || a.content != nil || a.copy != nil {
				return nil, fmt.Errorf("Document %d has more than one content command", w.Record.ID)
			}
			record, content := w.Record, w.Content
			a.create, a.content = &record, &content
		case durable.DocumentCopyWrite:
			a := get(w.Record.ID)
			if a.create != nil || a.content != nil || a.copy != nil {
				return nil, fmt.Errorf("Document %d has more than one content command", w.Record.ID)
			}
			record, source := w.Record, w.Source
			a.create, a.copy = &record, &source
		case durable.DocumentChangeWrite:
			a := get(w.ID)
			if a.content != nil || a.copy != nil {
				return nil, fmt.Errorf("Document %d has more than one content command", w.ID)
			}
			content := w.Content
			a.content = &content
		case durable.DocumentRetireWrite:
			a := get(w.ID)
			if a.retire {
				return nil, fmt.Errorf("Document %d is retired more than once", w.ID)
			}
			a.retire = true
		}
	}
	return actions, nil
}

func (s *Storage) documentRecord(ctx context.Context, q querier, id durable.DocumentID) (*durable.DocumentRecord, error) {
	return record[durable.DocumentRecord](ctx, q, `SELECT record FROM `+s.schema.Tables.Documents+` WHERE id = ?`, int64(id))
}

func (s *Storage) checkDocumentActions(ctx context.Context, q querier, actions []*documentAction) error {
	inBatch := map[durable.DocumentID]bool{}
	for _, a := range actions {
		inBatch[a.id] = true
	}
	live := map[string]int{}
	var addresses []string
	for _, a := range actions {
		if a.copy != nil && inBatch[a.copy.ID] {
			return fmt.Errorf("Document copy %d source is changed in the copy batch", a.id)
		}
		existing, err := s.documentRecord(ctx, q, a.id)
		if err != nil {
			return err
		}
		if a.create == nil && existing == nil {
			return fmt.Errorf("Unknown document: %d", a.id)
		}
		if a.create != nil && existing != nil {
			return fmt.Errorf("Document %d already exists", a.id)
		}
		if existing != nil && existing.RetiredAt != nil {
			return fmt.Errorf("Document %d is retired", a.id)
		}
		if a.content != nil && a.content.Kind == durable.ContentDelta {
			var version int
			err := q.QueryRowContext(ctx, `SELECT version FROM `+s.schema.Tables.DocumentRevisions+` WHERE document_id = ? ORDER BY seq DESC LIMIT 1`, int64(a.id)).Scan(&version)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("Document %d delta has no base", a.id)
			}
			if err != nil {
				return err
			}
			if version != a.content.Version {
				return fmt.Errorf("Document %d version transition requires a base", a.id)
			}
		}
		var parts addressParts
		if a.create != nil {
			parts = newAddressParts(a.create.Kind, a.create.Key, a.create.Scope)
		} else {
			parts = newAddressParts(existing.Kind, existing.Key, existing.Scope)
		}
		key := parts.key()
		n, seen := live[key]
		if !seen {
			current, err := s.currentDocumentID(ctx, q, parts)
			if err != nil {
				return err
			}
			if current {
				n = 1
			}
			addresses = append(addresses, key)
		}
		if a.retire && existing != nil {
			n--
		}
		if a.create != nil && !a.retire {
			n++
		}
		live[key] = n
	}
	for _, key := range addresses {
		if live[key] > 1 {
			return errors.New("Document address already has a current incarnation")
		}
	}
	return nil
}

func (s *Storage) currentDocumentID(ctx context.Context, q querier, p addressParts) (bool, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM `+s.schema.Tables.Documents+`
		WHERE kind = ? AND scope_kind = ? AND owner_id = ? AND family = ? AND key_value = ? AND retired_at IS NULL
		LIMIT 1`, p.kind, p.scopeKind, p.ownerID, p.family, p.keyValue).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Storage) claimID(ctx context.Context, q querier, id int64, table string) error {
	_, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO `+s.schema.Tables.RecordIDs+` (id, record_type) VALUES (?, ?)`, id, table)
	return err
}

func nullable[T ~int64](p *T) any {
	if p == nil {
		return nil
	}
	return int64(*p)
}

func (s *Storage) applyTableWrite(ctx context.Context, q querier, w durable.StorageWrite, seq durable.Seq) error {
	t := s.schema.Tables
	switch w := w.(type) {
	case durable.ConversationWrite:
		v := w.Value
		text, err := encodeJSON(v)
		if err != nil {
			return err
		}
		if err := s.claimID(ctx, q, int64(v.ID), "conversation"); err != nil {
			return err
		}
		var ownerConversation, ownerTask any
		if v.Owner != nil {
			ownerConversation, ownerTask = int64(v.Owner.ConversationID), int64(v.Owner.TaskID)
		}
		_, err = q.ExecContext(ctx, `INSERT INTO `+t.Conversations+` (id, owner_conversation_id, owner_task_id, record) VALUES (?, ?, ?, ?)`,
			int64(v.ID), ownerConversation, ownerTask, text)
		return err
	case durable.EntryWrite:
		v := w.Value
		if err := s.claimID(ctx, q, int64(v.ID), "entry"); err != nil {
			return err
		}
		text, err := encodeJSON(v)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `INSERT INTO `+t.Entries+` (id, conversation_id, head, commit_seq, record) VALUES (?, ?, ?, ?, ?)`,
			int64(v.ID), int64(v.ConversationID), nullable(v.Head), int64(seq), text)
		return err
	case durable.TaskWrite:
		v := w.Value
		if err := s.claimID(ctx, q, int64(v.ID), "task"); err != nil {
			return err
		}
		text, err := encodeJSON(v)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `INSERT INTO `+t.Tasks+` (id, conversation_id, kind, status, abort_requested, background, record)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET conversation_id = excluded.conversation_id, kind = excluded.kind,
			status = excluded.status, abort_requested = excluded.abort_requested,
			background = excluded.background, record = excluded.record`,
			int64(v.ID), int64(v.ConversationID), indexedString(v.Kind), v.State.Status,
			boolInt(v.AbortRequested), boolInt(v.Background), text)
		return err
	case durable.SubmissionWrite:
		v := w.Value
		if err := s.claimID(ctx, q, int64(v.ID), "submission"); err != nil {
			return err
		}
		text, err := encodeJSON(v)
		if err != nil {
			return err
		}
		var requestID any
		if v.RequestID != "" {
			requestID = indexedString(v.RequestID)
		}
		_, err = q.ExecContext(ctx, `INSERT INTO `+t.Submissions+` (id, conversation_id, request_id, status, record) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET conversation_id = excluded.conversation_id,
			request_id = excluded.request_id, status = excluded.status, record = excluded.record`,
			int64(v.ID), int64(v.ConversationID), requestID, v.Status, text)
		return err
	}
	return nil
}

func sameKey(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (s *Storage) copiedContent(ctx context.Context, q querier, a *documentAction) (*durable.DocumentContent, error) {
	stored, err := s.materializeDocument(ctx, q, a.copy.ID, a.copy.At)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("Fork source document %d cannot be read", a.copy.ID)
	}
	r, c := stored.Record, a.create
	if r.Scope.Kind != durable.ScopeConversation || c.Scope.Kind != durable.ScopeConversation ||
		r.Kind != c.Kind || !sameKey(r.Key, c.Key) || r.History != c.History || r.Fork != c.Fork {
		return nil, fmt.Errorf("Fork source document %d does not match the copied record", a.copy.ID)
	}
	return &durable.DocumentContent{Kind: durable.ContentBase, Version: stored.Version, Value: stored.Value}, nil
}

func (s *Storage) applyDocumentActions(ctx context.Context, q querier, actions []*documentAction, seq durable.Seq) error {
	t := s.schema.Tables
	for _, a := range actions {
		id := int64(a.id)
		content := a.content
		if a.copy != nil {
			copied, err := s.copiedContent(ctx, q, a)
			if err != nil {
				return fmt.Errorf("Document copy %d was rejected: %w", a.id, err)
			}
			content = copied
		}
		var r durable.DocumentRecord
		if a.create != nil {
			c := a.create
			r = durable.DocumentRecord{ID: c.ID, Kind: c.Kind, Key: c.Key, Scope: c.Scope, History: c.History, Fork: c.Fork, CreatedAt: seq}
			var retiredAt any
			if a.retire {
				retired := seq
				r.RetiredAt, retiredAt = &retired, int64(seq)
			}
			text, err := encodeJSON(r)
			if err != nil {
				return err
			}
			p := newAddressParts(c.Kind, c.Key, c.Scope)
			if err := s.claimID(ctx, q, id, "document"); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, `INSERT INTO `+t.Documents+`
				(id, kind, family, key_value, scope_kind, owner_id, created_at, retired_at, record)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				id, p.kind, p.family, p.keyValue, p.scopeKind, p.ownerID, int64(seq), retiredAt, text); err != nil {
				return err
			}
		} else {
			existing, err := s.documentRecord(ctx, q, a.id)
			if err != nil {
				return err
			}
			r = *existing
		}

		if content != nil {
			if content.Kind == durable.ContentBase && isCurrentOnly(r) {
				if _, err := q.ExecContext(ctx, `DELETE FROM `+t.DocumentRevisions+` WHERE document_id = ?`, id); err != nil {
					return err
				}
			}
			var encoded string
			var err error
			if content.Kind == durable.ContentBase {
				value := content.Value
				if value == nil {
					value = map[string]any{}
				}
				encoded, err = encodeJSON(value)
			} else {
				ops := content.Ops
				if ops == nil {
					ops = []delta.Op{}
				}
				encoded, err = encodeJSON(ops)
			}
			if err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, `INSERT INTO `+t.DocumentRevisions+` (document_id, seq, kind, version, content) VALUES (?, ?, ?, ?, ?)`,
				id, int64(seq), content.Kind, content.Version, encoded); err != nil {
				return err
			}
		}

		if a.retire {
			if a.create == nil {
				retired := seq
				r.RetiredAt = &retired
				text, err := encodeJSON(r)
				if err != nil {
					return err
				}
				if _, err := q.ExecContext(ctx, `UPDATE `+t.Documents+` SET retired_at = ?, record = ? WHERE id = ?`, int64(seq), text, id); err != nil {
					return err
				}
			}
			if isCurrentOnly(r) {
				if _, err := q.ExecContext(ctx, `DELETE FROM `+t.DocumentRevisions+` WHERE document_id = ?`, id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type revision struct {
	seq     int64
	kind    string
	version int
	content string
}

func (s *Storage) materializeDocument(ctx context.Context, q querier, id durable.DocumentID, at durable.DocumentPoint) (*durable.StoredDocument, error) {
	r, err := s.documentRecord(ctx, q, id)
	if err != nil || r == nil {
		return nil, err
	}
	if at != durable.Current && isCurrentOnly(*r) {
		return nil, durable.NewStorageRequestError("Document %d does not retain historical content", id)
	}
	if !isAliveAt(*r, at) {
		return nil, nil
	}
	upper := int64(maxSafeInteger)
	if at != durable.Current {
		upper = int64(at)
	}
	t := s.schema.Tables
	var base revision
	err = q.QueryRowContext(ctx, `SELECT seq, kind, version, content FROM `+t.DocumentRevisions+`
		WHERE document_id = ? AND kind = 'base' AND seq <= ? ORDER BY seq DESC LIMIT 1`, int64(id), upper).
		Scan(&base.seq, &base.kind, &base.version, &base.content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("Document %d is missing a required base", id)
	}
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(base.content), &value); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT seq, kind, version, content FROM `+t.DocumentRevisions+`
		WHERE document_id = ? AND seq > ? AND seq <= ? ORDER BY seq`, int64(id), base.seq, upper)
	if err != nil {
		return nil, err
	}
	var tail [][]delta.Op
	for rows.Next() {
		var rev revision
		if err := rows.Scan(&rev.seq, &rev.kind, &rev.version, &rev.content); err != nil {
			rows.Close()
			return nil, err
		}
		if rev.kind != durable.ContentDelta || rev.version != base.version {
			rows.Close()
			return nil, fmt.Errorf("Document %d crosses a stored version boundary without a base", id)
		}
		var raw []json.RawMessage
		if err := json.Unmarshal([]byte(rev.content), &raw); err != nil {
			rows.Close()
			return nil, err
		}
		ops := make([]delta.Op, len(raw))
		for i, op := range raw {
			if ops[i], err = delta.UnmarshalOp(op); err != nil {
				rows.Close()
				return nil, err
			}
		}
		tail = append(tail, ops)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	batches := func(yield func([]delta.Op) bool) {
		for _, ops := range tail {
			if !yield(ops) {
				return
			}
		}
	}
	value, err = delta.ApplyImmutableBatches(value, batches)
	if err != nil {
		return nil, err
	}
	return &durable.StoredDocument{Record: *r, Version: base.version, Value: value, DeltasSinceBase: len(tail)}, nil
}

func (s *Storage) readConversation(ctx context.Context, q querier, id durable.ConversationID) (*durable.ConversationRecord, error) {
	return record[durable.ConversationRecord](ctx, q, `SELECT record FROM `+s.schema.Tables.Conversations+` WHERE id = ?`, int64(id))
}

func (s *Storage) Conversation(ctx context.Context, id durable.ConversationID) (*durable.ConversationRecord, error) {
	if err := s.admit(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	return s.readConversation(ctx, s.db, id)
}

func (s *Storage) ScanConversations(ctx context.Context, query durable.ConversationQuery, limit int, cursor durable.Cursor) (durable.Page[durable.ConversationRecord], error) {
	var none durable.Page[durable.ConversationRecord]
	if err := s.admit(); err != nil {
		return none, err
	}
	defer s.life.RUnlock()
	start, err := durable.ResolveScanStart(query.Order, cursor, durable.Ascending)
	if err != nil {
		return none, err
	}
	clause, param, direction := scanSQL(start)
	clauses, params := []string{clause}, []any{param}
	if query.OwnerConversationID != nil {
		clauses, params = append(clauses, "owner_conversation_id = ?"), append(params, int64(*query.OwnerConversationID))
	}
	if query.OwnerTaskID != nil {
		clauses, params = append(clauses, "owner_task_id = ?"), append(params, int64(*query.OwnerTaskID))
	}
	values, err := records[durable.ConversationRecord](ctx, s.db, `SELECT record FROM `+s.schema.Tables.Conversations+
		` WHERE `+strings.Join(clauses, " AND ")+` ORDER BY id `+direction+` LIMIT ?`, append(params, limit+1)...)
	if err != nil {
		return none, err
	}
	return page(values, limit, start.Order, func(r durable.ConversationRecord) int64 { return int64(r.ID) }), nil
}

func (s *Storage) readEntry(ctx context.Context, id durable.EntryID) (*durable.StoredEntry, error) {
	var text string
	var commitSeq int64
	err := s.db.QueryRowContext(ctx, `SELECT record, commit_seq FROM `+s.schema.Tables.Entries+` WHERE id = ?`, int64(id)).Scan(&text, &commitSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e durable.EntryRecord
	if err := json.Unmarshal([]byte(text), &e); err != nil {
		return nil, err
	}
	return &durable.StoredEntry{Entry: e, CommitSeq: durable.Seq(commitSeq)}, nil
}

func (s *Storage) Entry(ctx context.Context, id durable.EntryID) (*durable.StoredEntry, error) {
	if err := s.admit(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	return s.readEntry(ctx, id)
}

func (s *Storage) knownConversation(ctx context.Context, id durable.ConversationID) (*durable.ConversationRecord, error) {
	c, err := s.readConversation(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, durable.NewStorageRequestError("Unknown conversation: %d", id)
	}
	return c, nil
}

// parentOf reads a conversation's parent, which storage keeps for every
// parent edge it stored.
func (s *Storage) parentOf(ctx context.Context, c *durable.ConversationRecord) (*durable.ConversationRecord, error) {
	parent, err := s.readConversation(ctx, s.db, c.Parent.ConversationID)
	if err == nil && parent == nil {
		err = fmt.Errorf("Conversation %d has a missing parent %d", c.ID, c.Parent.ConversationID)
	}
	return parent, err
}

func (s *Storage) VisibleEntry(ctx context.Context, conversationID durable.ConversationID, id durable.EntryID) (*durable.StoredEntry, error) {
	if err := s.admit(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	conversation, err := s.knownConversation(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	stored, err := s.readEntry(ctx, id)
	if err != nil || stored == nil {
		return nil, err
	}
	upper := int64(math.MaxInt64)
	for conversation.ID != stored.Entry.ConversationID {
		if conversation.Parent == nil {
			return nil, nil
		}
		upper = min(upper, int64(conversation.Parent.At))
		if conversation, err = s.parentOf(ctx, conversation); err != nil {
			return nil, err
		}
	}
	if int64(stored.Entry.ID) > upper {
		return nil, nil
	}
	return stored, nil
}

func (s *Storage) FindLatestHeadMarker(ctx context.Context, conversationID durable.ConversationID, atOrBefore *durable.EntryID) (*durable.EntryRecord, error) {
	if err := s.admit(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	conversation, err := s.knownConversation(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	var upper *int64
	if atOrBefore != nil {
		u := int64(*atOrBefore)
		upper = &u
	}
	entries := s.schema.Tables.Entries
	for {
		var e *durable.EntryRecord
		if upper == nil {
			e, err = record[durable.EntryRecord](ctx, s.db, `SELECT record FROM `+entries+` WHERE conversation_id = ? AND head IS NOT NULL ORDER BY id DESC LIMIT 1`, int64(conversation.ID))
		} else {
			e, err = record[durable.EntryRecord](ctx, s.db, `SELECT record FROM `+entries+` WHERE conversation_id = ? AND head IS NOT NULL AND id <= ? ORDER BY id DESC LIMIT 1`, int64(conversation.ID), *upper)
		}
		if err != nil || e != nil {
			return e, err
		}
		if conversation.Parent == nil {
			return nil, nil
		}
		at := int64(conversation.Parent.At)
		if upper == nil || at < *upper {
			upper = &at
		}
		if conversation, err = s.parentOf(ctx, conversation); err != nil {
			return nil, err
		}
	}
}

func (s *Storage) ScanEntries(ctx context.Context, query durable.EntryQuery, limit int, cursor durable.Cursor) (durable.Page[durable.EntryRecord], error) {
	var none durable.Page[durable.EntryRecord]
	if err := s.admit(); err != nil {
		return none, err
	}
	defer s.life.RUnlock()
	start, err := durable.ResolveScanStart(query.Order, cursor, durable.Descending)
	if err != nil {
		return none, err
	}
	if start.Order == durable.Ascending {
		return s.readEntriesAscending(ctx, query, limit, start)
	}
	conversation, err := s.knownConversation(ctx, query.ConversationID)
	if err != nil {
		return none, err
	}
	var upper *int64
	if query.MaxEntryID != nil {
		u := int64(*query.MaxEntryID)
		upper = &u
	}
	if start.HasAfter {
		u := int64(maxSafeInteger)
		if upper != nil {
			u = *upper
		}
		u = min(u, start.After-1)
		upper = &u
	}
	var values []durable.EntryRecord
	for {
		clauses, params := []string{"conversation_id = ?"}, []any{int64(conversation.ID)}
		if query.MinEntryID != nil {
			clauses, params = append(clauses, "id >= ?"), append(params, int64(*query.MinEntryID))
		}
		if upper != nil {
			clauses, params = append(clauses, "id <= ?"), append(params, *upper)
		}
		rows, err := records[durable.EntryRecord](ctx, s.db, `SELECT record FROM `+s.schema.Tables.Entries+
			` WHERE `+strings.Join(clauses, " AND ")+` ORDER BY id DESC LIMIT ?`, append(params, limit+1-len(values))...)
		if err != nil {
			return none, err
		}
		values = append(values, rows...)
		if len(values) > limit || conversation.Parent == nil {
			break
		}
		at := int64(conversation.Parent.At)
		if upper == nil || at < *upper {
			upper = &at
		}
		if query.MinEntryID != nil && *upper < int64(*query.MinEntryID) {
			break
		}
		if conversation, err = s.parentOf(ctx, conversation); err != nil {
			return none, err
		}
	}
	return page(values, limit, durable.Descending, func(e durable.EntryRecord) int64 { return int64(e.ID) }), nil
}

// readEntriesAscending reads oldest first: the fork chain's segments from
// the root conversation forward, each up to its fork point.
func (s *Storage) readEntriesAscending(ctx context.Context, query durable.EntryQuery, limit int, start durable.ScanStart) (durable.Page[durable.EntryRecord], error) {
	var none durable.Page[durable.EntryRecord]
	type segment struct {
		conversationID durable.ConversationID
		upper          *int64
	}
	var segments []segment
	conversation, err := s.knownConversation(ctx, query.ConversationID)
	if err != nil {
		return none, err
	}
	var upper *int64
	if query.MaxEntryID != nil {
		u := int64(*query.MaxEntryID)
		upper = &u
	}
	for {
		segments = append(segments, segment{conversation.ID, upper})
		if conversation.Parent == nil {
			break
		}
		at := int64(conversation.Parent.At)
		if upper == nil || at < *upper {
			upper = &at
		}
		if query.MinEntryID != nil && *upper < int64(*query.MinEntryID) {
			break
		}
		if conversation, err = s.parentOf(ctx, conversation); err != nil {
			return none, err
		}
	}
	var lower *int64
	if query.MinEntryID != nil {
		l := int64(*query.MinEntryID)
		lower = &l
	}
	if start.HasAfter {
		l := start.After + 1
		if lower != nil {
			l = max(*lower, l)
		}
		lower = &l
	}
	var values []durable.EntryRecord
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		clauses, params := []string{"conversation_id = ?"}, []any{int64(seg.conversationID)}
		if lower != nil {
			clauses, params = append(clauses, "id >= ?"), append(params, *lower)
		}
		if seg.upper != nil {
			clauses, params = append(clauses, "id <= ?"), append(params, *seg.upper)
		}
		rows, err := records[durable.EntryRecord](ctx, s.db, `SELECT record FROM `+s.schema.Tables.Entries+
			` WHERE `+strings.Join(clauses, " AND ")+` ORDER BY id ASC LIMIT ?`, append(params, limit+1-len(values))...)
		if err != nil {
			return none, err
		}
		values = append(values, rows...)
		if len(values) > limit {
			break
		}
	}
	return page(values, limit, durable.Ascending, func(e durable.EntryRecord) int64 { return int64(e.ID) }), nil
}

func (s *Storage) Task(ctx context.Context, id durable.TaskID) (*durable.TaskRecord, error) {
	if err := s.admit(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	return record[durable.TaskRecord](ctx, s.db, `SELECT record FROM `+s.schema.Tables.Tasks+` WHERE id = ?`, int64(id))
}

func (s *Storage) ScanTasks(ctx context.Context, query durable.TaskQuery, limit int, cursor durable.Cursor) (durable.Page[durable.TaskRecord], error) {
	var none durable.Page[durable.TaskRecord]
	if err := s.admit(); err != nil {
		return none, err
	}
	defer s.life.RUnlock()
	start, err := durable.ResolveScanStart(query.Order, cursor, durable.Ascending)
	if err != nil {
		return none, err
	}
	clause, param, direction := scanSQL(start)
	clauses, params := []string{clause}, []any{param}
	if query.ConversationID != nil {
		clauses, params = append(clauses, "conversation_id = ?"), append(params, int64(*query.ConversationID))
	}
	if query.Kind != "" {
		clauses, params = append(clauses, "kind = ?"), append(params, indexedString(query.Kind))
	}
	if query.Status != "" {
		clauses, params = append(clauses, "status = ?"), append(params, query.Status)
	}
	if query.AbortRequested != nil {
		clauses, params = append(clauses, "abort_requested = ?"), append(params, boolInt(*query.AbortRequested))
	}
	if query.Background != nil {
		clauses, params = append(clauses, "background = ?"), append(params, boolInt(*query.Background))
	}
	values, err := records[durable.TaskRecord](ctx, s.db, `SELECT record FROM `+s.schema.Tables.Tasks+
		` WHERE `+strings.Join(clauses, " AND ")+` ORDER BY id `+direction+` LIMIT ?`, append(params, limit+1)...)
	if err != nil {
		return none, err
	}
	return page(values, limit, start.Order, func(r durable.TaskRecord) int64 { return int64(r.ID) }), nil
}

func (s *Storage) Submission(ctx context.Context, id durable.SubmissionID) (*durable.SubmissionRecord, error) {
	if err := s.admit(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	return record[durable.SubmissionRecord](ctx, s.db, `SELECT record FROM `+s.schema.Tables.Submissions+` WHERE id = ?`, int64(id))
}

func (s *Storage) ScanSubmissions(ctx context.Context, query durable.SubmissionQuery, limit int, cursor durable.Cursor) (durable.Page[durable.SubmissionRecord], error) {
	var none durable.Page[durable.SubmissionRecord]
	if err := s.admit(); err != nil {
		return none, err
	}
	defer s.life.RUnlock()
	start, err := durable.ResolveScanStart(query.Order, cursor, durable.Ascending)
	if err != nil {
		return none, err
	}
	clause, param, direction := scanSQL(start)
	clauses, params := []string{clause}, []any{param}
	if query.ConversationID != nil {
		clauses, params = append(clauses, "conversation_id = ?"), append(params, int64(*query.ConversationID))
	}
	if query.Status != "" {
		clauses, params = append(clauses, "status = ?"), append(params, query.Status)
	}
	values, err := records[durable.SubmissionRecord](ctx, s.db, `SELECT record FROM `+s.schema.Tables.Submissions+
		` WHERE `+strings.Join(clauses, " AND ")+` ORDER BY id `+direction+` LIMIT ?`, append(params, limit+1)...)
	if err != nil {
		return none, err
	}
	return page(values, limit, start.Order, func(r durable.SubmissionRecord) int64 { return int64(r.ID) }), nil
}

func (s *Storage) SubmissionByRequest(ctx context.Context, conversationID durable.ConversationID, requestID string) (*durable.SubmissionRecord, error) {
	if err := s.admit(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	return record[durable.SubmissionRecord](ctx, s.db, `SELECT record FROM `+s.schema.Tables.Submissions+` WHERE conversation_id = ? AND request_id = ?`,
		int64(conversationID), indexedString(requestID))
}

func (s *Storage) FindDocument(ctx context.Context, address durable.DocumentAddress, at durable.DocumentPoint) (*durable.DocumentRecord, error) {
	if err := s.admit(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	p := newAddressParts(address.Kind, address.Key, address.Scope)
	params := []any{p.kind, p.scopeKind, p.ownerID, p.family, p.keyValue}
	query := `SELECT record FROM ` + s.schema.Tables.Documents + `
		WHERE kind = ? AND scope_kind = ? AND owner_id = ? AND family = ? AND key_value = ?
		AND retired_at IS NULL ORDER BY created_at DESC LIMIT 1`
	if at != durable.Current {
		query = `SELECT record FROM ` + s.schema.Tables.Documents + `
		WHERE kind = ? AND scope_kind = ? AND owner_id = ? AND family = ? AND key_value = ?
		AND created_at <= ? AND (retired_at IS NULL OR retired_at > ?)
		ORDER BY created_at DESC LIMIT 1`
		params = append(params, int64(at), int64(at))
	}
	return record[durable.DocumentRecord](ctx, s.db, query, params...)
}

// Document reads the record and its revisions in one transaction, so they
// observe one committed state: a commit between them can replace the base.
func (s *Storage) Document(ctx context.Context, id durable.DocumentID, at durable.DocumentPoint) (*durable.StoredDocument, error) {
	if err := s.admit(); err != nil {
		return nil, err
	}
	defer s.life.RUnlock()
	var stored *durable.StoredDocument
	err := transact(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		stored, err = s.materializeDocument(ctx, tx, id, at)
		return err
	})
	return stored, err
}

func (s *Storage) ScanDocuments(ctx context.Context, query durable.DocumentQuery, limit int, cursor durable.Cursor) (durable.Page[durable.DocumentRecord], error) {
	var none durable.Page[durable.DocumentRecord]
	if err := s.admit(); err != nil {
		return none, err
	}
	defer s.life.RUnlock()
	after, ok, err := cursorID(cursor)
	if err != nil {
		return none, err
	}
	if !ok {
		after = -1
	}
	scopeKind, ownerID := scopeColumns(query.Scope)
	clauses, params := []string{"scope_kind = ?", "owner_id = ?", "id > ?"}, []any{scopeKind, ownerID, after}
	if query.Kind != "" {
		clauses, params = append(clauses, "kind = ?"), append(params, indexedString(query.Kind))
	}
	if query.At == durable.Current {
		clauses = append(clauses, "retired_at IS NULL")
	} else {
		clauses, params = append(clauses, "created_at <= ?", "(retired_at IS NULL OR retired_at > ?)"), append(params, int64(query.At), int64(query.At))
	}
	values, err := records[durable.DocumentRecord](ctx, s.db, `SELECT record FROM `+s.schema.Tables.Documents+
		` WHERE `+strings.Join(clauses, " AND ")+` ORDER BY id LIMIT ?`, append(params, limit+1)...)
	if err != nil {
		return none, err
	}
	return page(values, limit, durable.Ascending, func(r durable.DocumentRecord) int64 { return int64(r.ID) }), nil
}
