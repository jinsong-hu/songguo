package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/store"
)

// Load counts a request for as long as songguo holds it — through the upstream
// wait, not just until the handler has read the body — and gives back exactly
// what it took, so the status page's in-flight and buffered figures return to
// zero instead of drifting.
func TestLoadCountsARequestUntilItEnds(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		close(arrived)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer mock.Close()

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, singleVendorYAML(mock.URL, "vendorA", "credA", "sk")), st)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	done := make(chan *http.Response, 1)
	go func() { done <- env.post(t, "/v1/chat/completions", key, body) }()

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the upstream")
	}
	if got, want := env.handler.Load(), (Load{InFlight: 1, Started: 1, BufferedBytes: int64(len(body))}); got != want {
		t.Errorf("while upstream holds it: load = %+v, want %+v", got, want)
	}

	close(release)
	resp := <-done
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The handler returns just after the client sees the end of the body.
	deadline := time.Now().Add(5 * time.Second)
	for env.handler.Load().InFlight != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got, want := env.handler.Load(), (Load{Started: 1}); got != want {
		t.Errorf("after it ends: load = %+v, want %+v", got, want)
	}

	// A refused request is still a request the gateway handled.
	resp = env.post(t, "/v1/chat/completions", "not-a-key", body)
	resp.Body.Close()
	if got := env.handler.Load(); got.Started != 2 || got.InFlight != 0 {
		t.Errorf("after a refusal: load = %+v, want 2 started and none in flight", got)
	}
}
