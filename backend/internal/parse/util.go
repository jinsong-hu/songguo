package parse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// modelFromBody pulls the "model" field from a JSON request body, "" on failure.
func modelFromBody(body []byte) string {
	var shallow struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &shallow)
	return shallow.Model
}

// asInt coerces a JSON number (decoded as float64) to int.
func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// topLevelTokens reads a best-effort token breakdown from a body's top-level
// "usage" object, covering both OpenAI (prompt/completion_tokens) and Anthropic
// (input/output_tokens) field names. Used by the generic/embeddings parsers.
func topLevelTokens(body []byte) Tokens {
	var env struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return Tokens{}
	}
	return tokensFrom(env.Usage)
}

// tokensFrom maps a vendor usage object to the canonical Tokens, tolerating the
// OpenAI and Anthropic field names and their nested detail objects.
func tokensFrom(u map[string]any) Tokens {
	var t Tokens
	t.Input = asInt(u["prompt_tokens"])
	if t.Input == 0 {
		t.Input = asInt(u["input_tokens"])
	}
	t.Output = asInt(u["completion_tokens"])
	if t.Output == 0 {
		t.Output = asInt(u["output_tokens"])
	}
	// Cache reads: OpenAI nests under prompt_tokens_details.cached_tokens;
	// Anthropic and some OpenAI-compatibles report top-level fields.
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		t.CachedInput = asInt(d["cached_tokens"])
	}
	if t.CachedInput == 0 {
		t.CachedInput = asInt(u["cached_tokens"])
	}
	if t.CachedInput == 0 {
		t.CachedInput = asInt(u["cache_read_input_tokens"])
	}
	t.CacheWrite = asInt(u["cache_creation_input_tokens"])
	// Reasoning tokens: OpenAI nests under completion_tokens_details.
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		t.Reasoning = asInt(d["reasoning_tokens"])
	}
	return t
}

// flexText extracts plain text from a content field that may be a bare string
// or an array of typed parts ([{type:"text",text:"..."}] for OpenAI, or
// [{type:"text",text:"..."}] / {type:"tool_use",...} for Anthropic). Non-text
// parts (images, tool blocks) are skipped here; tool blocks are handled by the
// protocol parsers.
func flexText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if txt, ok := m["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(txt)
			}
		}
		return b.String()
	}
	return ""
}

// sseEvent is one parsed Server-Sent Event: its "event:" type (often empty for
// OpenAI) and the concatenated "data:" payload.
type sseEvent struct {
	event string
	data  string
}

// scanSSE splits a captured SSE body into events. Events are separated by blank
// lines; "data:" lines within an event are concatenated. The OpenAI sentinel
// "[DONE]" is skipped. Tolerant of truncation: a trailing partial event is
// emitted with whatever lines were seen.
func scanSSE(body []byte) []sseEvent {
	var (
		events []sseEvent
		ev     sseEvent
		have   bool
	)
	flush := func() {
		if have {
			events = append(events, ev)
		}
		ev, have = sseEvent{}, false
	}

	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			ev.event = strings.TrimSpace(line[len("event:"):])
			have = true
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimSpace(line[len("data:"):])
			if d == "[DONE]" {
				continue
			}
			if ev.data != "" {
				ev.data += "\n"
			}
			ev.data += d
			have = true
		}
	}
	flush()
	return events
}
