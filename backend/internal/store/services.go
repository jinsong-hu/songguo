package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Service is one configured upstream: an adapter speaking a wire protocol, a
// base URL, a single API key, and the models it serves with their per-model
// prices. It is the SQLite-backed successor to the file-based vendor config;
// the config manager projects each enabled, complete service into a
// config.Vendor for routing. The key is stored plaintext at rest (it must be
// replayed upstream, so it cannot be hashed); it is never serialized to the
// API in the clear, only masked.
type Service struct {
	ID        string
	Name      string
	Vendor    string // catalog vendor grouping, for display only
	Adapter   string
	BaseURL   string
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
	Models         []ServiceModel
	Wires          []string
}

// ServiceModel is a model a service serves, with its true per-model price.
// CachedInput is the rate for cache-hit input tokens; 0 means "charge the full
// Input rate" (no cache discount).
type ServiceModel struct {
	Model       string
	Input       float64
	Output      float64
	CachedInput float64
	Unit        string
}

// NewService describes a service to create. APIKey carries the plaintext key.
type NewService struct {
	Name           string
	Vendor         string
	Adapter        string
	BaseURL        string
	Priority       int
	Weight         int
	Enabled        bool
	CatalogID      string
	AllowUnmatched bool
	Quirks         map[string]string
	APIKey         string
	Models         []ServiceModel
	Wires          []string
}

// ServiceUpdate carries optional scalar updates; nil pointers leave a field
// unchanged. When Models or Wires is non-nil it fully replaces that set.
type ServiceUpdate struct {
	Name           *string
	Vendor         *string
	Adapter        *string
	BaseURL        *string
	Priority       *int
	Weight         *int
	Enabled        *bool
	AllowUnmatched *bool
	APIKey         *string
	Quirks         *map[string]string
	Models         []ServiceModel
	Wires          []string
}

// AppSettings is the gateway-wide settings singleton.
type AppSettings struct {
	Capture         bool
	CaptureMaxBytes int
	CaptureRetain   int
}

// CountServices returns the number of configured services.
func (s *Store) CountServices() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM services`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count services: %w", err)
	}
	return n, nil
}

// ListServices returns all services, newest first, each with its models and
// wires assembled. It uses bulk queries rather than per-service follow-ups.
func (s *Store) ListServices() ([]Service, error) {
	rows, err := s.db.Query(`SELECT id, name, vendor, adapter, base_url, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at
		FROM services ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list services: %w", err)
	}
	defer rows.Close()

	var svcs []Service
	index := make(map[string]int)
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan service: %w", err)
		}
		index[svc.ID] = len(svcs)
		svcs = append(svcs, svc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list services: %w", err)
	}
	if len(svcs) == 0 {
		return nil, nil
	}

	modelRows, err := s.db.Query(`SELECT service_id, model, input, output, cached_input, unit FROM service_models ORDER BY model`)
	if err != nil {
		return nil, fmt.Errorf("store: list models: %w", err)
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var (
			m   ServiceModel
			sid string
		)
		if err := modelRows.Scan(&sid, &m.Model, &m.Input, &m.Output, &m.CachedInput, &m.Unit); err != nil {
			return nil, fmt.Errorf("store: scan model: %w", err)
		}
		if i, ok := index[sid]; ok {
			svcs[i].Models = append(svcs[i].Models, m)
		}
	}
	if err := modelRows.Err(); err != nil {
		return nil, fmt.Errorf("store: list models: %w", err)
	}

	wireRows, err := s.db.Query(`SELECT service_id, wire FROM service_wires ORDER BY wire`)
	if err != nil {
		return nil, fmt.Errorf("store: list wires: %w", err)
	}
	defer wireRows.Close()
	for wireRows.Next() {
		var sid, w string
		if err := wireRows.Scan(&sid, &w); err != nil {
			return nil, fmt.Errorf("store: scan wire: %w", err)
		}
		if i, ok := index[sid]; ok {
			svcs[i].Wires = append(svcs[i].Wires, w)
		}
	}
	if err := wireRows.Err(); err != nil {
		return nil, fmt.Errorf("store: list wires: %w", err)
	}

	return svcs, nil
}

