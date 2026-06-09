// Package ledger records per-token usage and enforces budgets.
//
// It holds only pure domain types: an append-only Entry is written for every
// proxied call attempt (including each failover attempt). Persistence lives in
// the store package; budget enforcement and dashboard views are queries over
// the resulting ledger.
package ledger

import "time"

// Modality is the kind of AI call a ledger Entry records.
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
	ModalityUnknown   Modality = "unknown"
)

// Entry is one append-only ledger record (one call attempt).
type Entry struct {
	ID           int64
	TS           time.Time // when the call completed
	TokenID      string    // which Songguo token (may be "" for admin/unknown)
	Model        string
	Modality     Modality
	Vendor       string         // serving vendor name
	CredentialID string         // which credential in the 号池 served it
	Attempt      int            // 1-based attempt number (failover increments)
	Status       int            // upstream HTTP status (0 if no response)
	Err          string         // error detail if any
	Usage        map[string]any // raw extracted usage (tokens/images/seconds/...), JSON-encoded in DB
	Cost         float64        // computed cost in USD (0 if unknown/free)
	LatencyMS    int64
	Stream       bool
	Tags         map[string]string // optional business attribution (may be nil)
}
