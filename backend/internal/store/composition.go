package store

import (
	"encoding/json"
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
	if _, err := s.db.Exec(
		`INSERT OR REPLACE INTO context_composition (call_id, total, cached, sources, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		callID, float64(c.Total), float64(c.Cached), string(sources), time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("store: save composition: %w", err)
	}
	return nil
}

// AggComposition is the aggregated context decomposition over a window. Sources
// partition OfficialInput exactly (a synthetic "unattributed" source absorbs any
// input tokens from calls that carry no composition), so it equals the Overview
// Tokens KPI. Requests counts window calls with input_tokens>0; AvgTotal is
// OfficialInput/Requests.
type AggComposition struct {
	Sources  []compose.Source
	Requests int
	AvgTotal float64
}

// AggregateComposition sums context decompositions over the optional [since,
// until) window. Totals stay anchored to the vendor's official input tokens: it
// sums input_tokens over ALL calls in the window (every modality) and adds a
// synthetic "unattributed" source for whatever the composition rows did not
// account for. Sources are sorted by tokens desc.
func (s *Store) AggregateComposition(since, until *time.Time) (AggComposition, error) {
	// Official input over every call in the window (all modalities), plus the
	// request count (calls that actually carried input tokens).
	clause, args := windowClause(since, until)
	var officialInput float64
	var requests int
	if err := s.db.QueryRow(
		`SELECT COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(CASE WHEN input_tokens > 0 THEN 1 ELSE 0 END), 0)
		   FROM calls`+clause, args...,
	).Scan(&officialInput, &requests); err != nil {
		return AggComposition{}, fmt.Errorf("store: aggregate composition totals: %w", err)
	}

	// Composition rows joined to their window calls. The window clause filters on
	// bare `ts`, which is unambiguous here — only calls carries a ts column.
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
	var attributed int64
	for rows.Next() {
		var total float64
		var raw string
		if err := rows.Scan(&total, &raw); err != nil {
			return AggComposition{}, fmt.Errorf("store: scan composition: %w", err)
		}
		attributed += int64(total)
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

	out := make([]compose.Source, 0, len(order)+1)
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

	// Synthetic "unattributed" source: input tokens from calls without a
	// composition (or non-chat modalities). Guarantees Σ sources.tokens ==
	// officialInput == the Overview Tokens KPI.
	if unattributed := int64(officialInput) - attributed; unattributed > 0 {
		out = append(out, compose.Source{Key: "unattributed", Tokens: unattributed})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return out[i].Key < out[j].Key
	})

	avg := 0.0
	if requests > 0 {
		avg = officialInput / float64(requests)
	}
	return AggComposition{Sources: out, Requests: requests, AvgTotal: avg}, nil
}

// SessionCompositionRow is one call's decomposition within a session, carrying
// the call's timestamp and agent id for turn ordering and labeling.
type SessionCompositionRow struct {
	CallID  int64
	TS      time.Time
	AgentID string
	C       compose.Composition
}

// SessionComposition returns every composition row for a session's calls,
// ordered by call timestamp ascending (oldest turn first).
func (s *Store) SessionComposition(sessionID string) ([]SessionCompositionRow, error) {
	rows, err := s.db.Query(
		`SELECT cc.call_id, c.ts, c.agent_id, cc.total, cc.cached, cc.sources
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
			raw      string
		)
		if err := rows.Scan(&r.CallID, &tsMillis, &r.AgentID, &total, &cached, &raw); err != nil {
			return nil, fmt.Errorf("store: scan session composition: %w", err)
		}
		r.TS = time.UnixMilli(tsMillis)
		r.C.Total = int64(total)
		r.C.Cached = int64(cached)
		if err := json.Unmarshal([]byte(raw), &r.C.Sources); err != nil {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: session composition rows: %w", err)
	}
	return out, nil
}
