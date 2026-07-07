// Package calls records per-user usage and enforces budgets.
//
// It holds only pure domain types: an append-only Entry is written for every
// proxied call. Persistence lives in the store package; budget enforcement and
// dashboard views are queries over the resulting call log.
package calls

import "time"

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
	ID           int64
	TS           time.Time // when the call completed
	UserID       string    // which Songguo user (may be "" for admin/unknown)
	Model        string
	Modality     Modality
	Vendor       string         // serving vendor name
	CredentialID string         // which credential in the 号池 served it
	Wire         string         // matched wire name (e.g. "openai/chat"); "" if unmatched
	Confidence   Confidence     // metering trustworthiness
	Status       int            // upstream HTTP status (0 if no response)
	Err          string         // error detail if any
	Usage        map[string]any // raw extracted usage (tokens/images/seconds/...), JSON-encoded in DB
	// Normalized cross-vendor token counts (persisted as typed columns so usage
	// is queryable without parsing the per-vendor `Usage` JSON). CachedTokens is
	// a subset of InputTokens billed at the cached rate.
	InputTokens  float64
	OutputTokens float64
	CachedTokens float64
	Cost         float64 // computed cost in USD (0 if unknown/free)
	LatencyMS    int64
	Stream       bool
	Tags         map[string]string // optional business attribution (may be nil)
	// Coding-agent attribution, captured verbatim from known request headers
	// (read-only; no bytes are mutated). Empty for ordinary API traffic. SessionID
	// groups a run's calls; AgentID + ParentAgentID reconstruct the
	// main-loop→subagent tree when available.
	SessionID     string
	AgentID       string
	ParentAgentID string
}
