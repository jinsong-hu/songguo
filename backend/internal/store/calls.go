package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/songguo/songguo/internal/calls"
)

// defaultCallsLimit and maxCallsLimit bound QueryCalls result sizes.
const (
	defaultCallsLimit = 100
	maxCallsLimit     = 1000
)

// CreateCall writes phase 1 of the two-phase call lifecycle (see
// docs/arch-gateway.md): it inserts a row for a call that has just started, keyed
// by the caller-minted UUID e.ID, with status = StatusPending and no end time.
// The gateway later calls FinalizeCall on the same id. Only the identity fields
// known at request-start are persisted here; the rest are filled in at finalize.
// Usage and Tags are JSON-encoded; ts is stored as unix milliseconds.
func (s *Store) CreateCall(e calls.Entry) error {
	if e.ID == "" {
		return fmt.Errorf("store: create call: empty id")
	}
	tagsJSON, err := marshalStringMap(e.Tags)
	if err != nil {
		return fmt.Errorf("store: encode tags: %w", err)
	}
	ts := e.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	modality := e.Modality
	if modality == "" {
		modality = calls.ModalityUnknown
	}
	if _, err := s.db.Exec(
		`INSERT INTO calls
		 (id, ts, ts_end, user_id, model, modality, vendor, credential_id, status, err, usage, cost, latency_ms, ttft_ms, generation_ms, stream, tags, wire, confidence, input_tokens, output_tokens, cached_tokens, session_id, agent_id, parent_agent_id, client_name, client_version, client_os, client_os_version)
		 VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, '', '{}', 0, 0, 0, 0, ?, ?, '', '', 0, 0, 0, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, ts.UnixMilli(), e.UserID, e.Model, string(modality), e.Vendor, e.CredentialID,
		calls.StatusPending, boolToInt(e.Stream), tagsJSON, e.SessionID, e.AgentID, e.ParentAgentID,
		e.ClientName, e.ClientVersion, e.ClientOS, e.ClientOSVersion,
	); err != nil {
		return fmt.Errorf("store: create call: %w", err)
	}
	return nil
}

// FinalizeCall writes phase 2: it updates the row created by CreateCall with the
// call's outcome — status, error, usage, tokens, cost, latency, stream, wire,
// confidence, modality (which a matched wire may refine), and the end time. The
// identity fields written at create are left untouched except model/modality/
// vendor/credential, which finalize overwrites with their resolved values (a
// denial may know them only at the end). Usage and Tags are JSON-encoded.
func (s *Store) FinalizeCall(e calls.Entry) error {
	if e.ID == "" {
		return fmt.Errorf("store: finalize call: empty id")
	}
	usageJSON, err := marshalMap(e.Usage)
	if err != nil {
		return fmt.Errorf("store: encode usage: %w", err)
	}
	tsEnd := e.TSEnd
	if tsEnd.IsZero() {
		tsEnd = time.Now()
	}
	modality := e.Modality
	if modality == "" {
		modality = calls.ModalityUnknown
	}
	if _, err := s.db.Exec(
		`UPDATE calls SET
		   ts_end = ?, model = ?, modality = ?, vendor = ?, credential_id = ?,
		   status = ?, err = ?, usage = ?, cost = ?, latency_ms = ?, ttft_ms = ?,
		   generation_ms = ?, stream = ?,
		   wire = ?, confidence = ?, input_tokens = ?, output_tokens = ?, cached_tokens = ?
		 WHERE id = ?`,
		tsEnd.UnixMilli(), e.Model, string(modality), e.Vendor, e.CredentialID,
		e.Status, e.Err, usageJSON, e.Cost, e.LatencyMS, e.TTFTMS, e.GenerationMS, boolToInt(e.Stream),
		e.Wire, string(e.Confidence), e.InputTokens, e.OutputTokens, e.CachedTokens,
		e.ID,
	); err != nil {
		return fmt.Errorf("store: finalize call: %w", err)
	}
	return nil
}

// AppendCall writes a call in a single shot: it mints a UUID if the entry has
// none, opens the row (phase 1), immediately finalizes it (phase 2), and returns
// the id. It is the convenience used by paths with no real two-phase lifecycle —
// tests seeding the ledger, and any caller that already has the complete outcome
// — layered over CreateCall/FinalizeCall. The gateway's live path uses the two
// phases directly (see docs/arch-gateway.md). If the entry has no TSEnd, the
// finalize stamps it to TS (or now).
func (s *Store) AppendCall(e calls.Entry) (string, error) {
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	if e.TSEnd.IsZero() {
		e.TSEnd = e.TS
	}
	if err := s.CreateCall(e); err != nil {
		return "", err
	}
	if err := s.FinalizeCall(e); err != nil {
		return "", err
	}
	// Single-shot callers get the session rollup folded in synchronously, so the
	// store stays self-consistent without the proxy's async insights fork. The
	// live gateway path does NOT use AppendCall (it splits the two phases and
	// forks the session update off the hot path), so this never double-counts.
	if err := s.UpsertSessionCall(e, ""); err != nil {
		return "", err
	}
	return e.ID, nil
}

// CallFilter selects and pages call rows. Zero-value fields are ignored.
type CallFilter struct {
	Since     *time.Time
	Until     *time.Time
	UserID    string
	Model     string
	Vendor    string
	Status    *int
	SessionID string
	Limit     int
	Offset    int
}

// where builds the shared WHERE clause and its positional arguments.
func (f CallFilter) where() (string, []any) {
	var (
		conds []string
		args  []any
	)
	if f.Since != nil {
		conds = append(conds, "ts >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if f.Until != nil {
		conds = append(conds, "ts < ?")
		args = append(args, f.Until.UnixMilli())
	}
	if f.UserID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, f.Model)
	}
	if f.Vendor != "" {
		conds = append(conds, "vendor = ?")
		args = append(args, f.Vendor)
	}
	if f.Status != nil {
		conds = append(conds, "status = ?")
		args = append(args, *f.Status)
	}
	if f.SessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

const callsSelect = `SELECT id, ts, ts_end, user_id, model, modality, vendor, credential_id, status, err, usage, cost, latency_ms, ttft_ms, generation_ms, stream, tags, wire, confidence, input_tokens, output_tokens, cached_tokens, session_id, agent_id, parent_agent_id, client_name, client_version, client_os, client_os_version FROM calls`

// QueryCalls returns matching entries ordered by ts DESC. Limit defaults to
// 100 and is capped at 1000.
func (s *Store) QueryCalls(f CallFilter) ([]calls.Entry, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultCallsLimit
	}
	if limit > maxCallsLimit {
		limit = maxCallsLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	clause, args := f.where()
	query := callsSelect + clause + " ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query calls: %w", err)
	}
	defer rows.Close()

	var out []calls.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan call: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query calls: %w", err)
	}
	return out, nil
}

// GetCall returns a single call entry by id, or ErrNotFound if absent.
func (s *Store) GetCall(id string) (calls.Entry, error) {
	rows, err := s.db.Query(callsSelect+" WHERE id = ? LIMIT 1", id)
	if err != nil {
		return calls.Entry{}, fmt.Errorf("store: get call: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return calls.Entry{}, fmt.Errorf("store: get call: %w", err)
		}
		return calls.Entry{}, ErrNotFound
	}
	e, err := scanEntry(rows)
	if err != nil {
		return calls.Entry{}, fmt.Errorf("store: scan call: %w", err)
	}
	return e, nil
}

// CountCalls returns the number of rows matching the filter (Limit/Offset are
// ignored).
func (s *Store) CountCalls(f CallFilter) (int, error) {
	clause, args := f.where()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM calls`+clause, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count calls: %w", err)
	}
	return n, nil
}

