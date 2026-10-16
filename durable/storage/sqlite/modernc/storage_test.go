package modernc_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/chord/delta"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/sqlite/modernc"
)

// pi test/sqlite-storage.test.ts, the cases after the conformance suite.

const root = durable.RootConversationID

type obj = map[string]any

// must returns v, panicking on err: a test failure with a stack, usable around
// a call with two results.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func equal(t *testing.T, got, want any, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got  %#v\n want %#v", what, got, want)
	}
}

func fails(t *testing.T, err error, includes string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), includes) {
		t.Fatalf("error %v, want one containing %q", err, includes)
	}
}

func commit(t *testing.T, s durable.Storage, writes ...durable.StorageWrite) durable.Seq {
	t.Helper()
	return must(s.Commit(ctx, writes))
}

func createRoot(t *testing.T, s durable.Storage) durable.Seq {
	t.Helper()
	return commit(t, s, durable.ConversationWrite{Value: durable.ConversationRecord{ID: root}})
}

func mint[I ~int64](s durable.Storage) I { return I(must(s.MintID(ctx))) }

func entry(id durable.EntryID, data any) durable.EntryRecord {
	e := durable.EntryRecord{ID: id, ConversationID: root, Kind: "message"}
	if data != nil {
		e.Data = must(json.Marshal(data))
	}
	return e
}

func pendingTask(id durable.TaskID) durable.TaskRecord {
	return durable.TaskRecord{
		ID: id, ConversationID: root, Kind: "test.task", Version: 1, Input: json.RawMessage("null"),
		State: durable.TaskState{Status: durable.TaskPending, Checkpoint: json.RawMessage(`{"phase":"ready"}`)},
	}
}

func base(value obj) durable.DocumentContent {
	return durable.DocumentContent{Kind: durable.ContentBase, Version: 1, Value: value}
}

// ops decodes pi's wire form of a delta batch.
func ops(t *testing.T, wire string) durable.DocumentContent {
	t.Helper()
	var raw []json.RawMessage
	noErr(t, json.Unmarshal([]byte(wire), &raw))
	out := make([]delta.Op, len(raw))
	for i, r := range raw {
		out[i] = must(delta.UnmarshalOp(r))
	}
	return durable.DocumentContent{Kind: durable.ContentDelta, Version: 1, Ops: out}
}

// rawDB opens the file through a connection of its own, as pi's tests open
// DatabaseSync beside the storage.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db := must(sql.Open("sqlite", path))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func scalar(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var v int64
	noErr(t, db.QueryRow(query, args...).Scan(&v))
	return v
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	return must(os.Stat(path)).Size()
}

func TestPersistsRecordsSequencesAndIDsAcrossReopen(t *testing.T) {
	path := tempPath(t)
	s := must(modernc.OpenStorage(ctx, path, modernc.Options{}))
	equal(t, createRoot(t, s), durable.Seq(1), "root sequence")
	entryID := mint[durable.EntryID](s)
	equal(t, commit(t, s, durable.EntryWrite{Value: entry(entryID, nil)}), durable.Seq(2), "entry sequence")
	noErr(t, s.Close(ctx))

	reopened := openStorage(t, path, modernc.Options{})
	equal(t, must(reopened.Entry(ctx, entryID)), &durable.StoredEntry{Entry: entry(entryID, nil), CommitSeq: 2}, "entry")
	equal(t, mint[durable.EntryID](reopened), entryID+1, "minted ID")
	equal(t, commit(t, reopened, durable.TaskWrite{Value: pendingTask(mint[durable.TaskID](reopened))}), durable.Seq(3), "task sequence")
}

