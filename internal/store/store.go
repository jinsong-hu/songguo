// Package store persists tokens, budgets, and usage in SQLite.
//
// It uses the pure-Go (cgo-free) modernc.org/sqlite driver via database/sql so
// the gateway ships as a single static binary. A single *sql.DB is shared and
// is safe for concurrent use; WAL mode allows concurrent readers with one
// writer.
package store

import (
	"database/sql"
	"errors"
	"fmt"

	// Pure-Go SQLite driver, registered under the name "sqlite".
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a requested row does not exist (or is revoked
// where an active row was required).
var ErrNotFound = errors.New("store: not found")

// Store is a handle to the SQLite-backed calls and token tables.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path, applies the
// required pragmas, and runs idempotent migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}

	// WAL allows concurrent readers + one writer; the driver serializes
	// writes through the shared *sql.DB. busy_timeout avoids spurious
	// SQLITE_BUSY under contention.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: %s: %w", p, err)
		}
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

// migrate creates tables and indexes if they do not already exist. It is safe
// to call repeatedly.
func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tokens (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			key_hash   TEXT NOT NULL UNIQUE,
			key_prefix TEXT NOT NULL,
			budget     REAL,
			scope      TEXT NOT NULL DEFAULT '[]',
			rpm        INTEGER NOT NULL DEFAULT 0,
			capture    INTEGER,
			created_at INTEGER NOT NULL,
			revoked_at INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS calls (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			ts            INTEGER NOT NULL,
			token_id      TEXT NOT NULL DEFAULT '',
			model         TEXT NOT NULL DEFAULT '',
			modality      TEXT NOT NULL DEFAULT 'unknown',
			vendor        TEXT NOT NULL DEFAULT '',
			credential_id TEXT NOT NULL DEFAULT '',
			attempt       INTEGER NOT NULL DEFAULT 1,
			status        INTEGER NOT NULL DEFAULT 0,
			err           TEXT NOT NULL DEFAULT '',
			usage         TEXT NOT NULL DEFAULT '{}',
			cost          REAL NOT NULL DEFAULT 0,
			latency_ms    INTEGER NOT NULL DEFAULT 0,
			stream        INTEGER NOT NULL DEFAULT 0,
			tags          TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE TABLE IF NOT EXISTS payloads (
			call_id          INTEGER PRIMARY KEY REFERENCES calls(id) ON DELETE CASCADE,
			req_headers      TEXT NOT NULL DEFAULT '{}',
			req_body         BLOB,
			req_content_type TEXT NOT NULL DEFAULT '',
			req_truncated    INTEGER NOT NULL DEFAULT 0,
			resp_headers     TEXT NOT NULL DEFAULT '{}',
			resp_body        BLOB,
			resp_content_type TEXT NOT NULL DEFAULT '',
			resp_truncated   INTEGER NOT NULL DEFAULT 0,
			created_at       INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_ts ON calls(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_token_id ON calls(token_id)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_model ON calls(model)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_vendor ON calls(vendor)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_status ON calls(status)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	return nil
}
