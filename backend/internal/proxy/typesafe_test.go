package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/songguo/songguo/internal/store"
)

// TestSystemOneForwardsAndMeters walks a System One call through the whole
// proxy: endpoint match, credential swap, verbatim forward, metering, pricing.
//
// The body assertion is the one that matters. System One's payload is a state
// plus a map of typed questions — a shape nothing else here speaks — so it is
// exactly the kind of body a well-meaning normalizer would "fix" on the way
// past. It must arrive byte-identical.
//
// The cost assertion is the second: TypeSafe bills input tokens and gives
// output away, so a published output rate of zero has to contribute nothing
// rather than fall back to some other number.
func TestSystemOneForwardsAndMeters(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotAuth, gotBody = r.URL.Path, r.Header.Get("Authorization"), string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"jev-1.13.0","answers":{"is_urgent":{"type":"noul","noul":0.95}},"usage":{"input_tokens":296,"output_tokens":20}}`)
	}))
	defer up.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: typesafe
    origin: %s
    served_models: [jev-latest]
    priority: 1
    wires: [typesafe/systemone]
    credential: {id: credT, api_key: real-typesafe-key}
    prices:
      jev-latest: { cost: { input: 0.042, output: 0 } }
`, up.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	reqBody := `{"state":"Help! My payouts have been failing for 3 days.","model":"jev-latest","questions":{"is_urgent":{"type":"noul","instructions":"Does this convey urgency?"}}}`
	resp := env.post(t, "/v1/systemone", key, reqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if gotPath != "/v1/systemone" {
		t.Errorf("upstream path = %q, want /v1/systemone", gotPath)
	}
	if gotAuth != "Bearer real-typesafe-key" {
		t.Errorf("upstream auth = %q, want the vendor key as Bearer", gotAuth)
	}
	if gotBody != reqBody {
		t.Errorf("body was not forwarded verbatim:\n got %s\nwant %s", gotBody, reqBody)
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.Wire != "typesafe/systemone" {
		t.Errorf("wire = %q, want typesafe/systemone", got.Wire)
	}
	if got.Model != "jev-latest" {
		t.Errorf("model = %q, want jev-latest", got.Model)
	}
	if n := got.Usage["input_tokens"]; n != float64(296) {
		t.Errorf("usage input_tokens = %v, want 296", n)
	}
	// Input only: output is free, so the 20 output tokens must add nothing.
	if want := 296 * 0.042 / 1e6; !approxEqual(got.Cost, want) {
		t.Errorf("cost = %v, want %v (input axis alone)", got.Cost, want)
	}
}
