package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Payload is the captured request/response body pair for a single call,
// stored 1:1 with calls.id. Header maps are already redacted by the caller.
type Payload struct {
	CallID          int64
	ReqHeaders      map[string]string
	ReqBody         []byte
	ReqContentType  string
	ReqTruncated    bool
	RespHeaders     map[string]string
	RespBody        []byte
	RespContentType string
	RespTruncated   bool
	CreatedAt       time.Time
}

// SavePayload upserts the payload for a call (INSERT OR REPLACE, keyed by
// call_id) and then prunes the table to the newest retain rows. A retain of 0
// or less disables pruning. It is safe to call concurrently: all work goes
// through the shared *sql.DB which serializes writes.
func (s *Store) SavePayload(p Payload, retain int) error {
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
		`INSERT OR REPLACE INTO payloads
		 (call_id, req_headers, req_body, req_content_type, req_truncated,
		  resp_headers, resp_body, resp_content_type, resp_truncated, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.CallID, reqHeaders, p.ReqBody, p.ReqContentType, boolToInt(p.ReqTruncated),
		respHeaders, p.RespBody, p.RespContentType, boolToInt(p.RespTruncated), created.UnixMilli(),
	); err != nil {
		return fmt.Errorf("store: save payload: %w", err)
	}

	if retain > 0 {
		if _, err := s.db.Exec(
			`DELETE FROM payloads WHERE call_id NOT IN (
			   SELECT call_id FROM payloads ORDER BY call_id DESC LIMIT ?
			 )`,
			retain,
		); err != nil {
			return fmt.Errorf("store: prune payloads: %w", err)
		}
	}
	return nil
}

// GetPayload returns the payload for a call, or ErrNotFound if none was
// captured.
func (s *Store) GetPayload(callID int64) (Payload, error) {
	row := s.db.QueryRow(
		`SELECT call_id, req_headers, req_body, req_content_type, req_truncated,
		        resp_headers, resp_body, resp_content_type, resp_truncated, created_at
		 FROM payloads WHERE call_id = ?`,
		callID,
	)

	var (
		p             Payload
		reqHeaders    string
		respHeaders   string
		reqTruncated  int
		respTruncated int
		createdMillis int64
	)
	err := row.Scan(
		&p.CallID, &reqHeaders, &p.ReqBody, &p.ReqContentType, &reqTruncated,
		&respHeaders, &p.RespBody, &p.RespContentType, &respTruncated, &createdMillis,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Payload{}, fmt.Errorf("store: payload %d: %w", callID, ErrNotFound)
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
	p.ReqTruncated = reqTruncated != 0
	p.RespTruncated = respTruncated != 0
	p.CreatedAt = time.UnixMilli(createdMillis)
	return p, nil
}

// HasPayloads reports, for each call id, whether a payload row exists. The
// returned map only includes ids that have a payload (true); absent ids are
// implicitly false. It is a single indexed lookup over the primary key.
func (s *Store) HasPayloads(callIDs []int64) (map[int64]bool, error) {
	out := make(map[int64]bool, len(callIDs))
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

	query := "SELECT call_id FROM payloads WHERE call_id IN (" + string(placeholders) + ")"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: has payloads: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
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
