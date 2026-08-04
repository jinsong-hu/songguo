// Package calls records per-user usage and enforces budgets.
//
// It holds only pure domain types: an append-only Entry is written for every
// proxied call. Persistence lives in the store package; budget enforcement and
// dashboard views are queries over the resulting call log.
package calls

import (
	"strings"
	"time"
)

// Modality is the kind of AI call an Entry records.
type Modality string

// Known modalities. ModalityUnknown is the zero-value fallback.
const (
	ModalityChat      Modality = "chat"
	ModalityEmbedding Modality = "embedding"
	ModalityImage     Modality = "image"
	ModalityVideo     Modality = "video"
	ModalityTTS       Modality = "tts"
	ModalitySTT       Modality = "stt"
	ModalityMCP       Modality = "mcp"
	ModalityRealtime  Modality = "realtime"
	ModalityUnknown   Modality = "unknown"
)

// Confidence grades how trustworthy an Entry's metering is.
type Confidence string

const (
	// ConfidenceMeasured: usage was parsed from the upstream response by a
	// matching wire extractor (or the wire is zero-cost by definition).
	ConfidenceMeasured Confidence = "measured"
	// ConfidenceDerived: usage was estimated from request-side data (e.g.
	// counting TTS input characters) rather than reported by the upstream.
	ConfidenceDerived Confidence = "derived"
	// ConfidenceUnknown: no usage could be determined; cost is 0 and the
	// entry under-counts real spend.
	ConfidenceUnknown Confidence = "unknown"
)

// Entry is one append-only call record (one call attempt).
type Entry struct {
	ID string // UUID minted by the gateway at request-start
	// TS is when the call started (phase 1, create-at-start); TSEnd is when it
	// finished (phase 2, update-at-end) and is the zero value while the call is
	// still in flight. See docs/arch-gateway.md.
	TS           time.Time
	TSEnd        time.Time
	UserID       string // which Songguo user (may be "" for admin/unknown)
	Model        string
	Modality     Modality
	Vendor       string     // serving vendor name
	CredentialID string     // which credential in the 号池 served it
	Wire         string     // matched wire name (e.g. "openai/chat"); "" if unmatched
	Confidence   Confidence // metering trustworthiness
	// Status is the HTTP status the CLIENT received: the provider's own code for
	// a forwarded call, the gateway's for a denial, StatusPending while in
	// flight. Read it with Err — see OutcomeOf — never alone, or a 429 songguo
	// issued is indistinguishable from a 429 the provider returned.
	Status int
	// Err says whose doing the outcome was: "" for anything forwarded to a
	// provider, otherwise one of the Err* slugs above. Never a duplicate of
	// Status; the pair is what carries the meaning.
	Err   string
	Usage map[string]any // raw extracted usage (tokens/images/seconds/...), JSON-encoded in DB
	// Normalized cross-vendor token counts (persisted as typed columns so usage
	// is queryable without parsing the per-vendor `Usage` JSON). The three
	// input-side fields are DISJOINT and sum to total input: InputTokens is fresh
	// (uncached) input, CachedTokens is cache reads (persisted as
	// cache_read_input_tokens), CacheCreationTokens is cache writes. ThinkingTokens
	// is a subset of OutputTokens (reasoning/thinking), never added to totals.
	InputTokens         float64
	OutputTokens        float64
	CachedTokens        float64
	CacheCreationTokens float64
	ThinkingTokens      float64
	// Non-token metered units, persisted as typed columns like the token fields
	// so speech usage is queryable without parsing the per-vendor `Usage` JSON.
	// Seconds is billed audio duration (ASR, per_second); Chars is billed text
	// length (TTS, per_char). Both 0 for token-metered traffic. Images stays in
	// the raw `Usage` JSON only — no wire meters it into a column yet.
	Seconds      float64
	Chars        float64
	Cost         float64 // computed cost in USD (0 if unknown/free)
	LatencyMS    int64   // full upstream request duration through response body completion
	TTFTMS       int64   // upstream request start to first generated stream delta; 0 when unavailable
	GenerationMS int64   // first generated stream delta to stream completion; 0 when unavailable
	Stream       bool
	// Tool-use metrics for the just-completed tool round-trip this call carries
	// (see compose.ToolTurn): the number of tool calls the assistant issued that
	// turn and a LOCAL o200k token estimate of the results that came back. Read
	// from the request tail only, so they sum cleanly across a session without
	// double-counting cumulative history. ToolTokens is a derived estimate,
	// decoupled from InputTokens/OutputTokens — it does NOT reconcile to billed
	// usage. Both are 0 for non-chat traffic and for turns that carry no results.
	ToolCalls     int
	ToolTokens    float64
	Tags          map[string]string // optional business attribution (may be nil)
	ClientName    string            // normalized caller client (e.g. claude-code, codex)
	ClientVersion string            // caller client version parsed from User-Agent
	// Best-effort caller OS. ClientOS is a normalized family (e.g. MacOS, Linux,
	// Windows); ClientOSVersion is the OS version when the source carries one.
	// Both empty when unavailable — see ParseClientInfo.
	ClientOS        string
	ClientOSVersion string
	// Coding-agent attribution, captured verbatim from known request headers
	// (read-only; no bytes are mutated). Empty for ordinary API traffic. SessionID
	// groups a run's calls; AgentID + ParentAgentID reconstruct the
	// main-loop→subagent tree when available.
	SessionID     string
	AgentID       string
	ParentAgentID string
	// Entrypoint labels why the call was made — a visible main-loop turn vs a
	// harness utility call (monitor, count_tokens, title/compaction). Classified
	// read-only from the request path + body at request-start (see
	// ClassifyEntrypoint). Empty on legacy rows is treated as main. Read-only
	// tag; never a routing input.
	Entrypoint Entrypoint
}

