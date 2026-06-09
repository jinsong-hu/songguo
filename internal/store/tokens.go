package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// keyPrefixLen is how many leading characters of a plaintext key are stored
// for display (e.g. "sg-3f9a2b8c1d").
const keyPrefixLen = 12

// base62 alphabet for the random portion of a generated key.
const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Token is the public view of a consumer API key. It never exposes key_hash or
// the plaintext key.
type Token struct {
	ID        string
	Name      string
	KeyPrefix string
	Budget    *float64 // nil = unlimited
	Scope     []string // empty = all allowed
	RPM       int      // 0 = unlimited
	Capture   *bool    // nil = inherit global, false = off, true = on
	CreatedAt time.Time
	RevokedAt *time.Time // nil = active
}

// NewToken describes a token to create.
type NewToken struct {
	Name    string
	Budget  *float64
	Scope   []string
	RPM     int
	Capture *bool
}

// TokenUpdate carries optional field updates; nil pointers leave a field
// unchanged.
type TokenUpdate struct {
	Name    *string
	Budget  *float64
	Scope   *[]string
	RPM     *int
	Capture *bool
}

// HashKey returns the lowercase hex sha256 of a plaintext key. The proxy auth
// path reuses this to look up tokens.
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// randID returns a short random id (16 lowercase hex chars).
func randID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: random id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// genKey returns a plaintext key like "sg-" + 40 random base62 chars.
func genKey() (string, error) {
	const n = 40
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: random key: %w", err)
	}
	for i := range b {
		b[i] = base62[int(b[i])%len(base62)]
	}
	return "sg-" + string(b), nil
}

// CreateToken generates a fresh key, stores its hash and prefix, and returns
// the public Token plus the plaintext key, which is shown to the caller only
// once.
func (s *Store) CreateToken(nt NewToken) (Token, string, error) {
	id, err := randID()
	if err != nil {
		return Token{}, "", err
	}
	key, err := genKey()
	if err != nil {
		return Token{}, "", err
	}

	scope := nt.Scope
	if scope == nil {
		scope = []string{}
	}
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return Token{}, "", fmt.Errorf("store: encode scope: %w", err)
	}

	prefix := key
	if len(prefix) > keyPrefixLen {
		prefix = prefix[:keyPrefixLen]
	}
	createdAt := time.Now()

	_, err = s.db.Exec(
		`INSERT INTO tokens (id, name, key_hash, key_prefix, budget, scope, rpm, capture, created_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		id, nt.Name, HashKey(key), prefix, nt.Budget, string(scopeJSON), nt.RPM, boolPtrToNull(nt.Capture), createdAt.Unix(),
	)
	if err != nil {
		return Token{}, "", fmt.Errorf("store: create token: %w", err)
	}

	return Token{
		ID:        id,
		Name:      nt.Name,
		KeyPrefix: prefix,
		Budget:    nt.Budget,
		Scope:     scope,
		RPM:       nt.RPM,
		Capture:   nt.Capture,
		CreatedAt: createdAt.Truncate(time.Second),
	}, key, nil
}

// tokenSelect is the shared column list for scanning a Token.
const tokenSelect = `SELECT id, name, key_prefix, budget, scope, rpm, capture, created_at, revoked_at FROM tokens`

// scanToken reads a single Token row from a *sql.Row or *sql.Rows.
func scanToken(sc interface{ Scan(...any) error }) (Token, error) {
	var (
		t         Token
		budget    sql.NullFloat64
		scopeJSON string
		capture   sql.NullInt64
		createdAt int64
		revokedAt sql.NullInt64
	)
	if err := sc.Scan(&t.ID, &t.Name, &t.KeyPrefix, &budget, &scopeJSON, &t.RPM, &capture, &createdAt, &revokedAt); err != nil {
		return Token{}, err
	}
	if budget.Valid {
		b := budget.Float64
		t.Budget = &b
	}
	if capture.Valid {
		c := capture.Int64 != 0
		t.Capture = &c
	}
	if err := json.Unmarshal([]byte(scopeJSON), &t.Scope); err != nil {
		return Token{}, fmt.Errorf("store: decode scope: %w", err)
	}
	if t.Scope == nil {
		t.Scope = []string{}
	}
	t.CreatedAt = time.Unix(createdAt, 0)
	if revokedAt.Valid {
		rt := time.Unix(revokedAt.Int64, 0)
		t.RevokedAt = &rt
	}
	return t, nil
}

// GetToken returns the token with the given id.
func (s *Store) GetToken(id string) (Token, error) {
	row := s.db.QueryRow(tokenSelect+` WHERE id = ?`, id)
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, fmt.Errorf("store: token %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Token{}, fmt.Errorf("store: get token: %w", err)
	}
	return t, nil
}

// GetTokenByKey hashes the plaintext key and returns the matching active
// token. Missing or revoked tokens yield ErrNotFound.
func (s *Store) GetTokenByKey(plaintext string) (Token, error) {
	row := s.db.QueryRow(tokenSelect+` WHERE key_hash = ? AND revoked_at IS NULL`, HashKey(plaintext))
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, fmt.Errorf("store: token by key: %w", ErrNotFound)
	}
	if err != nil {
		return Token{}, fmt.Errorf("store: get token by key: %w", err)
	}
	return t, nil
}

// ListTokens returns all tokens, newest first.
func (s *Store) ListTokens() ([]Token, error) {
	rows, err := s.db.Query(tokenSelect + ` ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list tokens: %w", err)
	}
	defer rows.Close()

	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan token: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list tokens: %w", err)
	}
	return out, nil
}

// UpdateToken applies the non-nil fields of upd to the token and returns the
// updated row.
func (s *Store) UpdateToken(id string, upd TokenUpdate) (Token, error) {
	var (
		sets []string
		args []any
	)
	if upd.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *upd.Name)
	}
	if upd.Budget != nil {
		sets = append(sets, "budget = ?")
		args = append(args, *upd.Budget)
	}
	if upd.Scope != nil {
		scope := *upd.Scope
		if scope == nil {
			scope = []string{}
		}
		scopeJSON, err := json.Marshal(scope)
		if err != nil {
			return Token{}, fmt.Errorf("store: encode scope: %w", err)
		}
		sets = append(sets, "scope = ?")
		args = append(args, string(scopeJSON))
	}
	if upd.RPM != nil {
		sets = append(sets, "rpm = ?")
		args = append(args, *upd.RPM)
	}
	if upd.Capture != nil {
		sets = append(sets, "capture = ?")
		args = append(args, boolToInt(*upd.Capture))
	}

	if len(sets) > 0 {
		query := "UPDATE tokens SET " + sets[0]
		for _, s := range sets[1:] {
			query += ", " + s
		}
		query += " WHERE id = ?"
		args = append(args, id)

		res, err := s.db.Exec(query, args...)
		if err != nil {
			return Token{}, fmt.Errorf("store: update token: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return Token{}, fmt.Errorf("store: token %q: %w", id, ErrNotFound)
		}
	}

	return s.GetToken(id)
}

// RevokeToken marks a token revoked as of now. Revoking an already-revoked
// token refreshes the timestamp.
func (s *Store) RevokeToken(id string) error {
	res, err := s.db.Exec(`UPDATE tokens SET revoked_at = ? WHERE id = ?`, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("store: revoke token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: token %q: %w", id, ErrNotFound)
	}
	return nil
}

// boolPtrToNull converts a *bool into a value suitable for a nullable INTEGER
// column: nil yields a SQL NULL, otherwise 0/1.
func boolPtrToNull(b *bool) any {
	if b == nil {
		return nil
	}
	return boolToInt(*b)
}