func TestNamespacesTablesAndIndexesWithAPrefixAcrossReopen(t *testing.T) {
	path := tempPath(t)
	host := must(sql.Open("sqlite", path))
	must(host.Exec("CREATE TABLE tasks (text TEXT)"))
	noErr(t, host.Close())
	s := must(modernc.OpenStorage(ctx, path, modernc.Options{Prefix: "pi_"}))
	equal(t, createRoot(t, s), durable.Seq(1), "root sequence")
	noErr(t, s.Close(ctx))

	reopened := openStorage(t, path, modernc.Options{Prefix: "pi_"})
	equal(t, must(reopened.Conversation(ctx, root)), &durable.ConversationRecord{ID: root}, "root")
	rows := must(rawDB(t, path).Query("SELECT name FROM sqlite_master WHERE type IN ('table', 'index') AND name NOT LIKE 'sqlite_%'"))
	defer rows.Close()
	var names, unprefixed []string
	for rows.Next() {
		var name string
		noErr(t, rows.Scan(&name))
		names = append(names, name)
		if !strings.HasPrefix(name, "pi_") {
			unprefixed = append(unprefixed, name)
		}
	}
	equal(t, unprefixed, []string{"tasks"}, "unprefixed names")
	if !slices.Contains(names, "pi_tasks") {
		t.Fatalf("names %v lack pi_tasks", names)
	}
}

func TestRejectsAnInvalidPrefixBeforeCreatingTheDatabase(t *testing.T) {
	nested := filepath.Join(t.TempDir(), "nested")
	_, err := modernc.OpenStorage(ctx, filepath.Join(nested, "storage.sqlite"), modernc.Options{Prefix: "bad-prefix"})
	fails(t, err, "Invalid SQLite schema prefix")
	if _, err := os.Stat(nested); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the database directory exists: %v", err)
	}
}

func TestRejectsPersistedMetadataCorruptionOnReopen(t *testing.T) {
	path := tempPath(t)
	noErr(t, must(modernc.OpenStorage(ctx, path, modernc.Options{})).Close(ctx))
	db := must(sql.Open("sqlite", path))
	must(db.Exec("DELETE FROM durable_metadata"))
	noErr(t, db.Close())
	_, err := modernc.OpenStorage(ctx, path, modernc.Options{})
	fails(t, err, "Durable SQLite metadata is missing")
}

func TestRejectsADocumentWhoseRequiredBaseIsMissing(t *testing.T) {
	path := tempPath(t)
	s := openStorage(t, path, modernc.Options{})
	createRoot(t, s)
	id := mint[durable.DocumentID](s)
	commit(t, s, durable.DocumentCreateWrite{Record: durable.DocumentCreate{ID: id, Kind: "corrupt", Scope: durable.SessionScope()}, Content: base(obj{"retained": true})})
	must(rawDB(t, path).Exec("DELETE FROM document_revisions WHERE document_id = ?", int64(id)))
	_, err := s.Document(ctx, id, durable.Current)
	fails(t, err, fmt.Sprintf("Document %d is missing a required base", id))
}

func TestReplaysDetachedRootReplacementsAndRejectsCorruptOperations(t *testing.T) {
	path := tempPath(t)
	s := openStorage(t, path, modernc.Options{})
	createRoot(t, s)
	id := mint[durable.DocumentID](s)
	commit(t, s, durable.DocumentCreateWrite{Record: durable.DocumentCreate{ID: id, Kind: "replay", Scope: durable.SessionScope()}, Content: base(obj{"nested": obj{"value": 1.0}, "rows": []any{}})})
	commit(t, s, durable.DocumentChangeWrite{ID: id, Content: ops(t, `[["r",{"nested":{"value":2},"rows":[{"id":1}]}]]`)})
	commit(t, s, durable.DocumentChangeWrite{ID: id, Content: ops(t, `[["s",["nested","value"],3],["p",["rows"],1,0,[{"id":2}]],["m",["rows"],[1,0]]]`)})
	commit(t, s, durable.DocumentChangeWrite{ID: id, Content: ops(t, `[["s",["nested","value"],4]]`)})

	expected := obj{"nested": obj{"value": 4.0}, "rows": []any{obj{"id": 2.0}, obj{"id": 1.0}}}
	first := must(s.Document(ctx, id, durable.Current))
	equal(t, first.Value, expected, "value")
	first.Value["nested"].(obj)["value"] = 99.0
	first.Value["rows"].([]any)[0].(obj)["id"] = 99.0
	equal(t, must(s.Document(ctx, id, durable.Current)).Value, expected, "value after changing a read")

	must(rawDB(t, path).Exec(`UPDATE document_revisions SET content = ? WHERE document_id = ? AND seq =
		(SELECT max(seq) FROM document_revisions WHERE document_id = ?)`, `[["unknown"]]`, int64(id), int64(id)))
	_, err := s.Document(ctx, id, durable.Current)
	fails(t, err, "unknown op verb")
}

