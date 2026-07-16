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
	// EntrypointUtility is a non-main harness call — title/summary, compaction,
	// topic detection, background/memory maintenance — recognized by a per-call
	// cc_workload marker in the billing header. Not to be confused with
	// cc_entrypoint, which is a fixed per-process launch tag and says nothing
	// about why an individual call was made.
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
//  3. a cc_workload marker in the billing header — a per-call tag the harness
//     stamps on non-conversation work (compact, title, topic, cron, memory,
//     background). Its mere presence flips the call to utility.
//
// Anything else is EntrypointMain. Signals 2–3 are heuristics on client-shaped
// fields, so the default deliberately favors Main: over-excluding would drop a
// real turn from the user's context history, which is worse than leaving a
// stray harness call on the chart.
//
// NOTE: we key on cc_workload, NOT cc_entrypoint. cc_entrypoint is
// process.env.CLAUDE_CODE_ENTRYPOINT — a fixed per-process launch tag ("cli",
// "sdk-cli", "sdk-ts", …) stamped identically onto every request the process
// makes, main turns included. An SDK/IDE-launched session (e.g. the VS Code
// extension, which uses "sdk-cli") therefore carries an SDK marker on 100% of
// its calls; keying utility off it mislabeled entire conversations. cc_workload
// is the per-call signal: the client wraps only background/harness operations in
// it, so a main-loop turn carries no cc_workload at all.
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

	// A cc_workload marker names a non-conversation harness operation. Any value
	// (compact, title, topic, cron, memory, background, …) flips the call to
	// utility; a visible main-loop turn carries none, so absence stays main.
	if billingField(env.System, "cc_workload") != "" {
		return EntrypointUtility
	}

	return EntrypointMain
}

// billingField extracts a "key=value" token from the Anthropic
// x-anthropic-billing-header, which the client stows in the request's `system`
// field — a plain string or an array of content blocks. The header is a text
// block shaped like
// "x-anthropic-billing-header: cc_version=…; cc_entrypoint=sdk-cli; cc_workload=compact;".
// Returns the value of the first matching key, or "" when absent.
func billingField(system any, key string) string {
	switch v := system.(type) {
	case string:
		return parseBillingField(v, key)
	case []any:
		for _, blk := range v {
			m, ok := blk.(map[string]any)
			if !ok {
				continue
			}
			if txt, _ := m["text"].(string); txt != "" {
				if val := parseBillingField(txt, key); val != "" {
					return val
				}
			}
		}
	}
	return ""
}

// parseBillingField pulls the value for `key` out of a billing-header text blob,
// tolerant of surrounding "key=value;" pairs and whitespace. `key` is matched
// bare (e.g. "cc_workload"); the "=" is appended here.
func parseBillingField(s, key string) string {
	marker := key + "="
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	// Value runs to the next ';', whitespace, or end of string.
	end := strings.IndexAny(rest, "; \t\r\n")
	if end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}
