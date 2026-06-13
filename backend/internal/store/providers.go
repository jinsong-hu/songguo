package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Provider is one configured vendor: a single API key plus the set of endpoints
// it serves and the models it prices. An endpoint binds one wire to a base URL
// (1:1 with the wire); the config manager groups a provider's endpoints by
// (base_url, adapter) and projects each group into a config.Vendor for routing.
// The key is stored plaintext at rest (it must be replayed upstream, so it
// cannot be hashed); it is never serialized to the API in the clear, only masked.
type Provider struct {
	ID        string
	Name      string
	Vendor    string // catalog vendor grouping, for display only
	Priority  int
	Weight    int
	Enabled   bool
	CatalogID string // provenance: which catalog preset this came from, if any
	APIKey    string
	// AllowUnmatched forwards requests whose path matches no enabled wire
	// (metered zero, confidence unknown) instead of denying them.
	AllowUnmatched bool
	Quirks         map[string]string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Models         []ProviderModel
	Endpoints      []ProviderEndpoint
}

// ProviderEndpoint binds one wire to a base URL and the auth scheme (adapter)
// that base URL expects. A provider holds several; endpoints sharing a
// (base_url, adapter) are grouped into one routing vendor by the config manager.
type ProviderEndpoint struct {
	Wire    string
	BaseURL string
	Adapter string
}

// ProviderModel is a model a provider serves, with its true per-model price.
// CachedInput is the rate for cache-hit input tokens; 0 means "charge the full
// Input rate" (no cache discount).
type ProviderModel struct {
	Model       string
	Input       float64
	Output      float64
	CachedInput float64
	Unit        string
}

// NewProvider describes a provider to create. APIKey carries the plaintext key.
type NewProvider struct {
	Name           string
	Vendor         string
	Priority       int
	Weight         int
	Enabled        bool
	CatalogID      string
	AllowUnmatched bool
	Quirks         map[string]string
	APIKey         string
	Models         []ProviderModel
	Endpoints      []ProviderEndpoint
}

// ProviderUpdate carries optional scalar updates; nil pointers leave a field
// unchanged. When Models or Endpoints is non-nil it fully replaces that set.
type ProviderUpdate struct {
	Name           *string
	Vendor         *string
	Priority       *int
	Weight         *int
	Enabled        *bool
	AllowUnmatched *bool
	APIKey         *string
	Quirks         *map[string]string
	Models         []ProviderModel
	Endpoints      []ProviderEndpoint
}

// AppSettings is the gateway-wide settings singleton.
type AppSettings struct {
	Capture         bool
	CaptureMaxBytes int
	CaptureRetain   int
}

// CountProviders returns the number of configured providers.
func (s *Store) CountProviders() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM providers`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count providers: %w", err)
	}
	return n, nil
}

// ListProviders returns all providers, newest first, each with its models and
// endpoints assembled. It uses bulk queries rather than per-provider follow-ups.
func (s *Store) ListProviders() ([]Provider, error) {
	rows, err := s.db.Query(`SELECT id, name, vendor, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at
		FROM providers ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list providers: %w", err)
	}
	defer rows.Close()

	var pvds []Provider
	index := make(map[string]int)
	for rows.Next() {
		pvd, err := scanProvider(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan provider: %w", err)
		}
		index[pvd.ID] = len(pvds)
		pvds = append(pvds, pvd)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list providers: %w", err)
	}
	if len(pvds) == 0 {
		return nil, nil
	}

	modelRows, err := s.db.Query(`SELECT provider_id, model, input, output, cached_input, unit FROM provider_models ORDER BY model`)
	if err != nil {
		return nil, fmt.Errorf("store: list models: %w", err)
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var (
			m   ProviderModel
			pid string
		)
		if err := modelRows.Scan(&pid, &m.Model, &m.Input, &m.Output, &m.CachedInput, &m.Unit); err != nil {
			return nil, fmt.Errorf("store: scan model: %w", err)
		}
		if i, ok := index[pid]; ok {
			pvds[i].Models = append(pvds[i].Models, m)
		}
	}
	if err := modelRows.Err(); err != nil {
		return nil, fmt.Errorf("store: list models: %w", err)
	}

	epRows, err := s.db.Query(`SELECT provider_id, wire, base_url, adapter FROM provider_endpoints ORDER BY wire`)
	if err != nil {
		return nil, fmt.Errorf("store: list endpoints: %w", err)
	}
	defer epRows.Close()
	for epRows.Next() {
		var (
			ep  ProviderEndpoint
			pid string
		)
		if err := epRows.Scan(&pid, &ep.Wire, &ep.BaseURL, &ep.Adapter); err != nil {
			return nil, fmt.Errorf("store: scan endpoint: %w", err)
		}
		if i, ok := index[pid]; ok {
			pvds[i].Endpoints = append(pvds[i].Endpoints, ep)
		}
	}
	if err := epRows.Err(); err != nil {
		return nil, fmt.Errorf("store: list endpoints: %w", err)
	}

	return pvds, nil
}

