// Package compose estimates how a chat request's input context window
// decomposes across sources (system prompt, tool schemas, tool results, ...).
//
// The one invariant: we never count tokens ourselves. We measure BYTES only to
// derive per-block ratios, then multiply the vendor's official input-token total
// by those ratios. Every displayed total/subtotal equals a vendor usage field;
// only the proportions between sources are byte-estimated. Distribution uses
// largest-remainder rounding so the integer per-source tokens sum EXACTLY to the
// official input total, and the cached split front-fills the official
// cache-read total across blocks in prompt order so it too sums exactly.
//
// This is pure, read-only sniffing of the already-buffered request body — the
// same category as reading `model` or metering `usage`. It never mutates bytes.
package compose

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// Producer is one attributed origin within a source (e.g. a tool that produced
// a result, or the server that owns an MCP tool schema).
type Producer struct {
	Key    string `json:"key"`
	Tokens int64  `json:"tokens"`
}

// Source is one top-level context source with its estimated token share, the
// cached (cache-read) portion of that share, and optional producer breakdown.
type Source struct {
	Key      string     `json:"key"`
	Tokens   int64      `json:"tokens"`
	Cached   int64      `json:"cached"`
	Children []Producer `json:"children,omitempty"`
}

// Composition is the full decomposition for one request. Total and Cached equal
// the vendor's official input and cache-read token counts; Sources partitions
// Total exactly.
type Composition struct {
	Total   int64    `json:"total"`
	Cached  int64    `json:"cached"`
	Sources []Source `json:"sources"`
}

// unit is one indivisible block in render order (tools → system → messages).
// bytes is the compact-JSON byte length used as the estimation weight.
type unit struct {
	src   string
	prod  string
	bytes int64
}

// Compose decomposes body's input context across sources, distributing
// inputTokens (and cachedTokens) by byte weight. It returns ok=false when the
// wire is unsupported, the body cannot be parsed, or nothing weighable is found
// — in which case the caller records no composition and never fails the request.
func Compose(wireName string, body []byte, inputTokens, cachedTokens int64) (Composition, bool) {
	var units []unit
	switch {
	case strings.Contains(wireName, "anthropic"):
		units = parseAnthropic(body)
	case strings.Contains(wireName, "openai"), strings.Contains(wireName, "chat"):
		units = parseOpenAI(body)
	default:
		return Composition{}, false
	}
	if len(units) == 0 || inputTokens <= 0 {
		return Composition{}, false
	}

	tokens := distribute(units, inputTokens)
	cached := frontFill(units, tokens, cachedTokens)

	return Composition{
		Total:   inputTokens,
		Cached:  cachedTokens,
		Sources: aggregate(units, tokens, cached),
	}, true
}

// distribute splits total across units proportional to each unit's byte weight,
// using largest-remainder rounding so the integer results sum to total EXACTLY.
func distribute(units []unit, total int64) []int64 {
	n := len(units)
	out := make([]int64, n)

	var totalBytes int64
	for _, u := range units {
		totalBytes += u.bytes
	}
	if totalBytes <= 0 || total <= 0 {
		return out
	}

	type rem struct {
		idx int
		num int64 // remainder numerator (larger = rounds up first)
	}
	rems := make([]rem, n)
	var allocated int64
	for i, u := range units {
		prod := total * u.bytes
		out[i] = prod / totalBytes
		rems[i] = rem{idx: i, num: prod % totalBytes}
		allocated += out[i]
	}
	leftover := total - allocated

	// Stable sort by remainder desc; ties keep original (render) order.
	sort.SliceStable(rems, func(a, b int) bool { return rems[a].num > rems[b].num })
	for k := int64(0); k < leftover && k < int64(n); k++ {
		out[rems[k].idx]++
	}
	return out
}

