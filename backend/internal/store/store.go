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
	"strings"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/wire"

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
	// Detect whether provider_endpoints already existed, to decide whether to
	// backfill it from the legacy per-provider base_url/adapter + provider_wires.
	hadProviderEndpoints, _ := s.tableExists("provider_endpoints")

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
	// Rename the legacy per-call body table payloads → raw before CREATE, so the
	// IF NOT EXISTS raw table below adopts the migrated rows rather than creating
	// an empty sibling. Idempotent (guarded on the live schema).
	if err := s.renamePayloadsToRaw(); err != nil {
		return err
	}
	// Convert calls.id (and the child FKs) from INTEGER AUTOINCREMENT to a TEXT
	// UUID column. Must run before CREATE so the rebuilt table is what the
	// CREATE IF NOT EXISTS matches. Idempotent (gated on the live id type).
	if err := s.migrateCallsToUUID(); err != nil {
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
			created_at INTEGER NOT NULL,
			revoked_at INTEGER
		)`,
		// calls is the per-call stats ledger (see docs/arch.md). id is a UUID
		// string minted by the gateway at request-start; the row is written in two
		// phases (create-at-start with status = pending / ts_end = NULL, then
		// update-at-end), so an incomplete call stays visible instead of vanishing.
		// Legacy integer ids are converted to their string form by
		// migrateCallsToUUID; the base shape here is the post-migration schema so a
		// fresh database is born correct.
		`CREATE TABLE IF NOT EXISTS calls (
			id            TEXT PRIMARY KEY,
			ts            INTEGER NOT NULL,
			ts_end        INTEGER,
			user_id       TEXT NOT NULL DEFAULT '',
			model         TEXT NOT NULL DEFAULT '',
			modality      TEXT NOT NULL DEFAULT 'unknown',
			vendor        TEXT NOT NULL DEFAULT '',
			credential_id TEXT NOT NULL DEFAULT '',
			status        INTEGER NOT NULL DEFAULT 0,
			err           TEXT NOT NULL DEFAULT '',
			usage         TEXT NOT NULL DEFAULT '{}',
			cost          REAL NOT NULL DEFAULT 0,
			latency_ms    INTEGER NOT NULL DEFAULT 0,
			ttft_ms       INTEGER NOT NULL DEFAULT 0,
			generation_ms INTEGER NOT NULL DEFAULT 0,
			stream        INTEGER NOT NULL DEFAULT 0,
			tags          TEXT NOT NULL DEFAULT '{}'
		)`,
		// raw holds the full request/response bodies (the previous "payloads"
		// table, renamed — see docs/arch-gateway.md). Capture-gated, redacted,
		// 1:1 with calls.id, pruned at 7 days independently of the 90-day calls
		// prune. renamePayloadsToRaw migrates pre-existing databases.
		`CREATE TABLE IF NOT EXISTS raw (
			call_id          TEXT PRIMARY KEY REFERENCES calls(id) ON DELETE CASCADE,
			req_headers      TEXT NOT NULL DEFAULT '{}',
			req_body         BLOB,
			req_content_type TEXT NOT NULL DEFAULT '',
			resp_headers     TEXT NOT NULL DEFAULT '{}',
			resp_body        BLOB,
			resp_content_type TEXT NOT NULL DEFAULT '',
			created_at       INTEGER NOT NULL
		)`,
		// parsed_calls holds the structured, protocol-neutral view produced by
		// the async parse pipeline (internal/parse), 1:1 with calls.id. `data`
		// is the JSON-encoded parse.Call; `format` names the parser used.
		`CREATE TABLE IF NOT EXISTS parsed_calls (
			call_id    TEXT PRIMARY KEY REFERENCES calls(id) ON DELETE CASCADE,
			format     TEXT NOT NULL DEFAULT '',
			data       TEXT NOT NULL DEFAULT '{}',
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_ts ON calls(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_user_id ON calls(user_id)`,
		// context_composition holds the estimated context-window decomposition for
		// a chat call, 1:1 with calls.id. `sources` is the JSON-encoded
		// []compose.Source partition; `blocks` is the itemized local counter
		// output without raw prompt text. Written read-only, off the hot path.
		`CREATE TABLE IF NOT EXISTS context_composition (
			call_id    TEXT PRIMARY KEY REFERENCES calls(id) ON DELETE CASCADE,
			total      REAL NOT NULL,
			cached     REAL NOT NULL,
			sources    TEXT NOT NULL,
			blocks     TEXT NOT NULL DEFAULT '[]',
			created_at INTEGER NOT NULL
		)`,
		// sessions is the materialized coding-agent session rollup owned by the
		// insights layer (see docs/arch-insights.md). It is a write-through cache:
		// incrementally maintained as each session-bearing call finalizes and
		// NEVER recomputed from calls. first_ts/last_ts bound the session;
		// last_status drives the inferred outcome; last_ts is the prune key.
		`CREATE TABLE IF NOT EXISTS sessions (
			id             TEXT PRIMARY KEY,
			title          TEXT NOT NULL DEFAULT '',
			first_ts       INTEGER NOT NULL,
			last_ts        INTEGER NOT NULL,
			turns          INTEGER NOT NULL DEFAULT 0,
			error_count    INTEGER NOT NULL DEFAULT 0,
			input_tokens   REAL NOT NULL DEFAULT 0,
			output_tokens  REAL NOT NULL DEFAULT 0,
			cost           REAL NOT NULL DEFAULT 0,
			last_status    INTEGER NOT NULL DEFAULT 0,
			has_subagents  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_model ON calls(model)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_vendor ON calls(vendor)`,
		`CREATE INDEX IF NOT EXISTS idx_calls_status ON calls(status)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_last_ts ON sessions(last_ts)`,

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
			cached_input REAL NOT NULL DEFAULT 0,
			unit        TEXT NOT NULL DEFAULT 'per_1m_tokens',
			price_override INTEGER NOT NULL DEFAULT 0,
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
		// An endpoint binds one wire to its full upstream URL + adapter (auth
		// scheme). The base_url column is renamed to `endpoint` and its values
		// rewritten to full per-wire URLs by migrateEndpointsToFull below; the
		// config manager then groups a provider's endpoints by (origin, adapter)
		// into routing vendors.
		`CREATE TABLE IF NOT EXISTS provider_endpoints (
			provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			wire        TEXT NOT NULL,
			base_url    TEXT NOT NULL DEFAULT '',
			adapter     TEXT NOT NULL DEFAULT 'openai-compatible',
			PRIMARY KEY (provider_id, wire)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_provider_models_provider ON provider_models(provider_id)`,
		`CREATE INDEX IF NOT EXISTS idx_provider_wires_provider ON provider_wires(provider_id)`,
		`CREATE INDEX IF NOT EXISTS idx_provider_endpoints_provider ON provider_endpoints(provider_id)`,
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
		// Normalized token counts (cross-vendor), persisted so token usage is
		// queryable without parsing the heterogeneous raw `usage` JSON. The three
		// input-side fields are DISJOINT and sum to total input: input_tokens is
		// fresh (uncached), cache_read_input_tokens is cache reads,
		// cache_creation_input_tokens is cache writes. thinking_tokens is a subset of
		// output_tokens. cache_read_input_tokens is (re)added below via the
		// cached_tokens rename step so pre-change data is preserved. Default 0; rows
		// written before these columns undercount until new traffic accrues.
		{"calls", "input_tokens", "REAL NOT NULL DEFAULT 0"},
		{"calls", "output_tokens", "REAL NOT NULL DEFAULT 0"},
		{"calls", "cache_creation_input_tokens", "REAL NOT NULL DEFAULT 0"},
		{"calls", "thinking_tokens", "REAL NOT NULL DEFAULT 0"},
		// Non-token metered units (speech wires): seconds is billed audio duration
		// (ASR, per_second), chars is billed text length (TTS, per_char). Default 0
		// for token-metered traffic and rows written before these columns.
		{"calls", "seconds", "REAL NOT NULL DEFAULT 0"},
		{"calls", "chars", "REAL NOT NULL DEFAULT 0"},
		// Streaming performance timings. Zero means unavailable (legacy row,
		// non-stream response, or a stream with no generated output delta).
		{"calls", "ttft_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"calls", "generation_ms", "INTEGER NOT NULL DEFAULT 0"},
		// Coding-agent attribution headers, captured read-only so the ledger can be
		// grouped by session and by the main-loop→subagent tree when available.
		// Empty for ordinary API traffic (and for rows written before these
		// columns).
		{"calls", "session_id", "TEXT NOT NULL DEFAULT ''"},
		{"calls", "agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"calls", "parent_agent_id", "TEXT NOT NULL DEFAULT ''"},
		// Why the call was made: a visible main-loop turn ('main') vs a harness
		// utility call (monitor, count_tokens, title/compaction). Classified
		// read-only from the request path + body. Empty on legacy rows ⇒ treated as
		// main. Lets the session rollup keep utility spend while excluding it from
		// the context/turn accretion metrics.
		{"calls", "entrypoint", "TEXT NOT NULL DEFAULT ''"},
		// Normalized client identity parsed from User-Agent. Empty for rows
		// written before this column existed or for unrecognized clients.
		{"calls", "client_name", "TEXT NOT NULL DEFAULT ''"},
		{"calls", "client_version", "TEXT NOT NULL DEFAULT ''"},
		// Best-effort caller OS: client_os is a normalized family (e.g. MacOS);
		// client_os_version is the OS version when the source carries one. Empty for
		// rows written before these columns or when the OS can't be determined.
		{"calls", "client_os", "TEXT NOT NULL DEFAULT ''"},
		{"calls", "client_os_version", "TEXT NOT NULL DEFAULT ''"},
		// Per-call tool-use metrics (compose.ToolTurn): count of tool calls the
		// carried turn issued, and a local o200k token estimate of their results.
		// Derived/estimate — see calls.Entry. Summed into the sessions rollup.
		{"calls", "tool_calls", "INTEGER NOT NULL DEFAULT 0"},
		{"calls", "tool_tokens", "REAL NOT NULL DEFAULT 0"},
		{"sessions", "title", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "tool_calls", "INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "tool_tokens", "REAL NOT NULL DEFAULT 0"},
		// Session-level token rollups mirroring the disjoint calls columns; summed
		// across the session's calls by UpsertSessionCall and the backfill/reseed.
		{"sessions", "cache_read_input_tokens", "REAL NOT NULL DEFAULT 0"},
		{"sessions", "cache_creation_input_tokens", "REAL NOT NULL DEFAULT 0"},
		{"sessions", "thinking_tokens", "REAL NOT NULL DEFAULT 0"},
		// Utility-call slice of the session: harness calls (monitor, count_tokens,
		// title/compaction) that share the wire but are not visible turns. Kept in
		// the session's spend (the token/cost columns above already include them),
		// and broken out here so the dashboard can show a separate utility track and
		// subtract it to get the context/turn view. utility_calls counts them;
		// utility_tokens/cost sum their spend. Turns and tool_calls above EXCLUDE
		// utility calls (accretion metrics), so turns is the visible-turn count.
		{"sessions", "utility_calls", "INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "utility_input_tokens", "REAL NOT NULL DEFAULT 0"},
		{"sessions", "utility_output_tokens", "REAL NOT NULL DEFAULT 0"},
		{"sessions", "utility_cache_read_input_tokens", "REAL NOT NULL DEFAULT 0"},
		{"sessions", "utility_cache_creation_input_tokens", "REAL NOT NULL DEFAULT 0"},
		{"sessions", "utility_cost", "REAL NOT NULL DEFAULT 0"},
		{"providers", "allow_unmatched", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "quirks", "TEXT NOT NULL DEFAULT '{}'"},
		{"providers", "api_key", "TEXT NOT NULL DEFAULT ''"},
		{"provider_models", "cached_input", "REAL NOT NULL DEFAULT 0"},
		{"provider_models", "price_override", "INTEGER NOT NULL DEFAULT 0"},
		// key_full stores the plaintext key so the dashboard can display and copy
		// it after creation. Empty for rows created before this column existed.
		{"users", "key_full", "TEXT NOT NULL DEFAULT ''"},
		{"users", "capture", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, a := range adds {
		if err := s.addColumn(a.table, a.col, a.decl); err != nil {
			return err
		}
	}
	if err := s.addColumn("context_composition", "blocks", `TEXT NOT NULL DEFAULT '[]'`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE users SET capture = 0 WHERE capture IS NULL`); err != nil {
		return fmt.Errorf("store: backfill users.capture: %w", err)
	}

	// Canonical token model: rename the old folded cached_tokens column to
	// cache_read_input_tokens and split the historical folded input_tokens into
	// the disjoint shape. Pre-change rows stored input_tokens = fresh + cache_read
	// + cache_create (a folded total) and cached_tokens = cache_read. The rename
	// preserves the cache-read values; the backfill subtracts them back out of
	// input_tokens so every historical row matches the new definition (fresh input
	// only). cache_create was never stored separately, so it stays absorbed inside
	// input_tokens — exactly how the folded billing already treated it, and how new
	// OpenAI rows treat it too. Guarded on the old column name, so it runs once and
	// is idempotent (re-running finds no cached_tokens and skips). tokensMigrated
	// gates the one-time sessions re-seed below.
	tokensMigrated := false
	if has, err := s.hasColumn("calls", "cached_tokens"); err != nil {
		return err
	} else if has {
		if _, err := s.db.Exec(`ALTER TABLE calls RENAME COLUMN cached_tokens TO cache_read_input_tokens`); err != nil {
			return fmt.Errorf("store: rename calls.cached_tokens: %w", err)
		}
		if _, err := s.db.Exec(
			`UPDATE calls SET input_tokens = MAX(0, input_tokens - cache_read_input_tokens)`,
		); err != nil {
			return fmt.Errorf("store: backfill calls.input_tokens: %w", err)
		}
		tokensMigrated = true
	}
	// Fresh databases (born without cached_tokens) and already-migrated ones need
	// the renamed column created directly.
	if err := s.addColumn("calls", "cache_read_input_tokens", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}

	// One-time re-seed of the sessions rollup after the calls backfill above made
	// historical rows disjoint. Only a sessions table that predates this change can
	// hold stale folded input_tokens (and zeros for the new columns), and that is
	// exactly the case tokensMigrated captures. This is a migration-time seed like
	// backfillSessions, NOT a runtime recompute, so the "never recompute sessions"
	// invariant holds. Empty sessions tables update no rows here and are seeded by
	// backfillSessions (Step 4) instead.
	if tokensMigrated {
		if _, err := s.db.Exec(`UPDATE sessions SET
			input_tokens                = COALESCE((SELECT SUM(c.input_tokens)                FROM calls c WHERE c.session_id = sessions.id), 0),
			output_tokens               = COALESCE((SELECT SUM(c.output_tokens)               FROM calls c WHERE c.session_id = sessions.id), 0),
			cache_read_input_tokens     = COALESCE((SELECT SUM(c.cache_read_input_tokens)     FROM calls c WHERE c.session_id = sessions.id), 0),
			cache_creation_input_tokens = COALESCE((SELECT SUM(c.cache_creation_input_tokens) FROM calls c WHERE c.session_id = sessions.id), 0),
			thinking_tokens             = COALESCE((SELECT SUM(c.thinking_tokens)             FROM calls c WHERE c.session_id = sessions.id), 0)`,
		); err != nil {
			return fmt.Errorf("store: reseed sessions tokens: %w", err)
		}
	}

	// Index the session id for the activity feed's session grouping. Created
	// after the adds loop because session_id is a post-v1 column (agent_id needs
	// no index — it is only ever scanned within a single session).
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_calls_session_id ON calls(session_id)`); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}

	// Step 3b: Drop columns retired post-v1. calls.attempt tracked per-call
	// failover, which no longer exists (one attempt per request); drop it from
	// pre-existing databases so the schema matches the CREATE above.
	drops := []struct{ table, col string }{
		{"calls", "attempt"},
	}
	for _, d := range drops {
		if err := s.dropColumn(d.table, d.col); err != nil {
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

	// Backfill provider_endpoints from the legacy shape (per-provider base_url +
	// adapter, one row per provider_wires entry) the first time this table
	// appears. INSERT OR IGNORE keeps it idempotent if interrupted.
	if !hadProviderEndpoints {
		if err := s.backfillEndpoints(); err != nil {
			return err
		}
	}

	// Rename base_url → endpoint and convert legacy base URLs into full per-wire
	// endpoints. Atomic and gated on the base_url column, so it runs once across
	// fresh, legacy, and already-endpoint-backed databases.
	if err := s.migrateEndpointsToFull(); err != nil {
		return err
	}

	// Seed the materialized sessions rollup from existing calls the first time
	// the table is empty on a database that already has session-bearing calls.
	// This is a ONE-TIME cache seed at migration, NOT a read-time recompute (the
	// rollup is never rebuilt afterward — see docs/arch-insights.md); without it,
	// historical sessions would show nothing until new traffic arrives.
	if err := s.backfillSessions(); err != nil {
		return err
	}
	return nil
}

// backfillSessions seeds the sessions table from existing session-bearing calls,
// but only when sessions is empty and calls has session traffic — so it runs at
// most once (a populated sessions table is left untouched; the rollup is
// incrementally maintained thereafter and never recomputed). The aggregation
// mirrors UpsertSessionCall's accumulation: turns, error count, token sums, time
// bounds, last-status by newest ts, and subagent presence.
func (s *Store) backfillSessions() error {
	hasSessions, _ := s.tableExists("sessions")
	hasCalls, _ := s.tableExists("calls")
	if !hasSessions || !hasCalls {
		return nil
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		return fmt.Errorf("store: count sessions: %w", err)
	}
	if n > 0 {
		return nil // already populated; never rebuild
	}
	// last_status is the status of the newest call per session (max ts, tie-broken
	// by id). A correlated subquery keeps this to one statement.
	//
	// The utility split mirrors UpsertSessionCall: a utility call (entrypoint set
	// and not 'main') adds 0 turns and 0 tool activity but still counts toward the
	// session's token/cost spend, and is broken out into the utility_* slice. The
	// isUtil predicate is `entrypoint != '' AND entrypoint != 'main'`; legacy rows
	// have entrypoint = '' so they all read as main — matching the "default to
	// main" classification, which is the only safe read of un-tagged history.
	if _, err := s.db.Exec(`INSERT INTO sessions
		(id, first_ts, last_ts, turns, error_count, input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, thinking_tokens, cost, tool_calls, tool_tokens, last_status, has_subagents,
		 utility_calls, utility_input_tokens, utility_output_tokens, utility_cache_read_input_tokens, utility_cache_creation_input_tokens, utility_cost)
		SELECT
			c.session_id,
			MIN(c.ts),
			MAX(COALESCE(c.ts_end, c.ts)),
			SUM(CASE WHEN c.entrypoint != '' AND c.entrypoint != 'main' THEN 0 ELSE 1 END),
			SUM(CASE WHEN c.status = 0 OR c.status >= 400 THEN 1 ELSE 0 END),
			COALESCE(SUM(c.input_tokens), 0),
			COALESCE(SUM(c.output_tokens), 0),
			COALESCE(SUM(c.cache_read_input_tokens), 0),
			COALESCE(SUM(c.cache_creation_input_tokens), 0),
			COALESCE(SUM(c.thinking_tokens), 0),
			COALESCE(SUM(c.cost), 0),
			COALESCE(SUM(CASE WHEN c.entrypoint != '' AND c.entrypoint != 'main' THEN 0 ELSE c.tool_calls END), 0),
			COALESCE(SUM(CASE WHEN c.entrypoint != '' AND c.entrypoint != 'main' THEN 0 ELSE c.tool_tokens END), 0),
			(SELECT l.status FROM calls l
			  WHERE l.session_id = c.session_id
			  ORDER BY COALESCE(l.ts_end, l.ts) DESC, l.id DESC LIMIT 1),
			MAX(CASE WHEN c.parent_agent_id != '' THEN 1 ELSE 0 END),
			SUM(CASE WHEN c.entrypoint != '' AND c.entrypoint != 'main' THEN 1 ELSE 0 END),
			COALESCE(SUM(CASE WHEN c.entrypoint != '' AND c.entrypoint != 'main' THEN c.input_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN c.entrypoint != '' AND c.entrypoint != 'main' THEN c.output_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN c.entrypoint != '' AND c.entrypoint != 'main' THEN c.cache_read_input_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN c.entrypoint != '' AND c.entrypoint != 'main' THEN c.cache_creation_input_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN c.entrypoint != '' AND c.entrypoint != 'main' THEN c.cost ELSE 0 END), 0)
		FROM calls c
		WHERE c.session_id != ''
		GROUP BY c.session_id`); err != nil {
		return fmt.Errorf("store: backfill sessions: %w", err)
	}
	return nil
}

// backfillEndpoints seeds provider_endpoints from each provider's legacy
// base_url + adapter columns joined with its provider_wires rows, so existing
// single-base-URL providers become endpoint-backed with unchanged routing.
func (s *Store) backfillEndpoints() error {
	hasWires, _ := s.tableExists("provider_wires")
	if !hasWires {
		return nil
	}
	hasBase, _ := s.hasColumn("providers", "base_url")
	hasAdapter, _ := s.hasColumn("providers", "adapter")
	if !hasBase || !hasAdapter {
		return nil
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO provider_endpoints (provider_id, wire, base_url, adapter)
		SELECT pw.provider_id, pw.wire, p.base_url, p.adapter
		FROM provider_wires pw JOIN providers p ON p.id = pw.provider_id`); err != nil {
		return fmt.Errorf("store: backfill endpoints: %w", err)
	}
	return nil
}

// migrateEndpointsToFull renames provider_endpoints.base_url → endpoint and
// rewrites each legacy base URL into a full per-wire endpoint, used as-is by the
// proxy. Model-routed wires (chat/embedding) get their canonical path suffix
// appended; origin-only wires (model listings, speech) keep the base. The whole
// step runs in one transaction and is gated on the base_url column, so it
// executes exactly once and an interrupted run is retried (never half-applied).
func (s *Store) migrateEndpointsToFull() error {
	has, err := s.hasColumn("provider_endpoints", "base_url")
	if err != nil {
		return fmt.Errorf("store: check endpoint column: %w", err)
	}
	if !has {
		return nil // already migrated (column is now `endpoint`) or fresh
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin endpoint migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`ALTER TABLE provider_endpoints RENAME COLUMN base_url TO endpoint`); err != nil {
		return fmt.Errorf("store: rename base_url to endpoint: %w", err)
	}

	rows, err := tx.Query(`SELECT provider_id, wire, endpoint FROM provider_endpoints`)
	if err != nil {
		return fmt.Errorf("store: read endpoints for migration: %w", err)
	}
	type epRow struct{ pid, wire, endpoint string }
	var updates []epRow
	for rows.Next() {
		var r epRow
		if err := rows.Scan(&r.pid, &r.wire, &r.endpoint); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan endpoint for migration: %w", err)
		}
		w, ok := wire.Get(r.wire)
		if !ok || len(w.Suffixes) == 0 {
			continue
		}
		if w.Modality != calls.ModalityChat && w.Modality != calls.ModalityEmbedding {
			continue // origin-only wire: base is already the right value
		}
		trimmed := strings.TrimRight(r.endpoint, "/")
		if trimmed == "" || strings.HasSuffix(trimmed, w.Suffixes[0]) {
			continue // empty or already a full endpoint
		}
		updates = append(updates, epRow{r.pid, r.wire, trimmed + w.Suffixes[0]})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: iterate endpoints for migration: %w", err)
	}
	rows.Close()

	for _, u := range updates {
		if _, err := tx.Exec(`UPDATE provider_endpoints SET endpoint = ? WHERE provider_id = ? AND wire = ?`,
			u.endpoint, u.pid, u.wire); err != nil {
			return fmt.Errorf("store: convert endpoint to full: %w", err)
		}
	}
	return tx.Commit()
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
			if err := exec(`INSERT OR IGNORE INTO users (id, name, key_hash, key_prefix, budget, scope, rpm, created_at, revoked_at)
				SELECT id, name, key_hash, key_prefix, budget, scope, rpm, created_at, revoked_at FROM tokens`); err != nil {
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

// renamePayloadsToRaw renames the legacy per-call body table payloads → raw so
// the CREATE TABLE IF NOT EXISTS raw below adopts the migrated rows. Guarded on
// the live schema so it is idempotent and a no-op on fresh databases (where raw
// is created directly) and on already-migrated ones. The call_id column is
// converted to TEXT alongside calls.id by migrateCallsToUUID, which runs next.
func (s *Store) renamePayloadsToRaw() error {
	hasOld, _ := s.tableExists("payloads")
	if !hasOld {
		return nil // fresh DB or already renamed
	}
	if hasNew, _ := s.tableExists("raw"); hasNew {
		// An interrupted migration already created the new (empty) raw table; fold
		// the old rows in and drop the legacy table rather than renaming over it.
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO raw
			(call_id, req_headers, req_body, req_content_type, resp_headers, resp_body, resp_content_type, created_at)
			SELECT call_id, req_headers, req_body, req_content_type, resp_headers, resp_body, resp_content_type, created_at
			FROM payloads`); err != nil {
			return fmt.Errorf("store: fold payloads→raw: %w", err)
		}
		if _, err := s.db.Exec(`DROP TABLE payloads`); err != nil {
			return fmt.Errorf("store: drop payloads: %w", err)
		}
		return nil
	}
	if _, err := s.db.Exec(`ALTER TABLE payloads RENAME TO raw`); err != nil {
		return fmt.Errorf("store: rename payloads→raw: %w", err)
	}
	return nil
}

// migrateCallsToUUID converts calls.id from INTEGER AUTOINCREMENT to a TEXT UUID
// column (and the child foreign keys in lockstep), rebuilding the calls table
// because SQLite cannot alter a column type in place. It is gated on the live id
// type, so it runs exactly once and is a no-op on a fresh DB (born TEXT) and on
// an already-migrated one. The rebuild preserves every existing column and its
// data by introspecting the live schema — legacy integer ids become their string
// form (CAST id AS TEXT), which keeps existing rows and their captured
// req/response bodies linked. attempt (a retired failover column) is dropped in
// the process; ts_end (the two-phase end time) is added as NULL for historical
// rows.
func (s *Store) migrateCallsToUUID() error {
	hasCalls, _ := s.tableExists("calls")
	if !hasCalls {
		return nil // fresh DB: CREATE below makes a TEXT-id table directly
	}
	idType, err := s.columnType("calls", "id")
	if err != nil {
		return err
	}
	if strings.EqualFold(idType, "TEXT") {
		return nil // already migrated
	}

	// Rebuilding calls (and its children) means dropping a table other tables
	// reference by foreign key. Disable FK enforcement for the duration — it
	// cannot be toggled inside a transaction, so do it around the whole rebuild
	// and restore it after. Open() set it ON globally, so restore to ON.
	if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("store: disable fk for calls migration: %w", err)
	}
	defer s.db.Exec(`PRAGMA foreign_keys=ON`)

	// Preserve every current column except the retired `attempt`; map id → TEXT
	// and splice in ts_end (NULL for historical rows). Column list is discovered
	// so post-v1 additions (wire, input_tokens, session_id, …) carry over.
	cols, err := columnNames(s.db, "calls")
	if err != nil {
		return err
	}
	// Build the (new-table column list, select expression) pairs in a stable
	// order that matches the rebuilt table below.
	var newCols, selectExprs []string
	for _, c := range cols {
		if c == "attempt" {
			continue // retired
		}
		newCols = append(newCols, c)
		if c == "id" {
			selectExprs = append(selectExprs, `CAST(id AS TEXT)`)
		} else {
			selectExprs = append(selectExprs, c)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin calls uuid migration: %w", err)
	}
	defer tx.Rollback()

	// Rebuild under a temp name with the TEXT id, copy rows, swap in. The child
	// tables (raw, parsed_calls, context_composition) keep their own call_id
	// values — already the same integers, string-compatible via SQLite's dynamic
	// typing — and are re-typed to TEXT by the CREATE IF NOT EXISTS + their own
	// data being small; here we only need calls itself rebuilt.
	if _, err := tx.Exec(`CREATE TABLE calls_new (
		id            TEXT PRIMARY KEY,
		ts            INTEGER NOT NULL,
		ts_end        INTEGER,
		user_id       TEXT NOT NULL DEFAULT '',
		model         TEXT NOT NULL DEFAULT '',
		modality      TEXT NOT NULL DEFAULT 'unknown',
		vendor        TEXT NOT NULL DEFAULT '',
		credential_id TEXT NOT NULL DEFAULT '',
		status        INTEGER NOT NULL DEFAULT 0,
		err           TEXT NOT NULL DEFAULT '',
		usage         TEXT NOT NULL DEFAULT '{}',
		cost          REAL NOT NULL DEFAULT 0,
		latency_ms    INTEGER NOT NULL DEFAULT 0,
		ttft_ms       INTEGER NOT NULL DEFAULT 0,
		generation_ms INTEGER NOT NULL DEFAULT 0,
		stream        INTEGER NOT NULL DEFAULT 0,
		tags          TEXT NOT NULL DEFAULT '{}',
		wire          TEXT NOT NULL DEFAULT '',
		confidence    TEXT NOT NULL DEFAULT '',
		input_tokens  REAL NOT NULL DEFAULT 0,
		output_tokens REAL NOT NULL DEFAULT 0,
		cached_tokens REAL NOT NULL DEFAULT 0,
		session_id    TEXT NOT NULL DEFAULT '',
		agent_id      TEXT NOT NULL DEFAULT '',
		parent_agent_id TEXT NOT NULL DEFAULT '',
		client_name    TEXT NOT NULL DEFAULT '',
		client_version TEXT NOT NULL DEFAULT '',
		client_os      TEXT NOT NULL DEFAULT '',
		client_os_version TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return fmt.Errorf("store: create calls_new: %w", err)
	}

	// Only copy columns that exist in BOTH the old table and calls_new. ts_end is
	// new (no source column) and is left NULL. This intersection keeps the INSERT
	// valid regardless of which post-v1 columns the old DB had. client_name/
	// client_version are carried over when the upstream DB already has them.
	//
	// NOTE: this rebuild deliberately reproduces the PRE-canonical-token schema —
	// it keeps the old `cached_tokens` column name (not cache_read_input_tokens)
	// and omits cache_creation_input_tokens/thinking_tokens. It runs in Step 1,
	// before the Step 3 token migration, so its job is to preserve the old table's
	// cache-read data under the old name; the Step 3 rename+adds then carry this
	// (legacy integer-id) database forward identically to every other pre-existing
	// DB. Renaming here instead would drop cache-read data on the intersection copy.
	newSet := map[string]bool{
		"id": true, "ts": true, "user_id": true, "model": true, "modality": true,
		"vendor": true, "credential_id": true, "status": true, "err": true,
		"usage": true, "cost": true, "latency_ms": true, "ttft_ms": true,
		"generation_ms": true, "stream": true, "tags": true,
		"wire": true, "confidence": true, "input_tokens": true, "output_tokens": true,
		"cached_tokens": true, "session_id": true, "agent_id": true, "parent_agent_id": true,
		"client_name": true, "client_version": true,
		"client_os": true, "client_os_version": true,
	}
	var copyCols, copyExprs []string
	for i, c := range newCols {
		if !newSet[c] {
			continue
		}
		copyCols = append(copyCols, c)
		copyExprs = append(copyExprs, selectExprs[i])
	}
	insert := fmt.Sprintf(`INSERT INTO calls_new (%s) SELECT %s FROM calls`,
		strings.Join(copyCols, ", "), strings.Join(copyExprs, ", "))
	if _, err := tx.Exec(insert); err != nil {
		return fmt.Errorf("store: copy calls→calls_new: %w", err)
	}

	if _, err := tx.Exec(`DROP TABLE calls`); err != nil {
		return fmt.Errorf("store: drop old calls: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE calls_new RENAME TO calls`); err != nil {
		return fmt.Errorf("store: rename calls_new→calls: %w", err)
	}

	// The per-call child tables (raw, parsed_calls, context_composition) declare
	// call_id with INTEGER affinity, so their values stay integers even after the
	// parent id became a string — and SQLite compares across storage classes by
	// class first, so integer 5 never equals the string "5". Left alone this
	// silently breaks every historical trace join. A plain UPDATE ... CAST won't
	// fix it (the column's INTEGER affinity coerces the text right back), so each
	// child must be rebuilt with a TEXT call_id, its values converted in the copy.
	// Rebuilt here inside the same transaction, gated on the live affinity so it
	// is idempotent. The child's OTHER columns are preserved by name intersection,
	// mirroring the calls rebuild above.
	for _, spec := range []struct{ name, createBody string }{
		{"raw", `call_id          TEXT PRIMARY KEY REFERENCES calls(id) ON DELETE CASCADE,
			req_headers      TEXT NOT NULL DEFAULT '{}',
			req_body         BLOB,
			req_content_type TEXT NOT NULL DEFAULT '',
			resp_headers     TEXT NOT NULL DEFAULT '{}',
			resp_body        BLOB,
			resp_content_type TEXT NOT NULL DEFAULT '',
			created_at       INTEGER NOT NULL`},
		{"parsed_calls", `call_id    TEXT PRIMARY KEY REFERENCES calls(id) ON DELETE CASCADE,
			format     TEXT NOT NULL DEFAULT '',
			data       TEXT NOT NULL DEFAULT '{}',
			created_at INTEGER NOT NULL`},
		{"context_composition", `call_id    TEXT PRIMARY KEY REFERENCES calls(id) ON DELETE CASCADE,
			total      REAL NOT NULL,
			cached     REAL NOT NULL,
			sources    TEXT NOT NULL,
			blocks     TEXT NOT NULL DEFAULT '[]',
			created_at INTEGER NOT NULL`},
	} {
		if err := rebuildChildCallID(tx, s, spec.name, spec.createBody); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// rebuildChildCallID rebuilds a per-call child table so its call_id column has
// TEXT affinity, converting existing integer call_ids to their string form so
// they join the now-TEXT calls.id. A no-op when the table is absent or already
// TEXT-affinity. Runs inside the calls-UUID migration transaction.
func rebuildChildCallID(tx *sql.Tx, s *Store, table, createBody string) error {
	hasChild, _ := s.tableExists(table)
	if !hasChild {
		return nil
	}
	typ, err := s.columnType(table, "call_id")
	if err != nil {
		return err
	}
	if strings.EqualFold(typ, "TEXT") {
		return nil // already migrated
	}
	tmp := table + "_new"
	if _, err := tx.Exec(fmt.Sprintf(`CREATE TABLE %s (%s)`, tmp, createBody)); err != nil {
		return fmt.Errorf("store: create %s: %w", tmp, err)
	}
	cols, err := columnNames(tx, table)
	if err != nil {
		return err
	}
	targetCols, err := columnNames(tx, tmp)
	if err != nil {
		return err
	}
	targetSet := make(map[string]bool, len(targetCols))
	for _, c := range targetCols {
		targetSet[c] = true
	}
	var names, exprs []string
	for _, c := range cols {
		if !targetSet[c] {
			continue
		}
		names = append(names, c)
		if c == "call_id" {
			exprs = append(exprs, `CAST(call_id AS TEXT)`)
		} else {
			exprs = append(exprs, c)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s`,
		tmp, strings.Join(names, ", "), strings.Join(exprs, ", "), table)); err != nil {
		return fmt.Errorf("store: copy %s→%s: %w", table, tmp, err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`DROP TABLE %s`, table)); err != nil {
		return fmt.Errorf("store: drop %s: %w", table, err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, tmp, table)); err != nil {
		return fmt.Errorf("store: rename %s→%s: %w", tmp, table, err)
	}
	return nil
}

// columnType returns the declared type of a column, or "" if the column (or
// table) is absent.
func (s *Store) columnType(table, col string) (string, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return "", fmt.Errorf("store: table_info %s: %w", table, err)
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
			return "", fmt.Errorf("store: table_info %s: %w", table, err)
		}
		if name == col {
			return typ, nil
		}
	}
	return "", rows.Err()
}

// columnNames returns the column names of a table in declared order. queryer is
// either the store DB or an active migration transaction.
func columnNames(queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, table string) ([]string, error) {
	rows, err := queryer.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("store: table_info %s: %w", table, err)
	}
	defer rows.Close()
	var out []string
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
			return nil, fmt.Errorf("store: table_info %s: %w", table, err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
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
		"anthropic-compatible": {"anthropic/messages"},
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

// dropColumn removes a column from a table if it is still present, the inverse
// of addColumn and likewise idempotent (a no-op once the column is gone or on a
// fresh DB that never had it). Requires SQLite ≥ 3.35 (bundled by the driver).
func (s *Store) dropColumn(table, col string) error {
	has, err := s.hasColumn(table, col)
	if err != nil || !has {
		return err
	}
	if _, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", table, col)); err != nil {
		return fmt.Errorf("store: drop column %s.%s: %w", table, col, err)
	}
	return nil
}
