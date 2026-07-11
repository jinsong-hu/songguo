package wire

import (
	"encoding/json"
	"strings"

	"github.com/songguo/songguo/internal/calls"
)

func init() {
	register(Wire{
		Name:       "openai/responses",
		Suffixes:   []string{"/responses"},
		Modality:   calls.ModalityChat,
		Extract:    responsesExtract,
		NewScanner: newResponsesScanner,
	})
}

// responsesExtract meters a non-streaming Responses API body: top-level
// "usage" with input_tokens/output_tokens and a cached detail.
func responsesExtract(body []byte, _ Quirks) Extraction {
	return responsesNormalize(topLevelUsage(body))
}

// responsesNormalize maps a Responses-API usage object to the canonical view.
// Like chat-completions, input_tokens here is cache-INCLUSIVE, so the cached
// portion is subtracted to keep the canonical InputTokens fresh-only. Reasoning
// tokens are a subset of output_tokens.
func responsesNormalize(usage map[string]any) Extraction {
	if usage == nil {
		return Extraction{Confidence: calls.ConfidenceUnknown}
	}
	cached := numAt(usage, "input_tokens_details", "cached_tokens")
	return Extraction{
		Raw: usage,
		Norm: Normalized{
			InputTokens:       maxZero(numAt(usage, "input_tokens") - cached),
			OutputTokens:      numAt(usage, "output_tokens"),
			CachedInputTokens: cached,
			ThinkingTokens:    numAt(usage, "output_tokens_details", "reasoning_tokens"),
		},
		Confidence: calls.ConfidenceMeasured,
	}
}

// responsesScanner reads usage from Responses API event streams. Usage rides
// inside the response object of the terminal event ("response.completed"
// data carries {"response": {..., "usage": {...}}}); a top-level usage is
// accepted as a fallback for compatible vendors.
type responsesScanner struct {
	lineScanner
	usage map[string]any
}

func newResponsesScanner(_ Quirks) StreamScanner {
	s := &responsesScanner{}
	s.onLine = s.processLine
	return s
}

func (s *responsesScanner) processLine(line []byte) {
	payload, ok := ssePayload(line)
	if !ok {
		return
	}
	var env struct {
		Type     string         `json:"type"`
		Delta    any            `json:"delta"`
		Usage    map[string]any `json:"usage"`
		Response struct {
			Usage map[string]any `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	if strings.HasSuffix(env.Type, ".delta") && nonEmptyJSONValue(env.Delta) {
		s.markFirstToken()
	}
	if env.Response.Usage != nil {
		s.usage = env.Response.Usage
	} else if env.Usage != nil {
		s.usage = env.Usage
	}
}

func (s *responsesScanner) Result() Extraction {
	return responsesNormalize(s.usage)
}
