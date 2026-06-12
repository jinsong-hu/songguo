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

// User is the public view of a consumer API key. It never exposes key_hash or
// the plaintext key.
type User struct {
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

// NewUser describes a user to create.
type NewUser struct {
	Name    string
	Budget  *float64
	Scope   []string
	RPM     int
	Capture *bool
}

// UserUpdate carries optional field updates; nil pointers leave a field
// unchanged.
type UserUpdate struct {
	Name    *string
	Budget  *float64
	Scope   *[]string
	RPM     *int
	Capture *bool
}

// HashKey returns the lowercase hex sha256 of a plaintext key. The proxy auth
// path reuses this to look up users.
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

// CreateUser generates a fresh key, stores its hash and prefix, and returns
// the public User plus the plaintext key, which is shown to the caller only
// once.
func (s *Store) CreateUser(nu NewUser) (User, string, error) {
	id, err := randID()
	if err != nil {
		return User{}, "", err
	}
	key, err := genKey()
	if err != nil {
		return User{}, "", err
	}

	scope := nu.Scope
	if scope == nil {
		scope = []string{}
	}
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return User{}, "", fmt.Errorf("store: encode scope: %w", err)
	}

	prefix := key
	if len(prefix) > keyPrefixLen {
		prefix = prefix[:keyPrefixLen]
	}
	createdAt := time.Now()

	_, err = s.db.Exec(
		`INSERT INTO users (id, name, key_hash, key_prefix, budget, scope, rpm, capture, created_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		id, nu.Name, HashKey(key), prefix, nu.Budget, string(scopeJSON), nu.RPM, boolPtrToNull(nu.Capture), createdAt.Unix(),
	)
	if err != nil {
		return User{}, "", fmt.Errorf("store: create user: %w", err)
	}

	return User{
		ID:        id,
		Name:      nu.Name,
		KeyPrefix: prefix,
		Budget:    nu.Budget,
		Scope:     scope,
		RPM:       nu.RPM,
		Capture:   nu.Capture,
		CreatedAt: createdAt.Truncate(time.Second),
	}, key, nil
}

// userSelect is the shared column list for scanning a User.
const userSelect = `SELECT id, name, key_prefix, budget, scope, rpm, capture, created_at, revoked_at FROM users`

// scanUser reads a single User row from a *sql.Row or *sql.Rows.
func scanUser(sc interface{ Scan(...any) error }) (User, error) {
	var (
		u         User
		budget    sql.NullFloat64
		scopeJSON string
		capture   sql.NullInt64
		createdAt int64
		revokedAt sql.NullInt64
	)
	if err := sc.Scan(&u.ID, &u.Name, &u.KeyPrefix, &budget, &scopeJSON, &u.RPM, &capture, &createdAt, &revokedAt); err != nil {
		return User{}, err
	}
	if budget.Valid {
		b := budget.Float64
		u.Budget = &b
	}
	if capture.Valid {
		c := capture.Int64 != 0
		u.Capture = &c
	}
	if err := json.Unmarshal([]byte(scopeJSON), &u.Scope); err != nil {
		return User{}, fmt.Errorf("store: decode scope: %w", err)
	}
	if u.Scope == nil {
		u.Scope = []string{}
	}
	u.CreatedAt = time.Unix(createdAt, 0)
	if revokedAt.Valid {
		rt := time.Unix(revokedAt.Int64, 0)
		u.RevokedAt = &rt
	}
	return u, nil
}

// GetUser returns the user with the given id.
func (s *Store) GetUser(id string) (User, error) {
	row := s.db.QueryRow(userSelect+` WHERE id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("store: user %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("store: get user: %w", err)
	}
	return u, nil
}

// GetUserByKey hashes the plaintext key and returns the matching active
// user. Missing or revoked users yield ErrNotFound.
func (s *Store) GetUserByKey(plaintext string) (User, error) {
	row := s.db.QueryRow(userSelect+` WHERE key_hash = ? AND revoked_at IS NULL`, HashKey(plaintext))
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("store: user by key: %w", ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("store: get user by key: %w", err)
	}
	return u, nil
}

// ListUsers returns all users, newest first.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(userSelect + ` ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	return out, nil
}

// UpdateUser applies the non-nil fields of upd to the user and returns the
// updated row.
func (s *Store) UpdateUser(id string, upd UserUpdate) (User, error) {
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
			return User{}, fmt.Errorf("store: encode scope: %w", err)
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
		query := "UPDATE users SET " + sets[0]
		for _, s := range sets[1:] {
			query += ", " + s
		}
		query += " WHERE id = ?"
		args = append(args, id)

		res, err := s.db.Exec(query, args...)
		if err != nil {
			return User{}, fmt.Errorf("store: update user: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return User{}, fmt.Errorf("store: user %q: %w", id, ErrNotFound)
		}
	}

	return s.GetUser(id)
}

// RevokeUser marks a user revoked as of now. Revoking an already-revoked
// user refreshes the timestamp.
func (s *Store) RevokeUser(id string) error {
	res, err := s.db.Exec(`UPDATE users SET revoked_at = ? WHERE id = ?`, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("store: revoke user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: user %q: %w", id, ErrNotFound)
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
