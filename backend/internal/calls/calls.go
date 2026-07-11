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
	Vendor       string         // serving vendor name
	CredentialID string         // which credential in the 号池 served it
	Wire         string         // matched wire name (e.g. "openai/chat"); "" if unmatched
	Confidence   Confidence     // metering trustworthiness
	Status       int            // upstream HTTP status (0 if no response; StatusPending while in flight)
	Err          string         // error detail if any
	Usage        map[string]any // raw extracted usage (tokens/images/seconds/...), JSON-encoded in DB
	// Normalized cross-vendor token counts (persisted as typed columns so usage
	// is queryable without parsing the per-vendor `Usage` JSON). CachedTokens is
	// a subset of InputTokens billed at the cached rate.
	InputTokens   float64
	OutputTokens  float64
	CachedTokens  float64
	Cost          float64 // computed cost in USD (0 if unknown/free)
	LatencyMS     int64
	Stream        bool
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
// deliberately outside the HTTP status range and distinct from 0 (which means a
// finalized call that got no upstream response — a transport failure). A row
// left at StatusPending is a crashed/hung/aborted call that never reached
// update-at-end.
const StatusPending = -1