// ClientInfo is a normalized caller client parsed from User-Agent. Names match
// thesvg slugs used by the dashboard icons.
type ClientInfo struct {
	Name    string
	Version string
	// OS is the normalized caller OS family (e.g. MacOS, Linux, Windows); Version
	// is the OS version when the source carries one. Both empty when unknown.
	OS        string
	OSVersion string
}

// ParseClientInfo recognizes the coding-agent clients we render specially and
// makes a best-effort read of the caller OS. User-Agent product tokens and the
// X-Stainless-Os header are parsed read-only; the original headers are still
// forwarded verbatim.
//
// OS has two sources, both optional: the explicit X-Stainless-Os header sent by
// Stainless-generated SDKs (claude-code, and raw openai/anthropic SDK callers),
// which carries no version; else — for codex, whose UA embeds the platform in a
// browser-style comment, e.g. "codex_cli_rs/0.129.0 (Mac OS 26.5.2; arm64)" —
// the OS family and version parsed from that comment. Anything unrecognized
// leaves the OS fields empty.
func ParseClientInfo(ua, stainlessOS string) ClientInfo {
	var ci ClientInfo
	for _, raw := range strings.Fields(ua) {
		token := strings.Trim(raw, "(),;")
		product, version, ok := strings.Cut(token, "/")
		if !ok {
			continue
		}
		switch strings.ToLower(product) {
		case "claude-cli", "claude-code":
			ci.Name, ci.Version = "claude-code", version
		case "codex", "codex-cli", "codex-tui", "codex_cli_rs":
			ci.Name, ci.Version = "codex-openai", version
		}
		if ci.Name != "" {
			break
		}
	}
	// Prefer the explicit SDK header (no version); else parse codex's UA comment,
	// which is the only client UA known to carry the platform. Gating on codex
	// avoids mis-reading a non-OS comment (e.g. claude-cli's "(external, ...)").
	if os := strings.TrimSpace(stainlessOS); os != "" {
		ci.OS = normalizeOS(os)
	} else if ci.Name == "codex-openai" {
		if seg := uaCommentOS(ua); seg != "" {
			name, version := splitOSVersion(seg)
			ci.OS = normalizeOS(name)
			ci.OSVersion = version
		}
	}
	return ci
}

// normalizeOS maps a raw OS name to a canonical family. Known families collapse
// to a single spelling (notably MacOS, matching the X-Stainless-Os token);
// anything unrecognized is returned trimmed, best-effort, rather than dropped.
func normalizeOS(name string) string {
	name = strings.TrimSpace(name)
	switch key := strings.ReplaceAll(strings.ToLower(name), " ", ""); {
	case strings.HasPrefix(key, "macos"), key == "macosx", key == "osx", key == "darwin":
		return "MacOS"
	case strings.HasPrefix(key, "windows"), key == "win":
		return "Windows"
	case strings.HasPrefix(key, "linux"):
		return "Linux"
	case strings.HasPrefix(key, "ios"):
		return "iOS"
	case strings.HasPrefix(key, "android"):
		return "Android"
	}
	return name
}

