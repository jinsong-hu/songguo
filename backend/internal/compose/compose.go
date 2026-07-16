// Package compose estimates how a chat request's input context window
// decomposes across sources — one axis, "who put these bytes in the window":
//
//	system       — system/developer instructions
//	tool_schemas — tool definitions           (children: verbatim tool name)
//	user         — human-authored turns       (children: text, attachments)
//	assistant    — model prose + reasoning    (children: text, reasoning, attachments)
//	tool_calls   — tool invocation arguments  (children: verbatim tool name)
//	tool_results — what tools returned        (children: verbatim tool name)
//	other        — block/item types we do not recognize; an explicit residual
//	               beats silently guessing a bucket
//
// Producer keys are whatever identifier the request carries, verbatim — no
// lowercasing, no mcp__server__tool splitting, no builtin lump. songguo shows
// what the client sent; unifying names is the client's job.
//
// It counts text locally with the o200k_base tokenizer (tiktoken), adds Claude
// visual-token estimates for recognized images, and sums the per-block weights
// — deliberately decoupled from the vendor's official usage. This buys
// STABILITY: the same block of text/image content always counts the same, so a
// caller tuning prompt-cache reuse sees a fixed number for an unchanged prefix
// instead of a value that wobbles as the rest of the window shifts. The tradeoff,
// taken on purpose, is that these counts do NOT match the vendor's official
// input total (a different, proprietary tokenizer, plus message framing we don't
// see). So: official usage is authoritative for billing/totals; this
// decomposition is for proportions and growth trends only, and the UI labels it
// an estimate.
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
	"encoding/base64"
	"encoding/json"
	"hash/fnv"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"sort"
	"strconv"
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

// Block is one locally counted, itemized context block. It intentionally stores
// metadata and a content hash, not raw text: context composition is collected
// even when payload capture is off.
type Block struct {
	Seq      int    `json:"seq"`
	Source   string `json:"source"`
	Producer string `json:"producer,omitempty"`
	Type     string `json:"type"`
	Hash     string `json:"hash"`
	Tokens   int64  `json:"tokens"`
	Cached   int64  `json:"cached"`
}

// Composition is the full decomposition for one request. Total is the sum of
// the locally counted per-block tokens (NOT the vendor's official input); Cached
// is the official cache-read total, front-filled and clamped to Total. Sources
// partitions Total exactly.
type Composition struct {
	Total   int64    `json:"total"`
	Cached  int64    `json:"cached"`
	Sources []Source `json:"sources"`
	Blocks  []Block  `json:"blocks,omitempty"`
}

// unit is one indivisible decomposable block in render order (tools → system →
// messages). text is the semantic content tokenized to weigh the block; tokens
// carries pre-counted non-text weight such as Claude visual tokens. Opaque
// continuity state such as reasoning signatures is deliberately excluded: it is
// request metadata, not inspectable context content.
type unit struct {
	src    string
	prod   string
	label  string
	text   string
	tokens int64
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
		tokens[i] = countTokens(e, u.text) + u.tokens
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
		Blocks:  blocks(units, tokens, cached),
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

func blocks(units []unit, tokens, cached []int64) []Block {
	out := make([]Block, 0, len(units))
	for i, u := range units {
		if tokens[i] <= 0 {
			continue
		}
		label := u.label
		if label == "" {
			label = defaultBlockLabel(u.src)
		}
		out = append(out, Block{
			Seq:      i,
			Source:   u.src,
			Producer: u.prod,
			Type:     label,
			Hash:     blockHash(u.src, u.prod, label, u.text),
			Tokens:   tokens[i],
			Cached:   cached[i],
		})
	}
	return out
}

func defaultBlockLabel(src string) string {
	switch src {
	case "system":
		return "System prompt"
	case "tool_schemas":
		return "Tool schema"
	case "tool_calls":
		return "Tool use"
	case "tool_results":
		return "Tool result"
	case "assistant":
		return "Text block"
	case "user":
		return "User text"
	default:
		return "Context block"
	}
}

func blockHash(source, producer, label, text string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(source))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(producer))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(label))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(text))
	return strconv.FormatUint(h.Sum64(), 16)
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

