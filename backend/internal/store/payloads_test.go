package store

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

// appendBareCall inserts a minimal call row and returns its id, so payload
// tests have valid foreign keys to attach to.
func appendBareCall(t *testing.T, s *Store) string {
	t.Helper()
	id, err := s.AppendCall(calls.Entry{UserID: "tok", Model: "m", Status: 200})
	if err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	return id
}

func TestPayloadRoundTrip(t *testing.T) {
	s := openTestStore(t)
	callID := appendBareCall(t, s)

	// Include raw (non-UTF8) binary bytes to prove BLOB storage is byte-exact.
	reqBody := []byte{0x00, 0x01, 0xff, 0xfe, 'a', 'b'}
	respBody := []byte(`{"choices":[{"message":{"content":"hi"}}]}`)

	in := Payload{
		CallID:          callID,
		ReqHeaders:      map[string]string{"Content-Type": "application/json", "X-Trace": "1"},
		ReqBody:         reqBody,
		ReqContentType:  "application/json",
		RespHeaders:     map[string]string{"Content-Type": "application/json"},
		RespBody:        respBody,
		RespContentType: "application/json",
		CreatedAt:       time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC),
	}
	if err := s.SavePayload(in); err != nil {
		t.Fatalf("SavePayload: %v", err)
	}

	got, err := s.GetPayload(callID)
	if err != nil {
		t.Fatalf("GetPayload: %v", err)
	}
	if got.CallID != callID {
		t.Errorf("CallID = %q, want %q", got.CallID, callID)
	}
	if !bytes.Equal(got.ReqBody, reqBody) {
		t.Errorf("ReqBody = %v, want %v (binary must round-trip)", got.ReqBody, reqBody)
	}
	if !bytes.Equal(got.RespBody, respBody) {
		t.Errorf("RespBody = %q, want %q", got.RespBody, respBody)
	}
	if got.ReqContentType != "application/json" {
		t.Errorf("ReqContentType = %q", got.ReqContentType)
	}
	if got.ReqHeaders["X-Trace"] != "1" || got.ReqHeaders["Content-Type"] != "application/json" {
		t.Errorf("ReqHeaders round-trip = %v", got.ReqHeaders)
	}
	if got.RespHeaders["Content-Type"] != "application/json" {
		t.Errorf("RespHeaders round-trip = %v", got.RespHeaders)
	}
	if !got.CreatedAt.Equal(in.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, in.CreatedAt)
	}

	// INSERT OR REPLACE: a second save for the same call_id overwrites in place.
	in.RespContentType = "text/plain"
	if err := s.SavePayload(in); err != nil {
		t.Fatalf("SavePayload (replace): %v", err)
	}
	got2, err := s.GetPayload(callID)
	if err != nil {
		t.Fatalf("GetPayload (after replace): %v", err)
	}
	if got2.RespContentType != "text/plain" {
		t.Errorf("after replace RespContentType = %q, want text/plain", got2.RespContentType)
	}
}

func TestGetPayloadNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetPayload("nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPayload(missing) err = %v, want ErrNotFound", err)
	}
}

