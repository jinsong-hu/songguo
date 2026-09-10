package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// A coding agent re-sends its whole conversation on every turn, so a session's
// captured request bodies are overwhelmingly duplicates of each other: turn N
// contains everything turns 1..N-1 contained. On the production gateway a
// 284-turn session averaged 11 MB per request — 3.1 GB of captures holding one
// ~31 MB conversation, stored 284 times over.
//
// Reconstructing that session by reading every body (which is what the session
// Messages panel did) therefore reads gigabytes to render a conversation that
// one body already holds. This file picks the few bodies that actually carry
// distinct content, from metadata alone.
//
// THE RULE. Within a stretch of growing context, a request is redundant if a
// later one starts at the same place and reaches further:
//
//	same msg_head  +  msg_count non-decreasing  ⇒  the later one is a superset
//
// A maximal run of such requests collapses to its LAST member. A run ends when
// msg_head changes (the conversation was replaced — compaction substituted a
// summary for the history) or msg_count drops (messages were dropped), and the
// next request opens a new run. Equal counts additionally require equal
// msg_tail, which separates two same-length arrays that diverged — a fork.
//
// WHY THIS IS NOT A HEURISTIC. There is no threshold and no tuning constant
// here, and that is deliberate: a "context shrank by more than 30%, call it a
// compaction" rule would be inventing an explanation for a shape, which is the
// thing this codebase refuses to do elsewhere (a pending call is not called
// "crashed", a slow one is not called "hung"). A run boundary is an observed
// inequality between two stored values, and we never name its cause.
//
// It is also wire-agnostic for the same reason. The obvious alternative —
// keying on Anthropic's `cc_workload=compact` billing marker, which
// calls.ClassifyEntrypoint already parses — would work only for Claude Code and
// leave every OpenAI-shaped harness reading all 1000 bodies. The fingerprint is
// computed from parse.Call.Input, which is normalized across anthropic-messages,
// openai-chat and openai-responses alike.
//
// WHAT IT COSTS WHEN THE ASSUMPTION IS WRONG. The rule assumes each request is
// a contiguous slice of a conversation. When that breaks, the cost is almost
// always a redundant read, not a wrong answer, and the boundary between the two
// is exact:
//
//	a violation that perturbs msg_head or msg_count  →  a false run boundary
//	                                                 →  one extra body read
//	a violation that leaves both intact              →  invisible, wrong output
//
// Middle-drop truncation, truncated captures and mid-session capture starts all
// perturb the count and so fail safe. Forks leave both intact, which is what
// msg_tail is for. The one case that survives all three columns is an in-place
// edit of an older message while the array grows — a harness truncating a large
// tool result in an earlier turn. That costs FIDELITY, not structure: every
// message is still present and in order, an old tool output is shorter than it
// once was. The view that needs exact per-turn bytes is the per-call trace, a
// GetPayload point lookup that never comes through here.
//
// Unknown fingerprints (rows written before the columns existed, or never
// parsed) are never treated as redundant. That is the whole safety property:
// the fallback on missing information is to read the body, never to drop it.

// CoveringRequest identifies one call whose captured request body must be read
// to reconstruct a session's conversation.
type CoveringRequest struct {
	CallID  string
	AgentID string
	TS      time.Time
}

// callShape is one call's message-shape fingerprint plus what the cover needs to
// order and filter it.
type callShape struct {
	CallID  string
	AgentID string
	TS      time.Time
	Count   int64
	Head    []byte
	Tail    []byte
	HasRaw  bool
}

// known reports whether the parse pipeline stamped a fingerprint on this row. An
// unknown shape can never be proven redundant, so it always survives the cover.
func (c callShape) known() bool { return c.Count > 0 && len(c.Head) > 0 }

