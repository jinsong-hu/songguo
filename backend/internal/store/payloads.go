package store

import (
	"context"
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
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
//
// The cover is a reduction, not a bound, so the read is bounded separately:
// bodies are admitted newest first while their stored size fits maxBytes, and
// the oldest ones past it are skipped and counted in omitted. What is kept is a
// contiguous newest stretch, never a sample with holes in it. At least one body
// is always read, however large, so a session is never blank. maxBytes <= 0
// means no bound.
//
// The bodies are fetched one point lookup at a time, in the cover's order. A
// single query with ORDER BY made SQLite copy every selected BLOB into a temp
// B-tree to sort it — on disk, at the size of the BLOBs — and held one read
// transaction open for the whole scan, which pins the WAL so no checkpoint can
// complete. That is how one page view hung the production host (see the
// history note in messagecover.go). ctx stops the read between bodies, so a
// closed tab stops paying for it.
func (s *Store) SessionRequests(ctx context.Context, sessionID string, maxBytes int64) (out []SessionRequest, omitted int, err error) {
	cover, err := s.SessionCover(sessionID)
	if err != nil {
		return nil, 0, err
	}
	if len(cover) == 0 {
		return nil, 0, nil
	}

	ids := make([]string, len(cover))
	for i, c := range cover {
		ids[i] = c.CallID
	}
	sizes, err := s.RequestBodySizes(ctx, ids)
	if err != nil {
		return nil, 0, err
	}

	// Walk newest first; the first body that does not fit ends admission, and
	// everything older is omitted with it.
	first := len(cover)
	var total int64
	for i := len(cover) - 1; i >= 0; i-- {
		size, ok := sizes[cover[i].CallID]
		if !ok {
			continue // capture already pruned: nothing to read or to omit
		}
		if maxBytes > 0 && first < len(cover) && total+size > maxBytes {
			for _, c := range cover[:i+1] {
				if _, ok := sizes[c.CallID]; ok {
					omitted++
				}
			}
			break
		}
		total += size
		first = i
	}

	for _, c := range cover[first:] {
		if _, ok := sizes[c.CallID]; !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		var (
			r          = SessionRequest{CallID: c.CallID}
			reqHeaders string
		)
		err := s.db.QueryRowContext(ctx,
			`SELECT c.wire, r.req_headers, r.req_body, r.req_content_type
			   FROM raw r
			   JOIN calls c ON c.id = r.call_id
			  WHERE r.call_id = ? AND c.session_id = ?`, c.CallID, sessionID,
		).Scan(&r.Wire, &reqHeaders, &r.ReqBody, &r.ReqContentType)
		if errors.Is(err, sql.ErrNoRows) {
			continue // pruned between the size read and this one
		}
		if err != nil {
			return nil, 0, fmt.Errorf("store: session request: %w", err)
		}
		if err := json.Unmarshal([]byte(reqHeaders), &r.ReqHeaders); err != nil {
			return nil, 0, fmt.Errorf("store: decode session request headers: %w", err)
		}
		out = append(out, r)
	}
	return out, omitted, nil
}

// RequestBodySizes returns the stored size of each call's captured request
// body, for the ids that have a capture. It never reads a body: length() of a
// BLOB column is answered from the record header on the row's own page, so the
// overflow chain that holds a multi-MB body is not touched. That is what lets a
// reader decide what it can afford before paying for it.
func (s *Store) RequestBodySizes(ctx context.Context, callIDs []string) (map[string]int64, error) {
	out := make(map[string]int64, len(callIDs))
	if len(callIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(callIDs))
	for i, id := range callIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT call_id, length(req_body) FROM raw
		  WHERE call_id IN (?`+strings.Repeat(",?", len(callIDs)-1)+`)`, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: request body sizes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id   string
			size sql.NullInt64
		)
		if err := rows.Scan(&id, &size); err != nil {
			return nil, fmt.Errorf("store: scan request body size: %w", err)
		}
		out[id] = size.Int64
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: request body sizes: %w", err)
	}
	return out, nil
}
