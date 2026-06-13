package configsvc

import (
	"io"
	"log/slog"
	"path/filepath"
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
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", BaseURL: "https://api.openai.com/v1", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}
	// No API key → skipped.
	if _, err := st.CreateProvider(store.NewProvider{
		Name: "nokeys", Enabled: true,
		Models:    []store.ProviderModel{{Model: "m1", Unit: "per_1m_tokens"}},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", BaseURL: "https://x.example.com", Adapter: "openai-compatible"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Disabled → skipped.
	if _, err := st.CreateProvider(store.NewProvider{
		Name: "off", Enabled: false, APIKey: "sk-b",
		Models:    []store.ProviderModel{{Model: "m2", Unit: "per_1m_tokens"}},
		Endpoints: []store.ProviderEndpoint{{Wire: "openai/chat", BaseURL: "https://y.example.com", Adapter: "openai-compatible"}},
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

// A provider whose endpoints span two base URLs (e.g. DeepSeek's OpenAI and
// Anthropic surfaces) expands into two routing vendors sharing one key: the
// primary group keeps the provider name, the second gets an adapter suffix.
func TestProviderExpandsByBaseURL(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateProvider(store.NewProvider{
		Name: "deepseek", Enabled: true, APIKey: "sk-d",
		Models: []store.ProviderModel{{Model: "deepseek-v4-pro", Input: 1, Output: 2, Unit: "per_1m_tokens"}},
		Endpoints: []store.ProviderEndpoint{
			{Wire: "openai/chat", BaseURL: "https://api.deepseek.com", Adapter: "openai-compatible"},
			{Wire: "openai/models", BaseURL: "https://api.deepseek.com", Adapter: "openai-compatible"},
			{Wire: "anthropic/messages", BaseURL: "https://api.deepseek.com/anthropic", Adapter: "anthropic-compatible"},
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
		t.Fatalf("vendors = %d, want 2 (one per base_url)", got)
	}
	openai, ok := snap.Vendor("deepseek")
	if !ok {
		t.Fatal("expected primary group named 'deepseek'")
	}
	if openai.BaseURL != "https://api.deepseek.com" || openai.Adapter != "openai-compatible" {
		t.Errorf("primary group = %q/%q", openai.BaseURL, openai.Adapter)
	}
	anthro, ok := snap.Vendor("deepseek-anthropic")
	if !ok {
		t.Fatal("expected second group named 'deepseek-anthropic'")
	}
	if anthro.BaseURL != "https://api.deepseek.com/anthropic" || anthro.Adapter != "anthropic-compatible" {
		t.Errorf("second group = %q/%q", anthro.BaseURL, anthro.Adapter)
	}
	// Both groups carry the shared key and the model.
	if openai.Credential.APIKey != "sk-d" || anthro.Credential.APIKey != "sk-d" {
		t.Error("both groups should share the provider key")
	}
}
