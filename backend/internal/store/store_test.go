package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func ptrF(v float64) *float64 { return &v }
func ptrS(v string) *string   { return &v }
func ptrB(v bool) *bool       { return &v }

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, _, err := s1.CreateUser(NewUser{Name: "a"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotent migrate): %v", err)
	}
	defer s2.Close()

	toks, err := s2.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(toks) != 1 {
		t.Fatalf("expected 1 token after reopen, got %d", len(toks))
	}
}

func TestMigrationBackfillsNullUserCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-null-capture.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE users (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			key_hash   TEXT NOT NULL UNIQUE,
			key_prefix TEXT NOT NULL,
			key_full   TEXT NOT NULL DEFAULT '',
			budget     REAL,
			scope      TEXT NOT NULL DEFAULT '[]',
			rpm        INTEGER NOT NULL DEFAULT 0,
			capture    INTEGER,
			created_at INTEGER NOT NULL,
			revoked_at INTEGER
		);
		INSERT INTO users (id, name, key_hash, key_prefix, key_full, scope, rpm, capture, created_at)
		VALUES ('legacy-id', 'legacy', 'legacy-hash', 'sg-legacy', 'sg-legacy-key', '[]', 0, NULL, 1);
	`); err != nil {
		t.Fatalf("seed legacy users table: %v", err)
	}
	_ = db.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	users, err := s2.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("users len = %d, want 1", len(users))
	}
	if users[0].Capture {
		t.Error("NULL capture should backfill to false")
	}
	var nulls int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM users WHERE capture IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count null capture: %v", err)
	}
	if nulls != 0 {
		t.Fatalf("capture NULL rows = %d, want 0", nulls)
	}
}

func TestEnsureAdminUser(t *testing.T) {
	s := openTestStore(t)

	// Empty key is a no-op (admin API runs unprotected).
	if err := s.EnsureAdminUser(""); err != nil {
		t.Fatalf("EnsureAdminUser(\"\"): %v", err)
	}
	if users, _ := s.ListUsers(); len(users) != 0 {
		t.Fatalf("expected no users for empty admin key, got %d", len(users))
	}

	// Seeding makes the admin key authenticate proxy traffic via GetUserByKey.
	if err := s.EnsureAdminUser("admin-secret-1"); err != nil {
		t.Fatalf("EnsureAdminUser: %v", err)
	}
	u, err := s.GetUserByKey("admin-secret-1")
	if err != nil {
		t.Fatalf("GetUserByKey after seed: %v", err)
	}
	if u.ID != AdminUserID {
		t.Fatalf("admin user id = %q, want %q", u.ID, AdminUserID)
	}
	if u.Budget != nil {
		t.Fatalf("admin user should have unlimited budget, got %v", *u.Budget)
	}

	// Idempotent: re-seeding the same key keeps exactly one admin user.
	if err := s.EnsureAdminUser("admin-secret-1"); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	// Re-pointing: a changed admin key updates the hash; the old key stops working.
	if err := s.EnsureAdminUser("admin-secret-2"); err != nil {
		t.Fatalf("re-point: %v", err)
	}
	if _, err := s.GetUserByKey("admin-secret-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old admin key should no longer resolve, got %v", err)
	}
	if _, err := s.GetUserByKey("admin-secret-2"); err != nil {
		t.Fatalf("new admin key should resolve: %v", err)
	}

	users, _ := s.ListUsers()
	if len(users) != 1 {
		t.Fatalf("expected exactly one admin user, got %d", len(users))
	}
}

func TestUserLifecycle(t *testing.T) {
	s := openTestStore(t)

	budget := 12.5
	tok, plaintext, err := s.CreateUser(NewUser{
		Name:    "primary",
		Budget:  &budget,
		Scope:   []string{"gpt-4o", "text-embedding-3-small"},
		RPM:     60,
		Capture: true,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Plaintext returned once, with expected shape and prefix.
	if plaintext == "" {
		t.Fatal("expected non-empty plaintext key")
	}
	if got, want := plaintext[:3], "sg-"; got != want {
		t.Errorf("key prefix = %q, want %q", got, want)
	}
	if want := plaintext[:keyPrefixLen]; tok.KeyPrefix != want {
		t.Errorf("KeyPrefix = %q, want %q", tok.KeyPrefix, want)
	}
	if tok.Budget == nil || *tok.Budget != budget {
		t.Errorf("Budget = %v, want %v", tok.Budget, budget)
	}
	if len(tok.Scope) != 2 {
		t.Errorf("Scope = %v, want len 2", tok.Scope)
	}
	if tok.RPM != 60 {
		t.Errorf("RPM = %d, want 60", tok.RPM)
	}
	if !tok.Capture {
		t.Error("Capture = false, want true")
	}
	if tok.RevokedAt != nil {
		t.Errorf("new token should be active, got RevokedAt=%v", tok.RevokedAt)
	}

	// GetUser round-trips.
	got, err := s.GetUser(tok.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.ID != tok.ID || got.Name != "primary" {
		t.Errorf("GetUser = %+v", got)
	}

	// GetUserByKey works for the active token.
	byKey, err := s.GetUserByKey(plaintext)
	if err != nil {
		t.Fatalf("GetUserByKey: %v", err)
	}
	if byKey.ID != tok.ID {
		t.Errorf("GetUserByKey id = %q, want %q", byKey.ID, tok.ID)
	}

	// Wrong key -> ErrNotFound.
	if _, err := s.GetUserByKey("sg-nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUserByKey(wrong) err = %v, want ErrNotFound", err)
	}

	// HashKey is deterministic and stored (not the plaintext).
	if HashKey(plaintext) == plaintext {
		t.Error("HashKey returned plaintext")
	}

	// Update name/budget/scope/rpm; nil leaves unchanged.
	upd, err := s.UpdateUser(tok.ID, UserUpdate{
		Name:    ptrS("renamed"),
		Scope:   &[]string{"only-model"},
		Capture: ptrB(false),
	})
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if upd.Name != "renamed" {
		t.Errorf("after update Name = %q, want renamed", upd.Name)
	}
	if len(upd.Scope) != 1 || upd.Scope[0] != "only-model" {
		t.Errorf("after update Scope = %v", upd.Scope)
	}
	if upd.Budget == nil || *upd.Budget != budget {
		t.Errorf("Budget should be unchanged, got %v", upd.Budget)
	}
	if upd.RPM != 60 {
		t.Errorf("RPM should be unchanged, got %d", upd.RPM)
	}
	if upd.Capture {
		t.Error("Capture = true, want false after update")
	}

	// Revoke -> active lookup fails, GetUser still works (shows RevokedAt).
	if err := s.RevokeUser(tok.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := s.GetUserByKey(plaintext); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked GetUserByKey err = %v, want ErrNotFound", err)
	}
	revoked, err := s.GetUser(tok.ID)
	if err != nil {
		t.Fatalf("GetUser after revoke: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Error("expected RevokedAt set after revoke")
	}

	// Unknown ids -> ErrNotFound.
	if _, err := s.GetUser("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUser(missing) err = %v, want ErrNotFound", err)
	}
	if err := s.RevokeUser("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RevokeToken(missing) err = %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateUser("missing", UserUpdate{Name: ptrS("x")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateUser(missing) err = %v, want ErrNotFound", err)
	}
}

func TestCreateUserNilBudgetAndScope(t *testing.T) {
	s := openTestStore(t)
	tok, _, err := s.CreateUser(NewUser{Name: "free"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if tok.Budget != nil {
		t.Errorf("nil budget should stay nil (unlimited), got %v", *tok.Budget)
	}
	if tok.Scope == nil || len(tok.Scope) != 0 {
		t.Errorf("nil scope should become empty slice, got %v", tok.Scope)
	}
}

func TestListUsers(t *testing.T) {
	s := openTestStore(t)
	for _, n := range []string{"one", "two", "three"} {
		if _, _, err := s.CreateUser(NewUser{Name: n}); err != nil {
			t.Fatalf("CreateUser(%s): %v", n, err)
		}
	}
	toks, err := s.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(toks) != 3 {
		t.Fatalf("ListUsers len = %d, want 3", len(toks))
	}
}

func TestCallsAppendQueryAndAggregations(t *testing.T) {
	s := openTestStore(t)

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st200 := 200
	entries := []calls.Entry{
		{
			TS: base, UserID: "tokA", Model: "gpt-4o", Modality: calls.ModalityChat,
			Vendor: "openai", CredentialID: "c1", Status: 200,
			Usage: map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(5)},
			Cost:  0.10, LatencyMS: 120, Stream: true,
			Tags: map[string]string{"team": "eng"},
		},
		{
			TS: base.Add(1 * time.Minute), UserID: "tokA", Model: "gpt-4o", Modality: calls.ModalityChat,
			Vendor: "openai", CredentialID: "c1", Status: 500, Err: "upstream error",
			Cost: 0.05, LatencyMS: 300,
		},
		{
			TS: base.Add(2 * time.Minute), UserID: "tokB", Model: "text-embedding-3-small",
			Modality: calls.ModalityEmbedding, Vendor: "openai", CredentialID: "c2", Status: 200,
			Usage: map[string]any{"total_tokens": float64(42)}, Cost: 0.02, LatencyMS: 40,
		},
		{
			TS: base.Add(3 * time.Minute), UserID: "tokB", Model: "dall-e-3",
			Modality: calls.ModalityImage, Vendor: "openai", CredentialID: "c2", Status: 200,
			Cost: 0.40, LatencyMS: 900,
		},
	}

	seen := map[string]bool{}
	for i, e := range entries {
		id, err := s.AppendCall(e)
		if err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
		if id == "" {
			t.Fatalf("AppendCall[%d] id = %q", i, id)
		}
		if seen[id] {
			t.Fatalf("AppendCall[%d] duplicate id %q", i, id)
		}
		seen[id] = true
	}

	// Ordering: newest first.
	all, err := s.QueryCalls(CallFilter{})
	if err != nil {
		t.Fatalf("QueryCalls: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("QueryCalls all len = %d, want 4", len(all))
	}
	if !all[0].TS.After(all[1].TS) {
		t.Errorf("results not ordered ts DESC: %v then %v", all[0].TS, all[1].TS)
	}

	// Round-trip Usage and Tags on the most recent chat entry.
	var chat *calls.Entry
	for i := range all {
		if all[i].Model == "gpt-4o" && all[i].Status == 200 {
			chat = &all[i]
			break
		}
	}
	if chat == nil {
		t.Fatal("could not find the 200 gpt-4o entry")
	}
	if chat.Usage["prompt_tokens"] != float64(10) {
		t.Errorf("Usage round-trip = %v", chat.Usage)
	}
	if chat.Tags["team"] != "eng" {
		t.Errorf("Tags round-trip = %v", chat.Tags)
	}
	if !chat.Stream {
		t.Error("Stream round-trip = false, want true")
	}

	// Filter by token.
	tokA, err := s.QueryCalls(CallFilter{UserID: "tokA"})
	if err != nil {
		t.Fatalf("QueryCalls(tokA): %v", err)
	}
	if len(tokA) != 2 {
		t.Errorf("tokA rows = %d, want 2", len(tokA))
	}

	// Filter by model + status.
	chats, err := s.QueryCalls(CallFilter{Model: "gpt-4o", Status: &st200})
	if err != nil {
		t.Fatalf("QueryCalls(model+status): %v", err)
	}
	if len(chats) != 1 {
		t.Errorf("gpt-4o status=200 rows = %d, want 1", len(chats))
	}

	// Filter by vendor.
	vrows, err := s.QueryCalls(CallFilter{Vendor: "openai"})
	if err != nil {
		t.Fatalf("QueryCalls(vendor): %v", err)
	}
	if len(vrows) != 4 {
		t.Errorf("vendor rows = %d, want 4", len(vrows))
	}

	// Time window [since, until).
	since := base.Add(1 * time.Minute)
	until := base.Add(3 * time.Minute)
	win, err := s.QueryCalls(CallFilter{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("QueryCalls(window): %v", err)
	}
	if len(win) != 2 {
		t.Errorf("window rows = %d, want 2", len(win))
	}

	// Limit + Offset.
	page1, err := s.QueryCalls(CallFilter{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("QueryCalls(page1): %v", err)
	}
	page2, err := s.QueryCalls(CallFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("QueryCalls(page2): %v", err)
	}
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("paging lens = %d,%d want 2,2", len(page1), len(page2))
	}
	if page1[1].ID == page2[0].ID {
		t.Error("pages overlap")
	}

	// CountCalls respects filters and ignores paging.
	total, err := s.CountCalls(CallFilter{})
	if err != nil {
		t.Fatalf("CountCalls: %v", err)
	}
	if total != 4 {
		t.Errorf("CountCalls all = %d, want 4", total)
	}
	countA, err := s.CountCalls(CallFilter{UserID: "tokA", Limit: 1})
	if err != nil {
		t.Fatalf("CountCalls(tokA): %v", err)
	}
	if countA != 2 {
		t.Errorf("CountCalls(tokA) = %d, want 2", countA)
	}

	// SpendByUser sums all rows of a token (incl. error rows).
	spendA, err := s.SpendByUser("tokA", nil)
	if err != nil {
		t.Fatalf("SpendByUser: %v", err)
	}
	if !approx(spendA, 0.15) {
		t.Errorf("SpendByUser(tokA) = %v, want 0.15", spendA)
	}
	// since filter.
	spendASince, err := s.SpendByUser("tokA", &since)
	if err != nil {
		t.Fatalf("SpendByUser(since): %v", err)
	}
	if !approx(spendASince, 0.05) {
		t.Errorf("SpendByUser(tokA, since) = %v, want 0.05", spendASince)
	}
	// unknown token -> 0, no error.
	spendNone, err := s.SpendByUser("nope", nil)
	if err != nil {
		t.Fatalf("SpendByUser(nope): %v", err)
	}
	if spendNone != 0 {
		t.Errorf("SpendByUser(nope) = %v, want 0", spendNone)
	}

	// TotalSpend across all and within a window.
	tot, err := s.TotalSpend(nil, nil)
	if err != nil {
		t.Fatalf("TotalSpend: %v", err)
	}
	if !approx(tot, 0.57) {
		t.Errorf("TotalSpend = %v, want 0.57", tot)
	}

	// SpendByModality.
	byMod, err := s.SpendByModality(nil, nil)
	if err != nil {
		t.Fatalf("SpendByModality: %v", err)
	}
	if !approx(byMod[string(calls.ModalityChat)], 0.15) {
		t.Errorf("modality chat = %v, want 0.15", byMod[string(calls.ModalityChat)])
	}
	if !approx(byMod[string(calls.ModalityEmbedding)], 0.02) {
		t.Errorf("modality embedding = %v, want 0.02", byMod[string(calls.ModalityEmbedding)])
	}
	if !approx(byMod[string(calls.ModalityImage)], 0.40) {
		t.Errorf("modality image = %v, want 0.40", byMod[string(calls.ModalityImage)])
	}
}

func TestAppendCallDefaults(t *testing.T) {
	s := openTestStore(t)
	before := time.Now()
	id, err := s.AppendCall(calls.Entry{UserID: "x"}) // zero TS, modality
	if err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	got, err := s.QueryCalls(CallFilter{UserID: "x"})
	if err != nil || len(got) != 1 {
		t.Fatalf("QueryCalls: %v len=%d", err, len(got))
	}
	e := got[0]
	if e.ID != id {
		t.Errorf("id = %q, want %q", e.ID, id)
	}
	if e.Modality != calls.ModalityUnknown {
		t.Errorf("default Modality = %q, want unknown", e.Modality)
	}
	if e.TS.Before(before.Add(-time.Second)) {
		t.Errorf("default TS not set to ~now: %v", e.TS)
	}
	if e.Usage == nil || e.Tags == nil {
		t.Errorf("nil Usage/Tags should decode to non-nil maps: %v %v", e.Usage, e.Tags)
	}
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