func TestRollsSQLRowsAndSequenceAllocationBackAsOneTransaction(t *testing.T) {
	path := tempPath(t)
	s := openStorage(t, path, modernc.Options{})
	createRoot(t, s)
	transientID := mint[durable.EntryID](s)
	transientTaskID := mint[durable.TaskID](s)
	bad := pendingTask(transientTaskID)
	// pi's case is a circular input; in Go an input that is not JSON fails
	// to encode, after the entry's row is written.
	bad.Input = json.RawMessage("{bad")
	_, err := s.Commit(ctx, []durable.StorageWrite{durable.EntryWrite{Value: entry(transientID, nil)}, durable.TaskWrite{Value: bad}})
	if err == nil {
		t.Fatal("an input that is not JSON was committed")
	}
	equal(t, must(s.Entry(ctx, transientID)), (*durable.StoredEntry)(nil), "entry")
	equal(t, must(s.Task(ctx, transientTaskID)), (*durable.TaskRecord)(nil), "task")
	committedID := mint[durable.EntryID](s)
	equal(t, commit(t, s, durable.EntryWrite{Value: entry(committedID, nil)}), durable.Seq(2), "sequence")

	db := rawDB(t, path)
	equal(t, scalar(t, db, "SELECT count(*) FROM entries"), int64(1), "entries")
	equal(t, scalar(t, db, "SELECT next_seq FROM durable_metadata WHERE singleton = 1"), int64(3), "next sequence")
}

func TestReconstructsRecentAndAncientRewindablePointsAfterReopen(t *testing.T) {
	path := tempPath(t)
	s := must(modernc.OpenStorage(ctx, path, modernc.Options{}))
	createRoot(t, s)
	id := mint[durable.DocumentID](s)
	record := durable.DocumentCreate{ID: id, Kind: "history", Scope: durable.ConversationScope(root), History: durable.HistoryRewindable, Fork: durable.ForkAsOf}
	createdAt := commit(t, s, durable.DocumentCreateWrite{Record: record, Content: base(obj{"count": 0.0})})
	ancientAt, recentAt := createdAt, createdAt
	for count := 1; count <= 40; count++ {
		content := ops(t, fmt.Sprintf(`[["s",["count"],%d]]`, count))
		if count == 20 {
			content = base(obj{"count": float64(count)})
		}
		recentAt = commit(t, s, durable.DocumentChangeWrite{ID: id, Content: content})
		if count == 5 {
			ancientAt = recentAt
		}
	}
	noErr(t, s.Close(ctx))
	reopened := openStorage(t, path, modernc.Options{})
	equal(t, must(reopened.Document(ctx, id, ancientAt)).Value, obj{"count": 5.0}, "ancient value")
	equal(t, must(reopened.Document(ctx, id, recentAt)).Value, obj{"count": 40.0}, "recent value")
}

