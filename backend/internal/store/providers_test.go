package store

import (
	"testing"
)

func TestProviderCRUDRoundTrip(t *testing.T) {
	s := openTestStore(t)

	pvd, err := s.CreateProvider(NewProvider{
		Name:     "openai",
		Vendor:   "OpenAI",
		Priority: 1,
		Weight:   2,
		Enabled:  true,
		APIKey:   "sk-aaa",
		Models: []ProviderModel{
			{Model: "gpt-4o", Input: 2.5, Output: 10, Unit: "per_1m_tokens"},
			{Model: "gpt-4o-mini", Input: 0.15, Output: 0.6, Unit: "per_1m_tokens"},
		},
		Endpoints: []ProviderEndpoint{
			{Wire: "openai/chat", BaseURL: "https://api.openai.com/v1", Adapter: "openai-compatible"},
			{Wire: "openai/models", BaseURL: "https://api.openai.com/v1", Adapter: "openai-compatible"},
		},
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if pvd.ID == "" {
		t.Fatal("expected generated provider id")
	}
	if pvd.APIKey != "sk-aaa" {
		t.Fatalf("api key = %q, want sk-aaa", pvd.APIKey)
	}
	if len(pvd.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(pvd.Models))
	}
	if len(pvd.Endpoints) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(pvd.Endpoints))
	}
	if pvd.Weight != 2 {
		t.Errorf("weight = %d, want 2", pvd.Weight)
	}

	// Duplicate name must fail (UNIQUE).
	if _, err := s.CreateProvider(NewProvider{Name: "openai"}); err == nil {
		t.Error("expected duplicate name to fail")
	}

	// Update scalar + replace models + replace endpoints.
	newName := "openai-main"
	disabled := false
	updated, err := s.UpdateProvider(pvd.ID, ProviderUpdate{
		Name:      &newName,
		Enabled:   &disabled,
		Models:    []ProviderModel{{Model: "gpt-4o", Input: 3, Output: 12, Unit: "per_1m_tokens"}},
		Endpoints: []ProviderEndpoint{{Wire: "openai/chat", BaseURL: "https://api.openai.com/v1", Adapter: "openai-compatible"}},
	})
	if err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	if updated.Name != "openai-main" {
		t.Errorf("name = %q, want openai-main", updated.Name)
	}
	if updated.Enabled {
		t.Error("expected disabled")
	}
	if len(updated.Models) != 1 || updated.Models[0].Input != 3 {
		t.Errorf("models not replaced: %+v", updated.Models)
	}
	if len(updated.Endpoints) != 1 || updated.Endpoints[0].Wire != "openai/chat" {
		t.Errorf("endpoints not replaced: %+v", updated.Endpoints)
	}

	// Replace the API key.
	newKey := "sk-ccc"
	updated, err = s.UpdateProvider(pvd.ID, ProviderUpdate{APIKey: &newKey})
	if err != nil {
		t.Fatalf("UpdateProvider(api key): %v", err)
	}
	if updated.APIKey != "sk-ccc" {
		t.Fatalf("api key after replace = %q, want sk-ccc", updated.APIKey)
	}

	// List + count.
	if n, _ := s.CountProviders(); n != 1 {
		t.Errorf("CountProviders = %d, want 1", n)
	}
	list, err := s.ListProviders()
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(list) != 1 || list[0].APIKey != "sk-ccc" || len(list[0].Models) != 1 || len(list[0].Endpoints) != 1 {
		t.Errorf("ListProviders assembly wrong: %+v", list)
	}

	// Delete cascades.
	if err := s.DeleteProvider(pvd.ID); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if _, err := s.GetProvider(pvd.ID); err == nil {
		t.Error("expected provider gone")
	}
	if n, _ := s.CountProviders(); n != 0 {
		t.Errorf("CountProviders after delete = %d, want 0", n)
	}
}

func TestAppSettingsRoundTrip(t *testing.T) {
	s := openTestStore(t)

	as, err := s.GetAppSettings()
	if err != nil {
		t.Fatalf("GetAppSettings: %v", err)
	}
	if as.Capture {
		t.Error("capture should default off")
	}
	if as.CaptureMaxBytes != 32768 || as.CaptureRetain != 10000 {
		t.Errorf("defaults wrong: %+v", as)
	}

	if err := s.UpdateAppSettings(AppSettings{Capture: true, CaptureMaxBytes: 1000, CaptureRetain: 50}); err != nil {
		t.Fatalf("UpdateAppSettings: %v", err)
	}
	as, _ = s.GetAppSettings()
	if !as.Capture || as.CaptureMaxBytes != 1000 || as.CaptureRetain != 50 {
		t.Errorf("settings not persisted: %+v", as)
	}
}

