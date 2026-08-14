package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// busyFixture opens a store plus a second handle on the same file whose
// busy_timeout is 0, then takes the write lock on the store side. Any write
// through the returned handle therefore fails immediately with a real
// SQLITE_BUSY from the driver, rather than after the 5s the store's own DSN would
// wait. Fabricating the error is not an option — sqlite.Error's fields are
// unexported and it has no constructor — and a hand-rolled stand-in would not
// prove IsBusy recognizes what this driver actually returns.
func busyFixture(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })

	// WAL takes the write lock on the first write in the transaction, not at
	// BEGIN, so the INSERT is what actually holds it. Left open for the test.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.Exec(
		`INSERT INTO user_spend (user_id, spend, updated_at) VALUES ('holder', 1, 1)`); err != nil {
		t.Fatalf("take write lock: %v", err)
	}
	return other
}

// The whole point of IsBusy is that it fires on what the driver really produces
// under contention, so this drives a genuine lock conflict rather than a stub.
func TestIsBusyRecognizesRealLockContention(t *testing.T) {
	other := busyFixture(t)

	_, err := other.Exec(`INSERT INTO user_spend (user_id, spend, updated_at) VALUES ('rival', 2, 2)`)
	if err == nil {
		t.Fatal("second writer succeeded while the write lock was held; the fixture is not contending")
	}
	if !IsBusy(err) {
		t.Errorf("IsBusy(%v) = false, want true — a write that lost the lock must be classified retryable", err)
	}

	// Store methods return their errors wrapped ("store: insert provider: %w"), so
	// the classifier has to traverse the chain, not just type-assert the top.
	wrapped := fmt.Errorf("store: insert provider: %w", err)
	if !IsBusy(wrapped) {
		t.Errorf("IsBusy did not see through %%w wrapping: %v", wrapped)
	}
}

// IsBusy must be narrow. If it matched any driver error, the API layer would
// answer 503 "retry in a few seconds" to permanent failures — telling the
// operator to wait for something that will never resolve.
func TestIsBusyRejectsNonContentionErrors(t *testing.T) {
	if IsBusy(nil) {
		t.Error("IsBusy(nil) = true, want false")
	}
	if IsBusy(errors.New("store: something else went wrong")) {
		t.Error("IsBusy(plain error) = true, want false")
	}

	// A real sqlite error with a different result code: the duplicate primary key
	// is SQLITE_CONSTRAINT, permanent and not retryable.
	s := openTestStore(t)
	const ins = `INSERT INTO user_spend (user_id, spend, updated_at) VALUES ('dup', 1, 1)`
	if _, err := s.db.Exec(ins); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := s.db.Exec(ins)
	if err == nil {
		t.Fatal("duplicate primary key insert succeeded, want a constraint error")
	}
	if IsBusy(err) {
		t.Errorf("IsBusy(%v) = true, want false — a constraint violation is not lock contention", err)
	}
}
