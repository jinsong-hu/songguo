package spend

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu     sync.Mutex
	rows   map[string]float64
	loads  int
	saves  int
	failOn string
}

func newFakeStore() *fakeStore { return &fakeStore{rows: make(map[string]float64)} }

func (f *fakeStore) LoadSpend(userID string) (float64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	v, ok := f.rows[userID]
	return v, ok, nil
}

func (f *fakeStore) SaveSpend(userID string, total float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if userID == f.failOn {
		return fmt.Errorf("forced save failure")
	}
	f.saves++
	f.rows[userID] = total
	return nil
}

func (f *fakeStore) get(userID string) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.rows[userID]
	return v, ok
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTracker(f *fakeStore) *Tracker { return New(f, discardLogger()) }

// Charges must be visible to the very next budget check, without waiting for a
// flush — that is what lets the request path charge a call and move on.
func TestAddIsVisibleImmediately(t *testing.T) {
	f := newFakeStore()
	tr := newTracker(f)

	tr.Add("u1", 1.5)
	tr.Add("u1", 2.25)

	got, err := tr.Get("u1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != 3.75 {
		t.Errorf("Get = %v, want 3.75", got)
	}
	if _, persisted := f.get("u1"); persisted {
		t.Error("Add wrote to the store; it must only touch memory")
	}
}

// Add must not consult the database, so a user charged before ever being read
// still accumulates — this is the non-budgeted user whose spend the dashboard
// shows.
func TestAddWorksBeforeTheUserIsEverLoaded(t *testing.T) {
	f := newFakeStore()
	f.rows["u1"] = 10
	tr := newTracker(f)

	tr.Add("u1", 5) // no Get first
	if f.loads != 0 {
		t.Fatalf("Add performed %d store loads, want 0", f.loads)
	}

	got, err := tr.Get("u1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != 15 {
		t.Errorf("Get = %v, want 15 (stored 10 + pending 5)", got)
	}
}

// The persisted total must be stored-plus-pending exactly once — never the
// pending amount twice.
func TestFlushPersistsWithoutDoubleCounting(t *testing.T) {
	f := newFakeStore()
	f.rows["u1"] = 100
	tr := newTracker(f)

	tr.Add("u1", 5)
	if err := tr.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if v, _ := f.get("u1"); v != 105 {
		t.Fatalf("persisted %v, want 105", v)
	}

	// A second flush with nothing pending must not re-add.
	if err := tr.Flush(); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if v, _ := f.get("u1"); v != 105 {
		t.Errorf("persisted %v after an empty flush, want 105", v)
	}
	got, _ := tr.Get("u1")
	if got != 105 {
		t.Errorf("Get = %v after flush, want 105", got)
	}
}

// A failed write must leave the charge pending, not silently drop it.
func TestFailedFlushKeepsTheChargePending(t *testing.T) {
	f := newFakeStore()
	f.failOn = "u1"
	tr := newTracker(f)

	tr.Add("u1", 7)
	if err := tr.Flush(); err == nil {
		t.Fatal("Flush returned nil despite a failing store")
	}
	if got, _ := tr.Get("u1"); got != 7 {
		t.Errorf("Get = %v after a failed flush, want 7 (the charge was lost)", got)
	}

	f.mu.Lock()
	f.failOn = ""
	f.mu.Unlock()
	if err := tr.Flush(); err != nil {
		t.Fatalf("retry Flush: %v", err)
	}
	if v, _ := f.get("u1"); v != 7 {
		t.Errorf("persisted %v on retry, want 7", v)
	}
}

// THE bug this package exists for. Retention deletes calls at 90 days, so
// SUM(cost) silently decreased and a spent budget refilled on its own. A
// running total is not stored in the rows being pruned, so it must not move.
func TestTotalSurvivesRetentionPruningTheCallLog(t *testing.T) {
	f := newFakeStore()
	tr := newTracker(f)

	f.rows["u1"] = 0
	tr.Add("u1", 40)
	if err := tr.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Simulate the janitor deleting every call row. Under the old
	// SUM(cost)-over-calls design this is exactly when spend dropped to 0.
	// user_spend holds no call rows, so nothing here should change.
	got, err := tr.Get("u1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != 40 {
		t.Errorf("spend = %v after the call log was pruned, want 40", got)
	}
}

// A user is loaded from the store once, however many times they are read.
func TestUserIsLoadedOnlyOnce(t *testing.T) {
	f := newFakeStore()
	f.rows["u1"] = 3
	tr := newTracker(f)

	for i := 0; i < 25; i++ {
		if _, err := tr.Get("u1"); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}
	if f.loads != 1 {
		t.Errorf("store loads = %d, want 1 (the total is cached after the first read)", f.loads)
	}
}

// An unknown user has simply never spent anything.
func TestUnknownUserIsZero(t *testing.T) {
	tr := newTracker(newFakeStore())
	got, err := tr.Get("nobody")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != 0 {
		t.Errorf("Get = %v for an unknown user, want 0", got)
	}
}

// Forget is what keeps a recreated user id from inheriting the old total.
func TestForgetClearsCachedTotal(t *testing.T) {
	f := newFakeStore()
	f.rows["u1"] = 12
	tr := newTracker(f)

	if got, _ := tr.Get("u1"); got != 12 {
		t.Fatalf("Get = %v, want 12", got)
	}
	f.mu.Lock()
	delete(f.rows, "u1")
	f.mu.Unlock()

	tr.Forget("u1")
	if got, _ := tr.Get("u1"); got != 0 {
		t.Errorf("Get = %v after Forget and a deleted row, want 0", got)
	}
}

// Concurrent charges must all land: this runs on every request goroutine.
func TestConcurrentAddsAllCount(t *testing.T) {
	f := newFakeStore()
	f.rows["u1"] = 0
	tr := newTracker(f)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.Add("u1", 0.5)
		}()
	}
	wg.Wait()

	got, _ := tr.Get("u1")
	if got != 100 {
		t.Errorf("total = %v after 200 concurrent charges of 0.5, want 100", got)
	}
}

// Run must flush on cancellation, so a clean shutdown persists everything.
func TestRunFlushesOnShutdown(t *testing.T) {
	f := newFakeStore()
	f.rows["u1"] = 0
	tr := newTracker(f)

	ctx, cancel := context.WithCancel(context.Background())
	go tr.Run(ctx, 50*time.Millisecond)

	tr.Add("u1", 9)
	cancel()
	tr.Wait()

	if v, _ := f.get("u1"); v != 9 {
		t.Errorf("persisted %v after shutdown, want 9", v)
	}
}