// uaCommentOS returns the first ';'-delimited segment inside a User-Agent's
// first "(...)" comment — where browser-style clients (codex) place the OS —
// or "" if there is no comment.
func uaCommentOS(ua string) string {
	i := strings.IndexByte(ua, '(')
	if i < 0 {
		return ""
	}
	j := strings.IndexByte(ua[i+1:], ')')
	if j < 0 {
		return ""
	}
	seg := ua[i+1 : i+1+j]
	if k := strings.IndexByte(seg, ';'); k >= 0 {
		seg = seg[:k]
	}
	return strings.TrimSpace(seg)
}

// splitOSVersion separates an OS segment like "Mac OS 26.5.2" into its family
// name ("Mac OS") and version ("26.5.2") at the first digit-led token. A segment
// with no numeric part (e.g. "Linux") yields an empty version.
func splitOSVersion(seg string) (name, version string) {
	fields := strings.Fields(seg)
	for i, f := range fields {
		if f != "" && f[0] >= '0' && f[0] <= '9' {
			return strings.Join(fields[:i], " "), strings.Join(fields[i:], " ")
		}
	}
	return strings.TrimSpace(seg), ""
}

// StatusPending is the sentinel status for a call that has been created (phase
// 1) but not yet finalized (phase 2): the request is in flight. It is
// deliberately outside the HTTP status range so it can never collide with a code
// a server actually sent. A row left at StatusPending never reached
// update-at-end — see OutcomeInFlight/OutcomeAbandoned for the two ways that
// happens and how they are told apart.
//
// It is an INTERNAL sentinel. It is still emitted verbatim on the API (callers
// filter on it), but nothing user-facing may ever render it as a status code.
const StatusPending = -1

// Error slugs the gateway writes into Entry.Err to say an outcome was its own
// doing rather than the provider's. A forwarded call always carries Err == "",
// so the (Status, Err) pair is a total, unambiguous encoding of what happened —
// which is why the outcome is derived from both and never persisted separately.
//
// Some of these double as the client-facing error type ("songguo_" + slug) and
// some do not; the two are deliberately decoupled, so changing a slug here does
// not change what a caller sees on the wire.
const (
	ErrBudgetExceeded     = "budget_exceeded"
	ErrRateLimited        = "rate_limited"
	ErrClientGone         = "client_gone"
	ErrModelNotAllowed    = "model_not_allowed"
	ErrVendorNotAllowed   = "vendor_not_allowed"
	ErrNoUpstream         = "no_upstream"
	ErrRequestBuildFailed = "request_build_failed"

	// ErrUpstreamError is legacy: before the split it covered a transport failure
	// and a request-build failure alike. Nothing writes it any more; rows in the
	// ledger still carry it and we cannot now tell which failure they were.
	ErrUpstreamError = "upstream_error"
)

// Slug prefixes, each followed by ": <detail>". The detail is the real error
// text, kept because "the vendor did not answer" and "the vendor's certificate
// has expired" are different facts and only one of them is actionable.
const (
	ErrPrefixUnmatched = "unmatched: "
	ErrPrefixTransport = "transport_error: "
	ErrPrefixStream    = "stream_error: "
)

// Outcome is what actually happened to a call.
//
// The status code alone cannot say. songguo mints statuses of its own — a budget
// refusal is 402, an unmatched wire 404, a routing miss 502 — so on the status
// int a denial we issued is indistinguishable from the same code coming back
// from a provider. Outcome is derived from (Status, Err) so the two never blur.
type Outcome string

const (
	// No verdict yet, or ever.
	OutcomeInFlight  Outcome = "in_flight" // running now
	OutcomeAbandoned Outcome = "abandoned" // the gateway restarted before it finished

	// The provider answered, or failed to.
	OutcomeOK             Outcome = "ok"
	OutcomeVendorError    Outcome = "vendor_error"    // the provider's own error, forwarded
	OutcomeTruncated      Outcome = "truncated"       // began answering, stream broke mid-body
	OutcomeTransportError Outcome = "transport_error" // never answered

	// songguo's own doing.
	OutcomeNoRoute      Outcome = "no_route"
	OutcomeBuildFailed  Outcome = "build_failed"
	OutcomeUnmatched    Outcome = "unmatched"
	OutcomeDeniedBudget Outcome = "denied_budget"
	OutcomeDeniedRate   Outcome = "denied_rate"
	OutcomeDeniedScope  Outcome = "denied_scope"

	// The caller's.
	OutcomeClientGone Outcome = "client_gone"

	// Legacy ErrUpstreamError rows: a transport failure or a build failure, and
	// we cannot tell which. Its own value rather than a guess at either.
	OutcomeUpstreamFailed Outcome = "upstream_failed"

	// An Err slug this build does not know. Exists so a future slug degrades to a
	// visible "we don't know" instead of being silently filed as a vendor fault.
	OutcomeUnknown Outcome = "unknown"
)

