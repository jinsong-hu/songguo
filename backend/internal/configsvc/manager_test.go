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

// A complete, enabled service is routable; an incomplete or disabled one is
// skipped without failing the whole snapshot.
func TestManagerSkipsIncompleteServices(t *testing.T) {
	st := openTestStore(t)

	// Complete + enabled → routes.
	if _, err := st.CreateService(store.NewService{
		Name: "good", Adapter: "openai-compatible", BaseURL: "https://api.openai.com/v1",
		Enabled: true, APIKey: "sk-a",
		Models: []store.ServiceModel{{Model: "gpt-4o", Input: 1, Output: 2, Unit: "per_1m_tokens"}},
	}); err != nil {
		t.Fatal(err)
	}
	// No API key → skipped.
	if _, err := st.CreateService(store.NewService{
		Name: "nokeys", BaseURL: "https://x.example.com", Enabled: true,
		Models: []store.ServiceModel{{Model: "m1", Unit: "per_1m_tokens"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Disabled → skipped.
	if _, err := st.CreateService(store.NewService{
		Name: "off", BaseURL: "https://y.example.com", Enabled: false,
		APIKey: "sk-b",
		Models: []store.ServiceModel{{Model: "m2", Unit: "per_1m_tokens"}},
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
	got, _ := st.ListServices()
	var draftID string
	for _, s := range got {
		if s.Name == "nokeys" {
			draftID = s.ID
		}
	}
	key := "sk-c"
	if _, err := st.UpdateService(draftID, store.ServiceUpdate{APIKey: &key}); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := len(m.Current().Vendors()); got != 2 {
		t.Fatalf("after completing draft, vendors = %d, want 2", got)
	}
}