// TestEndpointBackfillOnMigration simulates a database that predates the
// provider_endpoints table: a provider with the legacy per-provider base_url +
// adapter columns and provider_wires rows. When migrate() creates
// provider_endpoints, each wire must become an endpoint carrying the provider's
// base_url + adapter (or the provider would route nowhere after the upgrade).
func TestEndpointBackfillOnMigration(t *testing.T) {
	s := openTestStore(t)

	// Build a legacy provider directly: base_url/adapter on the row + wires in
	// provider_wires, and no endpoints yet.
	stmts := []string{
		`INSERT INTO providers (id, name, vendor, adapter, base_url, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at)
			VALUES ('p1', 'legacy', 'OpenAI', 'openai-compatible', 'https://api.openai.com/v1', 0, 1, 1, '', 'sk-x', 0, '{}', 100, 100)`,
		`INSERT INTO provider_wires (provider_id, wire) VALUES ('p1', 'openai/chat')`,
		`INSERT INTO provider_wires (provider_id, wire) VALUES ('p1', 'openai/models')`,
		`DROP TABLE provider_endpoints`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("setup %s: %v", q, err)
		}
	}

	if err := s.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, err := s.GetProvider("p1")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if len(got.Endpoints) != 2 {
		t.Fatalf("endpoints = %+v, want 2 backfilled", got.Endpoints)
	}
	for _, ep := range got.Endpoints {
		if ep.BaseURL != "https://api.openai.com/v1" || ep.Adapter != "openai-compatible" {
			t.Errorf("endpoint %q base/adapter = %q/%q, want legacy values", ep.Wire, ep.BaseURL, ep.Adapter)
		}
	}

	// Re-running migrate with the table present must not duplicate or re-add.
	if err := s.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	got, _ = s.GetProvider("p1")
	if len(got.Endpoints) != 2 {
		t.Errorf("endpoints after idempotent migrate = %d, want 2", len(got.Endpoints))
	}
}

// TestCredentialPoolFoldOnMigration simulates a database from the multi-key
// pool era: when migrate() finds a service_credentials table, each provider's
// oldest key must be folded into providers.api_key and the table dropped.
func TestCredentialPoolFoldOnMigration(t *testing.T) {
	s := openTestStore(t)

	pvd, err := s.CreateProvider(NewProvider{Name: "legacy"})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	// Rewind to the pool era: recreate the table with two keys, oldest first.
	stmts := []string{
		`CREATE TABLE service_credentials (
			id TEXT PRIMARY KEY,
			service_id TEXT NOT NULL,
			api_key TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`INSERT INTO service_credentials VALUES ('c1', '` + pvd.ID + `', 'sk-old', 100)`,
		`INSERT INTO service_credentials VALUES ('c2', '` + pvd.ID + `', 'sk-new', 200)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	if err := s.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, err := s.GetProvider(pvd.ID)
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.APIKey != "sk-old" {
		t.Errorf("api_key = %q, want oldest pool key sk-old", got.APIKey)
	}
	if ok, _ := s.tableExists("service_credentials"); ok {
		t.Error("service_credentials table should be dropped after fold")
	}
}

// TestServicesRenameOnMigration simulates a services-era database (old table and
// column names, base_url/adapter on the row, service_wires) and verifies
// migrate() renames everything to the providers naming and backfills endpoints
// with data intact.
func TestServicesRenameOnMigration(t *testing.T) {
	s := openTestStore(t)

	stmts := []string{
		`INSERT INTO providers (id, name, vendor, adapter, base_url, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at)
			VALUES ('p1', 'legacy', '', 'openai-compatible', 'https://x.example.com', 0, 1, 1, '', 'sk-x', 0, '{}', 100, 100)`,
		`INSERT INTO provider_models (provider_id, model, input, output, cached_input, unit) VALUES ('p1', 'm1', 1, 2, 0, 'per_1m_tokens')`,
		`INSERT INTO provider_wires (provider_id, wire) VALUES ('p1', 'openai/chat')`,
		`DROP TABLE provider_endpoints`,
		// Rewind to the services era: old table and column names.
		`PRAGMA legacy_alter_table=ON`,
		`ALTER TABLE providers RENAME TO services`,
		`ALTER TABLE provider_models RENAME TO service_models`,
		`ALTER TABLE service_models RENAME COLUMN provider_id TO service_id`,
		`ALTER TABLE provider_wires RENAME TO service_wires`,
		`ALTER TABLE service_wires RENAME COLUMN provider_id TO service_id`,
		`PRAGMA legacy_alter_table=OFF`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("setup %s: %v", q, err)
		}
	}

	if err := s.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, err := s.GetProvider("p1")
	if err != nil {
		t.Fatalf("GetProvider after rename: %v", err)
	}
	if len(got.Models) != 1 || got.Models[0].Model != "m1" {
		t.Errorf("models = %v, want m1 preserved", got.Models)
	}
	if len(got.Endpoints) != 1 || got.Endpoints[0].Wire != "openai/chat" || got.Endpoints[0].BaseURL != "https://x.example.com" {
		t.Errorf("endpoints = %v, want openai/chat @ x.example.com", got.Endpoints)
	}
	for _, old := range []string{"services", "service_models", "service_wires"} {
		if ok, _ := s.tableExists(old); ok {
			t.Errorf("table %s should be gone after rename", old)
		}
	}
}