// sessionShapes reads the message-shape fingerprints for a session's
// conversation calls, ordered by agent then time.
//
// Utility calls are excluded on the same predicate SessionComposition uses:
// monitor, count_tokens, title and compaction calls ride the same wire but are
// not part of the visible conversation, and their tiny message arrays would open
// a spurious run on every occurrence.
//
// The LEFT JOIN onto raw resolves capture availability in the same pass, because
// the cover has to know it: raw is pruned at 7 days while calls lives 90, so the
// last member of a run frequently has no body left to read.
//
// There is deliberately NO LIMIT. The rows are narrow (two ids, a timestamp, a
// count and two 16-byte hashes) so even a pathological session costs a few MB of
// metadata, and capping would be actively wrong here: a cap truncates the OLDEST
// calls, which is where run boundaries are, so the first surviving run would be
// detected against a predecessor that was cut away. Bounding the output is the
// job of the cover itself, and it returns a handful.
//
// This also drops a cap that was silently losing data: the previous query kept
// only a session's latest 1000 calls, so the production gateway's largest
// session (5,438 calls) rendered its conversation from the newest fifth and
// showed no sign the rest existed.
func (s *Store) sessionShapes(sessionID string) ([]callShape, error) {
	rows, err := s.db.Query(
		`SELECT c.id, c.agent_id, c.ts, c.msg_count, c.msg_head, c.msg_tail,
		        CASE WHEN r.call_id IS NULL THEN 0 ELSE 1 END
		   FROM calls c
		   LEFT JOIN raw r ON r.call_id = c.id
		  WHERE c.session_id = ?
		    AND (c.entrypoint = '' OR c.entrypoint = 'main')
		  ORDER BY c.agent_id ASC, c.ts ASC, c.id ASC`, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: session shapes: %w", err)
	}
	defer rows.Close()

	var out []callShape
	for rows.Next() {
		var (
			c       callShape
			tsMilli int64
			hasRaw  int
		)
		if err := rows.Scan(&c.CallID, &c.AgentID, &tsMilli, &c.Count, &c.Head, &c.Tail, &hasRaw); err != nil {
			return nil, fmt.Errorf("store: scan session shape: %w", err)
		}
		c.TS = time.UnixMilli(tsMilli)
		c.HasRaw = hasRaw != 0
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: session shapes: %w", err)
	}
	return out, nil
}

// extends reports whether cur is a superset of prev — same starting message and
// reaching at least as far. Equal lengths must also agree on the full-array hash,
// which is what separates a retry (identical, redundant) from a fork (same
// length, different content, NOT redundant).
//
// Either side being unknown yields false: absent evidence we keep both bodies.
func extends(prev, cur callShape) bool {
	if !prev.known() || !cur.known() {
		return false
	}
	if !bytes.Equal(prev.Head, cur.Head) {
		return false
	}
	if cur.Count < prev.Count {
		return false
	}
	if cur.Count == prev.Count && !bytes.Equal(prev.Tail, cur.Tail) {
		return false
	}
	return true
}

// messageCover reduces a session's calls to the minimum set of request bodies
// that together carry every message, given shapes ordered by agent then time.
//
// Runs never span agents: a subagent's context is an unrelated conversation, not
// a truncation of its parent's, so its shorter array must not read as a boundary
// in the parent's run. The ordering from sessionShapes puts each agent's calls
// together, and the walk resets whenever the agent id changes.
//
// Within a run the last member wins — but only if its body still exists. When it
// does not (raw pruned at 7 days), the walk falls back to the latest member of
// the same run that does. That is a strictly better answer than failing: those
// earlier requests are prefixes of the missing one, so they still carry every
// message up to where they end. A run with no captures at all contributes
// nothing, and the gap surfaces to the caller as messages that simply are not
// there — which is the honest outcome, not one to paper over.
func messageCover(shapes []callShape) []CoveringRequest {
	var out []CoveringRequest

	// best is the latest capture-bearing member of the run in progress.
	var best *callShape
	flush := func() {
		if best != nil {
			out = append(out, CoveringRequest{CallID: best.CallID, AgentID: best.AgentID, TS: best.TS})
			best = nil
		}
	}

	for i := range shapes {
		cur := shapes[i]
		newRun := i == 0 ||
			shapes[i-1].AgentID != cur.AgentID ||
			!extends(shapes[i-1], cur)
		if newRun {
			flush()
		}
		if cur.HasRaw {
			c := cur
			best = &c
		}
	}
	flush()

	// The walk emits agent-major (that is the order runs are detected in), but
	// callers reassemble a conversation in time order. Sorting here rather than
	// making them do it keeps the existing merge in sessionmessages.go reading
	// the same oldest-first sequence it always has.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].TS.Equal(out[j].TS) {
			return out[i].TS.Before(out[j].TS)
		}
		return out[i].CallID < out[j].CallID
	})
	return out
}

