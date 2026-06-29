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
func appendBareCall(t *testing.T, s *Store) int64 {
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
		t.Errorf("CallID = %d, want %d", got.CallID, callID)
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
	if _, err := s.GetPayload(999); !errors.Is(err, ErrNotFound) {
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

	got, err := s.HasPayloads([]int64{withTrace, without, 12345})
	if err != nil {
		t.Fatalf("HasPayloads: %v", err)
	}
	if !got[withTrace] {
		t.Errorf("expected has_trace true for %d", withTrace)
	}
	if got[without] {
		t.Errorf("expected has_trace false for %d", without)
	}
	if got[12345] {
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