func TestUsesIndexesForAddressesScopesHistoryAndRevisionTails(t *testing.T) {
	path := tempPath(t)
	noErr(t, must(modernc.OpenStorage(ctx, path, modernc.Options{})).Close(ctx))
	db := rawDB(t, path)
	plan := func(query string, args ...any) string {
		rows := must(db.Query("EXPLAIN QUERY PLAN "+query, args...))
		defer rows.Close()
		var details []string
		for rows.Next() {
			var id, parent, unused int64
			var detail string
			noErr(t, rows.Scan(&id, &parent, &unused, &detail))
			details = append(details, detail)
		}
		return strings.Join(details, "\n")
	}
	details := []string{
		plan(`SELECT record FROM documents
			WHERE kind = ? AND scope_kind = ? AND owner_id = ? AND family = ? AND key_value = ?
			AND retired_at IS NULL ORDER BY created_at DESC LIMIT 1`, "kind", "session", 0, 0, ""),
		plan(`SELECT record FROM documents
			WHERE kind = ? AND scope_kind = ? AND owner_id = ? AND family = ? AND key_value = ?
			AND created_at <= ? AND (retired_at IS NULL OR retired_at > ?)
			ORDER BY created_at DESC LIMIT 1`, "kind", "conversation", 1, 0, "", 10, 10),
		plan(`SELECT record FROM documents
			WHERE scope_kind = ? AND owner_id = ? AND kind = ? AND id > ? ORDER BY id LIMIT ?`, "task", 1, "kind", 0, 10),
		plan(`SELECT record FROM entries
			WHERE conversation_id = ? AND id <= ? ORDER BY id DESC LIMIT ?`, 1, 10, 10),
		plan(`SELECT record FROM entries
			WHERE conversation_id = ? AND head IS NOT NULL AND id <= ? ORDER BY id DESC LIMIT 1`, 1, 10),
		plan("SELECT record FROM tasks WHERE status = ? AND id > ? ORDER BY id LIMIT ?", "pending", 0, 10),
		plan(`SELECT seq, kind, version, content FROM document_revisions
			WHERE document_id = ? AND kind = 'base' AND seq <= ? ORDER BY seq DESC LIMIT 1`, 1, 10),
		plan(`SELECT seq, kind, version, content FROM document_revisions
			WHERE document_id = ? AND seq > ? AND seq <= ? ORDER BY seq`, 1, 5, 10),
	}
	for i, index := range []string{
		"documents_by_address", "documents_by_address", "documents_by_scope_kind", "entries_by_conversation",
		"entry_heads_by_conversation", "tasks_by_status", "document_revisions_by_kind", "sqlite_autoindex_document_revisions_1",
	} {
		if !strings.Contains(details[i], index) {
			t.Errorf("plan %d does not use %s:\n%s", i, index, details[i])
		}
	}
	for i, detail := range details[:3] {
		if strings.Contains(detail, "SCAN documents") {
			t.Errorf("plan %d scans documents:\n%s", i, detail)
		}
	}
	if strings.Contains(details[7], "SCAN document_revisions") {
		t.Errorf("the revision tail scans document_revisions:\n%s", details[7])
	}
}

func TestReclaimsCurrentOnlyRevisionsOnlyAfterABaseOrRetirement(t *testing.T) {
	path := tempPath(t)
	s := openStorage(t, path, modernc.Options{})
	createRoot(t, s)
	id := mint[durable.DocumentID](s)
	db := rawDB(t, path)
	revisions := func() int64 {
		return scalar(t, db, "SELECT count(*) FROM document_revisions WHERE document_id = ?", int64(id))
	}
	commit(t, s, durable.DocumentCreateWrite{Record: durable.DocumentCreate{ID: id, Kind: "latest", Scope: durable.SessionScope()}, Content: base(obj{"count": 0.0})})
	for count := 1; count <= 10; count++ {
		commit(t, s, durable.DocumentChangeWrite{ID: id, Content: ops(t, fmt.Sprintf(`[["s",["count"],%d]]`, count))})
	}
	equal(t, revisions(), int64(11), "revisions after deltas")
	var content string
	noErr(t, db.QueryRow("SELECT content FROM document_revisions WHERE document_id = ? AND kind = 'delta' ORDER BY seq DESC LIMIT 1", int64(id)).Scan(&content))
	equal(t, content, `[["s",["count"],10]]`, "last delta")
	commit(t, s, durable.DocumentChangeWrite{ID: id, Content: base(obj{"count": 11.0})})
	equal(t, revisions(), int64(1), "revisions after a base")
	commit(t, s, durable.DocumentChangeWrite{ID: id, Content: ops(t, `[["r",{"count":12}]]`)})
	equal(t, revisions(), int64(2), "revisions after a replacement delta")
	commit(t, s, durable.DocumentRetireWrite{ID: id})
	equal(t, revisions(), int64(0), "revisions after retirement")
}

