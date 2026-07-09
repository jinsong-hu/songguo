package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/parse"
	"github.com/songguo/songguo/internal/store"
)

// TestParsePipelinePersists submits a job, drains via Close(), then verifies the
// structured parse was stored against the call row.
func TestParsePipelinePersists(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	id, err := st.AppendCall(calls.Entry{Model: "gpt-4o", Vendor: "openai", Wire: "openai/chat"})
	if err != nil {
		t.Fatalf("AppendCall: %v", err)
	}

	p := newParsePipeline(st, nil, 1, 8)
	p.submit(parseJob{
		callID: id,
		in: parse.Input{
			Wire:     "openai/chat",
			ReqBody:  []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
			RespBody: []byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`),
		},
	})
	p.Close() // drains in-flight jobs

	pc, err := st.GetParsedCall(id)
	if err != nil {
		t.Fatalf("GetParsedCall: %v", err)
	}
	if pc.Format != "openai-chat" {
		t.Errorf("format = %q", pc.Format)
	}
	var c parse.Call
	if err := json.Unmarshal(pc.Data, &c); err != nil {
		t.Fatalf("unmarshal stored data: %v", err)
	}
	if c.Output[0].Text != "hello" || c.FinishReason != "stop" || c.Tokens.Input != 3 {
		t.Errorf("parsed call = %+v", c)
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
