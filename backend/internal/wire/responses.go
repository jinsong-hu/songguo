package wire

import (
	"encoding/json"
	"strings"

	"github.com/songguo/songguo/internal/calls"
)

func init() {
	register(Wire{
		Name:        "openai/responses",
		Suffixes:    []string{"/responses"},
		Modality:    calls.ModalityChat,
		Extract:     responsesExtract,
		NewScanner:  newResponsesScanner,
		DecodeReply: responsesDecodeReply,
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
	usage     map[string]any
	completed bool
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
	if env.Type == "response.completed" {
		s.completed = true
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

func (s *responsesScanner) StreamCompleted() bool {
	return s.completed
}

// responsesDecodeReply reads the assistant turn out of a stored Responses body.
// The items it returns are the response's own "output" array, which is already
// exactly the shape "input" items take on the way in — a reasoning item, a
// message item and a function_call item all render through the same path as
// their request-side twins.
func responsesDecodeReply(body []byte, streamed bool) []json.RawMessage {
	if !streamed {
		var resp struct {
			Output []json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil
		}
		return resp.Output
	}

	// A stream states its final output twice over: once item by item as each
	// one finishes, and once whole in the terminal event. Prefer the terminal
	// event — it is the vendor's own last word on what it sent — and fall back
	// to the per-item events, which are all a stream that died early left us.
	var (
		final []json.RawMessage
		items []json.RawMessage
	)
	for _, line := range storedSSELines(body) {
		payload, ok := ssePayload(line)
		if !ok {
			continue
		}
		var env struct {
			Type     string          `json:"type"`
			Item     json.RawMessage `json:"item"`
			Response struct {
				Output []json.RawMessage `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal(payload, &env); err != nil {
			continue
		}
		switch env.Type {
		case "response.completed", "response.incomplete":
			if env.Response.Output != nil {
				final = env.Response.Output
			}
		case "response.output_item.done":
			if hasJSONValue(env.Item) {
				items = append(items, env.Item)
			}
		}
	}
	if final != nil {
		return final
	}
	return items
}
