package store

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/catalog"
	sqlitedriver "modernc.org/sqlite"
)

var slowRawDeletes atomic.Int64

func init() {
	sqlitedriver.MustRegisterDeterministicScalarFunction(
		"songguo_test_slow_raw_delete",
		0,
		func(_ *sqlitedriver.FunctionContext, _ []driver.Value) (driver.Value, error) {
			slowRawDeletes.Add(1)
			time.Sleep(4 * time.Millisecond)
			return int64(1), nil
		},
	)
}

// The janitor, ledger, spend tracker and price feed all mutate the same SQLite
// file. This drives a deliberately slow raw prune while the three live writers
// arrive together. They must wait at the application gate and then all commit;
// none may lose its write to SQLITE_BUSY. The small raw batch and yield also
// require live writes to make progress before the complete prune finishes.
func TestJanitorAndRuntimeMutatorsShareWriteGate(t *testing.T) {
	s := openTestStore(t)
	old := time.Now().Add(-48 * time.Hour)
	const expired = 64
	for i := 0; i < expired; i++ {
		id := fmt.Sprintf("expired-%d", i)
		if err := s.CreateCall(calls.Entry{ID: id, TS: old, UserID: "u1"}); err != nil {
			t.Fatalf("CreateCall %s: %v", id, err)
		}
		if err := s.SavePayload(Payload{CallID: id, ReqBody: []byte("req"), RespBody: []byte("resp"), CreatedAt: old}); err != nil {
			t.Fatalf("SavePayload %s: %v", id, err)
		}
	}
	if _, err := s.db.Exec(`
		CREATE TRIGGER slow_raw_delete
		BEFORE DELETE ON raw
		BEGIN
			SELECT songguo_test_slow_raw_delete();
		END`); err != nil {
		t.Fatalf("create slow-delete trigger: %v", err)
	}
	t.Cleanup(func() { _, _ = s.db.Exec(`DROP TRIGGER slow_raw_delete`) })

	slowRawDeletes.Store(0)
	pruneDone := make(chan struct{})
	var pruned int64
	var pruneErr error
	go func() {
		pruned, pruneErr = s.PruneRaw(context.Background(), time.Now().Add(-24*time.Hour))
		close(pruneDone)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for slowRawDeletes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if slowRawDeletes.Load() == 0 {
		t.Fatal("slow raw-delete trigger did not run; prune did not start")
	}

	type result struct {
		name string
		err  error
	}
	results := make(chan result, 3)
	go func() {
		results <- result{"ledger", s.CreateCall(calls.Entry{ID: "live", TS: time.Now(), UserID: "u1"})}
	}()
	go func() {
		results <- result{"spend", s.SaveSpend("u1", 1)}
	}()
	go func() {
		results <- result{"price", s.ReplaceFeedPrices([]FeedPrice{{
			ProviderID: "openai",
			Model:      "gpt-5",
			Cost:       catalog.Cost{Input: 1, Output: 2},
			FetchedAt:  time.Now(),
		}}, time.Now())}
	}()

	// The live writes should be handed the gate at a batch boundary, not wait
	// for all four raw batches. If this times out, retention is still starving
	// runtime writes even though each DELETE is bounded.
	liveDone := make(chan struct{})
	go func() {
		for i := 0; i < 3; i++ {
			r := <-results
			if r.err != nil {
				t.Errorf("%s write failed: %v", r.name, r.err)
			}
		}
		close(liveDone)
	}()
	select {
	case <-liveDone:
	case <-pruneDone:
		t.Fatal("raw prune completed before live writes; test did not exercise interleaving")
	case <-time.After(3 * time.Second):
		t.Fatal("live writes did not make progress while raw prune was still running")
	}

	select {
	case <-pruneDone:
	case <-time.After(5 * time.Second):
		t.Fatal("raw prune did not finish")
	}
	if pruneErr != nil {
		t.Fatalf("PruneRaw: %v", pruneErr)
	}
	if pruned != expired {
		t.Fatalf("PruneRaw deleted %d rows, want %d", pruned, expired)
	}

	var callsLeft int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE id = 'live'`).Scan(&callsLeft); err != nil {
		t.Fatalf("check live call: %v", err)
	}
	if callsLeft != 1 {
		t.Fatalf("live call rows = %d, want 1", callsLeft)
	}
	var spend float64
	if err := s.db.QueryRow(`SELECT spend FROM user_spend WHERE user_id = 'u1'`).Scan(&spend); err != nil {
		t.Fatalf("check spend: %v", err)
	}
	if spend != 1 {
		t.Fatalf("spend = %v, want 1", spend)
	}
	var prices int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM feed_prices WHERE provider_id = 'openai' AND model = 'gpt-5'`).Scan(&prices); err != nil {
		t.Fatalf("check feed price: %v", err)
	}
	if prices != 1 {
		t.Fatalf("feed price rows = %d, want 1", prices)
	}
}

// This is the deterministic ordering half of the same contract: while the
// janitor owns the write gate, every other mutator must wait rather than race
// for a separate database/sql connection. Releasing the gate lets all four
// operations finish without any driver-level busy error.
func TestMutatorsWaitForJanitorGate(t *testing.T) {
	s := openTestStore(t)
	old := time.Now().Add(-48 * time.Hour)
	if err := s.CreateCall(calls.Entry{ID: "expired", TS: old, UserID: "u1"}); err != nil {
		t.Fatalf("CreateCall: %v", err)
	}
	if err := s.SavePayload(Payload{CallID: "expired", CreatedAt: old}); err != nil {
		t.Fatalf("SavePayload: %v", err)
	}

	s.writeMu.Lock()
	type result struct {
		name string
		err  error
	}
	results := make(chan result, 4)
	go func() {
		_, err := s.PruneRaw(context.Background(), time.Now().Add(-24*time.Hour))
		results <- result{"janitor", err}
	}()
	go func() {
		results <- result{"ledger", s.CreateCall(calls.Entry{ID: "live", TS: time.Now(), UserID: "u1"})}
	}()
	go func() { results <- result{"spend", s.SaveSpend("u1", 1)} }()
	go func() {
		results <- result{"price", s.ReplaceFeedPrices([]FeedPrice{{
			ProviderID: "openai", Model: "gpt-5", Cost: catalog.Cost{Input: 1}, FetchedAt: time.Now(),
		}}, time.Now())}
	}()
	select {
	case r := <-results:
		s.writeMu.Unlock()
		t.Fatalf("%s completed while janitor gate was held: %v", r.name, r.err)
	case <-time.After(25 * time.Millisecond):
	}
	s.writeMu.Unlock()

	for i := 0; i < 4; i++ {
		r := <-results
		if r.err != nil {
			t.Errorf("%s failed after gate release: %v", r.name, r.err)
		}
	}
}