// SessionCover returns the calls whose captured request bodies must be read to
// reconstruct a session's conversation, oldest first. Everything not returned is
// a provable duplicate of something that is.
func (s *Store) SessionCover(sessionID string) ([]CoveringRequest, error) {
	shapes, err := s.sessionShapes(sessionID)
	if err != nil {
		return nil, err
	}
	return messageCover(shapes), nil
}

// UnfingerprintedCall is one call awaiting a fingerprint, with the captured
// request material needed to compute one.
type UnfingerprintedCall struct {
	CallID         string
	Wire           string
	ReqBody        []byte
	ReqContentType string
	// ReqHeaders is returned whole rather than with Content-Encoding extracted in
	// SQL. Captured header keys keep whatever casing the client sent — every
	// reader in this codebase looks them up with strings.EqualFold — so a
	// json_extract on '$."Content-Encoding"' would silently miss a body sent with
	// 'content-encoding'. That failure is quiet and permanent: the body would go
	// undecoded, yield no messages, and be marked unavailable forever.
	ReqHeaders map[string]string
}

// CallsNeedingFingerprint returns up to limit session-bearing calls that have a
// captured request body but no fingerprint yet, newest first.
//
// Only rows with a surviving capture can be backfilled at all — raw is pruned at
// 7 days while calls lives 90 — so the reachable set is bounded by the capture
// window, not by the ledger. Newest first because those are the sessions someone
// is most likely to open.
//
// This reads BODIES, which is the expensive thing this whole feature exists to
// avoid, so it is deliberately batched and deliberately not wired into any
// automatic path. See cmd/msgbackfill for the pacing and the reason.
func (s *Store) CallsNeedingFingerprint(limit int) ([]UnfingerprintedCall, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT c.id, c.wire, r.req_body, r.req_content_type, r.req_headers
		   FROM calls c
		   JOIN raw r ON r.call_id = c.id
		  WHERE c.session_id <> ''
		    AND c.msg_count = 0
		    AND (c.entrypoint = '' OR c.entrypoint = 'main')
		  ORDER BY c.ts DESC
		  LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: calls needing fingerprint: %w", err)
	}
	defer rows.Close()

	var out []UnfingerprintedCall
	for rows.Next() {
		var (
			c       UnfingerprintedCall
			headers string
		)
		if err := rows.Scan(&c.CallID, &c.Wire, &c.ReqBody, &c.ReqContentType, &headers); err != nil {
			return nil, fmt.Errorf("store: scan call needing fingerprint: %w", err)
		}
		if err := json.Unmarshal([]byte(headers), &c.ReqHeaders); err != nil {
			// Unreadable headers only cost us the Content-Encoding hint; the body
			// may still parse. Keep the row rather than dropping it.
			c.ReqHeaders = nil
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: calls needing fingerprint: %w", err)
	}
	return out, nil
}

// MarkFingerprintUnavailable records that a call's body could not yield a
// fingerprint, so a backfill does not retry it forever.
//
// msg_count is set to -1: a sentinel distinct both from 0 ("not attempted") and
// from any real count. callShape.known() requires Count > 0, so -1 reads as
// unknown to the cover exactly as 0 does — the body is still read rather than
// skipped. This changes only what the backfill will pick up again, never what
// the reader trusts.
func (s *Store) MarkFingerprintUnavailable(callID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if callID == "" {
		return nil
	}
	if _, err := s.db.Exec(`UPDATE calls SET msg_count = -1 WHERE id = ? AND msg_count = 0`, callID); err != nil {
		return fmt.Errorf("store: mark fingerprint unavailable: %w", err)
	}
	return nil
}
