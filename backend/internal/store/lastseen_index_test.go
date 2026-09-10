package store

import (
	"strings"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

// LastSeenByUser runs once PER USER on every load of the users list, so its
// per-user cost is multiplied by the user count — in production that was ~25 s
// for one page.
//
// The fix is an index, not a rewrite, and the distinction the plan has to show
// is narrow enough to regress silently. With idx_calls_user_id alone SQLite
// still SEARCHes — it finds the user's rows by index — and then reads every one
// of them from the table to compare ts. That reads table pages scattered
// through a file far larger than the page cache, which is the actual cost. Only
// when ts is IN the index can SQLite seek straight to the end of the user's
// range, and the plan says so by calling the index COVERING.
//
// So asserting "not a full scan" would pass on the slow plan. This asserts the
// covering index by name.
func TestLastSeenByUserUsesCoveringIndex(t *testing.T) {
	s := openTestStore(t)

	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT MAX(ts) FROM calls WHERE user_id = ?`, "u1")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("EXPLAIN QUERY PLAN returned no rows")
	}

	joined := strings.Join(plan, "\n  ")
	if !strings.Contains(joined, "COVERING INDEX idx_calls_user_ts") {
		t.Errorf("last-seen lookup does not use the covering (user_id, ts) index, "+
			"so it reads a table row per call to find the max.\nplan:\n  %s", joined)
	}
}

// And the index must not change the answer: last seen is the newest call's ts,
// per user, and users with no calls have none.
func TestLastSeenByUserValue(t *testing.T) {
	s := openTestStore(t)

	base := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for _, e := range []calls.Entry{
		{TS: base, UserID: "u1", Status: 200},
		{TS: base.Add(2 * time.Hour), UserID: "u1", Status: 200},
		{TS: base.Add(time.Hour), UserID: "u1", Status: 200},
		{TS: base.Add(9 * time.Hour), UserID: "u2", Status: 200},
	} {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.LastSeenByUser("u1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Equal(base.Add(2*time.Hour)) {
		t.Errorf("u1 last seen = %v, want %v", got, base.Add(2*time.Hour))
	}

	none, err := s.LastSeenByUser("nobody")
	if err != nil {
		t.Fatal(err)
	}
	if none != nil {
		t.Errorf("unknown user last seen = %v, want nil", none)
	}
}