// SpendByUser sums cost for all call rows of a user, optionally since a
// time.
func (s *Store) SpendByUser(userID string, since *time.Time) (float64, error) {
	query := `SELECT COALESCE(SUM(cost), 0) FROM calls WHERE user_id = ?`
	args := []any{userID}
	if since != nil {
		query += " AND ts >= ?"
		args = append(args, since.UnixMilli())
	}
	var total float64
	if err := s.db.QueryRow(query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: spend by user: %w", err)
	}
	return total, nil
}

// TotalSpend sums cost across all rows within the optional [since, until)
// window.
func (s *Store) TotalSpend(since, until *time.Time) (float64, error) {
	var (
		conds []string
		args  []any
	)
	if since != nil {
		conds = append(conds, "ts >= ?")
		args = append(args, since.UnixMilli())
	}
	if until != nil {
		conds = append(conds, "ts < ?")
		args = append(args, until.UnixMilli())
	}
	query := `SELECT COALESCE(SUM(cost), 0) FROM calls`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	var total float64
	if err := s.db.QueryRow(query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: total spend: %w", err)
	}
	return total, nil
}

// SpendByModality returns cost summed per modality within the optional
// [since, until) window.
func (s *Store) SpendByModality(since, until *time.Time) (map[string]float64, error) {
	var (
		conds []string
		args  []any
	)
	if since != nil {
		conds = append(conds, "ts >= ?")
		args = append(args, since.UnixMilli())
	}
	if until != nil {
		conds = append(conds, "ts < ?")
		args = append(args, until.UnixMilli())
	}
	query := `SELECT modality, COALESCE(SUM(cost), 0) FROM calls`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " GROUP BY modality"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: spend by modality: %w", err)
	}
	defer rows.Close()

	out := make(map[string]float64)
	for rows.Next() {
		var (
			modality string
			cost     float64
		)
		if err := rows.Scan(&modality, &cost); err != nil {
			return nil, fmt.Errorf("store: scan spend by modality: %w", err)
		}
		out[modality] = cost
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: spend by modality: %w", err)
	}
	return out, nil
}

