package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Payload is the captured request/response body pair for a single call, stored
// 1:1 with calls.id in the `raw` table. Header maps are already redacted by the
// caller. See docs/arch-gateway.md — raw is capture-gated and pruned at 7 days.
type Payload struct {
	CallID          string
	ReqHeaders      map[string]string
	ReqBody         []byte
	ReqContentType  string
	RespHeaders     map[string]string
	RespBody        []byte
	RespContentType string
	CreatedAt       time.Time
}

// SavePayload upserts the raw body row for a call (INSERT OR REPLACE, keyed by
// call_id). It is safe to call concurrently: all work goes through the shared
// *sql.DB which serializes writes.
func (s *Store) SavePayload(p Payload) error {
	reqHeaders, err := marshalStringMap(p.ReqHeaders)
	if err != nil {
		return fmt.Errorf("store: encode req headers: %w", err)
	}
	respHeaders, err := marshalStringMap(p.RespHeaders)
	if err != nil {
		return fmt.Errorf("store: encode resp headers: %w", err)
	}

	created := p.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}

	if _, err := s.db.Exec(
		`INSERT OR REPLACE INTO raw
		 (call_id, req_headers, req_body, req_content_type,
		  resp_headers, resp_body, resp_content_type, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.CallID, reqHeaders, p.ReqBody, p.ReqContentType,
		respHeaders, p.RespBody, p.RespContentType, created.UnixMilli(),
	); err != nil {
		return fmt.Errorf("store: save payload: %w", err)
	}
	return nil
}

// GetPayload returns the raw body row for a call, or ErrNotFound if none was
// captured.
func (s *Store) GetPayload(callID string) (Payload, error) {
	row := s.db.QueryRow(
		`SELECT call_id, req_headers, req_body, req_content_type,
		        resp_headers, resp_body, resp_content_type, created_at
		 FROM raw WHERE call_id = ?`,
		callID,
	)

	var (
		p             Payload
		reqHeaders    string
		respHeaders   string
		createdMillis int64
	)
	err := row.Scan(
		&p.CallID, &reqHeaders, &p.ReqBody, &p.ReqContentType,
		&respHeaders, &p.RespBody, &p.RespContentType, &createdMillis,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Payload{}, fmt.Errorf("store: payload %s: %w", callID, ErrNotFound)
	}
	if err != nil {
		return Payload{}, fmt.Errorf("store: get payload: %w", err)
	}

	if err := json.Unmarshal([]byte(reqHeaders), &p.ReqHeaders); err != nil {
		return Payload{}, fmt.Errorf("store: decode req headers: %w", err)
	}
	if err := json.Unmarshal([]byte(respHeaders), &p.RespHeaders); err != nil {
		return Payload{}, fmt.Errorf("store: decode resp headers: %w", err)
	}
	p.CreatedAt = time.UnixMilli(createdMillis)
	return p, nil
}

// HasPayloads reports, for each call id, whether a raw body row exists. The
// returned map only includes ids that have one (true); absent ids are
// implicitly false. It is a single indexed lookup over the primary key.
func (s *Store) HasPayloads(callIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(callIDs))
	if len(callIDs) == 0 {
		return out, nil
	}

	placeholders := make([]byte, 0, len(callIDs)*2)
	args := make([]any, 0, len(callIDs))
	for i, id := range callIDs {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, id)
	}

	query := "SELECT call_id FROM raw WHERE call_id IN (" + string(placeholders) + ")"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: has payloads: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan has payloads: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: has payloads: %w", err)
	}
	return out, nil
}
