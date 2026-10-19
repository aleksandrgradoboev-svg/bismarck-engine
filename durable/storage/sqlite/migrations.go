package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Migration is one schema version and the statements that reach it.
type Migration struct {
	Version    int
	Statements []string
}

// NewMigrations builds the immutable, ordered schema history for a schema
// (pi createSqliteMigrations). The statements are pi's, so a database either
// side created reads the same.
func NewMigrations(schema Schema) []Migration {
	t, i := schema.Tables, schema.Indexes
	// next_id is TEXT because node:sqlite rejects INTEGER results outside
	// JavaScript's safe integer range.
	initial := []string{
		`CREATE TABLE ` + t.DurableMetadata + ` (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			next_id TEXT NOT NULL,
			next_seq INTEGER NOT NULL
		) STRICT`,
		`INSERT INTO ` + t.DurableMetadata + ` (singleton, next_id, next_seq) VALUES (1, '2', 1)`,
		`CREATE TABLE ` + t.RecordIDs + ` (
			id INTEGER PRIMARY KEY,
			record_type TEXT NOT NULL CHECK (record_type IN ('conversation', 'entry', 'task', 'submission', 'document'))
		) STRICT`,
		`CREATE TABLE ` + t.Conversations + ` (
			id INTEGER PRIMARY KEY,
			owner_conversation_id INTEGER,
			owner_task_id INTEGER,
			record TEXT NOT NULL CHECK (json_valid(record))
		) STRICT`,
		`CREATE INDEX ` + i.ConversationsByOwnerConversation + ` ON ` + t.Conversations + ` (owner_conversation_id, id)`,
		`CREATE INDEX ` + i.ConversationsByOwnerTask + ` ON ` + t.Conversations + ` (owner_task_id, id)`,
		`CREATE TABLE ` + t.Entries + ` (
			id INTEGER PRIMARY KEY,
			conversation_id INTEGER NOT NULL,
			head INTEGER,
			commit_seq INTEGER NOT NULL,
			record TEXT NOT NULL CHECK (json_valid(record))
		) STRICT`,
		`CREATE INDEX ` + i.EntriesByConversation + ` ON ` + t.Entries + ` (conversation_id, id DESC)`,
		`CREATE INDEX ` + i.EntryHeadsByConversation + ` ON ` + t.Entries + ` (conversation_id, id DESC) WHERE head IS NOT NULL`,
		`CREATE TABLE ` + t.Tasks + ` (
			id INTEGER PRIMARY KEY,
			conversation_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'waiting', 'completing', 'terminal')),
			abort_requested INTEGER NOT NULL CHECK (abort_requested IN (0, 1)),
			background INTEGER NOT NULL CHECK (background IN (0, 1)),
			record TEXT NOT NULL CHECK (json_valid(record))
		) STRICT`,
		`CREATE INDEX ` + i.TasksByStatus + ` ON ` + t.Tasks + ` (status, id)`,
		`CREATE INDEX ` + i.TasksByConversation + ` ON ` + t.Tasks + ` (conversation_id, id)`,
		`CREATE INDEX ` + i.TasksByKind + ` ON ` + t.Tasks + ` (kind, id)`,
		`CREATE INDEX ` + i.TasksByAbortRequested + ` ON ` + t.Tasks + ` (abort_requested, id)`,
		`CREATE INDEX ` + i.TasksByBackground + ` ON ` + t.Tasks + ` (background, id)`,
		`CREATE TABLE ` + t.Submissions + ` (
			id INTEGER PRIMARY KEY,
			conversation_id INTEGER NOT NULL,
			request_id TEXT,
			status TEXT NOT NULL CHECK (status IN ('queued', 'placed', 'done', 'unanswered')),
			record TEXT NOT NULL CHECK (json_valid(record))
		) STRICT`,
		`CREATE INDEX ` + i.SubmissionsByRequest + ` ON ` + t.Submissions + ` (conversation_id, request_id)`,
		`CREATE INDEX ` + i.SubmissionsByConversation + ` ON ` + t.Submissions + ` (conversation_id, id)`,
		`CREATE INDEX ` + i.SubmissionsByStatus + ` ON ` + t.Submissions + ` (status, id)`,
		`CREATE TABLE ` + t.Documents + ` (
			id INTEGER PRIMARY KEY,
			kind TEXT NOT NULL,
			family INTEGER NOT NULL CHECK (family IN (0, 1)),
			key_value TEXT NOT NULL,
			scope_kind TEXT NOT NULL CHECK (scope_kind IN ('session', 'conversation', 'task')),
			owner_id INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			retired_at INTEGER,
			record TEXT NOT NULL CHECK (json_valid(record))
		) STRICT`,
		`CREATE INDEX ` + i.DocumentsByAddress + `
			ON ` + t.Documents + ` (kind, scope_kind, owner_id, family, key_value, created_at DESC, retired_at)`,
		`CREATE INDEX ` + i.DocumentsByScope + ` ON ` + t.Documents + ` (scope_kind, owner_id, id)`,
		`CREATE INDEX ` + i.DocumentsByScopeKind + ` ON ` + t.Documents + ` (scope_kind, owner_id, kind, id)`,
		`CREATE TABLE ` + t.DocumentRevisions + ` (
			document_id INTEGER NOT NULL,
			seq INTEGER NOT NULL,
			kind TEXT NOT NULL CHECK (kind IN ('base', 'delta')),
			version INTEGER NOT NULL,
			content TEXT NOT NULL CHECK (json_valid(content)),
			PRIMARY KEY (document_id, seq)
		) STRICT`,
		`CREATE INDEX ` + i.DocumentRevisionsByKind + ` ON ` + t.DocumentRevisions + ` (document_id, kind, seq DESC)`,
	}
	// Version 2 adds the sleeping task status. SQLite cannot change a CHECK
	// constraint in place, so the table is rebuilt.
	rebuiltTasks := t.Tasks + "_v2"
	sleepingStatus := []string{
		`CREATE TABLE ` + rebuiltTasks + ` (
			id INTEGER PRIMARY KEY,
			conversation_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'waiting', 'sleeping', 'completing', 'terminal')),
			abort_requested INTEGER NOT NULL CHECK (abort_requested IN (0, 1)),
			background INTEGER NOT NULL CHECK (background IN (0, 1)),
			record TEXT NOT NULL CHECK (json_valid(record))
		) STRICT`,
		`INSERT INTO ` + rebuiltTasks + ` (id, conversation_id, kind, status, abort_requested, background, record)
			SELECT id, conversation_id, kind, status, abort_requested, background, record FROM ` + t.Tasks,
		`DROP TABLE ` + t.Tasks,
		`ALTER TABLE ` + rebuiltTasks + ` RENAME TO ` + t.Tasks,
		`CREATE INDEX ` + i.TasksByStatus + ` ON ` + t.Tasks + ` (status, id)`,
		`CREATE INDEX ` + i.TasksByConversation + ` ON ` + t.Tasks + ` (conversation_id, id)`,
		`CREATE INDEX ` + i.TasksByKind + ` ON ` + t.Tasks + ` (kind, id)`,
		`CREATE INDEX ` + i.TasksByAbortRequested + ` ON ` + t.Tasks + ` (abort_requested, id)`,
		`CREATE INDEX ` + i.TasksByBackground + ` ON ` + t.Tasks + ` (background, id)`,
	}
	return []Migration{{Version: 1, Statements: initial}, {Version: 2, Statements: sleepingStatus}}
}

