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
