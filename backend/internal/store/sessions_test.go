package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

func TestUpsertSessionCallPersistsFirstTitle(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	if err := s.UpsertSessionCall(calls.Entry{
		SessionID: "sess", TS: base, TSEnd: base, Status: 200,
	}, ""); err != nil {
		t.Fatalf("initial UpsertSessionCall: %v", err)
	}
	if err := s.UpsertSessionCall(calls.Entry{
		SessionID: "sess", TS: base.Add(time.Minute), TSEnd: base.Add(time.Minute), Status: 200,
	}, "Persist captured session titles"); err != nil {
		t.Fatalf("title UpsertSessionCall: %v", err)
	}
	if err := s.UpsertSessionCall(calls.Entry{
		SessionID: "sess", TS: base.Add(2 * time.Minute), TSEnd: base.Add(2 * time.Minute), Status: 200,
	}, "Do not replace the title"); err != nil {
		t.Fatalf("replacement UpsertSessionCall: %v", err)
	}

	got, err := s.SessionTitle("sess")
	if err != nil {
		t.Fatalf("SessionTitle: %v", err)
	}
	if got != "Persist captured session titles" {
		t.Fatalf("SessionTitle = %q, want first non-empty title", got)
	}
}

func TestMigrationAddsSessionTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	if _, err := db.Exec(`
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
		INSERT INTO sessions (id, first_ts, last_ts, turns, last_status)
		VALUES ('legacy', 1, 2, 1, 200);
	`); err != nil {
		t.Fatalf("seed legacy sessions: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy DB: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrate legacy sessions: %v", err)
	}
	defer s.Close()

	title, err := s.SessionTitle("legacy")
	if err != nil {
		t.Fatalf("SessionTitle after migration: %v", err)
	}
	if title != "" {
		t.Fatalf("migrated title = %q, want empty", title)
	}
}
