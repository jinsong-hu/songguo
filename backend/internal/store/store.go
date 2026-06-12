// Package store persists users, budgets, and usage in SQLite.
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

// Store is a handle to the SQLite-backed calls and user tables.
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
	// Detect pre-rename tables before creating the new ones.
	hasOldWires, _ := s.tableExists("service_wires")
	hadCredPool, _ := s.tableExists("service_credentials")
	// Detect whether provider_wires already existed before this migrate run
	// (either from a previous run with new names, or via rename from service_wires).
	hadProviderWires, _ := s.tableExists("provider_wires")

	// Step 1: Rename legacy tables services → providers, tokens → users, etc.
	// Must run before CREATE TABLE so old tables are gone when new ones are
	// created. Each statement is guarded by the current schema state so an
	// interrupted earlier migration is repaired rather than skipped.
	if err := s.renameServicesToProviders(); err != nil {
		return err
	}
	if err := s.renameTokensToUsers(); err != nil {
		return err
	}

	// Step 2: Create tables (new names). IF NOT EXISTS means this is safe for
	// fresh databases and for databases that just went through the rename.
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
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
			user_id       TEXT NOT NULL DEFAULT '',
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
		`CREATE INDEX IF NOT EXISTS idx_calls_user_id ON calls(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_model ON calls(model)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_vendor ON calls(vendor)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_status ON calls(status)`,

		// Provider config lives in SQLite (managed from the dashboard),
		// the source of truth for routing. A provider
		// is one configured upstream: an adapter + base_url + a single API key +
		// the models it serves with their per-model prices.
		`CREATE TABLE IF NOT EXISTS providers (
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
		`CREATE TABLE IF NOT EXISTS provider_models (
			provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			model       TEXT NOT NULL,
			input       REAL NOT NULL DEFAULT 0,
			output      REAL NOT NULL DEFAULT 0,
			unit        TEXT NOT NULL DEFAULT 'per_1m_tokens',
			PRIMARY KEY (provider_id, model)
		)`,
		// Per-provider wire allowlist: which wire-protocol entries (path pattern +
		// usage extractor, see internal/wire) the proxy may serve for a provider.
		// Paths matching no enabled wire are denied unless allow_unmatched is set.
		`CREATE TABLE IF NOT EXISTS provider_wires (
			provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			wire        TEXT NOT NULL,
			PRIMARY KEY (provider_id, wire)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_provider_models_provider ON provider_models(provider_id)`,
		`CREATE INDEX IF NOT EXISTS idx_provider_wires_provider ON provider_wires(provider_id)`,

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

	// Step 3: Add post-v1 columns. These live here rather than in the CREATE
	// statements so the same path serves fresh and pre-existing databases.
	adds := []struct{ table, col, decl string }{
		{"calls", "wire", "TEXT NOT NULL DEFAULT ''"},
		{"calls", "confidence", "TEXT NOT NULL DEFAULT ''"},
		{"providers", "allow_unmatched", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "quirks", "TEXT NOT NULL DEFAULT '{}'"},
		{"providers", "api_key", "TEXT NOT NULL DEFAULT ''"},
		{"provider_models", "cached_input", "REAL NOT NULL DEFAULT 0"},
	}
	for _, a := range adds {
		if err := s.addColumn(a.table, a.col, a.decl); err != nil {
			return err
		}
	}

	// Step 4: Legacy migrations that only run on older databases.
	if hadCredPool {
		if err := s.foldCredentialPool(); err != nil {
			return err
		}
	}

	// Backfill wires only if neither provider_wires nor service_wires existed
	// before this migrate call (fresh DB or pre-wire-era DB). If either table
	// already existed — even if wire rows were manually deleted — we don't
	// re-add them. INSERT OR IGNORE makes the actual inserts idempotent anyway,
	// but skipping the work is cleaner.
	if !hadProviderWires && !hasOldWires {
		if err := s.backfillWires(); err != nil {
			return err
		}
	}
	return nil
}

// renameServicesToProviders migrates the services-era schema to the providers
// naming. Every step checks the live schema first, so it is idempotent and
// also repairs databases left half-migrated by an interrupted run (e.g. a
// renamed table whose column rename never happened, or an old service_wires
// coexisting with a freshly created empty provider_wires).
func (s *Store) renameServicesToProviders() error {
	s.db.Exec(`PRAGMA legacy_alter_table=ON`)
	defer s.db.Exec(`PRAGMA legacy_alter_table=OFF`)

	exec := func(stmt string) error {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("store: rename services→providers: %w", err)
		}
		return nil
	}

	if has, _ := s.tableExists("services"); has {
		if err := exec(`ALTER TABLE services RENAME TO providers`); err != nil {
			return err
		}
	}
	if has, _ := s.tableExists("service_models"); has {
		if err := exec(`ALTER TABLE service_models RENAME TO provider_models`); err != nil {
			return err
		}
	}
	if has, _ := s.hasColumn("provider_models", "service_id"); has {
		if err := exec(`ALTER TABLE provider_models RENAME COLUMN service_id TO provider_id`); err != nil {
			return err
		}
	}
	if has, _ := s.tableExists("service_wires"); has {
		if hasNew, _ := s.tableExists("provider_wires"); hasNew {
			// An interrupted migration already created the new (empty) table;
			// fold the old rows in instead of renaming over it.
			if err := exec(`INSERT OR IGNORE INTO provider_wires (provider_id, wire)
				SELECT service_id, wire FROM service_wires`); err != nil {
				return err
			}
			if err := exec(`DROP TABLE service_wires`); err != nil {
				return err
			}
		} else {
			if err := exec(`ALTER TABLE service_wires RENAME TO provider_wires`); err != nil {
				return err
			}
		}
	}
	if has, _ := s.hasColumn("provider_wires", "service_id"); has {
		if err := exec(`ALTER TABLE provider_wires RENAME COLUMN service_id TO provider_id`); err != nil {
			return err
		}
	}
	if err := exec(`DROP INDEX IF EXISTS idx_service_models_service`); err != nil {
		return err
	}
	return exec(`DROP INDEX IF EXISTS idx_service_wires_service`)
}

// renameTokensToUsers migrates the tokens-era schema to the users naming.
// Every step checks the live schema first, so it is idempotent and also
// repairs databases left half-migrated by an interrupted run.
func (s *Store) renameTokensToUsers() error {
	s.db.Exec(`PRAGMA legacy_alter_table=ON`)
	defer s.db.Exec(`PRAGMA legacy_alter_table=OFF`)

	exec := func(stmt string) error {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("store: rename tokens→users: %w", err)
		}
		return nil
	}

	if has, _ := s.tableExists("tokens"); has {
		if hasNew, _ := s.tableExists("users"); hasNew {
			// An interrupted migration already created the new (empty) table;
			// fold the old rows in instead of renaming over it.
			if err := exec(`INSERT OR IGNORE INTO users (id, name, key_hash, key_prefix, budget, scope, rpm, capture, created_at, revoked_at)
				SELECT id, name, key_hash, key_prefix, budget, scope, rpm, capture, created_at, revoked_at FROM tokens`); err != nil {
				return err
			}
			if err := exec(`DROP TABLE tokens`); err != nil {
				return err
			}
		} else {
			if err := exec(`ALTER TABLE tokens RENAME TO users`); err != nil {
				return err
			}
		}
	}
	if has, _ := s.hasColumn("calls", "token_id"); has {
		if err := exec(`ALTER TABLE calls RENAME COLUMN token_id TO user_id`); err != nil {
			return err
		}
	}
	return exec(`DROP INDEX IF EXISTS idx_calls_token_id`)
}

// foldCredentialPool migrates from the multi-key pool era: each provider keeps
// its oldest key as providers.api_key (any extra keys are dropped — one key per
// provider by design), then the pool table is removed.
func (s *Store) foldCredentialPool() error {
	if _, err := s.db.Exec(`UPDATE providers SET api_key = COALESCE(
			(SELECT sc.api_key FROM service_credentials sc
			 WHERE sc.service_id = providers.id
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

// backfillWires grants pre-wire-era providers the default allowlist for their
// adapter (names must stay in sync with internal/wire registrations). Runs
// only on the migration that introduces provider_wires.
func (s *Store) backfillWires() error {
	defaults := map[string][]string{
		"anthropic-compatible": {"anthropic/messages", "anthropic/models"},
		"":                     {"openai/chat", "openai/completions", "openai/embeddings", "openai/models"},
	}
	rows, err := s.db.Query(`SELECT id, adapter FROM providers`)
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
			if _, err := s.db.Exec(`INSERT OR IGNORE INTO provider_wires (provider_id, wire) VALUES (?, ?)`, v.id, w); err != nil {
				return fmt.Errorf("store: backfill wires: %w", err)
			}
		}
	}
	return nil
}

// hasColumn reports whether a table has a column of the given name. A missing
// table yields (false, nil): PRAGMA table_info returns no rows for it.
func (s *Store) hasColumn(table, col string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("store: table_info %s: %w", table, err)
	}
	defer rows.Close()
	found := false
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
			return false, fmt.Errorf("store: table_info %s: %w", table, err)
		}
		if name == col {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: table_info %s: %w", table, err)
	}
	return found, nil
}

// addColumn adds a column to a table if it is not already present, making
// schema evolution idempotent without a version table.
func (s *Store) addColumn(table, col, decl string) error {
	has, err := s.hasColumn(table, col)
	if err != nil || has {
		return err
	}
	if _, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, decl)); err != nil {
		return fmt.Errorf("store: add column %s.%s: %w", table, col, err)
	}
	return nil
}
