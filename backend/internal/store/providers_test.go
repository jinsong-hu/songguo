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
			{Model: "gpt-4o", Input: 2.5, Output: 10, Unit: "per_1m_tokens", PriceOverride: true},
			{Model: "gpt-4o-mini", Input: 0.15, Output: 0.6, Unit: "per_1m_tokens"},
		},
		Endpoints: []ProviderEndpoint{
			{Wire: "openai/chat", Endpoint: "https://api.openai.com/v1/chat/completions", Adapter: "openai-compatible"},
			{Wire: "openai/models", Endpoint: "https://api.openai.com/v1", Adapter: "openai-compatible"},
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
	if !pvd.Models[0].PriceOverride {
		t.Fatalf("price override flag was not persisted: %+v", pvd.Models)
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
		Models:    []ProviderModel{{Model: "gpt-4o", Input: 3, Output: 12, Unit: "per_1m_tokens", PriceOverride: true}},
		Endpoints: []ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://api.openai.com/v1/chat/completions", Adapter: "openai-compatible"}},
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
	if len(updated.Models) != 1 || updated.Models[0].Input != 3 || !updated.Models[0].PriceOverride {
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

func TestFreshProviderSchemaIsCanonical(t *testing.T) {
	s := openTestStore(t)
	assertCanonicalProviderSchema(t, s)

	pvd, err := s.CreateProvider(NewProvider{
		Name:    "cun-ai",
		Enabled: true,
		Weight:  1,
		APIKey:  "sk-test",
		Models: []ProviderModel{{
			Model: "claude-opus-5", Input: 5, Output: 25, CachedInput: 0.5,
			Unit: "per_1m_tokens",
		}},
		Endpoints: []ProviderEndpoint{{
			Wire: "anthropic/messages", Endpoint: "https://www.cun.ai/v1/messages",
			Adapter: "anthropic-compatible",
		}},
	})
	if err != nil {
		t.Fatalf("CreateProvider on canonical schema: %v", err)
	}
	if pvd.Name != "cun-ai" || len(pvd.Models) != 1 || len(pvd.Endpoints) != 1 {
		t.Fatalf("created provider = %+v", pvd)
	}
	if pvd.Endpoints[0].Wire != "anthropic/messages" ||
		pvd.Endpoints[0].Endpoint != "https://www.cun.ai/v1/messages" ||
		pvd.Endpoints[0].Adapter != "anthropic-compatible" {
		t.Fatalf("created endpoints = %+v", pvd.Endpoints)
	}
}

func assertCanonicalProviderSchema(t *testing.T, s *Store) {
	t.Helper()
	for _, column := range []string{"base_url", "adapter"} {
		if has, err := s.hasColumn("providers", column); err != nil || has {
			t.Fatalf("providers.%s present = %v, err = %v; want retired", column, has, err)
		}
	}
	if has, err := s.tableExists("provider_wires"); err != nil || has {
		t.Fatalf("provider_wires present = %v, err = %v; want retired", has, err)
	}
	if has, err := s.hasColumn("provider_endpoints", "endpoint"); err != nil || !has {
		t.Fatalf("provider_endpoints.endpoint present = %v, err = %v; want canonical column", has, err)
	}
	if has, err := s.hasColumn("provider_endpoints", "base_url"); err != nil || has {
		t.Fatalf("provider_endpoints.base_url present = %v, err = %v; want retired", has, err)
	}
}

func rewindProviderSchema(t *testing.T, s *Store, withWires bool) {
	t.Helper()
	stmts := []string{
		`ALTER TABLE providers ADD COLUMN adapter TEXT NOT NULL DEFAULT 'openai-compatible'`,
		`ALTER TABLE providers ADD COLUMN base_url TEXT NOT NULL DEFAULT ''`,
		`DROP TABLE provider_endpoints`,
	}
	if withWires {
		stmts = append(stmts, `CREATE TABLE provider_wires (
			provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			wire        TEXT NOT NULL,
			PRIMARY KEY (provider_id, wire)
		)`)
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("rewind provider schema with %q: %v", stmt, err)
		}
	}
}

// Weight 0 is a value the store must keep, not a missing one to fill in: it
// parks the provider (see Provider.Weight). Only a negative weight is corrected,
// and to 0 rather than to an invented share.
func TestProviderWeightZeroRoundTrips(t *testing.T) {
	s := openTestStore(t)

	parked, err := s.CreateProvider(NewProvider{Name: "parked", Enabled: true, Weight: 0, APIKey: "sk-a"})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if parked.Weight != 0 {
		t.Fatalf("created weight = %d, want 0 preserved", parked.Weight)
	}

	zero, negative := 0, -3
	weighted, err := s.CreateProvider(NewProvider{Name: "weighted", Enabled: true, Weight: 4, APIKey: "sk-b"})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	updated, err := s.UpdateProvider(weighted.ID, ProviderUpdate{Weight: &zero})
	if err != nil {
		t.Fatalf("UpdateProvider(0): %v", err)
	}
	if updated.Weight != 0 {
		t.Fatalf("weight after parking = %d, want 0", updated.Weight)
	}

	updated, err = s.UpdateProvider(weighted.ID, ProviderUpdate{Weight: &negative})
	if err != nil {
		t.Fatalf("UpdateProvider(-3): %v", err)
	}
	if updated.Weight != 0 {
		t.Fatalf("weight after a negative = %d, want it clamped to 0", updated.Weight)
	}
}

func TestProviderModelRoutingRoundTripAndPreserve(t *testing.T) {
	s := openTestStore(t)
	pvd, err := s.CreateProvider(NewProvider{
		Name: "pool", Enabled: true, Priority: 1, Weight: 2, APIKey: "sk-a",
		Models: []ProviderModel{{Model: "m", Input: 1, Output: 2}},
		Endpoints: []ProviderEndpoint{{
			Wire: "openai/chat", Endpoint: "https://example.com/v1/chat/completions",
			Adapter: "openai-compatible",
		}},
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	disabled := false
	priority, weight := 4, 9
	if err := s.UpdateProviderModelRouting(
		pvd.ID, "m", &disabled, &priority, true, &weight, true,
	); err != nil {
		t.Fatalf("UpdateProviderModelRouting: %v", err)
	}
	got, err := s.GetProvider(pvd.ID)
	if err != nil {
		t.Fatal(err)
	}
	route := got.Models[0]
	if route.RoutingEnabled || route.PriorityOverride == nil || *route.PriorityOverride != 4 ||
		route.WeightOverride == nil || *route.WeightOverride != 9 {
		t.Fatalf("routing = %+v", route)
	}

	// Replacing model pricing through the provider editor preserves the route.
	got, err = s.UpdateProvider(pvd.ID, ProviderUpdate{
		Models: []ProviderModel{{Model: "m", Input: 3, Output: 6}},
	})
	if err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	route = got.Models[0]
	if route.RoutingEnabled || route.PriorityOverride == nil || *route.PriorityOverride != 4 ||
		route.WeightOverride == nil || *route.WeightOverride != 9 {
		t.Fatalf("routing after model replace = %+v", route)
	}

	if err := s.UpdateProviderModelRouting(pvd.ID, "m", nil, nil, true, nil, true); err != nil {
		t.Fatalf("clear routing overrides: %v", err)
	}
	got, _ = s.GetProvider(pvd.ID)
	if got.Models[0].PriorityOverride != nil || got.Models[0].WeightOverride != nil {
		t.Fatalf("overrides not cleared: %+v", got.Models[0])
	}
}

// TestEndpointBackfillOnMigration simulates a database that predates the
// provider_endpoints table: a provider with the legacy per-provider base_url +
// adapter columns and provider_wires rows. When migrate() creates canonical
// provider_endpoints it backfills each wire from the provider's base_url and
// rewrites model-routed wires into full endpoints (so the chat wire's URL gains
// /chat/completions; the model-listing wire keeps the base).
func TestEndpointBackfillOnMigration(t *testing.T) {
	s := openTestStore(t)
	rewindProviderSchema(t, s, true)

	// Build a legacy provider directly: base_url/adapter on the row + wires in
	// provider_wires, and no endpoints yet.
	stmts := []string{
		`INSERT INTO providers (id, name, vendor, adapter, base_url, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at)
			VALUES ('p1', 'legacy', 'OpenAI', 'openai-compatible', 'https://api.openai.com/v1', 0, 1, 1, '', 'sk-x', 0, '{}', 100, 100)`,
		`INSERT INTO provider_wires (provider_id, wire) VALUES ('p1', 'openai/chat')`,
		`INSERT INTO provider_wires (provider_id, wire) VALUES ('p1', 'openai/models')`,
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
	want := map[string]string{
		"openai/chat":   "https://api.openai.com/v1/chat/completions", // model-routed → full
		"openai/models": "https://api.openai.com/v1",                  // origin-only → base kept
	}
	for _, ep := range got.Endpoints {
		if ep.Adapter != "openai-compatible" {
			t.Errorf("endpoint %q adapter = %q, want openai-compatible", ep.Wire, ep.Adapter)
		}
		if ep.Endpoint != want[ep.Wire] {
			t.Errorf("endpoint %q = %q, want %q", ep.Wire, ep.Endpoint, want[ep.Wire])
		}
	}
	assertCanonicalProviderSchema(t, s)

	// Re-running migrate must be idempotent: no duplicate or double-appended suffix.
	if err := s.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	got, _ = s.GetProvider("p1")
	if len(got.Endpoints) != 2 {
		t.Errorf("endpoints after idempotent migrate = %d, want 2", len(got.Endpoints))
	}
	for _, ep := range got.Endpoints {
		if ep.Endpoint != want[ep.Wire] {
			t.Errorf("endpoint %q after second migrate = %q, want %q (not double-converted)", ep.Wire, ep.Endpoint, want[ep.Wire])
		}
	}
}

// A pre-wire database had only provider-level base_url/adapter. Preserve its
// historical adapter defaults when creating canonical endpoint rows.
func TestPreWireProviderBackfillOnMigration(t *testing.T) {
	s := openTestStore(t)
	rewindProviderSchema(t, s, false)

	if _, err := s.db.Exec(`INSERT INTO providers
		(id, name, vendor, adapter, base_url, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at)
		VALUES ('p1', 'legacy-anthropic', 'Anthropic', 'anthropic-compatible', 'https://api.anthropic.com/v1', 0, 1, 1, '', 'sk-x', 0, '{}', 100, 100)`); err != nil {
		t.Fatalf("insert pre-wire provider: %v", err)
	}

	if err := s.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got, err := s.GetProvider("p1")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if len(got.Endpoints) != 1 ||
		got.Endpoints[0].Wire != "anthropic/messages" ||
		got.Endpoints[0].Endpoint != "https://api.anthropic.com/v1/messages" ||
		got.Endpoints[0].Adapter != "anthropic-compatible" {
		t.Fatalf("pre-wire endpoints = %+v", got.Endpoints)
	}
	assertCanonicalProviderSchema(t, s)
}

// The first endpoint-backed schema still named the URL column base_url. Missing
// endpoint rows are recovered from provider_wires before that column is renamed.
func TestOldEndpointSchemaBackfillOnMigration(t *testing.T) {
	s := openTestStore(t)
	rewindProviderSchema(t, s, true)

	stmts := []string{
		`CREATE TABLE provider_endpoints (
			provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			wire        TEXT NOT NULL,
			base_url    TEXT NOT NULL DEFAULT '',
			adapter     TEXT NOT NULL DEFAULT 'openai-compatible',
			PRIMARY KEY (provider_id, wire)
		)`,
		`INSERT INTO providers (id, name, vendor, adapter, base_url, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at)
			VALUES ('p1', 'legacy-endpoints', 'OpenAI', 'openai-compatible', 'https://api.openai.com/v1', 0, 1, 1, '', 'sk-x', 0, '{}', 100, 100)`,
		`INSERT INTO provider_wires (provider_id, wire) VALUES ('p1', 'openai/chat')`,
		`INSERT INTO provider_wires (provider_id, wire) VALUES ('p1', 'openai/models')`,
		`INSERT INTO provider_endpoints (provider_id, wire, base_url, adapter)
			VALUES ('p1', 'openai/chat', 'https://azure.example/openai/deployments/{model}?api-version=2026-01-01', 'openai-compatible')`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("setup %s: %v", stmt, err)
		}
	}

	if err := s.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got, err := s.GetProvider("p1")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	want := map[string]string{
		"openai/chat":   "https://azure.example/openai/deployments/{model}/chat/completions?api-version=2026-01-01",
		"openai/models": "https://api.openai.com/v1",
	}
	if len(got.Endpoints) != len(want) {
		t.Fatalf("endpoints = %+v, want %d", got.Endpoints, len(want))
	}
	for _, endpoint := range got.Endpoints {
		if endpoint.Endpoint != want[endpoint.Wire] {
			t.Errorf("endpoint %q = %q, want %q", endpoint.Wire, endpoint.Endpoint, want[endpoint.Wire])
		}
	}
	assertCanonicalProviderSchema(t, s)
}

// Canonical endpoint rows are authoritative. Retired wire rows must never
// recreate endpoints an operator removed, and full URLs must not be rewritten.
func TestCanonicalEndpointSchemaDoesNotRestoreLegacyWires(t *testing.T) {
	s := openTestStore(t)
	pvd, err := s.CreateProvider(NewProvider{
		Name: "azure", Enabled: true, Weight: 1,
		Endpoints: []ProviderEndpoint{{
			Wire:     "openai/chat",
			Endpoint: "https://azure.example/openai/deployments/{model}/chat/completions?api-version=2026-01-01",
			Adapter:  "openai-compatible",
		}},
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	stmts := []string{
		`ALTER TABLE providers ADD COLUMN adapter TEXT NOT NULL DEFAULT 'openai-compatible'`,
		`ALTER TABLE providers ADD COLUMN base_url TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE provider_wires (
			provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			wire        TEXT NOT NULL,
			PRIMARY KEY (provider_id, wire)
		)`,
		`INSERT INTO provider_wires (provider_id, wire) VALUES ('` + pvd.ID + `', 'openai/chat')`,
		`INSERT INTO provider_wires (provider_id, wire) VALUES ('` + pvd.ID + `', 'openai/models')`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("setup %s: %v", stmt, err)
		}
	}

	if err := s.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got, err := s.GetProvider(pvd.ID)
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if len(got.Endpoints) != 1 || got.Endpoints[0].Wire != "openai/chat" ||
		got.Endpoints[0].Endpoint != "https://azure.example/openai/deployments/{model}/chat/completions?api-version=2026-01-01" {
		t.Fatalf("canonical endpoints changed = %+v", got.Endpoints)
	}
	assertCanonicalProviderSchema(t, s)
}

// A populated database with neither canonical endpoints nor enough legacy
// fields to reconstruct them is not a recognized migration path. Fail without
// committing the newly created table so an operator can inspect the database.
func TestProviderSchemaMigrationRejectsUnrecoverableShapeAtomically(t *testing.T) {
	s := openTestStore(t)
	pvd, err := s.CreateProvider(NewProvider{Name: "unrecoverable", Enabled: true, Weight: 1})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if _, err := s.db.Exec(`DROP TABLE provider_endpoints`); err != nil {
		t.Fatalf("drop provider_endpoints: %v", err)
	}

	if err := s.migrate(); err == nil {
		t.Fatal("migrate succeeded on an unrecoverable provider schema")
	}
	if has, err := s.tableExists("provider_endpoints"); err != nil || has {
		t.Fatalf("provider_endpoints present after rollback = %v, err = %v", has, err)
	}
	var providers int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM providers WHERE id = ?`, pvd.ID).Scan(&providers); err != nil {
		t.Fatalf("count preserved provider: %v", err)
	}
	if providers != 1 {
		t.Fatalf("preserved providers = %d, want 1", providers)
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
	rewindProviderSchema(t, s, true)

	stmts := []string{
		`INSERT INTO providers (id, name, vendor, adapter, base_url, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at)
			VALUES ('p1', 'legacy', '', 'openai-compatible', 'https://x.example.com', 0, 1, 1, '', 'sk-x', 0, '{}', 100, 100)`,
		`INSERT INTO provider_models (provider_id, model, input, output, cached_input, unit) VALUES ('p1', 'm1', 1, 2, 0, 'per_1m_tokens')`,
		`INSERT INTO provider_wires (provider_id, wire) VALUES ('p1', 'openai/chat')`,
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
	if len(got.Endpoints) != 1 || got.Endpoints[0].Wire != "openai/chat" || got.Endpoints[0].Endpoint != "https://x.example.com/chat/completions" {
		t.Errorf("endpoints = %v, want openai/chat @ x.example.com/chat/completions", got.Endpoints)
	}
	for _, old := range []string{"services", "service_models", "service_wires"} {
		if ok, _ := s.tableExists(old); ok {
			t.Errorf("table %s should be gone after rename", old)
		}
	}
	assertCanonicalProviderSchema(t, s)
}

// An interrupted services rename can leave both old and new wire tables. Fold
// both sets into endpoints before retiring either table.
func TestServiceWireRenameMergesCoexistingTables(t *testing.T) {
	s := openTestStore(t)
	rewindProviderSchema(t, s, true)

	stmts := []string{
		`INSERT INTO providers (id, name, vendor, adapter, base_url, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at)
			VALUES ('p1', 'interrupted', '', 'openai-compatible', 'https://x.example.com/v1', 0, 1, 1, '', 'sk-x', 0, '{}', 100, 100)`,
		`INSERT INTO provider_wires (provider_id, wire) VALUES ('p1', 'openai/chat')`,
		`CREATE TABLE service_wires (
			service_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			wire       TEXT NOT NULL,
			PRIMARY KEY (service_id, wire)
		)`,
		`INSERT INTO service_wires (service_id, wire) VALUES ('p1', 'openai/models')`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("setup %s: %v", stmt, err)
		}
	}

	if err := s.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got, err := s.GetProvider("p1")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	want := map[string]string{
		"openai/chat":   "https://x.example.com/v1/chat/completions",
		"openai/models": "https://x.example.com/v1",
	}
	if len(got.Endpoints) != len(want) {
		t.Fatalf("endpoints = %+v, want %d", got.Endpoints, len(want))
	}
	for _, endpoint := range got.Endpoints {
		if endpoint.Endpoint != want[endpoint.Wire] {
			t.Errorf("endpoint %q = %q, want %q", endpoint.Wire, endpoint.Endpoint, want[endpoint.Wire])
		}
	}
	if has, err := s.tableExists("service_wires"); err != nil || has {
		t.Fatalf("service_wires present = %v, err = %v; want retired", has, err)
	}
	assertCanonicalProviderSchema(t, s)
}
