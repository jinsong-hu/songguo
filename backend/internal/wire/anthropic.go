package wire

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/songguo/songguo/internal/calls"
)

func init() {
	register(Wire{
		Name:        "anthropic/messages",
		Suffixes:    []string{"/messages"},
		Modality:    calls.ModalityChat,
		Extract:     anthropicExtract,
		NewScanner:  newAnthropicScanner,
		DecodeReply: anthropicDecodeReply,
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
// Anthropic is the reference shape: it reports input_tokens (fresh), cache reads,
// and cache creation as three disjoint fields, mapped straight through. Cache
// creation is billed at the full input rate (its 1.25x premium is ignored as a
// deliberate simplification). Thinking tokens are a subset of output_tokens.
func anthropicNormalize(usage map[string]any) Extraction {
	if usage == nil {
		return Extraction{Confidence: calls.ConfidenceUnknown}
	}
	return Extraction{
		Raw: usage,
		Norm: Normalized{
			InputTokens:         numAt(usage, "input_tokens"),
			OutputTokens:        numAt(usage, "output_tokens"),
			CachedInputTokens:   numAt(usage, "cache_read_input_tokens"),
			CacheCreationTokens: numAt(usage, "cache_creation_input_tokens"),
			ThinkingTokens:      numAt(usage, "output_tokens_details", "thinking_tokens"),
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
	merged    map[string]any
	completed bool
	streamErr string
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
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	if env.Type == "content_block_delta" && len(env.Delta) > 0 {
		s.markFirstToken()
	}
	switch env.Type {
	case "message_stop":
		// The protocol's terminal event. Everything after it is epilogue, so a
		// client that closes here has a complete answer, not a truncated one.
		s.completed = true
	case "error":
		// An in-band failure the vendor reported itself. First one wins: it is
		// the cause, and anything after is fallout.
		if s.streamErr == "" {
			s.streamErr = anthropicErrorText(env.Error.Type, env.Error.Message)
		}
	}
	s.merge(env.Message.Usage)
	s.merge(env.Usage)
}

// anthropicErrorText renders an SSE error event as one line, preferring the
// vendor's own message and never returning the empty string — "" is the
// StreamErrorReporter signal for "no error seen", so an error event with an
// empty body must still read as an error.
func anthropicErrorText(errType, message string) string {
	switch {
	case errType != "" && message != "":
		return errType + ": " + message
	case message != "":
		return message
	case errType != "":
		return errType
	default:
		return "vendor reported a stream error with no detail"
	}
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

func (s *anthropicScanner) StreamCompleted() bool { return s.completed }

func (s *anthropicScanner) StreamError() string { return s.streamErr }

func (s *anthropicScanner) Result() Extraction {
	return anthropicNormalize(s.merged)
}

// anthropicDecodeReply reads the assistant turn out of a stored Messages body.
// It returns a single item shaped like the request's own assistant messages —
// {"role":"assistant","content":[...]} — so a thinking block, a text block and
// a tool_use block render exactly as they do when a later turn sends them back
// as history.
func anthropicDecodeReply(body []byte, streamed bool) []json.RawMessage {
	var content json.RawMessage
	if streamed {
		content = anthropicStreamContent(body)
	} else {
		var resp struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil
		}
		content = resp.Content
	}
	if !hasJSONValue(content) {
		return nil
	}
	item, err := json.Marshal(struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}{Role: "assistant", Content: content})
	if err != nil {
		return nil
	}
	return []json.RawMessage{item}
}

// anthropicReplyBlock accumulates one content block of a stream. The start
// event carries the block's shape with its variable-length fields blank; the
// deltas fill exactly those fields, so the block is rebuilt by starting from
// the template and writing the accumulated text back over it.
type anthropicReplyBlock struct {
	tmpl        map[string]any
	text        strings.Builder
	thinking    strings.Builder
	signature   strings.Builder
	partialJSON strings.Builder
}

// anthropicStreamContent rebuilds the content array of a streamed reply.
func anthropicStreamContent(body []byte) json.RawMessage {
	blocks := map[int]*anthropicReplyBlock{}
	block := func(index int) *anthropicReplyBlock {
		b, ok := blocks[index]
		if !ok {
			b = &anthropicReplyBlock{}
			blocks[index] = b
		}
		return b
	}

	for _, line := range storedSSELines(body) {
		payload, ok := ssePayload(line)
		if !ok {
			continue
		}
		var env struct {
			Type         string         `json:"type"`
			Index        int            `json:"index"`
			ContentBlock map[string]any `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(payload, &env); err != nil {
			continue
		}
		switch env.Type {
		case "content_block_start":
			b := block(env.Index)
			b.tmpl = env.ContentBlock
			// Seed from the template so a relay that puts content in the start
			// event rather than in a delta is not silently truncated.
			seedBuilder(&b.text, env.ContentBlock, "text")
			seedBuilder(&b.thinking, env.ContentBlock, "thinking")
			seedBuilder(&b.signature, env.ContentBlock, "signature")
		case "content_block_delta":
			b := block(env.Index)
			b.text.WriteString(env.Delta.Text)
			b.thinking.WriteString(env.Delta.Thinking)
			b.signature.WriteString(env.Delta.Signature)
			b.partialJSON.WriteString(env.Delta.PartialJSON)
		}
	}
	if len(blocks) == 0 {
		return nil
	}

	indexes := make([]int, 0, len(blocks))
	for index := range blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	out := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, blocks[index].value())
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return encoded
}

// value renders one accumulated block back into its whole form.
func (b *anthropicReplyBlock) value() map[string]any {
	out := make(map[string]any, len(b.tmpl)+1)
	for k, v := range b.tmpl {
		out[k] = v
	}
	setIfBuilt(out, "text", &b.text)
	setIfBuilt(out, "thinking", &b.thinking)
	setIfBuilt(out, "signature", &b.signature)
	if b.partialJSON.Len() > 0 {
		var input any
		// A tool call whose argument JSON never finished arriving keeps the
		// template's input rather than a half-parsed guess; the partial text is
		// still in the trace panel for anyone who needs it.
		if err := json.Unmarshal([]byte(b.partialJSON.String()), &input); err == nil {
			out["input"] = input
		}
	}
	return out
}

func seedBuilder(dst *strings.Builder, tmpl map[string]any, key string) {
	if s, ok := tmpl[key].(string); ok {
		dst.WriteString(s)
	}
}

func setIfBuilt(out map[string]any, key string, b *strings.Builder) {
	if b.Len() > 0 {
		out[key] = b.String()
	}
}
