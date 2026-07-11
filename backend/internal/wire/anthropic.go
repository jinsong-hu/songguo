package wire

import (
	"encoding/json"

	"github.com/songguo/songguo/internal/calls"
)

func init() {
	register(Wire{
		Name:       "anthropic/messages",
		Suffixes:   []string{"/messages"},
		Modality:   calls.ModalityChat,
		Extract:    anthropicExtract,
		NewScanner: newAnthropicScanner,
	})
	// Token counting (POST /v1/messages/count_tokens). Same request shape and
	// model as Messages, but Anthropic bills it as free, so it's ZeroCost: the
	// call is logged for observability and never priced. Its longer suffix
	// (/messages/count_tokens) always outranks /messages, so the two never
	// collide. The response ({"input_tokens":N}, not a "usage" object) isn't
	// parsed for billing.
	register(Wire{
		Name:     "anthropic/count_tokens",
		Suffixes: []string{"/messages/count_tokens"},
		Modality: calls.ModalityChat,
		Extract:  zeroCostExtract,
		ZeroCost: true,
	})
	// No anthropic/models wire: model listing is served by openai/models only.
	// (Both claimed the /models suffix, so a provider exposing both adapters on
	// one origin routed /v1/models ambiguously.)
}

// anthropicExtract meters a non-streaming Messages body: top-level "usage"
// with input_tokens/output_tokens plus cache fields.
func anthropicExtract(body []byte, _ Quirks) Extraction {
	return anthropicNormalize(topLevelUsage(body))
}

// anthropicNormalize maps an Anthropic usage object to the canonical view.
// Anthropic reports cache reads/creation OUTSIDE input_tokens, while pricing
// treats CachedInputTokens as a subset of InputTokens — so cache fields are
// folded into InputTokens here. Cache creation is billed at the full input
// rate (its 1.25x premium is ignored as a deliberate simplification).
func anthropicNormalize(usage map[string]any) Extraction {
	if usage == nil {
		return Extraction{Confidence: calls.ConfidenceUnknown}
	}
	cacheRead := numAt(usage, "cache_read_input_tokens")
	cacheCreate := numAt(usage, "cache_creation_input_tokens")
	return Extraction{
		Raw: usage,
		Norm: Normalized{
			InputTokens:       numAt(usage, "input_tokens") + cacheRead + cacheCreate,
			OutputTokens:      numAt(usage, "output_tokens"),
			CachedInputTokens: cacheRead,
		},
		Confidence: calls.ConfidenceMeasured,
	}
}

// anthropicScanner merges usage across an Anthropic SSE stream: input-side
// counts arrive nested in the message_start event (message.usage); output
// counts arrive in message_delta events (top-level usage, cumulative). Both
// must be read or input tokens are silently dropped.
type anthropicScanner struct {
	lineScanner
	merged map[string]any
}

func newAnthropicScanner(_ Quirks) StreamScanner {
	s := &anthropicScanner{}
	s.onLine = s.processLine
	return s
}

func (s *anthropicScanner) processLine(line []byte) {
	payload, ok := ssePayload(line)
	if !ok {
		return
	}
	var env struct {
		Type    string         `json:"type"`
		Delta   map[string]any `json:"delta"`
		Usage   map[string]any `json:"usage"`
		Message struct {
			Usage map[string]any `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	if env.Type == "content_block_delta" && len(env.Delta) > 0 {
		s.markFirstToken()
	}
	s.merge(env.Message.Usage)
	s.merge(env.Usage)
}

func (s *anthropicScanner) merge(usage map[string]any) {
	if usage == nil {
		return
	}
	if s.merged == nil {
		s.merged = make(map[string]any, len(usage))
	}
	for k, v := range usage {
		if v != nil {
			s.merged[k] = v
		}
	}
}

func (s *anthropicScanner) Result() Extraction {
	return anthropicNormalize(s.merged)
}
