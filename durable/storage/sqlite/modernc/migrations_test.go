package modernc_test

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/sqlite"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/sqlite/modernc"
)

// pi test/sqlite-migrations.test.ts.

func openDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db := must(modernc.OpenDatabase(ctx, path, modernc.Options{}))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestCreatesTheCurrentSchemaRepeatedly(t *testing.T) {
	db := openDatabase(t, tempPath(t))
	noErr(t, sqlite.ApplyMigrations(ctx, db, nil))
	noErr(t, sqlite.ApplyMigrations(ctx, db, nil))
	equal(t, scalar(t, db, "SELECT version FROM durable_schema WHERE singleton = 1"), int64(sqlite.CurrentSchemaVersion), "version")
	var nextID string
	var nextSeq int64
	noErr(t, db.QueryRow("SELECT next_id, next_seq FROM durable_metadata WHERE singleton = 1").Scan(&nextID, &nextSeq))
	equal(t, nextID, "2", "next ID")
	equal(t, nextSeq, int64(1), "next sequence")
}

func TestRejectsADatabaseNewerThanThePortableCore(t *testing.T) {
	path := tempPath(t)
	db := must(modernc.OpenDatabase(ctx, path, modernc.Options{}))
	noErr(t, sqlite.ApplyMigrations(ctx, db, nil))
	must(db.Exec("UPDATE durable_schema SET version = ? WHERE singleton = 1", sqlite.CurrentSchemaVersion+1))
	noErr(t, db.Close())
	_, err := modernc.OpenStorage(ctx, path, modernc.Options{})
	fails(t, err, "is newer than supported version")
}

func TestRollsBootstrapAndEveryPendingMigrationBackTogether(t *testing.T) {
	db := openDatabase(t, tempPath(t))
	failed := []sqlite.Migration{
		{Version: 1, Statements: []string{"CREATE TABLE migration_first (value TEXT) STRICT", "INSERT INTO migration_first (value) VALUES ('retained')"}},
		{Version: 2, Statements: []string{"CREATE TABLE migration_second (value TEXT) STRICT", "THIS IS NOT SQL"}},
	}
	if err := sqlite.ApplyMigrations(ctx, db, failed); err == nil {
		t.Fatal("a migration that is not SQL applied")
	}
	equal(t, scalar(t, db, "SELECT count(*) FROM sqlite_schema WHERE name IN ('durable_schema', 'migration_first', 'migration_second')"), int64(0), "created objects")

	noErr(t, sqlite.ApplyMigrations(ctx, db, []sqlite.Migration{failed[0], {Version: 2, Statements: []string{"CREATE TABLE migration_second (value TEXT) STRICT"}}}))
	equal(t, scalar(t, db, "SELECT version FROM durable_schema WHERE singleton = 1"), int64(2), "version")
	var value string
	noErr(t, db.QueryRow("SELECT value FROM migration_first").Scan(&value))
	equal(t, value, "retained", "retained value")
}

func TestRollsAFailedMigrationBackAndKeepsStoredDataForARetry(t *testing.T) {
	path := tempPath(t)
	s := must(modernc.OpenStorage(ctx, path, modernc.Options{}))
	commit(t, s,
		durable.ConversationWrite{Value: durable.ConversationRecord{ID: root}},
		durable.EntryWrite{Value: durable.EntryRecord{ID: 2, ConversationID: root, Kind: "retained", Data: json.RawMessage(`{"retained":true}`)}},
	)
	noErr(t, s.Close(ctx))

	db := openDatabase(t, path)
	next := sqlite.CurrentSchemaVersion + 1
	withProbe := func(statements ...string) []sqlite.Migration {
		return append(append([]sqlite.Migration(nil), sqlite.Migrations...), sqlite.Migration{Version: next, Statements: statements})
	}
	if err := sqlite.ApplyMigrations(ctx, db, withProbe("CREATE TABLE migration_probe (value TEXT) STRICT", "THIS IS NOT SQL")); err == nil {
		t.Fatal("a migration that is not SQL applied")
	}
	equal(t, scalar(t, db, "SELECT version FROM durable_schema WHERE singleton = 1"), int64(sqlite.CurrentSchemaVersion), "version after failure")
	equal(t, scalar(t, db, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'migration_probe'"), int64(0), "probe tables")

	noErr(t, sqlite.ApplyMigrations(ctx, db, withProbe("CREATE TABLE migration_probe (value TEXT) STRICT")))
	equal(t, scalar(t, db, "SELECT version FROM durable_schema WHERE singleton = 1"), int64(next), "version after retry")
	var record string
	var commitSeq int64
	noErr(t, db.QueryRow("SELECT record, commit_seq FROM entries WHERE id = 2").Scan(&record, &commitSeq))
	// pi's JSON.stringify of the same entry, byte for byte.
	equal(t, record, `{"id":2,"conversationId":1,"kind":"retained","data":{"retained":true}}`, "entry record")
	equal(t, commitSeq, int64(1), "commit sequence")
	var nextID string
	var nextSeq int64
	noErr(t, db.QueryRow("SELECT next_id, next_seq FROM durable_metadata WHERE singleton = 1").Scan(&nextID, &nextSeq))
	equal(t, nextID, "3", "next ID")
	equal(t, nextSeq, int64(2), "next sequence")
}
