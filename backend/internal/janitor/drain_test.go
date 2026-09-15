package janitor

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/store"
	_ "modernc.org/sqlite"
)

func openWithLegacyParsedCalls(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "drain.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	for _, stmt := range []string{
		`CREATE TABLE parsed_calls (call_id TEXT PRIMARY KEY, format TEXT NOT NULL DEFAULT '', data TEXT NOT NULL DEFAULT '{}', created_at INTEGER NOT NULL)`,
		`INSERT INTO parsed_calls (call_id, created_at) VALUES ('a', 0), ('b', 0), ('c', 0), ('d', 0), ('e', 0), ('f', 0)`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return st, raw
}

func parsedCallsExists(t *testing.T, raw *sql.DB) bool {
	t.Helper()
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='parsed_calls'`).Scan(&n); err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	return n > 0
}

func quietJanitor(st *store.Store) *Janitor {
	return New(st, slog.New(slog.NewTextHandler(io.Discard, nil)), Windows{}, time.Hour)
}

// Run drives the drain to the end on its own and waits for it before Wait
// returns, so the store is never closed under a batch.
func TestRunDrainsParsedCalls(t *testing.T) {
	st, raw := openWithLegacyParsedCalls(t)
	j := quietJanitor(st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go j.Run(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for j.DrainStatus().State != DrainDone {
		if time.Now().After(deadline) {
			t.Fatalf("drain not done within 10s: %+v", j.DrainStatus())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if parsedCallsExists(t, raw) {
		t.Error("drain reported done but parsed_calls still exists")
	}
	if s := j.DrainStatus(); s.Rows != 6 || s.StartedAtMS == 0 || s.DoneAtMS < s.StartedAtMS {
		t.Errorf("status = %+v, want 6 rows and start <= done", s)
	}
	cancel()
	j.Wait()
}

// Before Run the drain is pending, not done: "done" is a claim that the table
// is gone, and nothing has checked yet.
func TestDrainStatusBeforeRun(t *testing.T) {
	st, _ := openWithLegacyParsedCalls(t)
	if s := quietJanitor(st).DrainStatus(); s.State != DrainPending || s.Rows != 0 {
		t.Errorf("status = %+v, want pending with no rows", s)
	}
}

// While busy the drain deletes nothing, and a shutdown during its pause returns
// promptly instead of sleeping the pause out.
func TestDrainWaitsWhileBusy(t *testing.T) {
	st, raw := openWithLegacyParsedCalls(t)
	j := quietJanitor(st)
	var asked atomic.Int32
	j.PauseDrainWhen(func() bool { asked.Add(1); return true })

	ctx, cancel := context.WithCancel(context.Background())
	go j.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for asked.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	waited := make(chan struct{})
	go func() { j.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel while the drain was paused")
	}

	if asked.Load() == 0 {
		t.Fatal("the drain never consulted busy")
	}
	if s := j.DrainStatus(); s.State != DrainPaused || s.Rows != 0 {
		t.Errorf("status = %+v, want paused with no rows", s)
	}
	var rows int
	if err := raw.QueryRow(`SELECT count(*) FROM parsed_calls`).Scan(&rows); err != nil || rows != 6 {
		t.Errorf("parsed_calls rows = %d (%v), want all 6 kept while busy", rows, err)
	}
}