// frontFill walks units in prompt order and assigns the cached (cache-read)
// token total front-to-back: unit.cached = min(unit.tokens, remaining). The
// result sums to cachedTokens exactly (assuming cachedTokens <= sum tokens).
func frontFill(units []unit, tokens []int64, cachedTokens int64) []int64 {
	out := make([]int64, len(units))
	remaining := cachedTokens
	for i := range units {
		if remaining <= 0 {
			break
		}
		c := tokens[i]
		if remaining < c {
			c = remaining
		}
		out[i] = c
		remaining -= c
	}
	return out
}

// aggregate folds per-unit tokens/cached into Sources keyed by source, grouping
// non-empty producers under each source. Sources are sorted by tokens desc (tie
// by key); producers likewise.
func aggregate(units []unit, tokens, cached []int64) []Source {
	type acc struct {
		tokens int64
		cached int64
		prods  map[string]int64
	}
	byKey := map[string]*acc{}
	var order []string
	for i, u := range units {
		a := byKey[u.src]
		if a == nil {
			a = &acc{prods: map[string]int64{}}
			byKey[u.src] = a
			order = append(order, u.src)
		}
		a.tokens += tokens[i]
		a.cached += cached[i]
		if u.prod != "" {
			a.prods[u.prod] += tokens[i]
		}
	}

	sources := make([]Source, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		s := Source{Key: key, Tokens: a.tokens, Cached: a.cached}
		if len(a.prods) > 0 {
			for pk, pt := range a.prods {
				s.Children = append(s.Children, Producer{Key: pk, Tokens: pt})
			}
			sort.SliceStable(s.Children, func(i, j int) bool {
				if s.Children[i].Tokens != s.Children[j].Tokens {
					return s.Children[i].Tokens > s.Children[j].Tokens
				}
				return s.Children[i].Key < s.Children[j].Key
			})
		}
		sources = append(sources, s)
	}
	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].Tokens != sources[j].Tokens {
			return sources[i].Tokens > sources[j].Tokens
		}
		return sources[i].Key < sources[j].Key
	})
	return sources
}

// compactLen returns the byte length of raw re-encoded as compact JSON, falling
// back to the raw length if it is not valid JSON.
func compactLen(raw []byte) int64 {
	if len(raw) == 0 {
		return 0
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return int64(len(raw))
	}
	return int64(buf.Len())
}

// rawContentLen measures a message content field that may be a JSON string (use
// the unescaped string length) or a JSON array/object (use its compact length).
func rawContentLen(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return int64(len(s))
	}
	return compactLen(raw)
}

// toolProducer maps a Claude Code tool name to its producer key (used for
// tool_result attribution). Unknown names lower-case through; empty → unknown.
func toolProducer(name string) string {
	switch name {
	case "Read":
		return "read"
	case "Bash":
		return "bash"
	case "Grep":
		return "grep"
	case "Glob":
		return "glob"
	case "Task":
		return "task"
	case "WebFetch", "WebSearch":
		return "web"
	}
	if s, ok := mcpServer(name); ok {
		return "mcp:" + s
	}
	if name == "" {
		return "unknown"
	}
	return strings.ToLower(name)
}

// schemaProducer maps a tool name to its tool_schemas producer: an MCP server
// (mcp:<server>) or the builtin bucket.
func schemaProducer(name string) string {
	if s, ok := mcpServer(name); ok {
		return "mcp:" + s
	}
	return "builtin"
}

// mcpServer extracts the server segment from an mcp__<server>__<tool> name.
func mcpServer(name string) (string, bool) {
	if !strings.HasPrefix(name, "mcp__") {
		return "", false
	}
	parts := strings.Split(name, "__")
	if len(parts) > 1 && parts[1] != "" {
		return parts[1], true
	}
	return "mcp", true
}

// ---- Anthropic Messages request ----

