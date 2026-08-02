package ledger

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/compose"
	"github.com/songguo/songguo/internal/store"
)

// fakeStore records the order writes arrive in, and can be made slow or made to
// fail, so the writer's behaviour is testable without a database.
type fakeStore struct {
	mu      sync.Mutex
	order   []string
	created map[string]bool

	delay     time.Duration
	failEvery func(op string) bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{created: make(map[string]bool)}
}

func (f *fakeStore) record(op string) error {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEvery != nil && f.failEvery(op) {
		return fmt.Errorf("forced failure on %s", op)
	}
	f.order = append(f.order, op)
	return nil
}

func (f *fakeStore) CreateCall(e calls.Entry) error {
	f.mu.Lock()
	f.created[e.ID] = true
	f.mu.Unlock()
	return f.record("create:" + e.ID)
}

func (f *fakeStore) FinalizeCall(e calls.Entry) error {
	f.mu.Lock()
	ok := f.created[e.ID]
	f.mu.Unlock()
	if !ok {
		// This is the failure the single writer exists to prevent: an UPDATE
		// keyed by id that matches nothing, silently discarding the outcome.
		return fmt.Errorf("finalize before create for %s", e.ID)
	}
	return f.record("finalize:" + e.ID)
}

func (f *fakeStore) SavePayload(p store.Payload) error {
	f.mu.Lock()
	ok := f.created[p.CallID]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("payload before create for %s (FK violation)", p.CallID)
	}
	return f.record("payload:" + p.CallID)
}

func (f *fakeStore) SaveComposition(callID string, _ compose.Composition) error {
	return f.record("composition:" + callID)
}

func (f *fakeStore) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

// Ordering is the whole reason for a single writer: FinalizeCall is an UPDATE
// keyed by id, so a finalize that overtakes its create matches no row and the
// call's entire outcome is lost — silently, since the UPDATE succeeds.
func TestCreateAlwaysPrecedesFinalize(t *testing.T) {
	fs := newFakeStore()
	w := New(fs, discardLogger(), 0)

	const nCalls = 200
	for i := 0; i < nCalls; i++ {
		id := fmt.Sprintf("call-%d", i)
		w.Submit(Op{Kind: KindCreate, Entry: entry(id)})
		w.Submit(Op{Kind: KindFinalize, Entry: entry(id)})
		w.Submit(Op{Kind: KindPayload, CallID: id, Payload: &store.Payload{CallID: id}})
	}
	w.Close()

	seen := map[string]bool{}
	for _, op := range fs.snapshot() {
		seen[op] = true
	}
	for i := 0; i < nCalls; i++ {
		id := fmt.Sprintf("call-%d", i)
		for _, want := range []string{"create:" + id, "finalize:" + id, "payload:" + id} {
			if !seen[want] {
				t.Fatalf("missing %q — an op was reordered or dropped", want)
			}
		}
	}
	if n := len(fs.snapshot()); n != nCalls*3 {
		t.Errorf("wrote %d ops, want %d", n, nCalls*3)
	}
}

// Submitting from many goroutines at once must not interleave one call's ops
// with another's in a way that breaks the create-before-everything rule.
func TestOrderingHoldsUnderConcurrentSubmit(t *testing.T) {
	fs := newFakeStore()
	w := New(fs, discardLogger(), 0)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("c%d", i)
			// A real request submits these from one goroutine in this order.
			w.Submit(Op{Kind: KindCreate, Entry: entry(id)})
			w.Submit(Op{Kind: KindFinalize, Entry: entry(id)})
		}(i)
	}
	wg.Wait()
	w.Close()

	if n := len(fs.snapshot()); n != 128 {
		t.Errorf("wrote %d ops, want 128 (a finalize was rejected for arriving first)", n)
	}
}

// The defining property versus the other forks: saturation must never discard a
// call record. Submit blocks instead, and says so in Stats.
func TestFullQueueBlocksAndNeverDrops(t *testing.T) {
	fs := newFakeStore()
	fs.delay = 2 * time.Millisecond // writer slower than the producer
	w := New(fs, discardLogger(), 4)

	const total = 60
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			w.Submit(Op{Kind: KindCreate, Entry: entry(fmt.Sprintf("c%d", i))})
		}
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("submit never returned")
	}
	w.Close()

	if n := len(fs.snapshot()); n != total {
		t.Errorf("wrote %d ops, want %d — the queue dropped records under load", n, total)
	}
	st := w.Stats()
	if st.Blocked == 0 {
		t.Error("Stats.Blocked = 0, want > 0: a queue of 4 against 60 slow writes must have blocked")
	}
	if st.Written != total {
		t.Errorf("Stats.Written = %d, want %d", st.Written, total)
	}
}

