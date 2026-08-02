package store

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

// The pragmas that matter are per-CONNECTION, and database/sql keeps a pool.
// Applying them with db.Exec after opening reaches exactly one connection and
// every later one runs with SQLite's defaults instead — busy_timeout 0 and
// foreign_keys off. This test holds several connections open at once, which
// forces the pool to create distinct ones, and checks each.
func TestPragmasApplyToEveryConnection(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Hold them all simultaneously so the pool cannot hand back the same
	// connection each time.
	const n = 4
	conns := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn %d: %v", i, err)
		}
		t.Cleanup(func() { c.Close() })
		conns = append(conns, c)
	}

	for i, c := range conns {
		var fk, busy, sync int
		var journal string
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatalf("conn %d: read foreign_keys: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
			t.Fatalf("conn %d: read busy_timeout: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&sync); err != nil {
			t.Fatalf("conn %d: read synchronous: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
			t.Fatalf("conn %d: read journal_mode: %v", i, err)
		}

		if fk != 1 {
			t.Errorf("conn %d: foreign_keys = %d, want 1 (cascade deletes silently skip without it)", i, fk)
		}
		if busy != 5000 {
			t.Errorf("conn %d: busy_timeout = %d, want 5000 (0 turns any write contention into an instant SQLITE_BUSY)", i, busy)
		}
		if sync != 1 {
			t.Errorf("conn %d: synchronous = %d, want 1 (NORMAL)", i, sync)
		}
		if journal != "wal" {
			t.Errorf("conn %d: journal_mode = %q, want %q", i, journal, "wal")
		}
	}
}

// A path containing '?' would be split by the driver at that character: the
// filename would be silently truncated and our pragmas parsed as part of the
// user's path. Better to refuse than to open the wrong database with the wrong
// settings.
func TestOpenRejectsPathWithQuestionMark(t *testing.T) {
	if _, err := Open(t.TempDir() + "/we?rd.db"); err == nil {
		t.Fatal("Open accepted a path containing '?', want an error")
	}
}

// The cascade is the reason foreign_keys has to be on for every connection:
// PruneCalls relies on it to drop each pruned call's captured bodies. With FKs
// off the parent row goes and the child rows are orphaned forever.
func TestPruneCallsCascadesToChildren(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)

	if err := s.CreateCall(calls.Entry{ID: "c1", TS: old, UserID: "u1"}); err != nil {
		t.Fatalf("CreateCall: %v", err)
	}
	if err := s.SavePayload(Payload{
		CallID: "c1", ReqBody: []byte("req"), RespBody: []byte("resp"), CreatedAt: old,
	}); err != nil {
		t.Fatalf("SavePayload: %v", err)
	}

	n, err := s.PruneCalls(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PruneCalls: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d calls, want 1", n)
	}

	var orphans int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM raw WHERE call_id = 'c1'`).Scan(&orphans); err != nil {
		t.Fatalf("count raw: %v", err)
	}
	if orphans != 0 {
		t.Errorf("raw rows left after the parent call was pruned = %d, want 0 "+
			"(the ON DELETE CASCADE did not fire, so foreign_keys was off)", orphans)
	}
}

// Batched pruning must delete everything matching the cutoff, not just the
// first batch. Uses more rows than pruneBatch so the loop has to iterate.
func TestPruneDeletesBeyondASingleBatch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)

	total := pruneBatch + 37
	for i := 0; i < total; i++ {
		if err := s.CreateCall(calls.Entry{ID: callID(i), TS: old, UserID: "u1"}); err != nil {
			t.Fatalf("CreateCall %d: %v", i, err)
		}
	}
	// One recent row that must survive.
	if err := s.CreateCall(calls.Entry{ID: "keep", TS: time.Now(), UserID: "u1"}); err != nil {
		t.Fatalf("CreateCall keep: %v", err)
	}

	n, err := s.PruneCalls(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PruneCalls: %v", err)
	}
	if n != int64(total) {
		t.Errorf("pruned %d rows, want %d (the batch loop stopped early)", n, total)
	}

	var left int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM calls`).Scan(&left); err != nil {
		t.Fatalf("count calls: %v", err)
	}
	if left != 1 {
		t.Errorf("calls remaining = %d, want 1 (only the recent row)", left)
	}
}

// A cancelled sweep stops promptly instead of running to completion, and says
// so. The rows it did not reach are simply pruned by the next sweep.
func TestPruneStopsOnCancel(t *testing.T) {
	s := openTestStore(t)
	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 10; i++ {
		if err := s.CreateCall(calls.Entry{ID: callID(i), TS: old, UserID: "u1"}); err != nil {
			t.Fatalf("CreateCall %d: %v", i, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.PruneCalls(ctx, time.Now().Add(-24*time.Hour)); err == nil {
		t.Fatal("PruneCalls on a cancelled context returned nil, want ctx.Err()")
	}

	var left int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM calls`).Scan(&left); err != nil {
		t.Fatalf("count calls: %v", err)
	}
	if left != 10 {
		t.Errorf("calls remaining = %d, want 10 (cancel should stop before any batch)", left)
	}
}

func callID(i int) string { return "call-" + strconv.Itoa(i) }
