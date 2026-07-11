package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestTokenColumnMigration exercises the canonical-token migration end to end on a
// pre-change database: the folded cached_tokens column is renamed to
// cache_read_input_tokens, historical input_tokens is split back to fresh-only, the
// two new columns default to zero, and the sessions rollup is re-seeded from the
// now-disjoint calls. Re-running the migration must be a no-op (idempotent).
func TestTokenColumnMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-tokens.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}

	// Pre-change schema: calls has the folded input_tokens + cached_tokens (the
	// cache-read subset), and NO cache_creation_input_tokens / thinking_tokens.
	// sessions holds folded rollups and lacks the new columns too.
	if _, err := db.Exec(`
		CREATE TABLE calls (
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
			client_version TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE sessions (
			id             TEXT PRIMARY KEY,
			first_ts       INTEGER NOT NULL,
			last_ts        INTEGER NOT NULL,
			turns          INTEGER NOT NULL DEFAULT 0,
			error_count    INTEGER NOT NULL DEFAULT 0,
			input_tokens   REAL NOT NULL DEFAULT 0,
			output_tokens  REAL NOT NULL DEFAULT 0,
			cost           REAL NOT NULL DEFAULT 0,
			last_status    INTEGER NOT NULL DEFAULT 0,
			has_subagents  INTEGER NOT NULL DEFAULT 0
		);
		-- call1: folded input 100 = fresh 60 + cache_read 40. call2: no cache.
		INSERT INTO calls (id, ts, session_id, status, input_tokens, output_tokens, cached_tokens)
		VALUES ('c1', 1000, 's1', 200, 100, 20, 40),
		       ('c2', 2000, 's1', 200, 50, 5, 0);
		INSERT INTO sessions (id, first_ts, last_ts, turns, input_tokens, output_tokens, last_status)
		VALUES ('s1', 1000, 2000, 2, 150, 25, 200);
	`); err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// calls: cache_read carried over from cached_tokens; input_tokens split to fresh.
	type row struct{ in, cacheRead, cacheCreate, thinking float64 }
	readCall := func(id string) row {
		var r row
		if err := s.db.QueryRow(
			`SELECT input_tokens, cache_read_input_tokens, cache_creation_input_tokens, thinking_tokens
			   FROM calls WHERE id = ?`, id,
		).Scan(&r.in, &r.cacheRead, &r.cacheCreate, &r.thinking); err != nil {
			t.Fatalf("read call %s: %v", id, err)
		}
		return r
	}
	if got := readCall("c1"); got != (row{in: 60, cacheRead: 40, cacheCreate: 0, thinking: 0}) {
		t.Errorf("c1 = %+v, want {60 40 0 0}", got)
	}
	if got := readCall("c2"); got != (row{in: 50, cacheRead: 0, cacheCreate: 0, thinking: 0}) {
		t.Errorf("c2 = %+v, want {50 0 0 0}", got)
	}

	// sessions: re-seeded from the disjoint calls (input = 60+50, cache_read = 40).
	var sess row
	var sessOut float64
	if err := s.db.QueryRow(
		`SELECT input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, thinking_tokens
		   FROM sessions WHERE id = 's1'`,
	).Scan(&sess.in, &sessOut, &sess.cacheRead, &sess.cacheCreate, &sess.thinking); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.in != 110 || sessOut != 25 || sess.cacheRead != 40 || sess.cacheCreate != 0 || sess.thinking != 0 {
		t.Errorf("session s1 = in %v out %v read %v create %v think %v, want 110/25/40/0/0",
			sess.in, sessOut, sess.cacheRead, sess.cacheCreate, sess.thinking)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}

	// Idempotency: re-opening must not re-subtract the cache-read from input_tokens
	// (the rename gate finds no cached_tokens the second time) nor re-seed sessions.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	defer s2.Close()
	var in2 float64
	if err := s2.db.QueryRow(`SELECT input_tokens FROM calls WHERE id = 'c1'`).Scan(&in2); err != nil {
		t.Fatalf("re-read c1: %v", err)
	}
	if in2 != 60 {
		t.Errorf("c1 input_tokens after re-migrate = %v, want 60 (no double subtraction)", in2)
	}
}
