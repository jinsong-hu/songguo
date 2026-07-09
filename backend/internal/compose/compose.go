// Package compose estimates how a chat request's input context window
// decomposes across sources (system prompt, tool schemas, tool results, ...).
//
// It counts tokens LOCALLY with the o200k_base tokenizer (tiktoken), per block,
// and sums them — deliberately decoupled from the vendor's official usage. This
// buys STABILITY: the same block of text always counts the same, so a caller
// tuning prompt-cache reuse sees a fixed number for an unchanged prefix instead
// of a value that wobbles as the rest of the window shifts. The tradeoff, taken
// on purpose, is that these counts do NOT match the vendor's official input
// total (a different, proprietary tokenizer, plus message framing we don't see).
// So: official usage is authoritative for billing/totals; this decomposition is
// for proportions and growth trends only, and the UI labels it an estimate.
//
// The one number we cannot self-count is the cached (cache-read) portion — only
// the vendor knows which blocks were served from cache. So the official
// cache-read total is front-filled across blocks in prompt order; Cached is
// clamped to Total (our own accounting can't cache more than it holds).
//
// This is pure, read-only sniffing of the already-buffered request body — the
// same category as reading `model` or metering `usage`. It never mutates bytes.
package compose

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// enc is the shared o200k_base tokenizer, loaded once from the embedded offline
// vocab (no network fetch at runtime). If loading ever fails, encErr is set and
// Compose returns ok=false — the caller records no composition and never fails
// the request.
var (
	encOnce sync.Once
	enc     *tiktoken.Tiktoken
	encErr  error
)

func encoder() (*tiktoken.Tiktoken, error) {
	encOnce.Do(func() {
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
		enc, encErr = tiktoken.GetEncoding("o200k_base")
	})
	return enc, encErr
}

// countTokens returns the o200k_base token count of s (0 for empty).
func countTokens(e *tiktoken.Tiktoken, s string) int64 {
	if s == "" {
		return 0
	}
	return int64(len(e.Encode(s, nil, nil)))
}

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

// Composition is the full decomposition for one request. Total is the sum of
// the locally counted per-block tokens (NOT the vendor's official input); Cached
// is the official cache-read total, front-filled and clamped to Total. Sources
// partitions Total exactly.
type Composition struct {
	Total   int64    `json:"total"`
	Cached  int64    `json:"cached"`
	Sources []Source `json:"sources"`
}

// unit is one indivisible decomposable block in render order (tools → system →
// messages). text is the semantic content tokenized to weigh the block. Opaque
// continuity state such as reasoning signatures is deliberately excluded: it is
// request metadata, not inspectable context content.
type unit struct {
	src  string
	prod string
	text string
}

// Compose decomposes body's input context across sources, counting each block's
// tokens locally (o200k_base) and summing them into Total. cachedTokens is the
// vendor's official cache-read total, front-filled across blocks and clamped to
// Total. It returns ok=false when the wire is unsupported, the body cannot be
// parsed, the tokenizer is unavailable, or nothing weighable is found — in which
// case the caller records no composition and never fails the request.
func Compose(wireName string, body []byte, cachedTokens int64) (Composition, bool) {
	var units []unit
	switch {
	case strings.Contains(wireName, "anthropic"):
		units = parseAnthropic(body)
	case strings.Contains(wireName, "responses"):
		units = parseOpenAIResponses(body)
	case strings.Contains(wireName, "openai"), strings.Contains(wireName, "chat"):
		units = parseOpenAI(body)
	default:
		return Composition{}, false
	}
	if len(units) == 0 {
		return Composition{}, false
	}
	e, err := encoder()
	if err != nil {
		return Composition{}, false
	}

	tokens := make([]int64, len(units))
	var total int64
	for i, u := range units {
		tokens[i] = countTokens(e, u.text)
		total += tokens[i]
	}
	if total <= 0 {
		return Composition{}, false
	}

	cached := frontFill(tokens, cachedTokens)
	var cachedSum int64
	for _, c := range cached {
		cachedSum += c
	}

	return Composition{
		Total:   total,
		Cached:  cachedSum,
		Sources: aggregate(units, tokens, cached),
	}, true
}