func parseAnthropic(body []byte) []unit {
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	var units []unit

	// tools → system → messages (render order).
	for _, raw := range req.Tools {
		var t struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &t)
		units = append(units, unit{src: "tool_schemas", prod: schemaProducer(t.Name), bytes: compactLen(raw)})
	}

	if n := systemBytes(req.System); n > 0 {
		units = append(units, unit{src: "system", bytes: n})
	}

	// First pass: map tool_use id → tool name from assistant blocks.
	idToName := map[string]string{}
	for _, m := range req.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, raw := range anthBlocks(m.Content) {
			var b struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			_ = json.Unmarshal(raw, &b)
			if b.Type == "tool_use" && b.ID != "" {
				idToName[b.ID] = b.Name
			}
		}
	}

	for _, m := range req.Messages {
		// String content is plain text: user → user, assistant → actions.
		var str string
		if err := json.Unmarshal(m.Content, &str); err == nil {
			src := "user"
			if m.Role == "assistant" {
				src = "actions"
			}
			if n := int64(len(str)); n > 0 {
				units = append(units, unit{src: src, bytes: n})
			}
			continue
		}
		for _, raw := range anthBlocks(m.Content) {
			var b struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			}
			_ = json.Unmarshal(raw, &b)
			src, prod := anthClassify(m.Role, b.Type, b.ToolUseID, idToName)
			if src == "" {
				continue
			}
			units = append(units, unit{src: src, prod: prod, bytes: compactLen(raw)})
		}
	}
	return units
}

// anthClassify maps a (role, block type) to a source key and optional producer.
func anthClassify(role, typ, toolUseID string, idToName map[string]string) (src, prod string) {
	if role == "user" {
		switch typ {
		case "text":
			return "user", ""
		case "tool_result":
			name, ok := idToName[toolUseID]
			if !ok {
				return "tool_results", "unknown"
			}
			return "tool_results", toolProducer(name)
		case "image", "document":
			return "attachments", ""
		}
		return "user", ""
	}
	// assistant
	switch typ {
	case "text", "tool_use", "server_tool_use":
		return "actions", ""
	case "thinking", "redacted_thinking":
		return "reasoning", ""
	}
	return "actions", ""
}

// systemBytes returns the byte weight for the system field: string length for a
// plain string, compact length for the structured (array) form.
func systemBytes(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return int64(len(s))
	}
	return compactLen(raw)
}

// anthBlocks returns the array-form content blocks, or nil for non-array content.
func anthBlocks(raw json.RawMessage) []json.RawMessage {
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

// ---- OpenAI chat request ----

func parseOpenAI(body []byte) []unit {
	var req struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	var units []unit

	for _, raw := range req.Tools {
		var t struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		_ = json.Unmarshal(raw, &t)
		prod := "builtin"
		if strings.HasPrefix(t.Function.Name, "mcp") {
			prod = "mcp"
		}
		units = append(units, unit{src: "tool_schemas", prod: prod, bytes: compactLen(raw)})
	}

	// Map tool_call id → function name across all assistant messages.
	idToName := map[string]string{}
	for _, m := range req.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				idToName[tc.ID] = tc.Function.Name
			}
		}
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if n := rawContentLen(m.Content); n > 0 {
				units = append(units, unit{src: "system", bytes: n})
			}
		case "user":
			if n := rawContentLen(m.Content); n > 0 {
				units = append(units, unit{src: "user", bytes: n})
			}
		case "assistant":
			if n := rawContentLen(m.Content); n > 0 {
				units = append(units, unit{src: "actions", bytes: n})
			}
			for _, tc := range m.ToolCalls {
				raw, _ := json.Marshal(tc)
				units = append(units, unit{src: "actions", bytes: compactLen(raw)})
			}
		case "tool":
			name, ok := idToName[m.ToolCallID]
			prod := "unknown"
			if ok {
				prod = toolProducer(name)
			}
			if n := rawContentLen(m.Content); n > 0 {
				units = append(units, unit{src: "tool_results", prod: prod, bytes: n})
			}
		}
	}
	return units
}
