package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// RequestHeaders returns the captured REQUEST HEADERS for the given calls, keyed
// by call id. Calls with no capture are absent.
//
// This exists so a caller that wants a header does not read a body. GetPayload
// selects req_body and resp_body, which on the production gateway average 1.78 MB
// and 50 KB — so using it to read one User-Agent moves megabytes per call out of
// a 70 GB file to produce a short string. The headers live in their own column;
// this reads only that.
//
// > History: sessionData called GetPayload once per entry to fill in client info
// > for rows whose client_name predates that column (30% of the ledger). Opening
// > a 284-call session therefore read ~3 GB of BLOBs to extract 284 User-Agent
// > strings. One batched query over one column replaces it.
func (s *Store) RequestHeaders(callIDs []string) (map[string]map[string]string, error) {
	out := make(map[string]map[string]string, len(callIDs))
	if len(callIDs) == 0 {
		return out, nil
	}

	args := make([]any, 0, len(callIDs))
	placeholders := make([]string, 0, len(callIDs))
	for _, id := range callIDs {
		args = append(args, id)
		placeholders = append(placeholders, "?")
	}

	rows, err := s.db.Query(
		`SELECT call_id, req_headers FROM raw WHERE call_id IN (`+
			strings.Join(placeholders, ",")+`)`, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: request headers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("store: scan request headers: %w", err)
		}
		var h map[string]string
		if err := json.Unmarshal([]byte(raw), &h); err != nil {
			// A single malformed header blob must not fail the whole page; the
			// caller's enrichment is best-effort by construction.
			continue
		}
		out[id] = h
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: request headers: %w", err)
	}
	return out, nil
}

// SessionRequests returns the captured request bodies needed to reconstruct a
// session's conversation, ordered oldest-first to match QueryCalls on the detail
// endpoint. Calls without a raw row are absent, which is expected after raw
// retention has pruned captures while the longer-lived session summary still
// exists.
//
// It reads only the bodies the message cover selects, not every capture in the
// session. An agent re-sends its whole conversation each turn, so most of a
// session's bodies are provable duplicates of a later one; messagecover.go
// decides which from the fingerprint columns alone. The return shape is
// unchanged, so the prompt merge downstream is unaffected — it just receives a
// handful of requests instead of up to a thousand.
//
// > History: this read every capture among the session's latest 1000 calls and
// > handed all of them to the merge. On the production gateway that was 3.1 GB
// > of BLOBs for one 284-turn session — the same conversation re-read 284 times
// > — pulled out of a 70 GB file to render a panel that one body could fill.
func (s *Store) SessionRequests(sessionID string) ([]SessionRequest, error) {
	cover, err := s.SessionCover(sessionID)
	if err != nil {
		return nil, err
	}
	if len(cover) == 0 {
		return nil, nil
	}

	ids := make([]any, 0, len(cover)+1)
	ids = append(ids, sessionID)
	placeholders := make([]string, 0, len(cover))
	for _, c := range cover {
		ids = append(ids, c.CallID)
		placeholders = append(placeholders, "?")
	}

	rows, err := s.db.Query(
		`SELECT c.id, c.wire, r.req_headers, r.req_body, r.req_content_type
		   FROM calls c
		   JOIN raw r ON r.call_id = c.id
		  WHERE c.session_id = ?
		    AND c.id IN (`+strings.Join(placeholders, ",")+`)
		  ORDER BY c.ts ASC, c.id ASC`, ids...,
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
