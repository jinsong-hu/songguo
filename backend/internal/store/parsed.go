package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ParsedCall is the structured analysis view for a call, stored 1:1 with
// calls.id. Data is the JSON-encoded parse.Call (the store stays agnostic of
// its shape); Format names the parser that produced it.
type ParsedCall struct {
	CallID    string
	Format    string
	Data      []byte
	CreatedAt time.Time
}

// SaveParsedCall upserts the parsed view for a call (keyed by call_id). It is
// written off the hot path by the async parse pipeline; callers log failures
// rather than surfacing them. Safe to call concurrently (shared *sql.DB).
func (s *Store) SaveParsedCall(p ParsedCall) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	created := p.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	data := p.Data
	if len(data) == 0 {
		data = []byte("{}")
	}
	if _, err := s.db.Exec(
		`INSERT OR REPLACE INTO parsed_calls (call_id, format, data, created_at)
		 VALUES (?, ?, ?, ?)`,
		p.CallID, p.Format, string(data), created.UnixMilli(),
	); err != nil {
		return fmt.Errorf("store: save parsed call: %w", err)
	}
	return nil
}

// SaveMessageFingerprint records a call's message-shape fingerprint (see
// parse.Fingerprint) on its calls row. Written by the async parse pipeline, off
// the hot path, and best-effort like SaveParsedCall: a failure costs redundant
// body reads in the session view later, never a wrong conversation.
//
// A zero count writes nothing. The columns then stay at their "unknown"
// defaults, which messageCover treats as not-provably-redundant — so a call
// whose fingerprint never landed is read rather than skipped.
//
// The UPDATE is keyed on a call that is already in the ledger. A missing row
// (pruned between the call finishing and this running) affects zero rows, which
// is not an error.
// Takes primitives rather than a parse.Fingerprint so the store keeps knowing
// nothing about the parse package's shapes, exactly as SaveParsedCall does with
// its opaque Data blob.
func (s *Store) SaveMessageFingerprint(callID string, count int, head, tail []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if callID == "" || count <= 0 {
		return nil
	}
	if _, err := s.db.Exec(
		`UPDATE calls SET msg_count = ?, msg_head = ?, msg_tail = ? WHERE id = ?`,
		count, head, tail, callID,
	); err != nil {
		return fmt.Errorf("store: save message fingerprint: %w", err)
	}
	return nil
}

// GetParsedCall returns the parsed view for a call, or ErrNotFound if the call
// has not been parsed (yet, or at all).
func (s *Store) GetParsedCall(callID string) (ParsedCall, error) {
	row := s.db.QueryRow(
		`SELECT call_id, format, data, created_at FROM parsed_calls WHERE call_id = ?`,
		callID,
	)
	var (
		p             ParsedCall
		data          string
		createdMillis int64
	)
	err := row.Scan(&p.CallID, &p.Format, &data, &createdMillis)
	if errors.Is(err, sql.ErrNoRows) {
		return ParsedCall{}, fmt.Errorf("store: parsed call %s: %w", callID, ErrNotFound)
	}
	if err != nil {
		return ParsedCall{}, fmt.Errorf("store: get parsed call: %w", err)
	}
	p.Data = []byte(data)
	p.CreatedAt = time.UnixMilli(createdMillis)
	return p, nil
}
