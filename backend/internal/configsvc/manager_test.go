package configsvc

import (
	"bytes"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/songguo/songguo/internal/store"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// A complete, enabled provider is routable; an incomplete or disabled one is
// skipped without failing the whole snapshot.
func TestManagerSkipsIncompleteProviders(t *testing.T) {
	st := openTestStore(t)

	// Complete + enabled → routes.
	if _, err := st.CreateProvider(store.NewProvider{
		Name: "good", Enabled: true, APIKey: "sk-a",
		Models:    []store.ProviderModel{{Model: "gpt-4o", Input: 1, Output: 2, Unit: "per_1m_tokens"}},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://api.openai.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}
	// No API key → skipped.
	if _, err := st.CreateProvider(store.NewProvider{
		Name: "nokeys", Enabled: true,
		Models:    []store.ProviderModel{{Model: "m1", Unit: "per_1m_tokens"}},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://x.example.com/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Disabled → skipped.
	if _, err := st.CreateProvider(store.NewProvider{
		Name: "off", Enabled: false, APIKey: "sk-b",
		Models:    []store.ProviderModel{{Model: "m2", Unit: "per_1m_tokens"}},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://y.example.com/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	snap := m.Current()
	if got := len(snap.Vendors()); got != 1 {
		t.Fatalf("routable vendors = %d, want 1 (incomplete/disabled skipped)", got)
	}
	if _, ok := snap.Vendor("good"); !ok {
		t.Error("expected 'good' to be routable")
	}
	vs := snap.VendorsForModel("gpt-4o")
	if len(vs) != 1 || vs[0].Adapter != "openai-compatible" {
		t.Errorf("VendorsForModel(gpt-4o) = %+v", vs)
	}

	// Setting a key on the draft and reloading makes it routable.
	got, _ := st.ListProviders()
	var draftID string
	for _, p := range got {
		if p.Name == "nokeys" {
			draftID = p.ID
		}
	}
	key := "sk-c"
	if _, err := st.UpdateProvider(draftID, store.ProviderUpdate{APIKey: &key}); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := len(m.Current().Vendors()); got != 2 {
		t.Fatalf("after completing draft, vendors = %d, want 2", got)
	}
}

// A model that would silently meter $0 — a zero rate or an unrecognized unit —
// is warned about (not blocked); a well-formed price stays silent. The provider
// still routes regardless.
func TestManagerWarnsOnUnpriceableModels(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name: "warnme", Enabled: true, APIKey: "sk-z",
		Models: []store.ProviderModel{
			{Model: "free-model", Input: 0, Output: 0, Unit: "per_1m_tokens"}, // zero rate
			{Model: "typo-model", Input: 1, Output: 2, Unit: "per_1m_token"},  // unknown unit (missing s)
			{Model: "ok-model", Input: 1, Output: 2, Unit: "per_1m_tokens"},   // fine
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://api.openai.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	m, err := NewManager(st, logger)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Warnings are non-fatal: the provider still routes.
	if _, ok := m.Current().Vendor("warnme"); !ok {
		t.Fatal("provider with price warnings must still route")
	}

	out := buf.String()
	if !strings.Contains(out, "free-model") || !strings.Contains(out, "price is zero") {
		t.Errorf("expected zero-price warning for free-model; logs:\n%s", out)
	}
	if !strings.Contains(out, "typo-model") || !strings.Contains(out, "unit not recognized") {
		t.Errorf("expected unknown-unit warning for typo-model; logs:\n%s", out)
	}
	if strings.Contains(out, "ok-model") {
		t.Errorf("well-priced ok-model should not warn; logs:\n%s", out)
	}
}

func TestManagerUsesCatalogPriceUnlessOverridden(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name:      "openai",
		Vendor:    "OpenAI",
		CatalogID: "openai",
		Enabled:   true,
		APIKey:    "sk-a",
		Models: []store.ProviderModel{
			// Stale copied price: because PriceOverride is false, the catalog
			// value for gpt-5.6-luna should be used instead.
			{Model: "gpt-5.6-luna", Input: 99, Output: 99, Unit: "per_1m_tokens"},
			// Explicit override: the stored value should be preserved.
			{Model: "gpt-5.6-sol", Input: 7, Output: 8, Unit: "per_1m_tokens", PriceOverride: true},
		},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://api.openai.com/v1/chat/completions", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	luna, ok := m.Current().PriceFor("openai", "gpt-5.6-luna")
	if !ok {
		t.Fatal("missing catalog-tracked price")
	}
	if luna.Input != 1 || luna.Output != 6 || luna.CachedInput != 0.1 {
		t.Fatalf("gpt-5.6-luna price = %+v, want catalog price", luna)
	}
	sol, ok := m.Current().PriceFor("openai", "gpt-5.6-sol")
	if !ok {
		t.Fatal("missing override price")
	}
	if sol.Input != 7 || sol.Output != 8 {
		t.Fatalf("gpt-5.6-sol price = %+v, want explicit override", sol)
	}
}

// A provider whose endpoints span two (origin, adapter) groups (e.g. DeepSeek's
// OpenAI and Anthropic surfaces, same host but different auth) expands into two
// routing vendors sharing one key: the primary group keeps the provider name,
// the second gets an adapter suffix.
func TestProviderExpandsByOriginAdapter(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name: "deepseek", Enabled: true, APIKey: "sk-d",
		Models: []store.ProviderModel{{Model: "deepseek-v4-pro", Input: 1, Output: 2, Unit: "per_1m_tokens"}},
		Endpoints: []store.ProviderEndpoint{
			{Wire: "openai/chat", Endpoint: "https://api.deepseek.com/chat/completions", Adapter: "openai-compatible"},
			{Wire: "openai/models", Endpoint: "https://api.deepseek.com", Adapter: "openai-compatible"},
			{Wire: "anthropic/messages", Endpoint: "https://api.deepseek.com/anthropic/messages", Adapter: "anthropic-compatible"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(st, quietLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	snap := m.Current()
	if got := len(snap.Vendors()); got != 2 {
		t.Fatalf("vendors = %d, want 2 (one per (origin, adapter))", got)
	}
	openai, ok := snap.Vendor("deepseek")
	if !ok {
		t.Fatal("expected primary group named 'deepseek'")
	}
	if openai.Origin != "https://api.deepseek.com" || openai.Adapter != "openai-compatible" {
		t.Errorf("primary group = %q/%q", openai.Origin, openai.Adapter)
	}
	if openai.Endpoints["openai/chat"] != "https://api.deepseek.com/chat/completions" {
		t.Errorf("primary openai/chat endpoint = %q", openai.Endpoints["openai/chat"])
	}
	anthro, ok := snap.Vendor("deepseek-anthropic")
	if !ok {
		t.Fatal("expected second group named 'deepseek-anthropic'")
	}
	if anthro.Origin != "https://api.deepseek.com" || anthro.Adapter != "anthropic-compatible" {
		t.Errorf("second group = %q/%q", anthro.Origin, anthro.Adapter)
	}
	if anthro.Endpoints["anthropic/messages"] != "https://api.deepseek.com/anthropic/messages" {
		t.Errorf("second anthropic/messages endpoint = %q", anthro.Endpoints["anthropic/messages"])
	}
	// Both groups carry the shared key and the model.
	if openai.Credential.APIKey != "sk-d" || anthro.Credential.APIKey != "sk-d" {
		t.Error("both groups should share the provider key")
	}
}