// GetProvider returns one provider with its models and endpoints.
func (s *Store) GetProvider(id string) (Provider, error) {
	row := s.db.QueryRow(`SELECT id, name, vendor, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at
		FROM providers WHERE id = ?`, id)
	pvd, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, fmt.Errorf("store: provider %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Provider{}, fmt.Errorf("store: get provider: %w", err)
	}

	modelRows, err := s.db.Query(`SELECT model, input, output, cached_input, unit FROM provider_models WHERE provider_id = ? ORDER BY model`, id)
	if err != nil {
		return Provider{}, fmt.Errorf("store: get models: %w", err)
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var m ProviderModel
		if err := modelRows.Scan(&m.Model, &m.Input, &m.Output, &m.CachedInput, &m.Unit); err != nil {
			return Provider{}, fmt.Errorf("store: scan model: %w", err)
		}
		pvd.Models = append(pvd.Models, m)
	}
	if err := modelRows.Err(); err != nil {
		return Provider{}, fmt.Errorf("store: get models: %w", err)
	}

	epRows, err := s.db.Query(`SELECT wire, base_url, adapter FROM provider_endpoints WHERE provider_id = ? ORDER BY wire`, id)
	if err != nil {
		return Provider{}, fmt.Errorf("store: get endpoints: %w", err)
	}
	defer epRows.Close()
	for epRows.Next() {
		var ep ProviderEndpoint
		if err := epRows.Scan(&ep.Wire, &ep.BaseURL, &ep.Adapter); err != nil {
			return Provider{}, fmt.Errorf("store: scan endpoint: %w", err)
		}
		pvd.Endpoints = append(pvd.Endpoints, ep)
	}
	if err := epRows.Err(); err != nil {
		return Provider{}, fmt.Errorf("store: get endpoints: %w", err)
	}

	return pvd, nil
}

// scanProvider reads the scalar provider columns from a row.
func scanProvider(sc interface{ Scan(...any) error }) (Provider, error) {
	var (
		pvd            Provider
		enabled        int64
		allowUnmatched int64
		quirks         string
		createdAt      int64
		updatedAt      int64
	)
	if err := sc.Scan(&pvd.ID, &pvd.Name, &pvd.Vendor,
		&pvd.Priority, &pvd.Weight, &enabled, &pvd.CatalogID, &pvd.APIKey, &allowUnmatched, &quirks, &createdAt, &updatedAt); err != nil {
		return Provider{}, err
	}
	pvd.Enabled = enabled != 0
	pvd.AllowUnmatched = allowUnmatched != 0
	if quirks != "" {
		if err := json.Unmarshal([]byte(quirks), &pvd.Quirks); err != nil {
			return Provider{}, fmt.Errorf("decode quirks: %w", err)
		}
	}
	pvd.CreatedAt = time.Unix(createdAt, 0)
	pvd.UpdatedAt = time.Unix(updatedAt, 0)
	return pvd, nil
}

