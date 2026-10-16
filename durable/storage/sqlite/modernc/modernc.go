// Package modernc opens durable's SQLite storage on a file through
// modernc.org/sqlite, a SQLite without cgo (pi
// packages/durable/src/storage/sqlite/node.ts).
//
// It is a module of its own, so the engine's root module does not depend on
// a SQLite driver.
package modernc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/sqlite"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const (
	defaultWALAutoCheckpointPages = 1000
	defaultBusyTimeout            = 5 * time.Second
)

// Options are the connection settings of a storage file.
type Options struct {
	// WALAutoCheckpointPages is SQLite's WAL auto-checkpoint threshold: 0
	// is the default of 1,000 pages, a negative value disables it.
	WALAutoCheckpointPages int
	// BusyTimeout is how long SQLite waits for a competing file lock: 0 is
	// the default of 5 seconds, a negative value does not wait.
	BusyTimeout time.Duration
	// Prefix is put before every table and index name (sqlite.Options).
	Prefix string
}

// OpenDatabase opens and configures a database file, or ":memory:", as pi's
// adapter does: WAL journal, NORMAL synchronous, a busy timeout, BEGIN
// IMMEDIATE transactions, and one connection, which serializes operations
// and transactions in call order.
func OpenDatabase(ctx context.Context, path string, options Options) (*sql.DB, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("SQLite database path %q holds a URI delimiter", path)
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	pages := options.WALAutoCheckpointPages
	switch {
	case pages == 0:
		pages = defaultWALAutoCheckpointPages
	case pages < 0:
		pages = 0
	}
	timeout := options.BusyTimeout
	switch {
	case timeout == 0:
		timeout = defaultBusyTimeout
	case timeout < 0:
		timeout = 0
	}
	dsn := fmt.Sprintf("%s?_txlock=immediate&_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=wal_autocheckpoint(%d)",
		path, timeout.Milliseconds(), pages)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if err := db.PingContext(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

// truncateWAL checkpoints the WAL into the database and truncates it, as
// pi's adapter does on close.
func truncateWAL(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// OpenStorage opens or creates file-backed durable storage (pi
// openNodeSqliteStorage). The prefix is checked before the file is created.
func OpenStorage(ctx context.Context, path string, options Options) (*sqlite.Storage, error) {
	if options.Prefix != "" {
		if err := sqlite.ValidatePrefix(options.Prefix); err != nil {
			return nil, err
		}
	}
	db, err := OpenDatabase(ctx, path, options)
	if err != nil {
		return nil, err
	}
	return sqlite.Open(ctx, db, sqlite.Options{Prefix: options.Prefix, BeforeClose: truncateWAL})
}
