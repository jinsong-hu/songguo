package calls

import (
	"encoding/json"
	"strings"
)

// Entrypoint labels why a call was made, distinguishing a coding agent's
// visible main-loop turns from the harness utility calls that ride the same
// wire but are not part of the conversation. It is a read-only ledger tag,
// classified from the request path and buffered body — NEVER an input to
// routing (a monitor call and a main turn to the same model hit the same wire).
//
// The split exists so the session rollup can keep utility calls in the cost and
// token sums (they are real spend the user paid for but never saw) while
// excluding them from the accretion metrics — context growth, turn count, tool
// activity — which only mean something for a conversation that builds on itself.
type Entrypoint string

const (
	// EntrypointMain is a visible main-loop turn. It is the safe default: every
	// call that does not match a utility signal is Main, so an unrecognized call
	// stays on the conversation curve rather than being silently dropped from it.
	EntrypointMain Entrypoint = "main"
	// EntrypointCountTokens is a token-counting probe (e.g. Anthropic's
	// /v1/messages/count_tokens). Classified structurally by path — the most
	// reliable signal — and billed differently, so it wants its own row.
	EntrypointCountTokens Entrypoint = "count_tokens"
	// EntrypointMonitor is an auto-mode security/guardrail classifier call the
	// harness fires around a main action. Recognized by its </block> stop
	// sequence; one-shot and stateless.
	EntrypointMonitor Entrypoint = "monitor"
	// EntrypointUtility is a non-main harness call recognized only by a generic
	// SDK/utility billing entrypoint (cc_entrypoint), without a more specific
	// signal — e.g. title/summary or compaction calls.
	EntrypointUtility Entrypoint = "utility"
)

// IsUtility reports whether the entrypoint is a harness utility call (anything
// but a visible main-loop turn). Utility calls are excluded from the session's
// accretion metrics but kept in its spend.
func (e Entrypoint) IsUtility() bool {
	return e != "" && e != EntrypointMain
}

// ClassifyEntrypoint labels a call from its request path and buffered request
// body, read-only. Precedence runs most-reliable first:
//
//  1. path ending in /count_tokens — structural, unambiguous.
//  2. a </block> stop sequence — the auto-mode monitor's signature.
//  3. a non-interactive cc_entrypoint billing marker (e.g. sdk-ts) — a generic
//     harness/utility call with no finer signal.
//
// Anything else is EntrypointMain. Signals 2–3 are heuristics on client-shaped
// fields, so the default deliberately favors Main: over-excluding would drop a
// real turn from the user's context history, which is worse than leaving a
// stray harness call on the chart.
func ClassifyEntrypoint(path string, body []byte) Entrypoint {
	if p := strings.TrimRight(path, "/"); strings.HasSuffix(p, "/count_tokens") {
		return EntrypointCountTokens
	}
	if len(body) == 0 {
		return EntrypointMain
	}

	var env struct {
		StopSequences []string `json:"stop_sequences"`
		System        any      `json:"system"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return EntrypointMain // unparseable body: treat as an ordinary turn
	}

	for _, s := range env.StopSequences {
		if strings.Contains(s, "</block>") {
			return EntrypointMonitor
		}
	}

	// cc_entrypoint is stamped by the Anthropic client into a system text block
	// (x-anthropic-billing-header), e.g. "cc_entrypoint=sdk-ts". Only a KNOWN
	// SDK/utility marker flips the call to utility; any other value (including
	// "cli" and anything unrecognized) stays main. This favors main on ambiguity —
	// over-excluding would drop a real turn from the context history, which is
	// worse than leaving a stray harness call on the chart.
	if isUtilityEntrypointMarker(ccEntrypoint(env.System)) {
		return EntrypointUtility
	}

	return EntrypointMain
}

// isUtilityEntrypointMarker reports whether a cc_entrypoint value names a known
// SDK/utility harness path (vs the interactive CLI or an unknown value). Kept as
// a small allowlist so an unfamiliar entrypoint defaults to main.
func isUtilityEntrypointMarker(ep string) bool {
	switch ep {
	case "sdk-ts", "sdk-py", "sdk-cli":
		return true
	}
	return false
}

// ccEntrypoint extracts the cc_entrypoint value from an Anthropic request's
// `system` field, which may be a plain string or an array of content blocks.
// The billing header is a text block shaped like
// "x-anthropic-billing-header: cc_version=…; cc_entrypoint=sdk-ts;". Returns ""
// when no such marker is present.
func ccEntrypoint(system any) string {
	switch v := system.(type) {
	case string:
		return parseCCEntrypoint(v)
	case []any:
		for _, blk := range v {
			m, ok := blk.(map[string]any)
			if !ok {
				continue
			}
			if txt, _ := m["text"].(string); txt != "" {
				if ep := parseCCEntrypoint(txt); ep != "" {
					return ep
				}
			}
		}
	}
	return ""
}

// parseCCEntrypoint pulls the cc_entrypoint token out of a billing-header text
// blob, tolerant of surrounding "key=value;" pairs and whitespace.
func parseCCEntrypoint(s string) string {
	const key = "cc_entrypoint="
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key):]
	// Value runs to the next ';', whitespace, or end of string.
	end := strings.IndexAny(rest, "; \t\r\n")
	if end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}