// After runs only when the write succeeded, because it is how work that
// requires the row to exist (the parse pipeline, whose table has a FOREIGN KEY
// onto calls) is sequenced behind it.
func TestAfterRunsOnlyAfterASuccessfulWrite(t *testing.T) {
	fs := newFakeStore()
	fs.failEvery = func(op string) bool { return op == "create:bad" }
	w := New(fs, discardLogger(), 0)

	var good, bad atomic.Int64
	w.Submit(Op{Kind: KindCreate, Entry: entry("bad"), After: func() { bad.Add(1) }})
	w.Submit(Op{Kind: KindCreate, Entry: entry("good"), After: func() { good.Add(1) }})
	w.Close()

	if bad.Load() != 0 {
		t.Error("After ran for a write that failed; dependent work would hit a missing row")
	}
	if good.Load() != 1 {
		t.Errorf("After ran %d times for a successful write, want 1", good.Load())
	}
	if st := w.Stats(); st.Failed != 1 {
		t.Errorf("Stats.Failed = %d, want 1", st.Failed)
	}
}

// A failed write must not stall the ops behind it: they belong to other calls.
func TestOneFailureDoesNotBlockTheQueue(t *testing.T) {
	fs := newFakeStore()
	fs.failEvery = func(op string) bool { return op == "create:boom" }
	w := New(fs, discardLogger(), 0)

	w.Submit(Op{Kind: KindCreate, Entry: entry("boom")})
	for i := 0; i < 5; i++ {
		w.Submit(Op{Kind: KindCreate, Entry: entry(fmt.Sprintf("ok%d", i))})
	}
	w.Close()

	if n := len(fs.snapshot()); n != 5 {
		t.Errorf("wrote %d ops after a failure, want 5", n)
	}
}

// Close is the shutdown contract: everything already queued reaches the store.
// Without it a SIGTERM would drop call records, not just a derived rollup.
func TestCloseDrainsEverythingQueued(t *testing.T) {
	fs := newFakeStore()
	fs.delay = time.Millisecond
	w := New(fs, discardLogger(), 0)

	const n = 50
	for i := 0; i < n; i++ {
		w.Submit(Op{Kind: KindCreate, Entry: entry(fmt.Sprintf("c%d", i))})
	}
	w.Close()

	if got := len(fs.snapshot()); got != n {
		t.Errorf("wrote %d of %d queued ops; Close did not drain", got, n)
	}
}

// Flush is the read-your-writes barrier the tests and admin API use. It must
// wait for prior ops without shutting the writer down.
func TestFlushWaitsForPriorOpsAndLeavesWriterUsable(t *testing.T) {
	fs := newFakeStore()
	fs.delay = time.Millisecond
	w := New(fs, discardLogger(), 0)
	defer w.Close()

	for i := 0; i < 20; i++ {
		w.Submit(Op{Kind: KindCreate, Entry: entry(fmt.Sprintf("a%d", i))})
	}
	w.Flush()
	if got := len(fs.snapshot()); got != 20 {
		t.Fatalf("after Flush, wrote %d of 20", got)
	}

	w.Submit(Op{Kind: KindCreate, Entry: entry("later")})
	w.Flush()
	if got := len(fs.snapshot()); got != 21 {
		t.Errorf("writer unusable after Flush: wrote %d, want 21", got)
	}
}

// Depth should read ~0 in steady state; HighWater is what survives to show a
// burst happened at all.
func TestStatsReportCapacityAndHighWater(t *testing.T) {
	fs := newFakeStore()
	fs.delay = time.Millisecond
	w := New(fs, discardLogger(), 128)

	for i := 0; i < 40; i++ {
		w.Submit(Op{Kind: KindCreate, Entry: entry(fmt.Sprintf("c%d", i))})
	}
	high := w.Stats().HighWater
	w.Close()

	st := w.Stats()
	if st.Capacity != 128 {
		t.Errorf("Capacity = %d, want 128", st.Capacity)
	}
	if high == 0 {
		t.Error("HighWater = 0 after queueing 40 ops against a slow writer")
	}
	if st.Depth != 0 {
		t.Errorf("Depth = %d after Close, want 0", st.Depth)
	}
	if st.Written != 40 {
		t.Errorf("Written = %d, want 40", st.Written)
	}
}

// A nil writer is a no-op rather than a panic, matching the other forks.
func TestNilWriterIsSafe(t *testing.T) {
	var w *Writer
	w.Submit(Op{Kind: KindCreate})
	w.Flush()
	w.Close()
	if st := w.Stats(); st.Capacity != 0 {
		t.Errorf("nil writer Stats = %+v, want zero value", st)
	}
}

func entry(id string) calls.Entry { return calls.Entry{ID: id, UserID: "u1"} }

// discardLogger keeps the deliberate failure tests from printing alarming
// errors during an otherwise passing run.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