func TestAutoCheckpointsWALFramesAndTruncatesTheWALOnClose(t *testing.T) {
	path := tempPath(t)
	s := must(modernc.OpenStorage(ctx, path, modernc.Options{WALAutoCheckpointPages: 1}))
	createRoot(t, s)
	for index := range 20 {
		commit(t, s, durable.EntryWrite{Value: entry(mint[durable.EntryID](s), obj{"text": strings.Repeat("x", 32*1024), "index": index})})
	}
	wal := path + "-wal"
	if size := fileSize(t, wal); size >= 512*1024 {
		t.Fatalf("WAL size %d, want under 512 KiB", size)
	}
	observer := rawDB(t, path)
	equal(t, scalar(t, observer, "SELECT count(*) FROM entries"), int64(20), "entries")
	noErr(t, s.Close(ctx))
	equal(t, fileSize(t, wal), int64(0), "WAL size after close")
}

func TestReusesPagesReleasedByCurrentOnlyCheckpoints(t *testing.T) {
	path := tempPath(t)
	s := openStorage(t, path, modernc.Options{})
	createRoot(t, s)
	id := mint[durable.DocumentID](s)
	large := strings.Repeat("x", 512*1024)
	commit(t, s, durable.DocumentCreateWrite{Record: durable.DocumentCreate{ID: id, Kind: "reuse", Scope: durable.SessionScope()}, Content: base(obj{"text": large})})
	commit(t, s, durable.DocumentChangeWrite{ID: id, Content: base(obj{"text": "small"})})
	pages := func() (int64, int64) {
		db := must(sql.Open("sqlite", path))
		defer db.Close()
		return scalar(t, db, "SELECT page_count FROM pragma_page_count()"), scalar(t, db, "SELECT freelist_count FROM pragma_freelist_count()")
	}
	pagesAfterDelete, freeAfterDelete := pages()
	if freeAfterDelete == 0 {
		t.Fatal("the replaced base released no pages")
	}
	commit(t, s, durable.DocumentChangeWrite{ID: id, Content: base(obj{"text": large})})
	pagesAfterReuse, freeAfterReuse := pages()
	if pagesAfterReuse > pagesAfterDelete+2 {
		t.Fatalf("page count %d after reuse, want at most %d", pagesAfterReuse, pagesAfterDelete+2)
	}
	if freeAfterReuse >= freeAfterDelete {
		t.Fatalf("free pages %d after reuse, want under %d", freeAfterReuse, freeAfterDelete)
	}
}

func TestKeepsRepresentativeRowAndDocumentStorageBounded(t *testing.T) {
	path := tempPath(t)
	s := must(modernc.OpenStorage(ctx, path, modernc.Options{}))
	createRoot(t, s)
	for index := range 100 {
		commit(t, s, durable.EntryWrite{Value: entry(mint[durable.EntryID](s), obj{"index": index, "text": strings.Repeat("x", 1024)})})
	}
	id := mint[durable.DocumentID](s)
	commit(t, s, durable.DocumentCreateWrite{
		Record:  durable.DocumentCreate{ID: id, Kind: "size.history", Scope: durable.ConversationScope(root), History: durable.HistoryRewindable, Fork: durable.ForkAsOf},
		Content: base(obj{"count": 0.0}),
	})
	for count := 1; count <= 100; count++ {
		commit(t, s, durable.DocumentChangeWrite{ID: id, Content: ops(t, fmt.Sprintf(`[["s",["count"],%d]]`, count))})
	}
	noErr(t, s.Close(ctx))
	if size := fileSize(t, path); size >= 1024*1024 {
		t.Fatalf("database size %d, want under 1 MiB", size)
	}
}
