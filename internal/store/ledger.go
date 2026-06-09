package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/songguo/songguo/internal/ledger"
)

// defaultLedgerLimit and maxLedgerLimit bound QueryLedger result sizes.
const (
	defaultLedgerLimit = 100
	maxLedgerLimit     = 1000
)

// AppendLedger writes one append-only entry and returns its autoincrement id.
// Usage and Tags are JSON-encoded; ts is stored as unix milliseconds.
func (s *Store) AppendLedger(e ledger.Entry) (int64, error) {
	usageJSON, err := marshalMap(e.Usage)
	if err != nil {
		return 0, fmt.Errorf("store: encode usage: %w", err)
	}
	tagsJSON, err := marshalStringMap(e.Tags)
	if err != nil {
		return 0, fmt.Errorf("store: encode tags: %w", err)
	}

	ts := e.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	modality := e.Modality
	if modality == "" {
		modality = ledger.ModalityUnknown
	}
	attempt := e.Attempt
	if attempt == 0 {
		attempt = 1
	}

	res, err := s.db.Exec(
		`INSERT INTO ledger
		 (ts, token_id, model, modality, vendor, credential_id, attempt, status, err, usage, cost, latency_ms, stream, tags)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts.UnixMilli(), e.TokenID, e.Model, string(modality), e.Vendor, e.CredentialID,
		attempt, e.Status, e.Err, usageJSON, e.Cost, e.LatencyMS, boolToInt(e.Stream), tagsJSON,
	)
	if err != nil {
		return 0, fmt.Errorf("store: append ledger: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: append ledger id: %w", err)
	}
	return id, nil
}

// LedgerFilter selects and pages ledger rows. Zero-value fields are ignored.
type LedgerFilter struct {
	Since   *time.Time
	Until   *time.Time
	TokenID string
	Model   string
	Vendor  string
	Status  *int
	Limit   int
	Offset  int
}

// where builds the shared WHERE clause and its positional arguments.
func (f LedgerFilter) where() (string, []any) {
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
	if f.TokenID != "" {
		conds = append(conds, "token_id = ?")
		args = append(args, f.TokenID)
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
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

const ledgerSelect = `SELECT id, ts, token_id, model, modality, vendor, credential_id, attempt, status, err, usage, cost, latency_ms, stream, tags FROM ledger`

// QueryLedger returns matching entries ordered by ts DESC. Limit defaults to
// 100 and is capped at 1000.
func (s *Store) QueryLedger(f LedgerFilter) ([]ledger.Entry, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLedgerLimit
	}
	if limit > maxLedgerLimit {
		limit = maxLedgerLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	clause, args := f.where()
	query := ledgerSelect + clause + " ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query ledger: %w", err)
	}
	defer rows.Close()

	var out []ledger.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan ledger: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query ledger: %w", err)
	}
	return out, nil
}

// CountLedger returns the number of rows matching the filter (Limit/Offset are
// ignored).
func (s *Store) CountLedger(f LedgerFilter) (int, error) {
	clause, args := f.where()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ledger`+clause, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count ledger: %w", err)
	}
	return n, nil
}

// SpendByToken sums cost for all ledger rows of a token, optionally since a
// time.
func (s *Store) SpendByToken(tokenID string, since *time.Time) (float64, error) {
	query := `SELECT COALESCE(SUM(cost), 0) FROM ledger WHERE token_id = ?`
	args := []any{tokenID}
	if since != nil {
		query += " AND ts >= ?"
		args = append(args, since.UnixMilli())
	}
	var total float64
	if err := s.db.QueryRow(query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: spend by token: %w", err)
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
	query := `SELECT COALESCE(SUM(cost), 0) FROM ledger`
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
	query := `SELECT modality, COALESCE(SUM(cost), 0) FROM ledger`
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

// scanEntry reads a single ledger.Entry from a *sql.Rows.
func scanEntry(rows *sql.Rows) (ledger.Entry, error) {
	var (
		e         ledger.Entry
		tsMillis  int64
		modality  string
		usageJSON string
		tagsJSON  string
		stream    int
	)
	if err := rows.Scan(
		&e.ID, &tsMillis, &e.TokenID, &e.Model, &modality, &e.Vendor, &e.CredentialID,
		&e.Attempt, &e.Status, &e.Err, &usageJSON, &e.Cost, &e.LatencyMS, &stream, &tagsJSON,
	); err != nil {
		return ledger.Entry{}, err
	}
	e.TS = time.UnixMilli(tsMillis)
	e.Modality = ledger.Modality(modality)
	e.Stream = stream != 0

	if err := json.Unmarshal([]byte(usageJSON), &e.Usage); err != nil {
		return ledger.Entry{}, fmt.Errorf("store: decode usage: %w", err)
	}
	if err := json.Unmarshal([]byte(tagsJSON), &e.Tags); err != nil {
		return ledger.Entry{}, fmt.Errorf("store: decode tags: %w", err)
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
