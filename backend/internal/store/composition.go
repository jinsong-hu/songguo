package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/songguo/songguo/internal/compose"
)

// SaveComposition upserts the context-window decomposition for a call (keyed by
// call_id). Written off the hot path after the client response is sent; callers
// log failures rather than surfacing them. Safe to call concurrently.
func (s *Store) SaveComposition(callID int64, c compose.Composition) error {
	sources, err := json.Marshal(c.Sources)
	if err != nil {
		return fmt.Errorf("store: marshal composition: %w", err)
	}
	blocks, err := json.Marshal(c.Blocks)
	if err != nil {
		return fmt.Errorf("store: marshal composition blocks: %w", err)
	}
	if _, err := s.db.Exec(
		`INSERT OR REPLACE INTO context_composition (call_id, total, cached, sources, blocks, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		callID, float64(c.Total), float64(c.Cached), string(sources), string(blocks), time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("store: save composition: %w", err)
	}
	return nil
}

// GetComposition returns a call's context-window decomposition, or ErrNotFound
// when the call has no decomposed composition row.
func (s *Store) GetComposition(callID int64) (compose.Composition, error) {
	var (
		total  float64
		cached float64
		rawSrc string
		rawBlk string
	)
	if err := s.db.QueryRow(
		`SELECT total, cached, sources, blocks
		   FROM context_composition
		  WHERE call_id = ?
		  LIMIT 1`, callID,
	).Scan(&total, &cached, &rawSrc, &rawBlk); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return compose.Composition{}, ErrNotFound
		}
		return compose.Composition{}, fmt.Errorf("store: get composition: %w", err)
	}
	var sources []compose.Source
	if err := json.Unmarshal([]byte(rawSrc), &sources); err != nil {
		return compose.Composition{}, fmt.Errorf("store: unmarshal composition: %w", err)
	}
	var blocks []compose.Block
	if err := json.Unmarshal([]byte(rawBlk), &blocks); err != nil {
		return compose.Composition{}, fmt.Errorf("store: unmarshal composition blocks: %w", err)
	}
	return compose.Composition{Total: int64(total), Cached: int64(cached), Sources: sources, Blocks: blocks}, nil
}

// AggComposition is the aggregated context decomposition over a window. Tokens
// are locally estimated per block (text tokens plus visual-token weights where
// available) and summed — deliberately NOT anchored to the vendor's official
// input total, so proportions stay stable for an unchanged prompt. It therefore
// covers only the requests we could decompose (chat calls with a parseable body);
// it does not reconcile to the Overview Tokens KPI. Requests counts those
// decomposed calls; AvgTotal is their mean self-counted window size.
type AggComposition struct {
	Sources  []compose.Source
	Requests int
	AvgTotal float64
}

// AggregateComposition sums context decompositions over the optional [since,
// until) window. Every total/subtotal is our own local token count; there is no
// official-anchored "unattributed" bucket — calls without a composition are
// simply absent from this view (the KPI, which is official, covers them). The
// window clause filters on the joined calls' bare `ts`, unambiguous here since
// only calls carries a ts column. Sources are sorted by tokens desc.
func (s *Store) AggregateComposition(since, until *time.Time) (AggComposition, error) {
	clause, args := windowClause(since, until)
	rows, err := s.db.Query(
		`SELECT cc.total, cc.sources
		   FROM context_composition cc
		   JOIN calls c ON c.id = cc.call_id`+clause, args...,
	)
	if err != nil {
		return AggComposition{}, fmt.Errorf("store: aggregate composition: %w", err)
	}
	defer rows.Close()

	type acc struct {
		tokens int64
		cached int64
		prods  map[string]int64
	}
	byKey := map[string]*acc{}
	var order []string
	var totalTokens int64
	var requests int
	for rows.Next() {
		var total float64
		var raw string
		if err := rows.Scan(&total, &raw); err != nil {
			return AggComposition{}, fmt.Errorf("store: scan composition: %w", err)
		}
		requests++
		totalTokens += int64(total)
		var sources []compose.Source
		if err := json.Unmarshal([]byte(raw), &sources); err != nil {
			// A corrupt row must not sink the aggregate; skip it.
			continue
		}
		for _, src := range sources {
			a := byKey[src.Key]
			if a == nil {
				a = &acc{prods: map[string]int64{}}
				byKey[src.Key] = a
				order = append(order, src.Key)
			}
			a.tokens += src.Tokens
			a.cached += src.Cached
			for _, p := range src.Children {
				a.prods[p.Key] += p.Tokens
			}
		}
	}
	if err := rows.Err(); err != nil {
		return AggComposition{}, fmt.Errorf("store: aggregate composition rows: %w", err)
	}

	out := make([]compose.Source, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		src := compose.Source{Key: key, Tokens: a.tokens, Cached: a.cached}
		for pk, pt := range a.prods {
			src.Children = append(src.Children, compose.Producer{Key: pk, Tokens: pt})
		}
		sort.SliceStable(src.Children, func(i, j int) bool {
			if src.Children[i].Tokens != src.Children[j].Tokens {
				return src.Children[i].Tokens > src.Children[j].Tokens
			}
			return src.Children[i].Key < src.Children[j].Key
		})
		out = append(out, src)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return out[i].Key < out[j].Key
	})

	avg := 0.0
	if requests > 0 {
		avg = float64(totalTokens) / float64(requests)
	}
	return AggComposition{Sources: out, Requests: requests, AvgTotal: avg}, nil
}

// SessionCompositionRow is one call's decomposition within a session, carrying
// the call's timestamp and agent id for turn ordering and labeling.
type SessionCompositionRow struct {
	CallID  int64
	TS      time.Time
	AgentID string
	Wire    string
	C       compose.Composition
}

// SessionComposition returns every composition row for a session's calls,
// ordered by call timestamp ascending (oldest turn first).
func (s *Store) SessionComposition(sessionID string) ([]SessionCompositionRow, error) {
	rows, err := s.db.Query(
		`SELECT cc.call_id, c.ts, c.agent_id, c.wire, cc.total, cc.cached, cc.sources, cc.blocks
		   FROM context_composition cc
		   JOIN calls c ON c.id = cc.call_id
		  WHERE c.session_id = ?
		  ORDER BY c.ts ASC`, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: session composition: %w", err)
	}
	defer rows.Close()

	var out []SessionCompositionRow
	for rows.Next() {
		var (
			r        SessionCompositionRow
			tsMillis int64
			total    float64
			cached   float64
			rawSrc   string
			rawBlk   string
		)
		if err := rows.Scan(&r.CallID, &tsMillis, &r.AgentID, &r.Wire, &total, &cached, &rawSrc, &rawBlk); err != nil {
			return nil, fmt.Errorf("store: scan session composition: %w", err)
		}
		r.TS = time.UnixMilli(tsMillis)
		r.C.Total = int64(total)
		r.C.Cached = int64(cached)
		if err := json.Unmarshal([]byte(rawSrc), &r.C.Sources); err != nil {
			continue
		}
		if err := json.Unmarshal([]byte(rawBlk), &r.C.Blocks); err != nil {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: session composition rows: %w", err)
	}
	return out, nil
}

// AggregateSessionComposition sums every decomposed request window in one
// session. Repeated blocks count once per request, matching the Overview
// context distribution mental model.
func (s *Store) AggregateSessionComposition(sessionID string) (AggComposition, error) {
	rows, err := s.SessionComposition(sessionID)
	if err != nil {
		return AggComposition{}, err
	}
	return aggregateCompositionRows(rows), nil
}

func aggregateCompositionRows(rows []SessionCompositionRow) AggComposition {
	type acc struct {
		tokens int64
		cached int64
		prods  map[string]int64
	}
	byKey := map[string]*acc{}
	var order []string
	var totalTokens int64
	for _, row := range rows {
		totalTokens += row.C.Total
		for _, src := range row.C.Sources {
			a := byKey[src.Key]
			if a == nil {
				a = &acc{prods: map[string]int64{}}
				byKey[src.Key] = a
				order = append(order, src.Key)
			}
			a.tokens += src.Tokens
			a.cached += src.Cached
			for _, p := range src.Children {
				a.prods[p.Key] += p.Tokens
			}
		}
	}

	out := make([]compose.Source, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		src := compose.Source{Key: key, Tokens: a.tokens, Cached: a.cached}
		for pk, pt := range a.prods {
			src.Children = append(src.Children, compose.Producer{Key: pk, Tokens: pt})
		}
		sort.SliceStable(src.Children, func(i, j int) bool {
			if src.Children[i].Tokens != src.Children[j].Tokens {
				return src.Children[i].Tokens > src.Children[j].Tokens
			}
			return src.Children[i].Key < src.Children[j].Key
		})
		out = append(out, src)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return out[i].Key < out[j].Key
	})

	avg := 0.0
	if len(rows) > 0 {
		avg = float64(totalTokens) / float64(len(rows))
	}
	return AggComposition{Sources: out, Requests: len(rows), AvgTotal: avg}
}
