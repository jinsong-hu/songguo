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

// SessionRequest is the request-side capture for one call in a session. It is
// intentionally narrower than Payload so session prompt reconstruction never
// reads the usually much larger response BLOBs from SQLite.
type SessionRequest struct {
	CallID         string
	Wire           string
	ReqHeaders     map[string]string
	ReqBody        []byte
	ReqContentType string
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

// SessionRequests returns captured request bodies among a session's latest
// 1000 calls, ordered oldest-first to match QueryCalls on the detail endpoint.
// Calls without a raw row are absent, which is expected after raw retention
// has pruned captures while the longer-lived session summary still exists.
func (s *Store) SessionRequests(sessionID string) ([]SessionRequest, error) {
	rows, err := s.db.Query(
		`SELECT c.id, c.wire, r.req_headers, r.req_body, r.req_content_type
		   FROM (
		         SELECT id, wire, ts
		           FROM calls
		          WHERE session_id = ?
		          ORDER BY ts DESC, id DESC
		          LIMIT ?
		        ) c
		   JOIN raw r ON r.call_id = c.id
		  ORDER BY c.ts ASC, c.id ASC`, sessionID, maxCallsLimit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: session requests: %w", err)
	}
	defer rows.Close()

	var out []SessionRequest
	for rows.Next() {
		var (
			r          SessionRequest
			reqHeaders string
		)
		if err := rows.Scan(&r.CallID, &r.Wire, &reqHeaders, &r.ReqBody, &r.ReqContentType); err != nil {
			return nil, fmt.Errorf("store: scan session request: %w", err)
		}
		if err := json.Unmarshal([]byte(reqHeaders), &r.ReqHeaders); err != nil {
			return nil, fmt.Errorf("store: decode session request headers: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: session request rows: %w", err)
	}
	return out, nil
}
