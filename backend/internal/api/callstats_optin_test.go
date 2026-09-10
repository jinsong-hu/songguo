package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/store"
)

// The three config endpoints — services, providers, vendors — describe what is
// CONFIGURED. Each of them used to also aggregate the whole `calls` table on
// every request, which in production cost 0.7-5.6 s for a field the SPA renders
// nowhere, on an endpoint (/api/vendors) the dashboard polls on a timer.
//
// The guard has two halves, and the second is the one that rots quietly:
//
//  1. Without ?stats=1 the key is ABSENT — not zeroed. A zero block would say
//     "0 requests, healthy", which is a claim about the ledger nobody made.
//     Testing the raw JSON rather than the decoded struct is deliberate: a
//     decoded zero value and an omitted key are indistinguishable in Go, and
//     the omission is the contract.
//  2. With ?stats=1 the numbers are still right, so "make it fast" cannot
//     quietly become "make it gone".
func TestCallStatsAreOptIn(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateProvider(store.NewProvider{
		Name: "openai", Enabled: true, Priority: 1, Weight: 1, APIKey: "sk-a",
		Models:    []store.ProviderModel{{Model: "gpt-4o"}},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://example.com/v1/chat/completions"}},
	}); err != nil {
		t.Fatal(err)
	}
	// One graded success and one graded failure, so a computed block is
	// distinguishable from a zeroed one on every field that matters.
	for _, st := range []int{200, 500} {
		if _, err := s.AppendCall(calls.Entry{
			TS: time.Now(), Vendor: "openai", Model: "gpt-4o", Status: st, LatencyMS: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	for _, path := range []string{"/api/services", "/api/providers", "/api/vendors"} {
		rec := do(h, "GET", path, "secret", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: code = %d", path, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, `"stats"`) {
			t.Errorf("%s without ?stats=1 carries a stats block: %s", path, body)
		}
	}

	// services: opted in, the per-model aggregate is real.
	rec := do(h, "GET", "/api/services?stats=1", "secret", nil)
	var services []serviceView
	decodeBody(t, rec, &services)
	if len(services) != 1 || services[0].Stats == nil {
		t.Fatalf("services with ?stats=1 = %+v", services)
	}
	if got := services[0].Stats.Requests; got != 2 {
		t.Errorf("service requests = %d, want 2", got)
	}

	// providers: opted in, the per-vendor aggregate is real. A vendor with an
	// error is unhealthy, which is the field a zeroed block would have inverted.
	rec = do(h, "GET", "/api/providers?stats=1", "secret", nil)
	var providers []providerView
	decodeBody(t, rec, &providers)
	if len(providers) != 1 || providers[0].Stats == nil {
		t.Fatalf("providers with ?stats=1 = %+v", providers)
	}
	if providers[0].Stats.Requests != 2 || providers[0].Stats.Healthy {
		t.Errorf("provider stats = %+v, want 2 requests and unhealthy", providers[0].Stats)
	}
}

// A consumer key gets no ledger aggregate even when it asks. sanitizeProviders
// ForUser drops the block rather than zeroing it, for the same reason as above:
// a redaction must not read as a measurement.
func TestProviderStatsStayHiddenFromConsumerKeys(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateProvider(store.NewProvider{
		Name: "openai", Enabled: true, Priority: 1, Weight: 1, APIKey: "sk-a",
		Models:    []store.ProviderModel{{Model: "gpt-4o"}},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://example.com/v1/chat/completions"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendCall(calls.Entry{
		TS: time.Now(), Vendor: "openai", Model: "gpt-4o", Status: 500, LatencyMS: 100,
	}); err != nil {
		t.Fatal(err)
	}
	u, key, err := s.CreateUser(store.NewUser{Name: "consumer"})
	if err != nil {
		t.Fatal(err)
	}
	_ = u

	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})
	rec := do(h, "GET", "/api/providers?stats=1", key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, `"stats"`) {
		t.Errorf("consumer key sees a stats block: %s", body)
	}

	// And the shape still decodes as the same type the SPA uses.
	var providers []providerView
	if err := json.Unmarshal(rec.Body.Bytes(), &providers); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(providers) != 1 || providers[0].Stats != nil {
		t.Fatalf("providers = %+v", providers)
	}
}
