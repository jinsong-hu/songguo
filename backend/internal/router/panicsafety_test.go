package router

import (
	"testing"
	"time"

	"github.com/songguo/songguo/internal/config"
)

// A panic while ranking must not leave the router's mutex held.
//
// rankVendors runs on the hot path for every request through the gateway. It
// used to Lock, run ~30 lines, and Unlock at the end with no defer — correct
// only for as long as nothing in between ever panicked. net/http recovers
// panics per connection, so the process would SURVIVE one while r.mu stayed
// locked forever: every subsequent Select blocks on it and the gateway stops
// serving entirely, with no crash and no log to explain it.
//
// The injected Rand here stands in for any panic in that region — the point is
// the lock discipline, not this particular trigger.
func TestPanicWhileRankingDoesNotStrandTheLock(t *testing.T) {
	snap := singleVendorSnapshot(t)
	boom := true
	r := New(func() *config.Snapshot { return snap }, Options{
		Rand: func() float64 {
			if boom {
				panic("boom")
			}
			return 0.5
		},
	})

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the injected panic to propagate")
			}
		}()
		_, _ = r.Select(Selector{Scope: ScopeAll})
	}()

	// The gateway is still up (net/http would have recovered). If the mutex
	// leaked, this second request blocks forever.
	boom = false
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := r.Select(Selector{Scope: ScopeAll}); err != nil {
			t.Errorf("Select after a recovered panic: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Select blocked after a panic — r.mu was left locked, " +
			"which is a silent total outage requiring a restart")
	}
}

// singleVendorSnapshot builds the smallest snapshot Select will route against.
func singleVendorSnapshot(t *testing.T) *config.Snapshot {
	t.Helper()
	snap, err := config.Parse([]byte(`
vendors:
  - name: vendorA
    origin: http://127.0.0.1:1/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/chat]
    credential: {id: credA, api_key: k}
`))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return snap
}