func TestHasPayloads(t *testing.T) {
	s := openTestStore(t)
	withTrace := appendBareCall(t, s)
	without := appendBareCall(t, s)
	if err := s.SavePayload(Payload{CallID: withTrace, ReqBody: []byte("x")}); err != nil {
		t.Fatalf("SavePayload: %v", err)
	}

	got, err := s.HasPayloads([]string{withTrace, without, "nonexistent"})
	if err != nil {
		t.Fatalf("HasPayloads: %v", err)
	}
	if !got[withTrace] {
		t.Errorf("expected has_trace true for %q", withTrace)
	}
	if got[without] {
		t.Errorf("expected has_trace false for %q", without)
	}
	if got["nonexistent"] {
		t.Error("expected has_trace false for nonexistent id")
	}

	// Empty input -> empty, non-nil map, no error.
	empty, err := s.HasPayloads(nil)
	if err != nil {
		t.Fatalf("HasPayloads(nil): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("HasPayloads(nil) = %v, want empty map", empty)
	}
}

func TestSessionRequests(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	id1, err := s.AppendCall(calls.Entry{
		TS: base, SessionID: "sess", Wire: "openai/responses", Status: 200,
	})
	if err != nil {
		t.Fatalf("AppendCall 1: %v", err)
	}
	id2, err := s.AppendCall(calls.Entry{
		TS: base.Add(time.Minute), SessionID: "sess", Wire: "anthropic/messages", Status: 200,
	})
	if err != nil {
		t.Fatalf("AppendCall 2: %v", err)
	}
	other, err := s.AppendCall(calls.Entry{
		TS: base.Add(2 * time.Minute), SessionID: "other", Wire: "openai/chat", Status: 200,
	})
	if err != nil {
		t.Fatalf("AppendCall other: %v", err)
	}

	for _, payload := range []Payload{
		{CallID: id2, ReqHeaders: map[string]string{"Content-Encoding": "gzip"}, ReqBody: []byte("second"), ReqContentType: "application/json", RespBody: []byte("large response")},
		{CallID: id1, ReqHeaders: map[string]string{"X-Test": "first"}, ReqBody: []byte("first"), ReqContentType: "application/json", RespBody: []byte("large response")},
		{CallID: other, ReqBody: []byte("other")},
	} {
		if err := s.SavePayload(payload); err != nil {
			t.Fatalf("SavePayload(%s): %v", payload.CallID, err)
		}
	}

	got, err := s.SessionRequests("sess")
	if err != nil {
		t.Fatalf("SessionRequests: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("SessionRequests len = %d, want 2", len(got))
	}
	if got[0].CallID != id1 || string(got[0].ReqBody) != "first" || got[0].ReqHeaders["X-Test"] != "first" {
		t.Errorf("first request = %+v, want call %q", got[0], id1)
	}
	if got[1].CallID != id2 || got[1].Wire != "anthropic/messages" || got[1].ReqHeaders["Content-Encoding"] != "gzip" {
		t.Errorf("second request = %+v, want call %q", got[1], id2)
	}

	empty, err := s.SessionRequests("missing")
	if err != nil {
		t.Fatalf("SessionRequests(missing): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("SessionRequests(missing) = %+v, want empty", empty)
	}
}

// The test above has no fingerprints on its rows, so it exercises the FALLBACK
// (unknown shapes are never treated as redundant) rather than the optimization.
// This one fingerprints a growing conversation end to end and asserts the whole
// path — SQL, cover, body fetch — returns only the last body.
//
// Without this, every existing SessionRequests test would keep passing if the
// cover silently degraded to reading everything, since that IS the fallback.
func TestSessionRequestsReadsOnlyTheCoveringBodies(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)

	// Three turns of one growing conversation, then a compaction that replaces
	// the history, then one more turn on the new context.
	turns := []struct {
		body  string
		count int
		head  string
		tail  string
	}{
		{"turn1", 3, "A", "t3"},
		{"turn2", 5, "A", "t5"},
		{"turn3", 7, "A", "t7"},
		{"turn4", 3, "S", "u3"}, // compacted: summary replaces the history
		{"turn5", 4, "S", "u4"},
	}

	var ids []string
	for i, turn := range turns {
		id, err := s.AppendCall(calls.Entry{
			TS: base.Add(time.Duration(i) * time.Minute), SessionID: "grow",
			Wire: "anthropic/messages", Status: 200,
		})
		if err != nil {
			t.Fatalf("AppendCall %d: %v", i, err)
		}
		if err := s.SavePayload(Payload{CallID: id, ReqBody: []byte(turn.body)}); err != nil {
			t.Fatalf("SavePayload %d: %v", i, err)
		}
		if err := s.SaveMessageFingerprint(id, turn.count, []byte(turn.head), []byte(turn.tail)); err != nil {
			t.Fatalf("SaveMessageFingerprint %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	got, err := s.SessionRequests("grow")
	if err != nil {
		t.Fatalf("SessionRequests: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d bodies, want 2 (last of each run); got %+v", len(got), got)
	}
	if got[0].CallID != ids[2] || string(got[0].ReqBody) != "turn3" {
		t.Errorf("first covering body = %+v, want turn3 (last before compaction)", got[0])
	}
	if got[1].CallID != ids[4] || string(got[1].ReqBody) != "turn5" {
		t.Errorf("second covering body = %+v, want turn5 (last of the new run)", got[1])
	}
}
