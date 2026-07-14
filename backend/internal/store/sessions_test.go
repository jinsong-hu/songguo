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

// TestUpsertSessionCallSplitsUtility verifies that a utility call (non-main
// entrypoint) is excluded from the accretion metrics (turns, tool_calls) but
// still counts toward the session's spend and lands in the utility_* slice.
func TestUpsertSessionCallSplitsUtility(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)

	// A visible main turn.
	if err := s.UpsertSessionCall(calls.Entry{
		SessionID: "sess", TS: base, TSEnd: base, Status: 200,
		Entrypoint:  calls.EntrypointMain,
		InputTokens: 100, OutputTokens: 20, Cost: 0.01, ToolCalls: 2, ToolTokens: 50,
	}, ""); err != nil {
		t.Fatalf("main UpsertSessionCall: %v", err)
	}
	// A monitor utility call: real spend, but not a visible turn and no tool activity.
	if err := s.UpsertSessionCall(calls.Entry{
		SessionID: "sess", TS: base.Add(time.Second), TSEnd: base.Add(time.Second), Status: 200,
		Entrypoint:  calls.EntrypointMonitor,
		InputTokens: 40, OutputTokens: 4, Cost: 0.002, ToolCalls: 9, ToolTokens: 999,
	}, ""); err != nil {
		t.Fatalf("utility UpsertSessionCall: %v", err)
	}

	var (
		turns, toolCalls, utilCalls int
		inTok, cost                 float64
		utilIn, utilCost            float64
		toolTokens, utilOut         float64
	)
	if err := s.db.QueryRow(`SELECT turns, tool_calls, tool_tokens, input_tokens, cost,
		utility_calls, utility_input_tokens, utility_output_tokens, utility_cost
		FROM sessions WHERE id = 'sess'`).Scan(
		&turns, &toolCalls, &toolTokens, &inTok, &cost,
		&utilCalls, &utilIn, &utilOut, &utilCost,
	); err != nil {
		t.Fatalf("read session row: %v", err)
	}

	// Accretion metrics: main call only.
	if turns != 1 {
		t.Errorf("turns = %d, want 1 (utility excluded)", turns)
	}
	if toolCalls != 2 {
		t.Errorf("tool_calls = %d, want 2 (utility tool activity excluded)", toolCalls)
	}
	if toolTokens != 50 {
		t.Errorf("tool_tokens = %v, want 50", toolTokens)
	}
	// Spend: both calls.
	if inTok != 140 {
		t.Errorf("input_tokens = %v, want 140 (main + utility)", inTok)
	}
	if cost != 0.012 {
		t.Errorf("cost = %v, want 0.012 (main + utility)", cost)
	}
	// Utility slice: monitor call only.
	if utilCalls != 1 {
		t.Errorf("utility_calls = %d, want 1", utilCalls)
	}
	if utilIn != 40 || utilOut != 4 {
		t.Errorf("utility tokens = (%v,%v), want (40,4)", utilIn, utilOut)
	}
	if utilCost != 0.002 {
		t.Errorf("utility_cost = %v, want 0.002", utilCost)
	}
}

// TestUpsertSessionCallUtilityFirstCall verifies a session whose first call is a
// utility call starts with 0 turns (the INSERT path also honors the split).
func TestUpsertSessionCallUtilityFirstCall(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	if err := s.UpsertSessionCall(calls.Entry{
		SessionID: "u", TS: base, TSEnd: base, Status: 200,
		Entrypoint: calls.EntrypointCountTokens, InputTokens: 10,
	}, ""); err != nil {
		t.Fatalf("UpsertSessionCall: %v", err)
	}
	var turns, utilCalls int
	if err := s.db.QueryRow(`SELECT turns, utility_calls FROM sessions WHERE id = 'u'`).Scan(&turns, &utilCalls); err != nil {
		t.Fatalf("read session row: %v", err)
	}
	if turns != 0 {
		t.Errorf("turns = %d, want 0 (first call was utility)", turns)
	}
	if utilCalls != 1 {
		t.Errorf("utility_calls = %d, want 1", utilCalls)
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
