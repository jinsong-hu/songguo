package configsvc

import (
	"io"
	"log/slog"
	"os"
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

// A complete, enabled service is routable; an incomplete or disabled one is
// skipped without failing the whole snapshot.
func TestManagerSkipsIncompleteServices(t *testing.T) {
	st := openTestStore(t)

	// Complete + enabled → routes.
	if _, err := st.CreateService(store.NewService{
		Name: "good", Adapter: "openai-compatible", BaseURL: "https://api.openai.com/v1",
		Enabled: true, APIKeys: []string{"sk-a"},
		Models: []store.ServiceModel{{Model: "gpt-4o", Input: 1, Output: 2, Unit: "per_1m_tokens"}},
	}); err != nil {
		t.Fatal(err)
	}
	// No credentials → skipped.
	if _, err := st.CreateService(store.NewService{
		Name: "nokeys", BaseURL: "https://x.example.com", Enabled: true,
		Models: []store.ServiceModel{{Model: "m1", Unit: "per_1m_tokens"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Disabled → skipped.
	if _, err := st.CreateService(store.NewService{
		Name: "off", BaseURL: "https://y.example.com", Enabled: false,
		APIKeys: []string{"sk-b"},
		Models:  []store.ServiceModel{{Model: "m2", Unit: "per_1m_tokens"}},
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

	// Adding a credential to the draft and reloading makes it routable.
	got, _ := st.ListServices()
	var draftID string
	for _, s := range got {
		if s.Name == "nokeys" {
			draftID = s.ID
		}
	}
	if _, err := st.AddCredential(draftID, "sk-c"); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := len(m.Current().Vendors()); got != 2 {
		t.Fatalf("after completing draft, vendors = %d, want 2", got)
	}
}

func TestSeedFromConfig(t *testing.T) {
	st := openTestStore(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const yaml = `
settings:
  capture: true
vendors:
  - name: openai
    base_url: https://api.openai.com/v1
    served_models: [gpt-4o, text-embedding-3-small]
    credentials:
      - id: k1
        api_key: sk-aaa
    prices:
      gpt-4o: { input: 2.5, output: 10, unit: per_1m_tokens }
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := SeedFromConfig(st, path, quietLogger())
	if err != nil {
		t.Fatalf("SeedFromConfig: %v", err)
	}
	if n != 1 {
		t.Fatalf("imported = %d, want 1", n)
	}

	svcs, _ := st.ListServices()
	if len(svcs) != 1 {
		t.Fatalf("services = %d, want 1", len(svcs))
	}
	if len(svcs[0].Models) != 2 {
		t.Errorf("models = %d, want 2 (served_models become models)", len(svcs[0].Models))
	}
	as, _ := st.GetAppSettings()
	if !as.Capture {
		t.Error("expected capture setting carried over from yaml")
	}

	// Second seed is a no-op (store already has services).
	n2, err := SeedFromConfig(st, path, quietLogger())
	if err != nil {
		t.Fatalf("second SeedFromConfig: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second seed imported = %d, want 0", n2)
	}
}
