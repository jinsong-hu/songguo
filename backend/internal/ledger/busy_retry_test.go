package ledger

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/compose"
	"github.com/songguo/songguo/internal/store"
)

// busyError opens a real SQLite lock conflict. The ledger must classify and
// retry this driver error rather than rely on a hand-written error string.
func busyError(t *testing.T) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	holder, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.Exec(`CREATE TABLE t (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	tx, err := holder.Begin()
	if err != nil {
		t.Fatalf("begin holder transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.Exec(`INSERT INTO t (id) VALUES ('holder')`); err != nil {
		t.Fatalf("take write lock: %v", err)
	}

	rival, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open rival: %v", err)
	}
	t.Cleanup(func() { _ = rival.Close() })
	_, err = rival.Exec(`INSERT INTO t (id) VALUES ('rival')`)
	if err == nil {
		t.Fatal("rival write succeeded while holder transaction was open")
	}
	if !store.IsBusy(err) {
		t.Fatalf("driver error %v is not classified as SQLite contention", err)
	}
	return err
}

type busyOnceStore struct {
	mu       sync.Mutex
	inner    *fakeStore
	busyErr  error
	attempts int
}

func (s *busyOnceStore) CreateCall(e calls.Entry) error {
	s.mu.Lock()
	if e.ID == "retry" && s.attempts == 0 {
		s.attempts++
		s.mu.Unlock()
		return s.busyErr
	}
	s.mu.Unlock()
	return s.inner.CreateCall(e)
}

func (s *busyOnceStore) FinalizeCall(e calls.Entry) error {
	return s.inner.FinalizeCall(e)
}

func (s *busyOnceStore) SavePayload(p store.Payload) error {
	return s.inner.SavePayload(p)
}

func (s *busyOnceStore) SaveComposition(id string, c compose.Composition) error {
	return s.inner.SaveComposition(id, c)
}

func TestBusyRetryPreservesCreateFinalizeChildOrder(t *testing.T) {
	// Keep the real driver error alive while the writer retries it.
	busy := busyError(t)
	fs := newFakeStore()
	bs := &busyOnceStore{inner: fs, busyErr: busy}
	w := New(bs, slog.New(slog.NewTextHandler(io.Discard, nil)), 16)

	w.Submit(Op{Kind: KindCreate, Entry: entry("retry")})
	w.Submit(Op{Kind: KindFinalize, Entry: entry("retry")})
	w.Submit(Op{Kind: KindPayload, CallID: "retry", Payload: &store.Payload{CallID: "retry"}})
	w.Close()

	if got := bs.attempts; got != 1 {
		t.Fatalf("busy create attempts = %d, want one failed attempt before retry", got)
	}
	want := []string{"create:retry", "finalize:retry", "payload:retry"}
	if got := fs.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("successful order = %v, want %v", got, want)
	}
	if st := w.Stats(); st.Failed != 0 || st.Written != 3 {
		t.Fatalf("writer stats = %+v, want no final failure and three writes", st)
	}
}

func TestBusyUpsertDoesNotRepeatCommittedCreate(t *testing.T) {
	busy := busyError(t)
	fs := newFakeStore()
	bs := &busyFinalizeStore{inner: fs, busyErr: busy}
	w := New(bs, slog.New(slog.NewTextHandler(io.Discard, nil)), 16)

	w.Submit(Op{Kind: KindUpsert, Entry: entry("upsert")})
	w.Close()

	if bs.attempts != 1 {
		t.Fatalf("busy finalize attempts = %d, want one failed attempt before retry", bs.attempts)
	}
	want := []string{"create:upsert", "finalize:upsert"}
	if got := fs.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("successful order = %v, want %v", got, want)
	}
}

type busyFinalizeStore struct {
	inner    *fakeStore
	busyErr  error
	attempts int
}

func (s *busyFinalizeStore) CreateCall(e calls.Entry) error {
	return s.inner.CreateCall(e)
}

func (s *busyFinalizeStore) FinalizeCall(e calls.Entry) error {
	if s.attempts == 0 {
		s.attempts++
		return s.busyErr
	}
	return s.inner.FinalizeCall(e)
}

func (s *busyFinalizeStore) SavePayload(p store.Payload) error {
	return s.inner.SavePayload(p)
}

func (s *busyFinalizeStore) SaveComposition(id string, c compose.Composition) error {
	return s.inner.SaveComposition(id, c)
}

func TestBusyRetryIsBoundedAtQueueHead(t *testing.T) {
	busy := busyError(t)
	fs := newFakeStore()
	bs := &headBusyStore{inner: fs, busyErr: busy}
	w := New(bs, slog.New(slog.NewTextHandler(io.Discard, nil)), 16)

	w.Submit(Op{Kind: KindCreate, Entry: entry("stuck")})
	w.Submit(Op{Kind: KindCreate, Entry: entry("after")})
	done := make(chan struct{})
	go func() {
		w.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("busy retry did not remain bounded")
	}
	if bs.calls != busyRetryAttempts {
		t.Fatalf("busy attempts = %d, want %d", bs.calls, busyRetryAttempts)
	}
	if got := fs.snapshot(); fmt.Sprint(got) != "[create:after]" {
		t.Fatalf("writes after permanently busy head = %v, want the next op to proceed after the bounded retry", got)
	}
}

type headBusyStore struct {
	inner   *fakeStore
	busyErr error
	calls   int
}

func (s *headBusyStore) CreateCall(e calls.Entry) error {
	if e.ID == "stuck" {
		s.calls++
		return s.busyErr
	}
	return s.inner.CreateCall(e)
}
func (s *headBusyStore) FinalizeCall(e calls.Entry) error { return s.inner.FinalizeCall(e) }
func (s *headBusyStore) SavePayload(p store.Payload) error {
	return s.inner.SavePayload(p)
}
func (s *headBusyStore) SaveComposition(id string, c compose.Composition) error {
	return s.inner.SaveComposition(id, c)
}
