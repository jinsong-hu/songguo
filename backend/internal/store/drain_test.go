package store

import (
	"context"
	"fmt"
	"testing"
)

// createLegacyParsedCalls recreates the retired table as an existing production
// database still has it, with rows to drain.
func createLegacyParsedCalls(t *testing.T, s *Store, rows int) {
	t.Helper()
	if _, err := s.db.Exec(`CREATE TABLE parsed_calls (
		call_id    TEXT PRIMARY KEY,
		format     TEXT NOT NULL DEFAULT '',
		data       TEXT NOT NULL DEFAULT '{}',
		created_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create parsed_calls: %v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := s.db.Exec(`INSERT INTO parsed_calls (call_id, created_at) VALUES (?, 0)`, fmt.Sprint("c", i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

func TestParsedCallsIsNotCreated(t *testing.T) {
	s := openTestStore(t)
	if exists, err := s.tableExists("parsed_calls"); err != nil || exists {
		t.Fatalf("parsed_calls exists = %v (%v) on a fresh database, want it retired", exists, err)
	}
}

// The drain deletes at most a batch per call, oldest first, and drops the
// table only once it is empty.
func TestDrainParsedCallsEmptiesThenDrops(t *testing.T) {
	s := openTestStore(t)
	createLegacyParsedCalls(t, s, 9)
	ctx := context.Background()

	var deleted int64
	for i := 0; ; i++ {
		n, done, err := s.DrainParsedCalls(ctx, 4)
		if err != nil {
			t.Fatalf("DrainParsedCalls: %v", err)
		}
		if n > 4 {
			t.Fatalf("one batch deleted %d rows, want at most 4", n)
		}
		if exists, _ := s.tableExists("parsed_calls"); done == exists {
			t.Fatalf("done = %v but table exists = %v", done, exists)
		}
		deleted += n
		if done {
			break
		}
		if i > 10 {
			t.Fatal("drain did not finish")
		}
	}
	if deleted != 9 {
		t.Errorf("deleted %d rows, want 9", deleted)
	}

	if n, done, err := s.DrainParsedCalls(ctx, 4); err != nil || !done || n != 0 {
		t.Errorf("drain after drop = %d, %v, %v; want 0, done, nil", n, done, err)
	}
}
