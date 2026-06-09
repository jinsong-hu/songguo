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
	id, err := s.AppendCall(calls.Entry{TokenID: "tok", Model: "m", Status: 200})
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
		ReqTruncated:    true,
		RespHeaders:     map[string]string{"Content-Type": "application/json"},
		RespBody:        respBody,
		RespContentType: "application/json",
		RespTruncated:   false,
		CreatedAt:       time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC),
	}
	if err := s.SavePayload(in, 0); err != nil {
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
	if !got.ReqTruncated {
		t.Error("ReqTruncated = false, want true")
	}
	if got.RespTruncated {
		t.Error("RespTruncated = true, want false")
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
	if err := s.SavePayload(in, 0); err != nil {
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

func TestPayloadRetentionKeepsNewest(t *testing.T) {
	s := openTestStore(t)

	// Insert 5 payloads, retaining only the newest 2 on each save.
	const retain = 2
	var ids []int64
	for i := 0; i < 5; i++ {
		id := appendBareCall(t, s)
		ids = append(ids, id)
		if err := s.SavePayload(Payload{CallID: id, ReqBody: []byte("x")}, retain); err != nil {
			t.Fatalf("SavePayload[%d]: %v", i, err)
		}
	}

	// The two newest (highest call_id) must survive; older ones pruned.
	newest := ids[len(ids)-2:]
	for _, id := range newest {
		if _, err := s.GetPayload(id); err != nil {
			t.Errorf("expected newest payload %d to survive, got %v", id, err)
		}
	}
	for _, id := range ids[:len(ids)-2] {
		if _, err := s.GetPayload(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("expected old payload %d pruned, got err %v", id, err)
		}
	}

	// retain <= 0 disables pruning: a fresh store keeps everything.
	s2 := openTestStore(t)
	var keepAll []int64
	for i := 0; i < 4; i++ {
		id := appendBareCall(t, s2)
		keepAll = append(keepAll, id)
		if err := s2.SavePayload(Payload{CallID: id}, 0); err != nil {
			t.Fatalf("SavePayload (no prune)[%d]: %v", i, err)
		}
	}
	for _, id := range keepAll {
		if _, err := s2.GetPayload(id); err != nil {
			t.Errorf("retain=0 should keep payload %d, got %v", id, err)
		}
	}
}

func TestHasPayloads(t *testing.T) {
	s := openTestStore(t)
	withTrace := appendBareCall(t, s)
	without := appendBareCall(t, s)
	if err := s.SavePayload(Payload{CallID: withTrace, ReqBody: []byte("x")}, 0); err != nil {
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

func TestTokenCaptureOverride(t *testing.T) {
	s := openTestStore(t)

	// Default: capture is nil (inherit global).
	inherit, _, err := s.CreateToken(NewToken{Name: "inherit"})
	if err != nil {
		t.Fatalf("CreateToken(inherit): %v", err)
	}
	if inherit.Capture != nil {
		t.Errorf("default Capture = %v, want nil (inherit)", *inherit.Capture)
	}
	got, err := s.GetToken(inherit.ID)
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if got.Capture != nil {
		t.Errorf("GetToken Capture = %v, want nil", *got.Capture)
	}

	// Create with explicit capture=true.
	on := true
	created, _, err := s.CreateToken(NewToken{Name: "on", Capture: &on})
	if err != nil {
		t.Fatalf("CreateToken(on): %v", err)
	}
	if created.Capture == nil || !*created.Capture {
		t.Errorf("created Capture = %v, want true", created.Capture)
	}
	reloaded, err := s.GetToken(created.ID)
	if err != nil {
		t.Fatalf("GetToken(on): %v", err)
	}
	if reloaded.Capture == nil || !*reloaded.Capture {
		t.Errorf("reloaded Capture = %v, want true", reloaded.Capture)
	}

	// Update to capture=false on the inherit token.
	off := false
	upd, err := s.UpdateToken(inherit.ID, TokenUpdate{Capture: &off})
	if err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	if upd.Capture == nil || *upd.Capture {
		t.Errorf("after update Capture = %v, want false", upd.Capture)
	}

	// Updating a different field must leave capture unchanged.
	upd2, err := s.UpdateToken(created.ID, TokenUpdate{Name: ptrS("renamed")})
	if err != nil {
		t.Fatalf("UpdateToken(name only): %v", err)
	}
	if upd2.Capture == nil || !*upd2.Capture {
		t.Errorf("capture changed by unrelated update: %v", upd2.Capture)
	}
}
