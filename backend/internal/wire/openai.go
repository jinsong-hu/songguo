package wire

import (
	"encoding/json"

	"github.com/songguo/songguo/internal/calls"
)

// Cache-token quirk values for the openai family. Vendors report cached input
// tokens under different fields despite sharing the chat wire format.
const (
	quirkCacheTokens = "cache_tokens"
	cacheDeepSeek    = "deepseek" // prompt_cache_hit_tokens (top level)
	cacheMiniMax     = "minimax"  // cached_tokens (top level)
	// default: prompt_tokens_details.cached_tokens (OpenAI, Ark, Zhipu)
)

func init() {
	register(Wire{
		Name:       "openai/chat",
		Suffixes:   []string{"/chat/completions"},
		Modality:   calls.ModalityChat,
		Extract:    openAIExtract,
		NewScanner: newOpenAIScanner,
	})
	register(Wire{
		Name:       "openai/completions",
		Suffixes:   []string{"/completions"},
		Modality:   calls.ModalityChat,
		Extract:    openAIExtract,
		NewScanner: newOpenAIScanner,
	})
	register(Wire{
		Name:     "openai/embeddings",
		Suffixes: []string{"/embeddings"},
		Modality: calls.ModalityEmbedding,
		Extract:  openAIExtract,
	})
	register(Wire{
		Name:     "openai/models",
		Suffixes: []string{"/models"},
		Modality: calls.ModalityUnknown,
		Extract:  zeroCostExtract,
		ZeroCost: true,
	})
	// Image generation (OpenAI-compatible /images/generations, e.g. Doubao
	// Seedream). Responses carry no token usage, so it's billed per_call.
	register(Wire{
		Name:     "openai/images",
		Suffixes: []string{"/images/generations", "/images/edits"},
		Modality: calls.ModalityImage,
		Extract:  perCallExtract,
	})
}

// zeroCostExtract meters management endpoints as free without parsing.
func zeroCostExtract(_ []byte, _ Quirks) Extraction {
	return Extraction{Confidence: calls.ConfidenceMeasured}
}

// openAIExtract meters a non-streaming chat-completions-shaped body: a
// top-level "usage" object with prompt/completion token counts.
func openAIExtract(body []byte, q Quirks) Extraction {
	return openAINormalize(topLevelUsage(body), q)
}

// openAINormalize maps a chat-completions usage object to the canonical view.
func openAINormalize(usage map[string]any, q Quirks) Extraction {
	if usage == nil {
		return Extraction{Confidence: calls.ConfidenceUnknown}
	}
	in := numAt(usage, "prompt_tokens")
	if in == 0 {
		in = numAt(usage, "input_tokens")
	}
	out := numAt(usage, "completion_tokens")
	if out == 0 {
		out = numAt(usage, "output_tokens")
	}

	var cached float64
	switch q[quirkCacheTokens] {
	case cacheDeepSeek:
		cached = numAt(usage, "prompt_cache_hit_tokens")
	case cacheMiniMax:
		cached = numAt(usage, "cached_tokens")
	default:
		cached = numAt(usage, "prompt_tokens_details", "cached_tokens")
	}

	return Extraction{
		Raw:        usage,
		Norm:       Normalized{InputTokens: in, OutputTokens: out, CachedInputTokens: cached},
		Confidence: calls.ConfidenceMeasured,
	}
}

// topLevelUsage parses a JSON body's top-level "usage" object, nil if absent
// or unparseable.
func topLevelUsage(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var env struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	return env.Usage
}

// openAIScanner keeps the latest non-null top-level usage seen in an SSE
// stream. OpenAI-family vendors emit usage in the final chunk (some only when
// the client requested stream_options.include_usage).
type openAIScanner struct {
	lineScanner
	quirks Quirks
	usage  map[string]any
}

func newOpenAIScanner(q Quirks) StreamScanner {
	s := &openAIScanner{quirks: q}
	s.onLine = s.processLine
	return s
}

func (s *openAIScanner) processLine(line []byte) {
	payload, ok := ssePayload(line)
	if !ok {
		return
	}
	var env struct {
		Usage   map[string]any `json:"usage"`
		Choices []struct {
			Text  string         `json:"text"`
			Delta map[string]any `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	for _, choice := range env.Choices {
		if choice.Text != "" ||
			nonEmptyJSONValue(choice.Delta["content"]) ||
			nonEmptyJSONValue(choice.Delta["reasoning_content"]) ||
			nonEmptyJSONValue(choice.Delta["tool_calls"]) ||
			nonEmptyJSONValue(choice.Delta["function_call"]) {
			s.markFirstToken()
			break
		}
	}
	if env.Usage != nil {
		s.usage = env.Usage
	}
}

func (s *openAIScanner) Result() Extraction {
	return openAINormalize(s.usage, s.quirks)
}