// GetService returns one service with its models and wires.
func (s *Store) GetService(id string) (Service, error) {
	row := s.db.QueryRow(`SELECT id, name, vendor, adapter, base_url, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at
		FROM services WHERE id = ?`, id)
	svc, err := scanService(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Service{}, fmt.Errorf("store: service %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Service{}, fmt.Errorf("store: get service: %w", err)
	}

	modelRows, err := s.db.Query(`SELECT model, input, output, cached_input, unit FROM service_models WHERE service_id = ? ORDER BY model`, id)
	if err != nil {
		return Service{}, fmt.Errorf("store: get models: %w", err)
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var m ServiceModel
		if err := modelRows.Scan(&m.Model, &m.Input, &m.Output, &m.CachedInput, &m.Unit); err != nil {
			return Service{}, fmt.Errorf("store: scan model: %w", err)
		}
		svc.Models = append(svc.Models, m)
	}
	if err := modelRows.Err(); err != nil {
		return Service{}, fmt.Errorf("store: get models: %w", err)
	}

	wireRows, err := s.db.Query(`SELECT wire FROM service_wires WHERE service_id = ? ORDER BY wire`, id)
	if err != nil {
		return Service{}, fmt.Errorf("store: get wires: %w", err)
	}
	defer wireRows.Close()
	for wireRows.Next() {
		var w string
		if err := wireRows.Scan(&w); err != nil {
			return Service{}, fmt.Errorf("store: scan wire: %w", err)
		}
		svc.Wires = append(svc.Wires, w)
	}
	if err := wireRows.Err(); err != nil {
		return Service{}, fmt.Errorf("store: get wires: %w", err)
	}

	return svc, nil
}

// scanService reads the scalar service columns from a row.
func scanService(sc interface{ Scan(...any) error }) (Service, error) {
	var (
		svc            Service
		enabled        int64
		allowUnmatched int64
		quirks         string
		createdAt      int64
		updatedAt      int64
	)
	if err := sc.Scan(&svc.ID, &svc.Name, &svc.Vendor, &svc.Adapter, &svc.BaseURL,
		&svc.Priority, &svc.Weight, &enabled, &svc.CatalogID, &svc.APIKey, &allowUnmatched, &quirks, &createdAt, &updatedAt); err != nil {
		return Service{}, err
	}
	svc.Enabled = enabled != 0
	svc.AllowUnmatched = allowUnmatched != 0
	if quirks != "" {
		if err := json.Unmarshal([]byte(quirks), &svc.Quirks); err != nil {
			return Service{}, fmt.Errorf("decode quirks: %w", err)
		}
	}
	svc.CreatedAt = time.Unix(createdAt, 0)
	svc.UpdatedAt = time.Unix(updatedAt, 0)
	return svc, nil
}

// CreateService inserts a service plus its models and wires in one transaction
// and returns the assembled row.
func (s *Store) CreateService(ns NewService) (Service, error) {
	id, err := randID()
	if err != nil {
		return Service{}, err
	}
	weight := ns.Weight
	if weight <= 0 {
		weight = 1
	}
	adapter := ns.Adapter
	if adapter == "" {
		adapter = "openai-compatible"
	}
	quirks, err := encodeQuirks(ns.Quirks)
	if err != nil {
		return Service{}, err
	}
	now := time.Now()

	tx, err := s.db.Begin()
	if err != nil {
		return Service{}, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`INSERT INTO services (id, name, vendor, adapter, base_url, priority, weight, enabled, catalog_id, api_key, allow_unmatched, quirks, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, ns.Name, ns.Vendor, adapter, ns.BaseURL, ns.Priority, weight, boolToInt(ns.Enabled), ns.CatalogID, ns.APIKey, boolToInt(ns.AllowUnmatched), quirks, now.Unix(), now.Unix())
	if err != nil {
		return Service{}, fmt.Errorf("store: insert service: %w", err)
	}

	if err := insertModels(tx, id, ns.Models); err != nil {
		return Service{}, err
	}

	if err := insertWires(tx, id, ns.Wires); err != nil {
		return Service{}, err
	}

	if err := tx.Commit(); err != nil {
		return Service{}, fmt.Errorf("store: commit: %w", err)
	}
	return s.GetService(id)
}

// UpdateService applies the non-nil scalar fields and, when Models or Wires is
// non-nil, replaces that set. It returns the updated service.
func (s *Store) UpdateService(id string, upd ServiceUpdate) (Service, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Service{}, fmt.Errorf("store: begin: %w", err)
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
	if upd.Adapter != nil {
		sets = append(sets, "adapter = ?")
		args = append(args, *upd.Adapter)
	}
	if upd.BaseURL != nil {
		sets = append(sets, "base_url = ?")
		args = append(args, *upd.BaseURL)
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
			return Service{}, err
		}
		sets = append(sets, "quirks = ?")
		args = append(args, quirks)
	}

	// Always bump updated_at when anything changes.
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().Unix())

	query := "UPDATE services SET " + sets[0]
	for _, st := range sets[1:] {
		query += ", " + st
	}
	query += " WHERE id = ?"
	args = append(args, id)

	res, err := tx.Exec(query, args...)
	if err != nil {
		return Service{}, fmt.Errorf("store: update service: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Service{}, fmt.Errorf("store: service %q: %w", id, ErrNotFound)
	}

	if upd.Models != nil {
		if _, err := tx.Exec(`DELETE FROM service_models WHERE service_id = ?`, id); err != nil {
			return Service{}, fmt.Errorf("store: clear models: %w", err)
		}
		if err := insertModels(tx, id, upd.Models); err != nil {
			return Service{}, err
		}
	}

	if upd.Wires != nil {
		if _, err := tx.Exec(`DELETE FROM service_wires WHERE service_id = ?`, id); err != nil {
			return Service{}, fmt.Errorf("store: clear wires: %w", err)
		}
		if err := insertWires(tx, id, upd.Wires); err != nil {
			return Service{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return Service{}, fmt.Errorf("store: commit: %w", err)
	}
	return s.GetService(id)
}

// DeleteService removes a service; its models and wires cascade.
func (s *Store) DeleteService(id string) error {
	res, err := s.db.Exec(`DELETE FROM services WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete service: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: service %q: %w", id, ErrNotFound)
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

// insertModels writes a service's model rows within a transaction.
func insertModels(tx *sql.Tx, serviceID string, models []ServiceModel) error {
	for _, m := range models {
		if m.Model == "" {
			continue
		}
		unit := m.Unit
		if unit == "" {
			unit = "per_1m_tokens"
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO service_models (service_id, model, input, output, cached_input, unit) VALUES (?, ?, ?, ?, ?, ?)`,
			serviceID, m.Model, m.Input, m.Output, m.CachedInput, unit); err != nil {
			return fmt.Errorf("store: insert model: %w", err)
		}
	}
	return nil
}

// insertWires writes a service's wire-allowlist rows within a transaction.
func insertWires(tx *sql.Tx, serviceID string, wires []string) error {
	for _, w := range wires {
		if w == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO service_wires (service_id, wire) VALUES (?, ?)`,
			serviceID, w); err != nil {
			return fmt.Errorf("store: insert wire: %w", err)
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
