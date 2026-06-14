// Package parse turns a captured request/response body pair into a
// protocol-neutral structured view of the call — messages, tool calls, and a
// token breakdown — for the asynchronous analysis pipeline.
//
// It is pure: no I/O, no store or proxy dependencies. Every parse is
// best-effort and defensive — malformed or truncated bodies yield whatever
// could be recovered plus a non-fatal error, never a panic. The synchronous
// hot path (routing + metering) does not use this package; the proxy hands
// captured bytes here from a background worker.
package parse

import "fmt"

// Input is the captured material for one call attempt.
type Input struct {
	Wire            string // matched wire name, e.g. "openai/chat" ("" if unmatched)
	Adapter         string // vendor adapter, e.g. "openai-compatible"
	Modality        string // classified modality, e.g. "chat"
	Stream          bool   // response was server-sent events
	ReqContentType  string
	RespContentType string
	ReqBody         []byte
	RespBody        []byte // full body (non-stream) or captured SSE text (stream)
}

// ToolCall is a single function/tool invocation requested by the model.
type ToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"` // raw JSON arguments, as sent
}

// Message is one turn in the conversation, request- or response-side.
type Message struct {
	Role       string     `json:"role"`
	Text       string     `json:"text,omitempty"`         // concatenated text content
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant-issued tool calls
	ToolCallID string     `json:"tool_call_id,omitempty"` // set on tool-result messages
	Name       string     `json:"name,omitempty"`
}

// Tokens is the breakdown extracted from the response usage object. Fields are
// omitted when zero so the stored JSON stays compact.
type Tokens struct {
	Input       int `json:"input,omitempty"`
	Output      int `json:"output,omitempty"`
	CachedInput int `json:"cached_input,omitempty"` // cache reads (subset of input)
	CacheWrite  int `json:"cache_write,omitempty"`  // cache-creation tokens
	Reasoning   int `json:"reasoning,omitempty"`    // reasoning/thinking tokens
}

// Call is the protocol-neutral parsed view persisted for later analysis.
type Call struct {
	Format        string    `json:"format"` // "openai-chat" | "anthropic-messages" | "openai-responses" | "embeddings" | "generic"
	Model         string    `json:"model,omitempty"`
	System        string    `json:"system,omitempty"`
	Input         []Message `json:"input,omitempty"`  // request messages
	Output        []Message `json:"output,omitempty"` // response message(s)
	Tools         []string  `json:"tools,omitempty"`  // declared tool/function names
	FinishReason  string    `json:"finish_reason,omitempty"`
	Tokens        Tokens    `json:"tokens"`
	Stream        bool      `json:"stream"`
	ToolCallCount int       `json:"tool_call_count"`
}

// Parse dispatches to the parser matching the call's wire (falling back to the
// adapter, then a generic shape). The returned Call is always usable; err is
// non-nil only to flag a partial/failed parse for logging.
func Parse(in Input) (Call, error) {
	switch in.Wire {
	case "openai/chat", "openai/completions":
		return parseOpenAIChat(in)
	case "anthropic/messages":
		return parseAnthropic(in)
	case "openai/responses":
		return parseOpenAIResponses(in)
	case "openai/embeddings":
		return parseEmbeddings(in)
	}
	// Unmatched wire: infer from adapter for the two big chat families, else
	// record a generic shape.
	switch in.Adapter {
	case "anthropic-compatible":
		return parseAnthropic(in)
	case "openai-compatible":
		if in.Modality == "chat" {
			return parseOpenAIChat(in)
		}
	}
	return parseGeneric(in)
}

// finalize sets derived counters shared across parsers.
func (c *Call) finalize(in Input) {
	c.Stream = in.Stream
	if c.Model == "" {
		c.Model = modelFromBody(in.ReqBody)
	}
	n := 0
	for _, m := range c.Output {
		n += len(m.ToolCalls)
	}
	c.ToolCallCount = n
}

// parseGeneric records the lowest-common-denominator view: format, model, and
// any top-level token usage. Used for image/video/tts/asr and unknown wires.
func parseGeneric(in Input) (Call, error) {
	c := Call{Format: "generic"}
	c.Tokens = topLevelTokens(in.RespBody)
	c.finalize(in)
	if len(in.RespBody) == 0 {
		return c, fmt.Errorf("parse: empty response body")
	}
	return c, nil
}

// parseEmbeddings records the token usage of an embeddings call; there is no
// conversational content to capture.
func parseEmbeddings(in Input) (Call, error) {
	c := Call{Format: "embeddings"}
	c.Tokens = topLevelTokens(in.RespBody)
	c.finalize(in)
	return c, nil
}