// scanEntry reads a single calls.Entry from a *sql.Rows.
func scanEntry(rows *sql.Rows) (calls.Entry, error) {
	var (
		e           calls.Entry
		tsMillis    int64
		tsEndMillis sql.NullInt64
		modality    string
		usageJSON   string
		tagsJSON    string
		stream      int
		confidence  string
	)
	if err := rows.Scan(
		&e.ID, &tsMillis, &tsEndMillis, &e.UserID, &e.Model, &modality, &e.Vendor, &e.CredentialID,
		&e.Status, &e.Err, &usageJSON, &e.Cost, &e.LatencyMS, &e.TTFTMS, &e.GenerationMS, &stream, &tagsJSON,
		&e.Wire, &confidence, &e.InputTokens, &e.OutputTokens, &e.CachedTokens,
		&e.SessionID, &e.AgentID, &e.ParentAgentID, &e.ClientName, &e.ClientVersion,
		&e.ClientOS, &e.ClientOSVersion,
	); err != nil {
		return calls.Entry{}, err
	}
	e.TS = time.UnixMilli(tsMillis)
	if tsEndMillis.Valid {
		e.TSEnd = time.UnixMilli(tsEndMillis.Int64)
	}
	e.Modality = calls.Modality(modality)
	e.Stream = stream != 0
	e.Confidence = calls.Confidence(confidence)

	if err := json.Unmarshal([]byte(usageJSON), &e.Usage); err != nil {
		return calls.Entry{}, fmt.Errorf("store: decode usage: %w", err)
	}
	if err := json.Unmarshal([]byte(tagsJSON), &e.Tags); err != nil {
		return calls.Entry{}, fmt.Errorf("store: decode tags: %w", err)
	}
	return e, nil
}

// marshalMap JSON-encodes a usage map, treating nil as an empty object.
func marshalMap(m map[string]any) (string, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// marshalStringMap JSON-encodes a tags map, treating nil as an empty object.
func marshalStringMap(m map[string]string) (string, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
