package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	ProxyTypeHTTPS  = "https"
	ProxyTypeSOCKS5 = "socks5"
)

// Proxy is one reusable outbound network route. Password is stored plaintext
// because it must be presented to the proxy, but API views never serialize it.
type Proxy struct {
	ID            string
	Name          string
	Type          string
	Host          string
	Port          int
	Username      string
	Password      string
	ProviderCount int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type NewProxy struct {
	Name     string
	Type     string
	Host     string
	Port     int
	Username string
	Password string
}

// ProxyUpdate carries optional field updates; nil pointers leave a field
// unchanged. Password may point to an empty string to clear stored auth.
type ProxyUpdate struct {
	Name     *string
	Type     *string
	Host     *string
	Port     *int
	Username *string
	Password *string
}

const proxySelect = `SELECT p.id, p.name, p.type, p.host, p.port, p.username, p.password,
	(SELECT COUNT(*) FROM providers v WHERE v.proxy_id = p.id),
	p.created_at, p.updated_at
	FROM proxies p`

func scanProxy(sc interface{ Scan(...any) error }) (Proxy, error) {
	var (
		p         Proxy
		createdAt int64
		updatedAt int64
	)
	if err := sc.Scan(
		&p.ID, &p.Name, &p.Type, &p.Host, &p.Port, &p.Username, &p.Password,
		&p.ProviderCount, &createdAt, &updatedAt,
	); err != nil {
		return Proxy{}, err
	}
	p.CreatedAt = time.Unix(createdAt, 0)
	p.UpdatedAt = time.Unix(updatedAt, 0)
	return p, nil
}

func (s *Store) ListProxies() ([]Proxy, error) {
	rows, err := s.db.Query(proxySelect + ` ORDER BY p.name, p.id`)
	if err != nil {
		return nil, fmt.Errorf("store: list proxies: %w", err)
	}
	defer rows.Close()

	var out []Proxy
	for rows.Next() {
		p, err := scanProxy(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan proxy: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list proxies: %w", err)
	}
	return out, nil
}

func (s *Store) GetProxy(id string) (Proxy, error) {
	p, err := scanProxy(s.db.QueryRow(proxySelect+` WHERE p.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Proxy{}, fmt.Errorf("store: proxy %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Proxy{}, fmt.Errorf("store: get proxy: %w", err)
	}
	return p, nil
}

func (s *Store) CreateProxy(np NewProxy) (Proxy, error) {
	id, err := randID()
	if err != nil {
		return Proxy{}, err
	}
	now := time.Now().Unix()
	_, err = s.db.Exec(
		`INSERT INTO proxies (id, name, type, host, port, username, password, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, np.Name, np.Type, np.Host, np.Port, np.Username, np.Password, now, now,
	)
	if err != nil {
		return Proxy{}, fmt.Errorf("store: create proxy: %w", err)
	}
	return s.GetProxy(id)
}

func (s *Store) UpdateProxy(id string, upd ProxyUpdate) (Proxy, error) {
	var (
		sets []string
		args []any
	)
	if upd.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *upd.Name)
	}
	if upd.Type != nil {
		sets = append(sets, "type = ?")
		args = append(args, *upd.Type)
	}
	if upd.Host != nil {
		sets = append(sets, "host = ?")
		args = append(args, *upd.Host)
	}
	if upd.Port != nil {
		sets = append(sets, "port = ?")
		args = append(args, *upd.Port)
	}
	if upd.Username != nil {
		sets = append(sets, "username = ?")
		args = append(args, *upd.Username)
	}
	if upd.Password != nil {
		sets = append(sets, "password = ?")
		args = append(args, *upd.Password)
	}

	if len(sets) > 0 {
		sets = append(sets, "updated_at = ?")
		args = append(args, time.Now().Unix())
		query := "UPDATE proxies SET " + sets[0]
		for _, set := range sets[1:] {
			query += ", " + set
		}
		query += " WHERE id = ?"
		args = append(args, id)

		res, err := s.db.Exec(query, args...)
		if err != nil {
			return Proxy{}, fmt.Errorf("store: update proxy: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return Proxy{}, fmt.Errorf("store: proxy %q: %w", id, ErrNotFound)
		}
	}
	return s.GetProxy(id)
}

// DeleteProxy refuses to remove an assigned proxy through the foreign key.
// The API checks ProviderCount first to return a useful conflict message.
func (s *Store) DeleteProxy(id string) error {
	res, err := s.db.Exec(`DELETE FROM proxies WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete proxy: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: proxy %q: %w", id, ErrNotFound)
	}
	return nil
}