// CreateProvider inserts a provider plus its models and wires in one transaction
// and returns the assembled row.
func (s *Store) CreateProvider(np NewProvider) (Provider, error) {
	id, err := randID()
	if err != nil {
		return Provider{}, err
	}
	weight := np.Weight
	if weight <= 0 {
		weight = 1
	}
	quirks, err := encodeQuirks(np.Quirks)
	if err != nil {
		return Provider{}, err
	}
	now := time.Now()

	tx, err := s.db.Begin()
	if err != nil {
		return Provider{}, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	// base_url and adapter are vestigial provider columns (NOT NULL); base_url
	// and auth now live per-endpoint. Insert harmless placeholders.
	_, err = tx.Exec(`INSERT INTO providers (id, name, vendor, base_url, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at)
		VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, np.Name, np.Vendor, np.Priority, weight, boolToInt(np.Enabled), np.CatalogID, np.APIKey, boolToInt(np.AllowUnmatched), quirks, now.Unix(), now.Unix())
	if err != nil {
		return Provider{}, fmt.Errorf("store: insert provider: %w", err)
	}

	if err := insertModels(tx, id, np.Models); err != nil {
		return Provider{}, err
	}

	if err := insertEndpoints(tx, id, np.Endpoints); err != nil {
		return Provider{}, err
	}

	if err := tx.Commit(); err != nil {
		return Provider{}, fmt.Errorf("store: commit: %w", err)
	}
	return s.GetProvider(id)
}

// UpdateProvider applies the non-nil scalar fields and, when Models or Wires is
// non-nil, replaces that set. It returns the updated provider.
func (s *Store) UpdateProvider(id string, upd ProviderUpdate) (Provider, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Provider{}, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	var (
		sets []string
		args []any
	)
	if upd.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *upd.Name)
	}
	if upd.Vendor != nil {
		sets = append(sets, "vendor = ?")
		args = append(args, *upd.Vendor)
	}
	if upd.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *upd.Priority)
	}
	if upd.Weight != nil {
		w := *upd.Weight
		if w <= 0 {
			w = 1
		}
		sets = append(sets, "weight = ?")
		args = append(args, w)
	}
	if upd.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*upd.Enabled))
	}
	if upd.AllowUnmatched != nil {
		sets = append(sets, "allow_unmatched = ?")
		args = append(args, boolToInt(*upd.AllowUnmatched))
	}
	if upd.APIKey != nil {
		sets = append(sets, "api_key = ?")
		args = append(args, *upd.APIKey)
	}
	if upd.Quirks != nil {
		quirks, err := encodeQuirks(*upd.Quirks)
		if err != nil {
			return Provider{}, err
		}
		sets = append(sets, "quirks = ?")
		args = append(args, quirks)
	}

	// Always bump updated_at when anything changes.
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().Unix())

	query := "UPDATE providers SET " + sets[0]
	for _, st := range sets[1:] {
		query += ", " + st
	}
	query += " WHERE id = ?"
	args = append(args, id)

	res, err := tx.Exec(query, args...)
	if err != nil {
		return Provider{}, fmt.Errorf("store: update provider: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Provider{}, fmt.Errorf("store: provider %q: %w", id, ErrNotFound)
	}

	if upd.Models != nil {
		if _, err := tx.Exec(`DELETE FROM provider_models WHERE provider_id = ?`, id); err != nil {
			return Provider{}, fmt.Errorf("store: clear models: %w", err)
		}
		if err := insertModels(tx, id, upd.Models); err != nil {
			return Provider{}, err
		}
	}

	if upd.Endpoints != nil {
		if _, err := tx.Exec(`DELETE FROM provider_endpoints WHERE provider_id = ?`, id); err != nil {
			return Provider{}, fmt.Errorf("store: clear endpoints: %w", err)
		}
		if err := insertEndpoints(tx, id, upd.Endpoints); err != nil {
			return Provider{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return Provider{}, fmt.Errorf("store: commit: %w", err)
	}
	return s.GetProvider(id)
}

// DeleteProvider removes a provider; its models and wires cascade.
func (s *Store) DeleteProvider(id string) error {
	res, err := s.db.Exec(`DELETE FROM providers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete provider: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: provider %q: %w", id, ErrNotFound)
	}
	return nil
}

// GetAppSettings returns the settings singleton, falling back to defaults.
func (s *Store) GetAppSettings() (AppSettings, error) {
	row := s.db.QueryRow(`SELECT capture, capture_max_bytes, capture_retain FROM app_settings WHERE id = 1`)
	var (
		as      AppSettings
		capture int64
	)
	err := row.Scan(&capture, &as.CaptureMaxBytes, &as.CaptureRetain)
	if errors.Is(err, sql.ErrNoRows) {
		return AppSettings{CaptureMaxBytes: 32768, CaptureRetain: 10000}, nil
	}
	if err != nil {
		return AppSettings{}, fmt.Errorf("store: get app settings: %w", err)
	}
	as.Capture = capture != 0
	return as, nil
}

// UpdateAppSettings overwrites the settings singleton.
func (s *Store) UpdateAppSettings(as AppSettings) error {
	if _, err := s.db.Exec(`UPDATE app_settings SET capture = ?, capture_max_bytes = ?, capture_retain = ? WHERE id = 1`,
		boolToInt(as.Capture), as.CaptureMaxBytes, as.CaptureRetain); err != nil {
		return fmt.Errorf("store: update app settings: %w", err)
	}
	return nil
}

// insertModels writes a provider's model rows within a transaction.
func insertModels(tx *sql.Tx, providerID string, models []ProviderModel) error {
	for _, m := range models {
		if m.Model == "" {
			continue
		}
		unit := m.Unit
		if unit == "" {
			unit = "per_1m_tokens"
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO provider_models (provider_id, model, input, output, cached_input, unit) VALUES (?, ?, ?, ?, ?, ?)`,
			providerID, m.Model, m.Input, m.Output, m.CachedInput, unit); err != nil {
			return fmt.Errorf("store: insert model: %w", err)
		}
	}
	return nil
}

// insertEndpoints writes a provider's endpoint rows (one wire bound to a base
// URL + adapter) within a transaction.
func insertEndpoints(tx *sql.Tx, providerID string, endpoints []ProviderEndpoint) error {
	for _, ep := range endpoints {
		if ep.Wire == "" {
			continue
		}
		adapter := ep.Adapter
		if adapter == "" {
			adapter = "openai-compatible"
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO provider_endpoints (provider_id, wire, base_url, adapter) VALUES (?, ?, ?, ?)`,
			providerID, ep.Wire, ep.BaseURL, adapter); err != nil {
			return fmt.Errorf("store: insert endpoint: %w", err)
		}
	}
	return nil
}

// encodeQuirks serializes a quirks map for storage; nil encodes as '{}'.
func encodeQuirks(q map[string]string) (string, error) {
	if len(q) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(q)
	if err != nil {
		return "", fmt.Errorf("store: encode quirks: %w", err)
	}
	return string(b), nil
}
