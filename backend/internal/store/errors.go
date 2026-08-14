package store

import (
	"errors"

	"modernc.org/sqlite"
)

// SQLite primary result codes for lock contention. These are part of SQLite's
// permanent public ABI, so they are named here rather than imported: the driver
// only exposes them from modernc.org/sqlite/lib, which is per-GOOS/GOARCH
// transpiled C and not something to take on as a direct dependency for two
// integers.
const (
	sqliteBusy   = 5 // SQLITE_BUSY
	sqliteLocked = 6 // SQLITE_LOCKED
)

// IsBusy reports whether err is SQLite's lock contention — the write lock was
// held by another connection for longer than busy_timeout (see dsnPragmas), so
// this attempt gave up. It is transient and the write did NOT happen: every
// store write is a single statement or one transaction, both of which leave
// nothing behind on failure. Retrying later is the correct response, which is
// what separates it from a genuine internal fault; the API layer uses this to
// answer 503 rather than 500 (see api.serverError).
//
// Both BUSY and LOCKED count. They differ in who holds the lock — another
// process versus another connection in this process — which the caller cannot
// act on differently.
func IsBusy(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	// Mask to the primary result code: SQLite ORs extension bits into the high
	// byte, so SQLITE_BUSY_SNAPSHOT (517) and SQLITE_BUSY_TIMEOUT (773) must
	// match too. Comparing against the bare 5 would silently miss them.
	switch se.Code() & 0xff {
	case sqliteBusy, sqliteLocked:
		return true
	}
	return false
}
