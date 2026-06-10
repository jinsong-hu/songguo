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
	// Detect whether service_wires predates this run: if the table is being
	// created now on a database that already has services, those services were
	// configured before wire allowlists existed and must be backfilled or they
	// would deny all traffic.
	hadWires, err := s.tableExists("service_wires")
	if err != nil {
		return err
	}
	// service_credentials predates the one-key-per-service model; when present
	// the oldest key is folded into services.api_key and the table dropped.
	hadCredPool, err := s.tableExists("service_credentials")
	if err != nil {
		return err
	}

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

		// Vendor/service config lives in SQLite (managed from the dashboard),
		// Vendor/service config lives in SQLite (managed from the dashboard),
		// the source of truth for routing. A service
		// is one configured upstream: an adapter + base_url + a single API key +
		// the models it serves with their per-model prices.
		`CREATE TABLE IF NOT EXISTS services (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL UNIQUE,
			vendor      TEXT NOT NULL DEFAULT '',
			adapter     TEXT NOT NULL DEFAULT 'openai-compatible',
			base_url    TEXT NOT NULL,
			priority    INTEGER NOT NULL DEFAULT 0,
			weight      INTEGER NOT NULL DEFAULT 1,
			enabled     INTEGER NOT NULL DEFAULT 1,
			catalog_id  TEXT NOT NULL DEFAULT '',
			api_key     TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS service_models (
			service_id  TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			model       TEXT NOT NULL,
			input       REAL NOT NULL DEFAULT 0,
			output      REAL NOT NULL DEFAULT 0,
			unit        TEXT NOT NULL DEFAULT 'per_1m_tokens',
			PRIMARY KEY (service_id, model)
		)`,
		// Per-service wire allowlist: which wire-protocol entries (path pattern +
		// usage extractor, see internal/wire) the proxy may serve for a service.
		// Paths matching no enabled wire are denied unless allow_unmatched is set.
		`CREATE TABLE IF NOT EXISTS service_wires (
			service_id  TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			wire        TEXT NOT NULL,
			PRIMARY KEY (service_id, wire)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_service_models_service ON service_models(service_id)`,
		`CREATE INDEX IF NOT EXISTS idx_service_wires_service ON service_wires(service_id)`,

		// Gateway-wide settings as a singleton row, hot-applied via the config
		// manager when changed from the dashboard.
		`CREATE TABLE IF NOT EXISTS app_settings (
			id                INTEGER PRIMARY KEY CHECK (id = 1),
			capture           INTEGER NOT NULL DEFAULT 0,
			capture_max_bytes INTEGER NOT NULL DEFAULT 32768,
			capture_retain    INTEGER NOT NULL DEFAULT 10000
		)`,
		`INSERT OR IGNORE INTO app_settings (id, capture, capture_max_bytes, capture_retain) VALUES (1, 0, 32768, 10000)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}

	// Post-v1 columns live here rather than in the CREATE statements so the
	// same path serves fresh and pre-existing databases.
	adds := []struct{ table, col, decl string }{
		{"calls", "wire", "TEXT NOT NULL DEFAULT ''"},
		{"calls", "confidence", "TEXT NOT NULL DEFAULT ''"},
		{"services", "allow_unmatched", "INTEGER NOT NULL DEFAULT 0"},
		{"services", "quirks", "TEXT NOT NULL DEFAULT '{}'"},
		{"services", "api_key", "TEXT NOT NULL DEFAULT ''"},
		{"service_models", "cached_input", "REAL NOT NULL DEFAULT 0"},
	}
	for _, a := range adds {
		if err := s.addColumn(a.table, a.col, a.decl); err != nil {
			return err
		}
	}

	if hadCredPool {
		if err := s.foldCredentialPool(); err != nil {
			return err
		}
	}

	if !hadWires {
		if err := s.backfillWires(); err != nil {
			return err
		}
	}
	return nil
}

// foldCredentialPool migrates from the multi-key pool era: each service keeps
// its oldest key as services.api_key (any extra keys are dropped — one key per
// service by design), then the pool table is removed.
func (s *Store) foldCredentialPool() error {
	if _, err := s.db.Exec(`UPDATE services SET api_key = COALESCE(
			(SELECT sc.api_key FROM service_credentials sc
			 WHERE sc.service_id = services.id
			 ORDER BY sc.created_at, sc.id LIMIT 1), '')
		WHERE api_key = ''`); err != nil {
		return fmt.Errorf("store: fold credential pool: %w", err)
	}
	if _, err := s.db.Exec(`DROP TABLE service_credentials`); err != nil {
		return fmt.Errorf("store: drop service_credentials: %w", err)
	}
	return nil
}

// tableExists reports whether a table is present in the schema.
func (s *Store) tableExists(name string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		return false, fmt.Errorf("store: table exists %s: %w", name, err)
	}
	return n > 0, nil
}

// backfillWires grants pre-wire-era services the default allowlist for their
// adapter (names must stay in sync with internal/wire registrations). Runs
// only on the migration that introduces service_wires.
func (s *Store) backfillWires() error {
	defaults := map[string][]string{
		"anthropic-compatible": {"anthropic/messages", "anthropic/models"},
		"":                     {"openai/chat", "openai/completions", "openai/embeddings", "openai/models"},
	}
	rows, err := s.db.Query(`SELECT id, adapter FROM services`)
	if err != nil {
		return fmt.Errorf("store: backfill wires: %w", err)
	}
	defer rows.Close()
	type svc struct{ id, adapter string }
	var svcs []svc
	for rows.Next() {
		var v svc
		if err := rows.Scan(&v.id, &v.adapter); err != nil {
			return fmt.Errorf("store: backfill wires: %w", err)
		}
		svcs = append(svcs, v)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: backfill wires: %w", err)
	}
	for _, v := range svcs {
		wires, ok := defaults[v.adapter]
		if !ok {
			wires = defaults[""]
		}
		for _, w := range wires {
			if _, err := s.db.Exec(`INSERT OR IGNORE INTO service_wires (service_id, wire) VALUES (?, ?)`, v.id, w); err != nil {
				return fmt.Errorf("store: backfill wires: %w", err)
			}
		}
	}
	return nil
}

// addColumn adds a column to a table if it is not already present, making
// schema evolution idempotent without a version table.
func (s *Store) addColumn(table, col, decl string) error {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("store: table_info %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("store: table_info %s: %w", table, err)
		}
		if name == col {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: table_info %s: %w", table, err)
	}
	if _, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, decl)); err != nil {
		return fmt.Errorf("store: add column %s.%s: %w", table, col, err)
	}
	return nil
}
