package api

import (
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/songguo/songguo/internal/store"
)

// realBusyError returns a genuine SQLITE_BUSY from the driver by holding the
// write lock on one handle and writing through a second whose busy_timeout is 0.
// The error has to be real: sqlite.Error cannot be constructed from outside the
// driver, and a stand-in would not prove serverError classifies what production
// actually hands it.
func realBusyError(t *testing.T) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	tx, err := holder.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	// WAL takes the write lock on the first write, not at BEGIN.
	if _, err := tx.Exec(
		`INSERT INTO user_spend (user_id, spend, updated_at) VALUES ('holder', 1, 1)`); err != nil {
		t.Fatalf("take write lock: %v", err)
	}

	loser, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open loser: %v", err)
	}
	t.Cleanup(func() { _ = loser.Close() })
	_, err = loser.Exec(`INSERT INTO user_spend (user_id, spend, updated_at) VALUES ('rival', 2, 2)`)
	if err == nil {
		t.Fatal("second writer succeeded while the lock was held; fixture is not contending")
	}
	return err
}

// A write that lost the database lock is transient and changed nothing, so the
// operator is told to retry. Flattening it into the opaque 500 "internal error" is
// what sent someone to the container logs to find out that adding a provider had
// merely collided with the retention sweep.
func TestServerErrorAnswers503ForLockContention(t *testing.T) {
	a := &api{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()

	a.serverError(rec, "create provider", realBusyError(t))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want %q", got, "5")
	}

	var body errorBody
	decodeBody(t, rec, &body)
	if body.Error.Type != "songguo_db_busy" {
		t.Errorf("type = %q, want %q", body.Error.Type, "songguo_db_busy")
	}
	if body.Error.Message == "internal error" || body.Error.Message == "" {
		t.Errorf("message = %q, want something the operator can act on", body.Error.Message)
	}
}

// The carve-out must stay narrow: a genuine internal fault still gets the opaque
// 500, because its detail belongs in the log and not in the response.
func TestServerErrorStillHides500Details(t *testing.T) {
	a := &api{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()

	a.serverError(rec, "create provider", errors.New("no such column: s3cret-table.password"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Error("Retry-After set on a non-retryable 500")
	}
	var body errorBody
	decodeBody(t, rec, &body)
	if body.Error.Message != "internal error" {
		t.Errorf("message = %q, want %q", body.Error.Message, "internal error")
	}
	if body.Error.Type != "songguo_internal" {
		t.Errorf("type = %q, want %q", body.Error.Type, "songguo_internal")
	}
	if strings.Contains(rec.Body.String(), "s3cret-table") {
		t.Error("the underlying error text leaked into the response")
	}
}