// OutcomeOf classifies a finalized call from the pair the ledger stores.
//
// pending callers must handle StatusPending themselves: telling in-flight from
// abandoned needs the gateway's boot time, which this package does not have.
func OutcomeOf(status int, err string) Outcome {
	if status == StatusPending {
		return OutcomeInFlight
	}
	if err == "" {
		// Legacy rows: before the slugs existed, status 0 with no detail was the
		// documented encoding for "no upstream response". Nothing writes it now.
		// Reading it as a transport failure is decoding an old convention, not
		// guessing — the old code and docs agree on what it meant.
		if status == 0 {
			return OutcomeTransportError
		}
		// Forwarded: the provider answered and the status is entirely theirs.
		if status >= 400 {
			return OutcomeVendorError
		}
		return OutcomeOK
	}
	switch {
	case strings.HasPrefix(err, ErrPrefixTransport):
		return OutcomeTransportError
	case strings.HasPrefix(err, ErrPrefixStream):
		return OutcomeTruncated
	case strings.HasPrefix(err, ErrPrefixUnmatched):
		return OutcomeUnmatched
	}
	switch err {
	case ErrClientGone:
		return OutcomeClientGone
	case ErrBudgetExceeded:
		return OutcomeDeniedBudget
	case ErrRateLimited:
		return OutcomeDeniedRate
	case ErrModelNotAllowed, ErrVendorNotAllowed:
		return OutcomeDeniedScope
	case ErrNoUpstream:
		return OutcomeNoRoute
	case ErrRequestBuildFailed:
		return OutcomeBuildFailed
	case ErrUpstreamError:
		return OutcomeUpstreamFailed
	}
	return OutcomeUnknown
}

// Blame is who an outcome is about. Kept separate from severity on purpose: a
// routing miss is as bad as a provider outage and colored the same, but it is
// our misconfiguration and must never count against the provider.
type Blame string

const (
	BlameProvider Blame = "provider"
	BlameGateway  Blame = "gateway"
	BlameCaller   Blame = "caller"
	BlameNone     Blame = "none" // no verdict is available
)

// BlameFor reports who an outcome is about. It is the single definition of
// "does this count against a provider", used by the aggregates so a success rate
// cannot disagree with the pill next to it.
func BlameFor(o Outcome) Blame {
	switch o {
	case OutcomeOK, OutcomeVendorError, OutcomeTruncated, OutcomeTransportError:
		return BlameProvider
	case OutcomeNoRoute, OutcomeBuildFailed, OutcomeUnmatched,
		OutcomeDeniedBudget, OutcomeDeniedRate, OutcomeDeniedScope:
		return BlameGateway
	case OutcomeClientGone:
		return BlameCaller
	}
	// in_flight, abandoned, upstream_failed, unknown: nothing is proven.
	return BlameNone
}

// IsProviderFailure reports whether an outcome counts against a provider's
// success rate. In-flight work has no verdict, a caller hanging up is the
// caller's business, and songguo's own refusals are ours — none is the provider
// failing.
func IsProviderFailure(o Outcome) bool {
	return BlameFor(o) == BlameProvider && o != OutcomeOK
}

// IsPolicyDenial reports whether an outcome is songguo enforcing a limit the
// operator configured — the gateway working, not failing. Such a call never
// reached a provider, so it is evidence about a budget or a rate limit and about
// nothing else; it belongs in neither side of a success rate, exactly like a
// pending row or a caller hanging up.
//
// Blame cannot express this. A routing miss and a budget refusal are both
// BlameGateway, but the first is a misconfiguration that should show up as a
// failure and the second is the configuration doing its job.
//
// The list is explicitly two cases rather than "gateway minus the malfunctions",
// so a slug added later has to be classified deliberately instead of being
// absorbed into the exemption by default. In particular OutcomeDeniedScope is
// NOT here: a budget resets and a rate window passes, but a caller asking for a
// model its key does not carry fails identically forever until an operator
// changes something, and no other panel would show it.
func IsPolicyDenial(o Outcome) bool {
	switch o {
	case OutcomeDeniedBudget, OutcomeDeniedRate:
		return true
	}
	return false
}