// producerKey is the verbatim tool identifier from the request — never
// normalized, cased, or grouped. Empty (unresolvable) names fall to "unknown".
func producerKey(name string) string {
	if name == "" {
		return "unknown"
	}
	return name
}

// ---- Anthropic Messages request ----

func parseAnthropic(body []byte) []unit {
	var req struct {
		Model    string          `json:"model"`
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
		label := t.Name
		if label == "" {
			label = "Tool schema"
		}
		units = append(units, unit{src: "tool_schemas", prod: producerKey(t.Name), label: label, text: compactStr(raw)})
	}

	units = append(units, systemUnits(req.System)...)

	// First pass: map tool-use id → tool name from assistant blocks, so
	// tool_result blocks can attribute back to the tool that produced them.
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
			switch b.Type {
			case "tool_use", "server_tool_use", "mcp_tool_use":
				if b.ID != "" {
					idToName[b.ID] = b.Name
				}
			}
		}
	}

	for _, m := range req.Messages {
		// String content is plain prose from whoever holds the role.
		var str string
		if err := json.Unmarshal(m.Content, &str); err == nil {
			if str != "" {
				units = append(units, anthTextUnit(m.Role, str))
			}
			continue
		}
		for _, raw := range anthBlocks(m.Content) {
			if u, ok := anthBlockUnit(m.Role, raw, req.Model, idToName); ok {
				units = append(units, u)
			}
		}
	}
	return units
}

// anthTextUnit classifies a plain-string message by role.
func anthTextUnit(role, text string) unit {
	switch role {
	case "assistant":
		return unit{src: "assistant", prod: "text", label: "Text block", text: text}
	case "system", "developer":
		return unit{src: "system", label: "System prompt", text: text}
	default:
		return unit{src: "user", prod: "text", label: "Text block", text: text}
	}
}

// anthBlockUnit classifies one content block by (role, block type). Types we
// do not recognize land in the explicit "other" bucket — an honest residual
// beats silently guessing a bucket.
func anthBlockUnit(role string, raw json.RawMessage, model string, idToName map[string]string) (unit, bool) {
	var b struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Text      string `json:"text"`
		Thinking  string `json:"thinking"`
		ToolUseID string `json:"tool_use_id"`
	}
	_ = json.Unmarshal(raw, &b)

	if role != "user" && role != "assistant" {
		if s := compactStr(raw); s != "" {
			return unit{src: "other", label: blockTypeLabel(b.Type), text: s}, true
		}
		return unit{}, false
	}
	roleSrc := "user"
	if role == "assistant" {
		roleSrc = "assistant"
	}

	switch b.Type {
	case "text":
		return unit{src: roleSrc, prod: "text", label: "Text block", text: b.Text}, b.Text != ""
	case "thinking":
		return unit{src: "assistant", prod: "reasoning", label: "Reasoning", text: b.Thinking}, b.Thinking != ""
	case "redacted_thinking":
		// Opaque continuity state — request metadata, not inspectable content.
		return unit{}, false
	case "tool_use", "server_tool_use", "mcp_tool_use":
		label := b.Name
		if label == "" {
			label = "Tool use"
		}
		return unit{src: "tool_calls", prod: producerKey(b.Name), label: label, text: compactStr(raw)}, true
	case "tool_result", "web_search_tool_result", "code_execution_tool_result", "mcp_tool_result":
		var res struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(raw, &res)
		text, tokens := anthropicContentWeight(res.Content, model)
		label := "Tool result"
		if b.ToolUseID != "" {
			label += " " + b.ToolUseID
		}
		return unit{src: "tool_results", prod: producerKey(idToName[b.ToolUseID]), label: label, text: text, tokens: tokens}, text != "" || tokens > 0
	case "image":
		tokens := anthropicImageTokens(raw, model)
		return unit{src: roleSrc, prod: "attachments", label: "Attachment", tokens: tokens}, tokens > 0
	case "document":
		text, tokens := anthropicDocumentWeight(raw, model)
		return unit{src: roleSrc, prod: "attachments", label: "Attachment", text: text, tokens: tokens}, text != "" || tokens > 0
	default:
		if s := compactStr(raw); s != "" {
			return unit{src: "other", label: blockTypeLabel(b.Type), text: s}, true
		}
		return unit{}, false
	}
}

