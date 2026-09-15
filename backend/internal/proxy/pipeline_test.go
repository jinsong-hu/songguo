package proxy

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/parse"
	"github.com/songguo/songguo/internal/store"
)

// TestParsePipelineStampsFingerprints submits two turns of one growing
// conversation, drains via Close(), and checks the session cover collapses them
// to the later body — which it can only do once both calls carry a fingerprint.
// Unfingerprinted, the cover keeps both.
func TestParsePipelineStampsFingerprints(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	bodies := []string{
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"more"}]}`,
	}
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	p := newParsePipeline(st, nil, 1, 8)
	var ids []string
	for i, body := range bodies {
		id, err := st.AppendCall(calls.Entry{TS: base.Add(time.Duration(i) * time.Minute), SessionID: "sess", Model: "gpt-4o", Wire: "openai/chat"})
		if err != nil {
			t.Fatalf("AppendCall: %v", err)
		}
		if err := st.SavePayload(store.Payload{CallID: id, ReqBody: []byte(body)}); err != nil {
			t.Fatalf("SavePayload: %v", err)
		}
		p.submit(parseJob{callID: id, in: parse.Input{Wire: "openai/chat", ReqBody: []byte(body)}})
		ids = append(ids, id)
	}
	p.Close() // drains in-flight jobs

	cover, err := st.SessionCover("sess")
	if err != nil {
		t.Fatalf("SessionCover: %v", err)
	}
	if len(cover) != 1 || cover[0].CallID != ids[1] {
		t.Fatalf("cover = %+v, want only the later turn (fingerprints stamped)", cover)
	}
}

// TestParsePipelineSubmitNeverBlocks fills the queue past capacity; submit must
// return immediately (dropping overflow) rather than block the caller.
func TestParsePipelineSubmitNeverBlocks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// No workers started: the queue cannot drain, so submits beyond capacity
	// must be dropped, not block.
	p := &parsePipeline{jobs: make(chan parseJob, 2), store: st, logger: slog.Default()}
	for i := 0; i < 50; i++ {
		p.submit(parseJob{callID: fmt.Sprintf("call-%d", i)})
	}
}
