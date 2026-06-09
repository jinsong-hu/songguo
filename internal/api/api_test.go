package api

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/store"
)

// rawAPIKey is a sentinel credential value; tests assert it never appears in
// any /api/vendors response body.
const rawAPIKey = "sk-SUPER-SECRET-VENDOR-KEY-do-not-leak-12345"

const testConfigYAML = `
settings:
  listen: ":8080"
vendors:
  - name: openai
    base_url: https://api.openai.com
    served_models: [gpt-4o, text-embedding-3-small]
    priority: 1
    weight: 2
    credentials:
      - id: openai-key-1
        api_key: ` + rawAPIKey + `
    prices:
      gpt-4o:                  { input: 2.50, output: 10.00, unit: per_1m_tokens }
      text-embedding-3-small: { input: 0.02, output: 0,     unit: per_1m_tokens }
  - name: deepseek
    base_url: https://api.deepseek.com
    served_models: [deepseek-chat]
    priority: 2
    credentials:
      - id: deepseek-key-1
        api_key: sk-another-secret
    prices:
      deepseek-chat: { input: 0.27, output: 1.10, unit: per_1m_tokens }
`

func mustSnapshot(t *testing.T, yaml string) *config.Snapshot {
	t.Helper()
	snap, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return snap
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "api_test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// testDeps returns a Deps with sensible defaults; callers tweak fields.
func testHandler(t *testing.T, d Deps) http.Handler {
	t.Helper()
	if d.Store == nil {
		d.Store = newTestStore(t)
	}
	if d.Snapshot == nil {
		snap := mustSnapshot(t, testConfigYAML)
		d.Snapshot = func() *config.Snapshot { return snap }
	}
	if d.Now == nil {
		fixed := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
		d.Now = func() time.Time { return fixed }
	}
	return NewHandler(d)
}

// do issues a request to h and returns the recorder.
func do(h http.Handler, method, target, adminKey string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	if adminKey != "" {
		req.Header.Set("Authorization", "Bearer "+adminKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
}

// --- (a) auth ---

func TestAuthRequiredOnAllEndpoints(t *testing.T) {
	h := testHandler(t, Deps{AdminKey: "secret"})

	endpoints := []struct{ method, path string }{
		{"GET", "/api/overview"},
		{"GET", "/api/usage/series"},
		{"GET", "/api/calls"},
		{"GET", "/api/calls/export?format=csv"},
		{"GET", "/api/tokens"},
		{"POST", "/api/tokens"},
		{"PATCH", "/api/tokens/x"},
		{"POST", "/api/tokens/x/revoke"},
		{"GET", "/api/vendors"},
		{"POST", "/api/vendors/openai/test"},
		{"GET", "/api/settings"},
		{"GET", "/api/pricing"},
	}

	for _, ep := range endpoints {
		// No key -> 401.
		rec := do(h, ep.method, ep.path, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s no key: code = %d, want 401", ep.method, ep.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s %s 401 content-type = %q", ep.method, ep.path, ct)
		}
		// Wrong key -> 401.
		rec = do(h, ep.method, ep.path, "wrong", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s wrong key: code = %d, want 401", ep.method, ep.path, rec.Code)
		}
	}
}

func TestAuthAllowsWithCorrectKey(t *testing.T) {
	h := testHandler(t, Deps{AdminKey: "secret"})
	rec := do(h, "GET", "/api/overview", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview with key: code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestUnprotectedModeAllowsAll(t *testing.T) {
	h := testHandler(t, Deps{AdminKey: ""})
	rec := do(h, "GET", "/api/overview", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unprotected overview: code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// --- (b) tokens ---

func TestTokenCreateListPatchRevoke(t *testing.T) {
	s := newTestStore(t)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	// Create.
	body := `{"name":"app1","budget":10.0,"scope":["gpt-4o"],"rpm":60}`
	rec := do(h, "POST", "/api/tokens", "secret", strings.NewReader(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created tokenView
	decodeBody(t, rec, &created)
	if created.Key == "" {
		t.Fatal("create: plaintext key missing")
	}
	if !strings.HasPrefix(created.Key, "sg-") {
		t.Errorf("create: key = %q, want sg- prefix", created.Key)
	}
	if created.Budget == nil || *created.Budget != 10.0 {
		t.Errorf("create: budget = %v, want 10", created.Budget)
	}
	if !created.Active {
		t.Error("create: token should be active")
	}
	tokenID := created.ID
	plaintext := created.Key

	// Seed calls so list shows computed spend.
	if _, err := s.AppendCall(calls.Entry{
		TS: time.Now(), TokenID: tokenID, Model: "gpt-4o", Modality: calls.ModalityChat,
		Vendor: "openai", Status: 200, Cost: 3.25,
	}); err != nil {
		t.Fatalf("AppendCall: %v", err)
	}

	// List shows it with spent computed.
	rec = do(h, "GET", "/api/tokens", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: code = %d", rec.Code)
	}
	var list []tokenView
	decodeBody(t, rec, &list)
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	if list[0].Spent != 3.25 {
		t.Errorf("list spent = %v, want 3.25", list[0].Spent)
	}
	// List must NOT include the plaintext key.
	if strings.Contains(rec.Body.String(), plaintext) {
		t.Error("list leaked the plaintext key")
	}

	// Patch name + rpm; budget unchanged.
	rec = do(h, "PATCH", "/api/tokens/"+tokenID, "secret", strings.NewReader(`{"name":"renamed","rpm":120}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var patched tokenView
	decodeBody(t, rec, &patched)
	if patched.Name != "renamed" {
		t.Errorf("patch name = %q, want renamed", patched.Name)
	}
	if patched.RPM != 120 {
		t.Errorf("patch rpm = %d, want 120", patched.RPM)
	}
	if patched.Budget == nil || *patched.Budget != 10.0 {
		t.Errorf("patch budget should be unchanged, got %v", patched.Budget)
	}

	// Patch unknown id -> 404.
	rec = do(h, "PATCH", "/api/tokens/nope", "secret", strings.NewReader(`{"name":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Errorf("patch unknown: code = %d, want 404", rec.Code)
	}

	// Revoke flips active.
	rec = do(h, "POST", "/api/tokens/"+tokenID+"/revoke", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: code = %d", rec.Code)
	}
	var revoked tokenView
	decodeBody(t, rec, &revoked)
	if revoked.Active {
		t.Error("revoke: token should be inactive")
	}
	if revoked.RevokedAt == nil {
		t.Error("revoke: revoked_at should be set")
	}

	// Revoke unknown -> 404.
	rec = do(h, "POST", "/api/tokens/nope/revoke", "secret", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("revoke unknown: code = %d, want 404", rec.Code)
	}
}

func TestCreateTokenValidation(t *testing.T) {
	h := testHandler(t, Deps{AdminKey: "secret"})
	// Missing name.
	rec := do(h, "POST", "/api/tokens", "secret", strings.NewReader(`{}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing name: code = %d, want 400", rec.Code)
	}
	// Bad JSON.
	rec = do(h, "POST", "/api/tokens", "secret", strings.NewReader(`{not json`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: code = %d, want 400", rec.Code)
	}
}

func TestCreateTokenNullBudget(t *testing.T) {
	h := testHandler(t, Deps{AdminKey: "secret"})
	rec := do(h, "POST", "/api/tokens", "secret", strings.NewReader(`{"name":"free"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var v tokenView
	decodeBody(t, rec, &v)
	if v.Budget != nil {
		t.Errorf("budget = %v, want null", v.Budget)
	}
	if v.Scope == nil {
		t.Error("scope should serialize as [], not null")
	}
}

// --- (c) calls ---

func seedCalls(t *testing.T, s *store.Store) time.Time {
	t.Helper()
	base := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	entries := []calls.Entry{
		{TS: base, TokenID: "tokA", Model: "gpt-4o", Modality: calls.ModalityChat, Vendor: "openai", Status: 200, Cost: 0.10, LatencyMS: 100},
		{TS: base.Add(1 * time.Minute), TokenID: "tokA", Model: "gpt-4o", Modality: calls.ModalityChat, Vendor: "openai", Status: 500, Err: "boom", Cost: 0, LatencyMS: 200},
		{TS: base.Add(2 * time.Minute), TokenID: "tokB", Model: "text-embedding-3-small", Modality: calls.ModalityEmbedding, Vendor: "openai", Status: 200, Cost: 0.02, LatencyMS: 50},
		{TS: base.Add(3 * time.Minute), TokenID: "tokB", Model: "deepseek-chat", Modality: calls.ModalityChat, Vendor: "deepseek", Status: 200, Cost: 0.30, LatencyMS: 300},
	}
	for i, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}
	return base
}

func TestCallsFiltersAndPagination(t *testing.T) {
	s := newTestStore(t)
	seedCalls(t, s)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	// All.
	rec := do(h, "GET", "/api/calls", "secret", nil)
	var all callsView
	decodeBody(t, rec, &all)
	if all.Total != 4 || len(all.Entries) != 4 {
		t.Fatalf("all: total=%d entries=%d, want 4/4", all.Total, len(all.Entries))
	}
	if all.Limit != 50 || all.Offset != 0 {
		t.Errorf("defaults limit=%d offset=%d, want 50/0", all.Limit, all.Offset)
	}

	// Filter by token.
	rec = do(h, "GET", "/api/calls?token_id=tokA", "secret", nil)
	var byTok callsView
	decodeBody(t, rec, &byTok)
	if byTok.Total != 2 {
		t.Errorf("token_id=tokA total = %d, want 2", byTok.Total)
	}

	// Filter by model.
	rec = do(h, "GET", "/api/calls?model=deepseek-chat", "secret", nil)
	var byModel callsView
	decodeBody(t, rec, &byModel)
	if byModel.Total != 1 {
		t.Errorf("model filter total = %d, want 1", byModel.Total)
	}

	// Filter by vendor.
	rec = do(h, "GET", "/api/calls?vendor=openai", "secret", nil)
	var byVendor callsView
	decodeBody(t, rec, &byVendor)
	if byVendor.Total != 3 {
		t.Errorf("vendor filter total = %d, want 3", byVendor.Total)
	}

	// Filter by status.
	rec = do(h, "GET", "/api/calls?status=500", "secret", nil)
	var byStatus callsView
	decodeBody(t, rec, &byStatus)
	if byStatus.Total != 1 {
		t.Errorf("status filter total = %d, want 1", byStatus.Total)
	}

	// Pagination: limit=2 keeps total at 4 but returns 2 entries.
	rec = do(h, "GET", "/api/calls?limit=2&offset=0", "secret", nil)
	var page1 callsView
	decodeBody(t, rec, &page1)
	if page1.Total != 4 || len(page1.Entries) != 2 || page1.Limit != 2 {
		t.Errorf("page1: total=%d entries=%d limit=%d", page1.Total, len(page1.Entries), page1.Limit)
	}
	rec = do(h, "GET", "/api/calls?limit=2&offset=2", "secret", nil)
	var page2 callsView
	decodeBody(t, rec, &page2)
	if len(page2.Entries) != 2 || page2.Offset != 2 {
		t.Errorf("page2: entries=%d offset=%d", len(page2.Entries), page2.Offset)
	}
	if page1.Entries[0].ID == page2.Entries[0].ID {
		t.Error("pages overlap")
	}

	// Entry shape: ts is RFC3339, usage/tags are objects.
	e := all.Entries[0]
	if _, err := time.Parse(time.RFC3339, e.TS); err != nil {
		t.Errorf("entry ts not RFC3339: %q", e.TS)
	}
	if e.Usage == nil || e.Tags == nil {
		t.Error("usage/tags should serialize as objects, not null")
	}
}

func TestCallsExportCSV(t *testing.T) {
	s := newTestStore(t)
	seedCalls(t, s)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	rec := do(h, "GET", "/api/calls/export?format=csv", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export csv: code = %d", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("missing attachment disposition: %q", cd)
	}
	r := csv.NewReader(bytes.NewReader(rec.Body.Bytes()))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(records) != 5 { // header + 4 rows
		t.Fatalf("csv rows = %d, want 5 (header+4)", len(records))
	}
	if records[0][0] != "ts" || records[0][len(records[0])-1] != "err" {
		t.Errorf("csv header = %v", records[0])
	}
}

func TestCallsExportJSON(t *testing.T) {
	s := newTestStore(t)
	seedCalls(t, s)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	rec := do(h, "GET", "/api/calls/export?format=json", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export json: code = %d", rec.Code)
	}
	var entries []entryView
	decodeBody(t, rec, &entries)
	if len(entries) != 4 {
		t.Fatalf("export json len = %d, want 4", len(entries))
	}
}

func TestCallsExportBadFormat(t *testing.T) {
	h := testHandler(t, Deps{AdminKey: "secret"})
	rec := do(h, "GET", "/api/calls/export?format=xml", "secret", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad format: code = %d, want 400", rec.Code)
	}
}

// --- (d) overview ---

func TestOverviewMath(t *testing.T) {
	s := newTestStore(t)

	// Create a budgeted token; seed its calls so runway math is exercised.
	budget := 100.0
	tok, _, err := s.CreateToken(store.NewToken{Name: "budgeted", Budget: &budget})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	// Spend within the last 7 days for daily_burn. Put 14.0 total over the week.
	weekEntries := []calls.Entry{
		{TS: now.Add(-24 * time.Hour), TokenID: tok.ID, Model: "gpt-4o", Modality: calls.ModalityChat, Vendor: "openai", Status: 200, Cost: 6.0, LatencyMS: 100},
		{TS: now.Add(-48 * time.Hour), TokenID: tok.ID, Model: "dall-e-3", Modality: calls.ModalityImage, Vendor: "openai", Status: 500, Cost: 1.0, LatencyMS: 300},
		{TS: now.Add(-72 * time.Hour), TokenID: tok.ID, Model: "tts-1", Modality: calls.ModalityTTS, Vendor: "openai", Status: 0, Cost: 7.0, LatencyMS: 200},
	}
	for i, e := range weekEntries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	h := testHandler(t, Deps{Store: s, AdminKey: "secret", Now: func() time.Time { return now }})
	rec := do(h, "GET", "/api/overview", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var ov overviewView
	decodeBody(t, rec, &ov)

	if !approxF(ov.TotalSpend, 14.0) {
		t.Errorf("total_spend = %v, want 14", ov.TotalSpend)
	}
	if !approxF(ov.SpendByModality["chat"], 6.0) {
		t.Errorf("spend_by_modality[chat] = %v, want 6", ov.SpendByModality["chat"])
	}
	if !approxF(ov.SpendByModality["image"], 1.0) {
		t.Errorf("spend_by_modality[image] = %v, want 1", ov.SpendByModality["image"])
	}
	if !approxF(ov.SpendByModality["tts"], 7.0) {
		t.Errorf("spend_by_modality[tts] = %v, want 7", ov.SpendByModality["tts"])
	}
	if ov.Requests != 3 {
		t.Errorf("requests = %d, want 3", ov.Requests)
	}
	if ov.Errors != 2 { // status 500 and 0
		t.Errorf("errors = %d, want 2", ov.Errors)
	}
	if !approxF(ov.ErrorRate, 2.0/3.0) {
		t.Errorf("error_rate = %v, want 0.666...", ov.ErrorRate)
	}
	// Latencies 100,200,300 -> p50=200, p95=300, p99=300.
	if ov.LatencyMS.P50 != 200 || ov.LatencyMS.P95 != 300 || ov.LatencyMS.P99 != 300 {
		t.Errorf("latency = %+v, want p50=200 p95=300 p99=300", ov.LatencyMS)
	}
	if ov.VendorsActive != 2 {
		t.Errorf("vendors_active = %d, want 2", ov.VendorsActive)
	}
	if ov.TokensActive != 1 {
		t.Errorf("tokens_active = %d, want 1", ov.TokensActive)
	}
	// daily_burn = 14 / 7 = 2.0.
	if !approxF(ov.DailyBurn, 2.0) {
		t.Errorf("daily_burn = %v, want 2", ov.DailyBurn)
	}
	// runway = remaining budget (100 - 14 = 86) / 2.0 = 43.
	if ov.RunwayDays == nil {
		t.Fatal("runway_days = nil, want a value")
	}
	if !approxF(*ov.RunwayDays, 43.0) {
		t.Errorf("runway_days = %v, want 43", *ov.RunwayDays)
	}
}

func TestOverviewNullRunwayNoBudget(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.CreateToken(store.NewToken{Name: "free"}); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	if _, err := s.AppendCall(calls.Entry{TS: now.Add(-time.Hour), Vendor: "openai", Status: 200, Cost: 5, LatencyMS: 10}); err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	h := testHandler(t, Deps{Store: s, AdminKey: "secret", Now: func() time.Time { return now }})
	rec := do(h, "GET", "/api/overview", "secret", nil)
	var ov overviewView
	decodeBody(t, rec, &ov)
	if ov.RunwayDays != nil {
		t.Errorf("runway_days = %v, want null (no budgeted tokens)", *ov.RunwayDays)
	}
}

// --- (e) usage series ---

func seedSeriesCalls(t *testing.T, s *store.Store, now time.Time) {
	t.Helper()
	// now is 2026-06-09 12:00 UTC. Put traffic on day -1 and day -3 (relative
	// to now), leaving day -2 as a gap inside the default 7-day window.
	entries := []calls.Entry{
		// day -1: 2 rows, 1 error (500); cost 0.10 + 0.20.
		{TS: now.Add(-24 * time.Hour), Vendor: "openai", Status: 200, Cost: 0.10, LatencyMS: 10},
		{TS: now.Add(-24 * time.Hour).Add(time.Hour), Vendor: "openai", Status: 500, Cost: 0.20, LatencyMS: 20},
		// day -3: 1 row, transport error (status 0); cost 1.0.
		{TS: now.Add(-72 * time.Hour), Vendor: "openai", Status: 0, Cost: 1.0, LatencyMS: 30},
	}
	for i, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}
}

func TestUsageSeriesDefaults(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	seedSeriesCalls(t, s, now)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret", Now: func() time.Time { return now }})

	rec := do(h, "GET", "/api/usage/series", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("series: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var v usageSeriesView
	decodeBody(t, rec, &v)

	// Default 7-day window -> range > 2 days -> day buckets.
	if v.Bucket != "day" {
		t.Errorf("bucket label = %q, want day", v.Bucket)
	}
	if len(v.Points) == 0 {
		t.Fatal("no points returned")
	}

	// Points must be ascending and contiguous (gap-filled): every step exactly
	// 24h, and every parseable RFC3339.
	var prev time.Time
	var totalCost float64
	var totalReq, totalErr int
	for i, p := range v.Points {
		ts, err := time.Parse(time.RFC3339, p.TS)
		if err != nil {
			t.Fatalf("point[%d] ts not RFC3339: %q", i, p.TS)
		}
		if i > 0 {
			if !ts.After(prev) {
				t.Errorf("points not ascending at %d: %v <= %v", i, ts, prev)
			}
			if d := ts.Sub(prev); d != 24*time.Hour {
				t.Errorf("gap between points %d-%d = %v, want 24h (not gap-filled)", i-1, i, d)
			}
		}
		prev = ts
		totalCost += p.Cost
		totalReq += p.Requests
		totalErr += p.Errors
	}
	// Totals across the window equal the seeded traffic.
	if !approxF(totalCost, 1.30) {
		t.Errorf("total cost = %v, want 1.30", totalCost)
	}
	if totalReq != 3 {
		t.Errorf("total requests = %d, want 3", totalReq)
	}
	if totalErr != 2 { // status 500 and status 0
		t.Errorf("total errors = %d, want 2", totalErr)
	}
}

func TestUsageSeriesExplicitHour(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret", Now: func() time.Time { return now }})

	since := now.Add(-3 * time.Hour).Unix()
	until := now.Unix()
	target := "/api/usage/series?bucket=hour&since=" +
		strconv.FormatInt(since, 10) + "&until=" + strconv.FormatInt(until, 10)
	rec := do(h, "GET", target, "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("series hour: code = %d", rec.Code)
	}
	var v usageSeriesView
	decodeBody(t, rec, &v)
	if v.Bucket != "hour" {
		t.Errorf("bucket = %q, want hour", v.Bucket)
	}
	if len(v.Points) != 3 {
		t.Errorf("points = %d, want 3 hourly buckets", len(v.Points))
	}
}

func TestUsageSeriesAuthAndBadBucket(t *testing.T) {
	h := testHandler(t, Deps{AdminKey: "secret"})

	// 401 without the key.
	rec := do(h, "GET", "/api/usage/series", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: code = %d, want 401", rec.Code)
	}

	// 400 on an invalid bucket value.
	rec = do(h, "GET", "/api/usage/series?bucket=week", "secret", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad bucket: code = %d, want 400", rec.Code)
	}
}

// --- (f) vendors ---

func TestVendorsNeverLeakAPIKey(t *testing.T) {
	s := newTestStore(t)
	// Seed some traffic so stats are computed.
	if _, err := s.AppendCall(calls.Entry{TS: time.Now(), Vendor: "openai", Status: 200, LatencyMS: 100}); err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	if _, err := s.AppendCall(calls.Entry{TS: time.Now(), Vendor: "openai", Status: 500, LatencyMS: 200}); err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	rec := do(h, "GET", "/api/vendors", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("vendors: code = %d", rec.Code)
	}
	bodyStr := rec.Body.String()
	// CRITICAL: the raw api key must never appear anywhere in the response.
	if strings.Contains(bodyStr, rawAPIKey) {
		t.Fatal("vendors response LEAKED the raw api_key")
	}
	if strings.Contains(bodyStr, "sk-another-secret") {
		t.Fatal("vendors response LEAKED the second raw api_key")
	}
	if strings.Contains(bodyStr, "api_key") {
		t.Error("vendors response contains an api_key field")
	}

	var vendors []vendorView
	decodeBody(t, rec, &vendors)
	var openai *vendorView
	for i := range vendors {
		if vendors[i].Name == "openai" {
			openai = &vendors[i]
		}
	}
	if openai == nil {
		t.Fatal("openai vendor missing")
	}
	if len(openai.Credentials) != 1 {
		t.Fatalf("openai credentials = %d, want 1", len(openai.Credentials))
	}
	mk := openai.Credentials[0].MaskedKey
	if mk == "" || strings.Contains(mk, rawAPIKey) {
		t.Errorf("masked_key invalid: %q", mk)
	}
	if !strings.HasPrefix(mk, rawAPIKey[:3]) {
		t.Errorf("masked_key = %q, want prefix %q", mk, rawAPIKey[:3])
	}
	// Stats: 2 requests, 1 error, error_rate 0.5, last status 500 => unhealthy.
	if openai.Stats.Requests != 2 || openai.Stats.Errors != 1 {
		t.Errorf("openai stats = %+v, want 2 req / 1 err", openai.Stats)
	}
	if !approxF(openai.Stats.ErrorRate, 0.5) {
		t.Errorf("error_rate = %v, want 0.5", openai.Stats.ErrorRate)
	}
	if openai.Stats.Healthy {
		t.Error("openai should be unhealthy (has errors)")
	}

	// deepseek has no traffic => healthy true.
	var ds *vendorView
	for i := range vendors {
		if vendors[i].Name == "deepseek" {
			ds = &vendors[i]
		}
	}
	if ds == nil {
		t.Fatal("deepseek vendor missing")
	}
	if !ds.Stats.Healthy {
		t.Error("deepseek (no traffic) should be healthy")
	}
}

// --- (f) test-connection ---

func TestVendorTestConnectionReachable(t *testing.T) {
	// The probe must target the host ORIGIN (scheme://host), not base_url's path
	// prefix, since base_url now carries a vendor-specific version path. Assert
	// the probe lands on "/" even though base_url ends in /compatible-mode/v1.
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusUnauthorized) // even 401 means the host answered.
	}))
	defer upstream.Close()

	yaml := `
settings:
  listen: ":8080"
vendors:
  - name: mock
    base_url: ` + upstream.URL + `/compatible-mode/v1
    served_models: [m1]
    credentials:
      - id: k1
        api_key: sk-mock-secret
    prices:
      m1: { input: 1, output: 1, unit: per_1m_tokens }
`
	snap := mustSnapshot(t, yaml)
	h := testHandler(t, Deps{
		AdminKey:   "secret",
		Snapshot:   func() *config.Snapshot { return snap },
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Now:        time.Now,
	})

	rec := do(h, "POST", "/api/vendors/mock/test", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: code = %d", rec.Code)
	}
	var res testVendorView
	decodeBody(t, rec, &res)
	if !res.Reachable {
		t.Errorf("reachable = false, want true (got %+v)", res)
	}
	if res.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.Status)
	}
	if gotPath != "/" {
		t.Errorf("probe path = %q, want / (origin probe, base_url path stripped)", gotPath)
	}
}

func TestVendorTestConnectionUnreachable(t *testing.T) {
	// Closed/invalid address: connection should fail.
	yaml := `
settings:
  listen: ":8080"
vendors:
  - name: dead
    base_url: http://127.0.0.1:1
    served_models: [m1]
    credentials:
      - id: k1
        api_key: sk-mock-secret
    prices:
      m1: { input: 1, output: 1, unit: per_1m_tokens }
`
	snap := mustSnapshot(t, yaml)
	h := testHandler(t, Deps{
		AdminKey:   "secret",
		Snapshot:   func() *config.Snapshot { return snap },
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
		Now:        time.Now,
	})

	rec := do(h, "POST", "/api/vendors/dead/test", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: code = %d", rec.Code)
	}
	var res testVendorView
	decodeBody(t, rec, &res)
	if res.Reachable {
		t.Errorf("reachable = true, want false (got %+v)", res)
	}
	if res.Error == "" {
		t.Error("expected an error message for unreachable host")
	}
}

func TestVendorTestConnectionUnknownVendor(t *testing.T) {
	h := testHandler(t, Deps{AdminKey: "secret"})
	rec := do(h, "POST", "/api/vendors/nonexistent/test", "secret", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown vendor: code = %d, want 404", rec.Code)
	}
}

// --- settings + pricing ---

func TestSettings(t *testing.T) {
	h := testHandler(t, Deps{AdminKey: "secret", ConfigPath: "/etc/songguo.yaml", DBPath: "/var/songguo.db", Version: "1.2.3"})
	rec := do(h, "GET", "/api/settings", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: code = %d", rec.Code)
	}
	var sv settingsView
	decodeBody(t, rec, &sv)
	if !sv.AdminProtected {
		t.Error("admin_protected should be true")
	}
	if sv.ConfigPath != "/etc/songguo.yaml" || sv.DBPath != "/var/songguo.db" {
		t.Errorf("paths = %q / %q", sv.ConfigPath, sv.DBPath)
	}
	if sv.Version != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", sv.Version)
	}
	if sv.Listen != ":8080" {
		t.Errorf("listen = %q, want :8080", sv.Listen)
	}
	// The admin key must never appear in settings output.
	if strings.Contains(rec.Body.String(), "secret") {
		t.Error("settings leaked the admin key")
	}
}

func TestSettingsUnprotected(t *testing.T) {
	h := testHandler(t, Deps{AdminKey: ""})
	rec := do(h, "GET", "/api/settings", "", nil)
	var sv settingsView
	decodeBody(t, rec, &sv)
	if sv.AdminProtected {
		t.Error("admin_protected should be false in unprotected mode")
	}
}

func TestPricing(t *testing.T) {
	h := testHandler(t, Deps{AdminKey: "secret"})
	rec := do(h, "GET", "/api/pricing", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pricing: code = %d", rec.Code)
	}
	var rows []pricingRow
	decodeBody(t, rec, &rows)
	// openai has 2 priced models, deepseek 1 => 3 rows.
	if len(rows) != 3 {
		t.Fatalf("pricing rows = %d, want 3", len(rows))
	}
	found := false
	for _, r := range rows {
		if r.Vendor == "openai" && r.Model == "gpt-4o" {
			found = true
			if r.Input != 2.50 || r.Output != 10.00 || r.Unit != "per_1m_tokens" {
				t.Errorf("gpt-4o row = %+v", r)
			}
		}
	}
	if !found {
		t.Error("missing openai/gpt-4o pricing row")
	}
}

func approxF(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
