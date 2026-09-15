package store

import "fmt"

// SaveMessageFingerprint records a call's message-shape fingerprint (see
// parse.Fingerprint) on its calls row. Written by the async parse pipeline, off
// the hot path, and best-effort: a failure costs redundant body reads in the
// session view later, never a wrong conversation.
//
// A zero count writes nothing. The columns then stay at their "unknown"
// defaults, which messageCover treats as not-provably-redundant — so a call
// whose fingerprint never landed is read rather than skipped.
//
// The UPDATE is keyed on a call that is already in the ledger. A missing row
// (pruned between the call finishing and this running) affects zero rows, which
// is not an error.
// Takes primitives rather than a parse.Fingerprint so the store keeps knowing
// nothing about the parse package's shapes.
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