func blockTypeLabel(typ string) string {
	if typ == "" {
		return "Context block"
	}
	return typ
}

// anthropicDocumentWeight estimates a document block's window weight. Plain
// text documents weigh their text. Base64 PDFs are NOT tokenized as base64
// text (that multiplies the true cost several-fold); Anthropic charges per
// page — text plus the page rendered as an image, ~1,500–3,000 tokens/page —
// so we count page objects and take the midpoint. URL/file documents carry no
// bytes we can weigh.
func anthropicDocumentWeight(raw json.RawMessage, model string) (string, int64) {
	var b struct {
		Source struct {
			Type    string          `json:"type"`
			Data    string          `json:"data"`
			Content json.RawMessage `json:"content"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return "", 0
	}
	switch b.Source.Type {
	case "text":
		return b.Source.Data, 0
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(b.Source.Data)
		if err != nil {
			return "", 0
		}
		return "", pdfPages(decoded) * 2250
	case "content":
		return anthropicContentWeight(b.Source.Content, model)
	default: // url, file — no bytes to weigh
		return "", 0
	}
}

// pdfPages counts page objects in a decoded PDF. Pages hidden inside
// compressed object streams are invisible to this scan; the document then
// weighs nothing rather than something invented.
func pdfPages(data []byte) int64 {
	pages := int64(bytes.Count(data, []byte("/Type /Page"))) - int64(bytes.Count(data, []byte("/Type /Pages")))
	pages += int64(bytes.Count(data, []byte("/Type/Page"))) - int64(bytes.Count(data, []byte("/Type/Pages")))
	if pages < 0 {
		return 0
	}
	return pages
}

func anthropicContentWeight(raw json.RawMessage, model string) (string, int64) {
	if len(raw) == 0 {
		return "", 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, 0
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return compactStr(raw), 0
	}
	var texts []string
	var tokens int64
	for _, block := range blocks {
		text, visual := anthropicContentBlockWeight(block, model)
		if text != "" {
			texts = append(texts, text)
		}
		tokens += visual
	}
	return strings.Join(texts, "\n"), tokens
}

func anthropicContentBlockWeight(raw json.RawMessage, model string) (string, int64) {
	var b struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &b)
	switch b.Type {
	case "text":
		return b.Text, 0
	case "image":
		return "", anthropicImageTokens(raw, model)
	case "document":
		return anthropicDocumentWeight(raw, model)
	default:
		return compactStr(raw), 0
	}
}

func anthropicImageTokens(raw json.RawMessage, model string) int64 {
	var b struct {
		Source struct {
			Type string `json:"type"`
			Data string `json:"data"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return 0
	}
	if b.Source.Type != "base64" || b.Source.Data == "" {
		return 0
	}
	decoded, err := base64.StdEncoding.DecodeString(b.Source.Data)
	if err != nil {
		return 0
	}
	cfg, ok := imageConfigFromBytes(decoded)
	if !ok {
		return 0
	}
	return visualTokensForSize(cfg.Width, cfg.Height, claudeImageTier(model))
}

func imageConfigFromDataURL(rawURL string) (image.Config, bool) {
	i := strings.Index(rawURL, ",")
	if !strings.HasPrefix(rawURL, "data:") || i < 0 || !strings.Contains(rawURL[:i], ";base64") {
		return image.Config{}, false
	}
	decoded, err := base64.StdEncoding.DecodeString(rawURL[i+1:])
	if err != nil {
		return image.Config{}, false
	}
	return imageConfigFromBytes(decoded)
}

func imageConfigFromBytes(decoded []byte) (image.Config, bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil {
		return image.Config{}, false
	}
	return cfg, true
}

type imageTier struct {
	maxLongEdge int
	maxTokens   int64
}

var (
	standardImageTier = imageTier{maxLongEdge: 1568, maxTokens: 1568}
	highImageTier     = imageTier{maxLongEdge: 2576, maxTokens: 4784}
)

func claudeImageTier(model string) imageTier {
	name := strings.ToLower(model)
	highModels := []string{
		"fable-5", "fable 5",
		"mythos-5", "mythos 5",
		// Real model ids use dash form (claude-opus-4-8); dot/space forms
		// cover display names.
		"opus-4-8", "opus-4.8", "opus 4.8",
		"opus-4-7", "opus-4.7", "opus 4.7",
		"sonnet-5", "sonnet 5",
	}
	for _, high := range highModels {
		if strings.Contains(name, high) {
			return highImageTier
		}
	}
	return standardImageTier
}

func visualTokensForSize(width, height int, tier imageTier) int64 {
	if width <= 0 || height <= 0 {
		return 0
	}

	w, h := float64(width), float64(height)
	long := math.Max(w, h)
	if long > float64(tier.maxLongEdge) {
		scale := float64(tier.maxLongEdge) / long
		w *= scale
		h *= scale
	}
	if visualPatchCount(w, h) <= tier.maxTokens {
		return visualPatchCount(w, h)
	}

	lo, hi := 0.0, 1.0
	for i := 0; i < 64; i++ {
		mid := (lo + hi) / 2
		if visualPatchCount(w*mid, h*mid) <= tier.maxTokens {
			lo = mid
		} else {
			hi = mid
		}
	}
	return visualPatchCount(w*lo, h*lo)
}

func visualPatchCount(width, height float64) int64 {
	if width <= 0 || height <= 0 {
		return 0
	}
	return int64(math.Ceil(width/28) * math.Ceil(height/28))
}

// systemUnits weighs the system/instructions field: one unit for the plain
// string form, one unit per block for the structured (array) form — block
// granularity is what lets the drill-down itemize a multi-part system prompt.
func systemUnits(raw json.RawMessage) []unit {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []unit{{src: "system", label: "System prompt", text: s}}
	}
	blocks := anthBlocks(raw)
	if blocks == nil {
		if s := compactStr(raw); s != "" {
			return []unit{{src: "system", label: "System prompt", text: s}}
		}
		return nil
	}
	units := make([]unit, 0, len(blocks))
	for _, block := range blocks {
		var b struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(block, &b)
		text := b.Text
		if b.Type != "text" || text == "" {
			text = compactStr(block)
		}
		if text != "" {
			units = append(units, unit{src: "system", label: "System prompt", text: text})
		}
	}
	return units
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
		Model    string `json:"model"`
		Messages []struct {
			Role       string            `json:"role"`
			Name       string            `json:"name"`
			Content    json.RawMessage   `json:"content"`
			ToolCallID string            `json:"tool_call_id"`
			ToolCalls  []json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	var units []unit

	for _, raw := range req.Tools {
		var t struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		_ = json.Unmarshal(raw, &t)
		name := t.Function.Name
		if name == "" {
			name = t.Name
		}
		label := name
		if label == "" {
			label = "Tool schema"
		}
		units = append(units, unit{src: "tool_schemas", prod: producerKey(name), label: label, text: compactStr(raw)})
	}

	// Map tool_call id → function name across all assistant messages.
	idToName := map[string]string{}
	for _, m := range req.Messages {
		for _, raw := range m.ToolCalls {
			var tc struct {
				ID       string `json:"id"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			_ = json.Unmarshal(raw, &tc)
			if tc.ID != "" {
				idToName[tc.ID] = tc.Function.Name
			}
		}
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			units = append(units, openAIContentUnits(m.Content, req.Model, "system", "")...)
		case "user":
			units = append(units, openAIContentUnits(m.Content, req.Model, "user", "text")...)
		case "assistant":
			units = append(units, openAIContentUnits(m.Content, req.Model, "assistant", "text")...)
			for _, raw := range m.ToolCalls {
				var tc struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				}
				_ = json.Unmarshal(raw, &tc)
				label := tc.Function.Name
				if label == "" {
					label = "Tool use"
				}
				units = append(units, unit{src: "tool_calls", prod: producerKey(tc.Function.Name), label: label, text: compactStr(raw)})
			}
		case "tool", "function":
			name := idToName[m.ToolCallID]
			if name == "" {
				// Legacy attribution: the message's own name field.
				name = m.Name
			}
			if s := rawContentStr(m.Content); s != "" {
				label := "Tool result"
				if m.ToolCallID != "" {
					label += " " + m.ToolCallID
				}
				units = append(units, unit{src: "tool_results", prod: producerKey(name), label: label, text: s})
			}
		default:
			if s := rawContentStr(m.Content); s != "" {
				units = append(units, unit{src: "other", label: blockTypeLabel(m.Role), text: s})
			}
		}
	}
	return units
}

func openAIContentUnits(raw json.RawMessage, model, src, textProd string) []unit {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []unit{{src: src, prod: textProd, label: "Text block", text: s}}
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return []unit{{src: src, prod: textProd, label: defaultBlockLabel(src), text: compactStr(raw)}}
	}
	units := make([]unit, 0, len(blocks))
	for _, block := range blocks {
		if u, ok := openAIContentBlockUnit(block, model, src, textProd); ok {
			units = append(units, u)
		}
	}
	return units
}

func openAIContentBlockUnit(raw json.RawMessage, model, src, textProd string) (unit, bool) {
	var b struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &b)
	switch b.Type {
	case "text", "input_text", "output_text":
		return unit{src: src, prod: textProd, label: "Text block", text: b.Text}, b.Text != ""
	case "image_url", "input_image":
		tokens := openAIImageTokens(raw, model)
		return unit{src: src, prod: "attachments", label: "Image", tokens: tokens}, tokens > 0
	case "file", "input_file":
		tokens := openAIFileTokens(raw)
		return unit{src: src, prod: "attachments", label: "Attachment", tokens: tokens}, tokens > 0
	case "input_audio":
		// Base64 audio is never tokenized as text; without a published weight
		// rule it weighs nothing rather than something invented.
		return unit{}, false
	default:
		if s := compactStr(raw); s != "" {
			label := b.Type
			if label == "" {
				label = defaultBlockLabel(src)
			}
			return unit{src: src, prod: textProd, label: label, text: s}, true
		}
		return unit{}, false
	}
}

func openAIImageTokens(raw json.RawMessage, model string) int64 {
	url, detail := openAIImageURLAndDetail(raw)
	if detail == "" {
		detail = "auto"
	}
	if spec, ok := openAIPatchSpec(model); ok {
		cfg, ok := imageConfigFromDataURL(url)
		if !ok {
			return 0
		}
		patches := openAIPatchCount(cfg.Width, cfg.Height, spec.patchBudget)
		return int64(math.Ceil(float64(patches) * spec.multiplier))
	}
	spec := tileSpecOrDefault(model)
	if detail == "low" {
		return spec.base
	}
	cfg, ok := imageConfigFromDataURL(url)
	if !ok {
		return spec.base
	}
	return openAITileTokens(cfg.Width, cfg.Height, spec)
}

// openAIVisualTokens weighs an image of known size on an OpenAI wire.
func openAIVisualTokens(width, height int, model string) int64 {
	if spec, ok := openAIPatchSpec(model); ok {
		patches := openAIPatchCount(width, height, spec.patchBudget)
		return int64(math.Ceil(float64(patches) * spec.multiplier))
	}
	return openAITileTokens(width, height, tileSpecOrDefault(model))
}

// tileSpecOrDefault fails open to the gpt-5 tile spec for models without a
// published spec — like the Claude tier default, so images never silently
// vanish from the taxonomy for a model we haven't catalogued.
func tileSpecOrDefault(model string) openAITileImageSpec {
	if spec, ok := openAITileSpec(model); ok {
		return spec
	}
	return openAITileImageSpec{base: 70, tile: 140}
}

func openAIImageURLAndDetail(raw json.RawMessage) (string, string) {
	var b struct {
		Detail   string          `json:"detail"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	_ = json.Unmarshal(raw, &b)
	detail := strings.ToLower(b.Detail)
	if len(b.ImageURL) == 0 {
		return "", detail
	}
	var url string
	if err := json.Unmarshal(b.ImageURL, &url); err == nil {
		return url, detail
	}
	var obj struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(b.ImageURL, &obj)
	if detail == "" {
		detail = strings.ToLower(obj.Detail)
	}
	return obj.URL, detail
}

type openAIPatchImageSpec struct {
	patchBudget int64
	multiplier  float64
}

func openAIPatchSpec(model string) (openAIPatchImageSpec, bool) {
	name := strings.ToLower(model)
	switch {
	case strings.Contains(name, "gpt-5.4-mini"), strings.Contains(name, "gpt-5-mini"), strings.Contains(name, "gpt-4.1-mini"):
		return openAIPatchImageSpec{patchBudget: 1536, multiplier: 1.62}, true
	case strings.Contains(name, "gpt-5.4-nano"), strings.Contains(name, "gpt-5-nano"), strings.Contains(name, "gpt-4.1-nano"):
		return openAIPatchImageSpec{patchBudget: 1536, multiplier: 2.46}, true
	case name == "o4-mini" || strings.Contains(name, "o4-mini-"):
		return openAIPatchImageSpec{patchBudget: 1536, multiplier: 1.72}, true
	default:
		return openAIPatchImageSpec{}, false
	}
}

func openAIPatchCount(width, height int, patchBudget int64) int64 {
	if width <= 0 || height <= 0 {
		return 0
	}
	w, h := float64(width), float64(height)
	if long := math.Max(w, h); long > 2048 {
		scale := 2048 / long
		w *= scale
		h *= scale
	}
	patches := patch32Count(w, h)
	if patches <= patchBudget {
		return patches
	}
	shrink := math.Sqrt((32 * 32 * float64(patchBudget)) / (w * h))
	adjusted := shrink * math.Min(
		math.Floor(w*shrink/32)/(w*shrink/32),
		math.Floor(h*shrink/32)/(h*shrink/32),
	)
	if adjusted <= 0 || math.IsNaN(adjusted) || math.IsInf(adjusted, 0) {
		return patchBudget
	}
	resizedW := math.Floor(w * adjusted)
	resizedH := math.Floor(h * adjusted)
	patches = patch32Count(resizedW, resizedH)
	if patches > patchBudget {
		return patchBudget
	}
	return patches
}

func patch32Count(width, height float64) int64 {
	if width <= 0 || height <= 0 {
		return 0
	}
	return int64(math.Ceil(width/32) * math.Ceil(height/32))
}

type openAITileImageSpec struct {
	base int64
	tile int64
}

func openAITileSpec(model string) (openAITileImageSpec, bool) {
	name := strings.ToLower(model)
	switch {
	case name == "gpt-5" || name == "gpt-5-chat-latest":
		return openAITileImageSpec{base: 70, tile: 140}, true
	case strings.Contains(name, "gpt-4o-mini"):
		return openAITileImageSpec{base: 2833, tile: 5667}, true
	case strings.Contains(name, "gpt-4o"), strings.Contains(name, "gpt-4.1"), strings.Contains(name, "gpt-4.5"):
		return openAITileImageSpec{base: 85, tile: 170}, true
	case name == "o1" || strings.Contains(name, "o1-") || name == "o1-pro" || strings.Contains(name, "o1-pro-") || name == "o3" || strings.Contains(name, "o3-"):
		return openAITileImageSpec{base: 75, tile: 150}, true
	case strings.Contains(name, "computer-use-preview"):
		return openAITileImageSpec{base: 65, tile: 129}, true
	default:
		return openAITileImageSpec{}, false
	}
}

func openAITileTokens(width, height int, spec openAITileImageSpec) int64 {
	if width <= 0 || height <= 0 {
		return spec.base
	}
	w, h := float64(width), float64(height)
	if long := math.Max(w, h); long > 2048 {
		scale := 2048 / long
		w *= scale
		h *= scale
	}
	if short := math.Min(w, h); short > 0 && short != 768 {
		scale := 768 / short
		w *= scale
		h *= scale
	}
	tiles := int64(math.Ceil(w/512) * math.Ceil(h/512))
	return spec.base + tiles*spec.tile
}

// ---- OpenAI Responses request ----

func parseOpenAIResponses(body []byte) []unit {
	var req struct {
		Model        string            `json:"model"`
		Instructions json.RawMessage   `json:"instructions"`
		Input        json.RawMessage   `json:"input"`
		Tools        []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	var units []unit

	for _, raw := range req.Tools {
		var t struct {
			Type     string `json:"type"`
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		_ = json.Unmarshal(raw, &t)
		name := t.Name
		if name == "" {
			name = t.Function.Name
		}
		if name == "" {
			// Hosted tools (web_search, code_interpreter, ...) have no name.
			name = t.Type
		}
		label := name
		if label == "" {
			label = "Tool schema"
		}
		units = append(units, unit{src: "tool_schemas", prod: producerKey(name), label: label, text: compactStr(raw)})
	}

	units = append(units, systemUnits(req.Instructions)...)

	// The short input form is a plain string: one user text turn.
	var inputStr string
	if err := json.Unmarshal(req.Input, &inputStr); err == nil {
		if inputStr != "" {
			units = append(units, unit{src: "user", prod: "text", label: "Text block", text: inputStr})
		}
		return units
	}
	var input []json.RawMessage
	if err := json.Unmarshal(req.Input, &input); err != nil {
		return units
	}

	// Any "*_call" item invokes a tool; its paired "*_output" item attributes
	// back via call_id/id. Items that carry no tool name (computer_call,
	// local_shell_call, ...) attribute as their type — the honest identifier
	// the request actually has.
	callIDToName := map[string]string{}
	for _, raw := range input {
		var item struct {
			Type   string `json:"type"`
			Name   string `json:"name"`
			CallID string `json:"call_id"`
			ID     string `json:"id"`
		}
		_ = json.Unmarshal(raw, &item)
		if !strings.HasSuffix(item.Type, "_call") {
			continue
		}
		name := item.Name
		if name == "" {
			name = item.Type
		}
		if item.CallID != "" {
			callIDToName[item.CallID] = name
		}
		if item.ID != "" {
			callIDToName[item.ID] = name
		}
	}

	for _, raw := range input {
		var item struct {
			Type        string          `json:"type"`
			Role        string          `json:"role"`
			Name        string          `json:"name"`
			ServerLabel string          `json:"server_label"`
			Content     json.RawMessage `json:"content"`
			CallID      string          `json:"call_id"`
			ID          string          `json:"id"`
		}
		_ = json.Unmarshal(raw, &item)

		switch {
		case item.Type == "message" || (item.Type == "" && item.Role != ""):
			src, textProd := responsesRoleSource(item.Role)
			if src == "" {
				if s := compactStr(raw); s != "" {
					units = append(units, unit{src: "other", label: blockTypeLabel(item.Role), text: s})
				}
				continue
			}
			units = append(units, openAIContentUnits(item.Content, req.Model, src, textProd)...)
		case item.Type == "reasoning":
			if s := responsesReasoningText(raw); s != "" {
				units = append(units, unit{src: "assistant", prod: "reasoning", label: "Reasoning", text: s})
			}
		case item.Type == "image_generation_call":
			// The generation item carries its result as base64 `result`;
			// weigh it visually, never as base64 text.
			var g struct {
				Result string `json:"result"`
			}
			_ = json.Unmarshal(raw, &g)
			var tokens int64
			if g.Result != "" {
				if decoded, err := base64.StdEncoding.DecodeString(g.Result); err == nil {
					if cfg, ok := imageConfigFromBytes(decoded); ok {
						tokens = openAIVisualTokens(cfg.Width, cfg.Height, req.Model)
					}
				}
			}
			if tokens > 0 {
				name := item.Name
				if name == "" {
					name = item.Type
				}
				units = append(units, unit{src: "tool_results", prod: producerKey(name), label: name, tokens: tokens})
			}
		case item.Type == "mcp_list_tools":
			// An MCP server's tool listing is schema weight, attributed to the
			// server label the request carries.
			if s := compactStr(raw); s != "" {
				units = append(units, unit{src: "tool_schemas", prod: producerKey(item.ServerLabel), label: blockTypeLabel(item.ServerLabel), text: s})
			}
		case item.Type == "additional_tools":
			// Some clients ship every schema via additional_tools items rather
			// than the top-level tools array; attribute per tool, verbatim.
			var at struct {
				Tools []json.RawMessage `json:"tools"`
			}
			_ = json.Unmarshal(raw, &at)
			for _, rawTool := range at.Tools {
				var t struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal(rawTool, &t)
				label := t.Name
				if label == "" {
					label = "Tool schema"
				}
				units = append(units, unit{src: "tool_schemas", prod: producerKey(t.Name), label: label, text: compactStr(rawTool)})
			}
		case strings.HasSuffix(item.Type, "_output"):
			name := callIDToName[item.CallID]
			if name == "" {
				name = callIDToName[item.ID]
			}
			text, tokens := responsesOutputWeight(raw, req.Model)
			if text != "" || tokens > 0 {
				label := "Tool result"
				if item.CallID != "" {
					label += " " + item.CallID
				} else if item.ID != "" {
					label += " " + item.ID
				}
				units = append(units, unit{src: "tool_results", prod: producerKey(name), label: label, text: text, tokens: tokens})
			}
		case strings.HasSuffix(item.Type, "_call"):
			name := item.Name
			if name == "" {
				name = item.Type
			}
			if s := compactStr(raw); s != "" {
				units = append(units, unit{src: "tool_calls", prod: producerKey(name), label: name, text: s})
			}
		default:
			if s := compactStr(raw); s != "" {
				units = append(units, unit{src: "other", label: blockTypeLabel(item.Type), text: s})
			}
		}
	}
	return units
}

func responsesRoleSource(role string) (src, textProd string) {
	switch role {
	case "system", "developer":
		return "system", ""
	case "user":
		return "user", "text"
	case "assistant":
		return "assistant", "text"
	default:
		return "", ""
	}
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

// responsesOutputWeight weighs a *_output item: string outputs weigh their
// text; content-list outputs weigh text parts plus image visual tokens;
// computer_call_output screenshots weigh as visual tokens — never as
// base64-tokenized text, which would multiply the true cost.
func responsesOutputWeight(raw json.RawMessage, model string) (string, int64) {
	var item struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || len(item.Output) == 0 {
		return compactStr(raw), 0
	}
	var s string
	if err := json.Unmarshal(item.Output, &s); err == nil {
		return s, 0
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(item.Output, &blocks); err == nil {
		var texts []string
		var tokens int64
		for _, block := range blocks {
			var b struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(block, &b)
			switch b.Type {
			case "text", "input_text", "output_text":
				if b.Text != "" {
					texts = append(texts, b.Text)
				}
			case "input_image", "image_url", "output_image":
				tokens += openAIImageTokens(block, model)
			default:
				if s := compactStr(block); s != "" {
					texts = append(texts, s)
				}
			}
		}
		return strings.Join(texts, "\n"), tokens
	}
	// computer_call_output: output is {type: "computer_screenshot", image_url: "data:..."}.
	var obj struct {
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(item.Output, &obj); err == nil && obj.ImageURL != "" {
		return "", responsesScreenshotTokens(obj.ImageURL, model)
	}
	return compactStr(item.Output), 0
}

// responsesScreenshotTokens weighs a screenshot data URL with the model's
// image spec.
func responsesScreenshotTokens(url, model string) int64 {
	cfg, ok := imageConfigFromDataURL(url)
	if !ok {
		if _, isPatch := openAIPatchSpec(model); isPatch {
			return 0
		}
		return tileSpecOrDefault(model).base
	}
	return openAIVisualTokens(cfg.Width, cfg.Height, model)
}

// openAIFileTokens weighs a chat file / responses input_file block. Base64
// PDF payloads are weighed per page like Anthropic documents — never
// tokenized as base64 text. Files referenced by id/url carry no bytes to
// weigh.
func openAIFileTokens(raw json.RawMessage) int64 {
	var b struct {
		FileData string `json:"file_data"`
		File     struct {
			FileData string `json:"file_data"`
		} `json:"file"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return 0
	}
	data := b.FileData
	if data == "" {
		data = b.File.FileData
	}
	if data == "" {
		return 0
	}
	if strings.HasPrefix(data, "data:") {
		i := strings.Index(data, ",")
		if i < 0 {
			return 0
		}
		data = data[i+1:]
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return 0
	}
	return pdfPages(decoded) * 2250
}