// Migrations is the schema history of DefaultSchema.
var Migrations = NewMigrations(DefaultSchema)

// CurrentSchemaVersion is the newest schema version this storage supports.
var CurrentSchemaVersion = Migrations[len(Migrations)-1].Version

// ApplyMigrations applies every pending migration of DefaultSchema
// atomically; nil migrations are Migrations.
func ApplyMigrations(ctx context.Context, db *sql.DB, migrations []Migration) error {
	return ApplyMigrationsForSchema(ctx, db, DefaultSchema, migrations)
}

// ApplyMigrationsForSchema applies migrations rendered for a schema in one
// transaction; nil migrations are NewMigrations(schema).
func ApplyMigrationsForSchema(ctx context.Context, db *sql.DB, schema Schema, migrations []Migration) error {
	if migrations == nil {
		migrations = NewMigrations(schema)
	}
	for index, m := range migrations {
		if m.Version != index+1 {
			return errors.New("Durable SQLite migrations must have contiguous versions starting at 1")
		}
	}
	durableSchema := schema.Tables.DurableSchema
	return transact(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+durableSchema+` (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			version INTEGER NOT NULL CHECK (version >= 0)
		) STRICT`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO `+durableSchema+` (singleton, version) VALUES (1, 0)`); err != nil {
			return err
		}
		var version int
		err := tx.QueryRowContext(ctx, `SELECT version FROM `+durableSchema+` WHERE singleton = 1`).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("Durable SQLite schema metadata is missing")
		}
		if err != nil {
			return err
		}
		current := 0
		if len(migrations) > 0 {
			current = migrations[len(migrations)-1].Version
		}
		if version > current {
			return fmt.Errorf("Durable SQLite schema version %d is newer than supported version %d", version, current)
		}
		for _, m := range migrations {
			if m.Version <= version {
				continue
			}
			for _, statement := range m.Statements {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE `+durableSchema+` SET version = ? WHERE singleton = 1`, m.Version); err != nil {
				return err
			}
		}
		return nil
	})
}

// transact runs fn in a transaction, committing when it succeeds and rolling
// back when it fails. A failed rollback is joined to fn's error, so the
// error is never mistaken for a guaranteed rollback.
func transact(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("SQLite transaction failed and rollback failed: %w", rollbackErr))
		}
		return err
	}
	return tx.Commit()
}
