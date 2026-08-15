package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/songguo/songguo/internal/catalog"
)

// FeedPrice is one model's rate as last fetched from the upstream price feed.
//
// The table holds CURRENT values only, one row per (provider, model), upserted
// on each refresh. It is deliberately not a history: calls.cost already records
// the rate that was actually applied, per call, permanently — tokens and cost
// give the effective rate for any row in the ledger — so a second table would
// be a parallel copy of a fact already stored.
//
// It exists so "a failed refresh keeps the last known good rate" survives a
// restart. Without it, a reboot during an upstream outage would silently fall
// back to the embedded seed, which may be months old.
type FeedPrice struct {
	ProviderID string // catalog provider id, e.g. "openai"
	Model      string
	Cost       catalog.Cost
	FetchedAt  time.Time
}

// ReplaceFeedPrices swaps in a whole refresh atomically.
//
// It replaces rather than merges: a model the feed stopped publishing must stop
// being quoted, or a rate could outlive its removal upstream indefinitely. An
// empty slice is therefore rejected — that is a failed fetch, not a feed with
// no models, and callers must keep the previous rows instead (see
// internal/pricefeed).
func (s *Store) ReplaceFeedPrices(prices []FeedPrice, at time.Time) error {
	if len(prices) == 0 {
		return fmt.Errorf("store: refusing to replace feed prices with an empty set")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: replace feed prices: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM feed_prices`); err != nil {
		return fmt.Errorf("store: clear feed prices: %w", err)
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	for _, p := range prices {
		blob, err := json.Marshal(p.Cost)
		if err != nil {
			return fmt.Errorf("store: encode feed cost for %s/%s: %w", p.ProviderID, p.Model, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO feed_prices (provider_id, model, cost, fetched_at) VALUES (?, ?, ?, ?)`,
			p.ProviderID, p.Model, string(blob), stamp); err != nil {
			return fmt.Errorf("store: insert feed price %s/%s: %w", p.ProviderID, p.Model, err)
		}
	}
	return tx.Commit()
}

// ListFeedPrices returns the last successful refresh, keyed [providerID][model].
// An empty result means no refresh has ever landed, which is the normal state on
// first boot and offline installs — callers fall back to the embedded catalog.
func (s *Store) ListFeedPrices() (map[string]map[string]FeedPrice, error) {
	rows, err := s.db.Query(`SELECT provider_id, model, cost, fetched_at FROM feed_prices`)
	if err != nil {
		return nil, fmt.Errorf("store: list feed prices: %w", err)
	}
	defer rows.Close()

	out := make(map[string]map[string]FeedPrice)
	for rows.Next() {
		var p FeedPrice
		var costJSON, at string
		if err := rows.Scan(&p.ProviderID, &p.Model, &costJSON, &at); err != nil {
			return nil, fmt.Errorf("store: scan feed price: %w", err)
		}
		if p.Cost, err = decodeCost(costJSON); err != nil {
			return nil, fmt.Errorf("store: feed price %s/%s: %w", p.ProviderID, p.Model, err)
		}
		p.FetchedAt, _ = time.Parse(time.RFC3339Nano, at)
		if out[p.ProviderID] == nil {
			out[p.ProviderID] = make(map[string]FeedPrice)
		}
		out[p.ProviderID][p.Model] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list feed prices: %w", err)
	}
	return out, nil
}