// frontFill walks blocks in prompt order and assigns the cached (cache-read)
// token total front-to-back: block.cached = min(block.tokens, remaining). The
// result sums to min(cachedTokens, Σtokens) — the official cache-read total,
// clamped so our accounting never caches more tokens than it holds.
func frontFill(tokens []int64, cachedTokens int64) []int64 {
	out := make([]int64, len(tokens))
	remaining := cachedTokens
	for i := range tokens {
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

// compactStr returns raw re-encoded as compact JSON, falling back to the raw
// string if it is not valid JSON.
func compactStr(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// rawContentStr returns a message content field that may be a JSON string (use
// the unescaped string) or a JSON array/object (use its compact form).
func rawContentStr(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return compactStr(raw)
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
		units = append(units, unit{src: "tool_schemas", prod: schemaProducer(t.Name), text: compactStr(raw)})
	}

	if s := systemText(req.System); s != "" {
		units = append(units, unit{src: "system", text: s})
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
			if str != "" {
				units = append(units, unit{src: src, text: str})
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
			if text := anthBlockText(raw, b.Type); text != "" {
				units = append(units, unit{src: src, prod: prod, text: text})
			}
		}
	}
	return units
}

func anthBlockText(raw json.RawMessage, typ string) string {
	if typ == "thinking" {
		var b struct {
			Thinking string `json:"thinking"`
		}
		_ = json.Unmarshal(raw, &b)
		return b.Thinking
	}
	if typ == "redacted_thinking" {
		return ""
	}
	return compactStr(raw)
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

// systemText returns the text weight for the system field: the string itself
// for a plain string, the compact form for the structured (array) form.
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return compactStr(raw)
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
		units = append(units, unit{src: "tool_schemas", prod: prod, text: compactStr(raw)})
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
			if s := rawContentStr(m.Content); s != "" {
				units = append(units, unit{src: "system", text: s})
			}
		case "user":
			if s := rawContentStr(m.Content); s != "" {
				units = append(units, unit{src: "user", text: s})
			}
		case "assistant":
			if s := rawContentStr(m.Content); s != "" {
				units = append(units, unit{src: "actions", text: s})
			}
			for _, tc := range m.ToolCalls {
				raw, _ := json.Marshal(tc)
				units = append(units, unit{src: "actions", text: compactStr(raw)})
			}
		case "tool":
			name, ok := idToName[m.ToolCallID]
			prod := "unknown"
			if ok {
				prod = toolProducer(name)
			}
			if s := rawContentStr(m.Content); s != "" {
				units = append(units, unit{src: "tool_results", prod: prod, text: s})
			}
		}
	}
	return units
}

// ---- OpenAI Responses request ----

func parseOpenAIResponses(body []byte) []unit {
	var req struct {
		Instructions json.RawMessage   `json:"instructions"`
		Input        []json.RawMessage `json:"input"`
		Tools        []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	var units []unit

	for _, raw := range req.Tools {
		var t struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &t)
		units = append(units, unit{src: "tool_schemas", prod: schemaProducer(t.Name), text: compactStr(raw)})
	}

	if s := systemText(req.Instructions); s != "" {
		units = append(units, unit{src: "system", text: s})
	}

	callIDToName := map[string]string{}
	for _, raw := range req.Input {
		var item struct {
			Type   string `json:"type"`
			Name   string `json:"name"`
			CallID string `json:"call_id"`
			ID     string `json:"id"`
		}
		_ = json.Unmarshal(raw, &item)
		if item.Type == "function_call" && item.Name != "" {
			if item.CallID != "" {
				callIDToName[item.CallID] = item.Name
			}
			if item.ID != "" {
				callIDToName[item.ID] = item.Name
			}
		}
	}

	for _, raw := range req.Input {
		var item struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			CallID  string          `json:"call_id"`
			ID      string          `json:"id"`
		}
		_ = json.Unmarshal(raw, &item)

		switch item.Type {
		case "message":
			src := responsesRoleSource(item.Role)
			if src == "" {
				continue
			}
			for _, text := range responsesContentTexts(item.Content) {
				if text != "" {
					units = append(units, unit{src: src, text: text})
				}
			}
		case "reasoning":
			if s := responsesReasoningText(raw); s != "" {
				units = append(units, unit{src: "reasoning", text: s})
			}
		case "function_call", "custom_tool_call", "web_search_call", "computer_call", "local_shell_call":
			if s := compactStr(raw); s != "" {
				units = append(units, unit{src: "actions", text: s})
			}
		case "function_call_output", "custom_tool_call_output", "computer_call_output", "local_shell_call_output":
			name := callIDToName[item.CallID]
			if name == "" {
				name = callIDToName[item.ID]
			}
			prod := "unknown"
			if name != "" {
				prod = toolProducer(name)
			}
			if s := responsesOutputText(raw); s != "" {
				units = append(units, unit{src: "tool_results", prod: prod, text: s})
			}
		default:
			if s := compactStr(raw); s != "" {
				units = append(units, unit{src: "actions", text: s})
			}
		}
	}
	return units
}

func responsesRoleSource(role string) string {
	switch role {
	case "system", "developer":
		return "system"
	case "user":
		return "user"
	case "assistant":
		return "actions"
	default:
		return ""
	}
}

func responsesContentTexts(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return []string{compactStr(raw)}
	}
	out := make([]string, 0, len(blocks))
	for _, rawBlock := range blocks {
		var b struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(rawBlock, &b)
		switch b.Type {
		case "input_text", "output_text", "refusal":
			if b.Text != "" {
				out = append(out, b.Text)
			}
		default:
			if s := compactStr(rawBlock); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func responsesReasoningText(raw json.RawMessage) string {
	var item struct {
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return ""
	}
	var parts []string
	for _, s := range item.Summary {
		if s.Text != "" {
			parts = append(parts, s.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func responsesOutputText(raw json.RawMessage) string {
	var item struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || len(item.Output) == 0 {
		return compactStr(raw)
	}
	var s string
	if err := json.Unmarshal(item.Output, &s); err == nil {
		return s
	}
	return compactStr(item.Output)
}
