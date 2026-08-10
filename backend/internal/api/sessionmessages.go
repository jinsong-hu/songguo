package api

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// sessionMessagesView is the compact prompt material needed by the session
// Messages panel. Values stay in their native wire shape so the frontend can
// render protocol-specific tool, image, and reasoning blocks without receiving
// full request/response traces.
//
// System and Tools collect the request's top-level fields plus the instruction
// material some clients carry inside the message array instead — see
// hoistInlinePrompt.
type sessionMessagesView struct {
	SessionID string            `json:"session_id"`
	Model     string            `json:"model"`
	System    []json.RawMessage `json:"system"`
	Tools     []json.RawMessage `json:"tools"`
	Messages  []json.RawMessage `json:"messages"`
}

type capturedPromptBody struct {
	Model        string          `json:"model"`
	System       json.RawMessage `json:"system"`
	Instructions json.RawMessage `json:"instructions"`
	Tools        json.RawMessage `json:"tools"`
	Messages     json.RawMessage `json:"messages"`
	Input        json.RawMessage `json:"input"`
}

type promptItem struct {
	raw json.RawMessage
	key string
}

func (a *api) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	view, err := a.sessionMessagesData(r.PathValue("id"))
	if err != nil {
		a.writeDataErr(w, "get session messages", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *api) sessionMessagesData(id string) (sessionMessagesView, error) {
	view := sessionMessagesView{
		SessionID: id,
		System:    []json.RawMessage{},
		Tools:     []json.RawMessage{},
		Messages:  []json.RawMessage{},
	}
	requests, err := a.store.SessionRequests(id)
	if err != nil {
		return view, err
	}

	// adoptInlineSystem is decided once for the whole session rather than per
	// request: a session that hoisted an inline preamble out of some requests but
	// not others would hand mergePromptItems two different message shapes for the
	// same conversation, and the overlap merge would append instead of merge.
	prompts := make([]capturedPromptBody, 0, len(requests))
	adoptInlineSystem := true
	for _, request := range requests {
		if request.Wire == "anthropic/count_tokens" {
			continue
		}
		body := request.ReqBody
		if decoded, ok := decodeTraceBody(body, headerValue(request.ReqHeaders, "Content-Encoding")); ok {
			body = decoded
		}

		var prompt capturedPromptBody
		if err := json.Unmarshal(body, &prompt); err != nil {
			// The former frontend path ignored malformed captures as well. One bad
			// request must not hide the rest of a session's conversation.
			continue
		}
		if hasJSONValue(topLevelSystem(prompt)) {
			adoptInlineSystem = false
		}
		prompts = append(prompts, prompt)
	}

	seenSystem := map[string]struct{}{}
	seenTools := map[string]struct{}{}
	var messages []promptItem
	for _, prompt := range prompts {
		if view.Model == "" && prompt.Model != "" {
			view.Model = prompt.Model
		}

		view.System = appendUniquePromptValue(view.System, seenSystem, topLevelSystem(prompt))

		if tools, ok := rawJSONArray(prompt.Tools); ok {
			for _, tool := range tools {
				view.Tools = appendUniquePromptValue(view.Tools, seenTools, tool)
			}
		}

		items := requestMessageItems(prompt.Messages, prompt.Input)
		inlineSystem, inlineTools, next := hoistInlinePrompt(items, adoptInlineSystem)
		for _, tool := range inlineTools {
			view.Tools = appendUniquePromptValue(view.Tools, seenTools, tool)
		}
		for _, block := range inlineSystem {
			view.System = appendUniquePromptValue(view.System, seenSystem, block)
		}
		messages = mergePromptItems(messages, next)
	}

	for _, item := range messages {
		view.Messages = append(view.Messages, item.raw)
	}
	return view, nil
}

// topLevelSystem is the request's out-of-band instruction field: Anthropic's
// system, or the Responses API's instructions.
func topLevelSystem(prompt capturedPromptBody) json.RawMessage {
	if hasJSONValue(prompt.System) {
		return prompt.System
	}
	return prompt.Instructions
}

// inlinePromptItem is the sliver of a message item that decides whether it is
// conversation or instruction material the client folded into the array.
type inlinePromptItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Tools   json.RawMessage `json:"tools"`
	Content json.RawMessage `json:"content"`
}

// hoistInlinePrompt splits a request's message items into the instruction
// preamble some clients carry inline and the conversation proper, mirroring how
// internal/compose already weighs the same items (additional_tools as
// tool_schemas, system/developer roles as system). Without it a client that
// declares neither field at the top level — Codex's responses-lite shape, where
// the schemas ride in an additional_tools item and the prompt in a run of
// developer messages — renders an empty System and Tools panel while its
// Context tab reads correctly.
//
// Tool schemas are lifted wherever they appear: an additional_tools item is
// never conversation. The system run is lifted only when adoptSystem says the
// session declares no top-level system, and only from the head of the array.
// Both restrictions exist because a developer item is not always instructions —
// clients that do declare instructions out of band still send developer items
// for per-turn context and compaction summaries, and those are conversation.
func hoistInlinePrompt(items []promptItem, adoptSystem bool) (system, tools []json.RawMessage, rest []promptItem) {
	rest = make([]promptItem, 0, len(items))
	head := adoptSystem
	for _, item := range items {
		var decoded inlinePromptItem
		if err := json.Unmarshal(item.raw, &decoded); err != nil {
			// A bare string item, or anything else that is not an object: ordinary
			// conversation, and the end of any preamble.
			head = false
			rest = append(rest, item)
			continue
		}
		if decoded.Type == "additional_tools" {
			if inline, ok := rawJSONArray(decoded.Tools); ok {
				tools = append(tools, inline...)
			}
			continue
		}
		if head && (decoded.Role == "system" || decoded.Role == "developer") {
			value := decoded.Content
			if !hasJSONValue(value) {
				value = item.raw
			}
			system = append(system, value)
			continue
		}
		head = false
		rest = append(rest, item)
	}
	return system, tools, rest
}

func requestMessageItems(messages, input json.RawMessage) []promptItem {
	items, ok := rawJSONArray(messages)
	if !ok {
		items, ok = rawJSONArray(input)
	}
	if !ok && isJSONString(input) {
		items = []json.RawMessage{cloneRawMessage(input)}
	}
	if len(items) == 0 {
		return nil
	}
	out := make([]promptItem, 0, len(items))
	for _, raw := range items {
		out = append(out, promptItem{
			raw: cloneRawMessage(raw),
			key: canonicalPromptJSON(raw, true),
		})
	}
	return out
}

// mergePromptItems mirrors the old frontend overlap merge: a cumulative next
// request contributes only the tail after the existing-suffix/next-prefix
// overlap. When an agent compacts its context and no overlap exists, both the
// earlier history and the compacted continuation are retained.
func mergePromptItems(existing, next []promptItem) []promptItem {
	if len(next) == 0 {
		return existing
	}
	overlap := min(len(existing), len(next))
	for overlap > 0 {
		existingStart := len(existing) - overlap
		matches := true
		for i := 0; i < overlap; i++ {
			if existing[existingStart+i].key != next[i].key {
				matches = false
				break
			}
		}
		if matches {
			break
		}
		overlap--
	}
	return append(existing, next[overlap:]...)
}

func appendUniquePromptValue(dst []json.RawMessage, seen map[string]struct{}, raw json.RawMessage) []json.RawMessage {
	if !hasJSONValue(raw) {
		return dst
	}
	key := canonicalPromptJSON(raw, false)
	if _, ok := seen[key]; ok {
		return dst
	}
	seen[key] = struct{}{}
	return append(dst, cloneRawMessage(raw))
}

func rawJSONArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var values []json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return nil, false
	}
	if values == nil {
		values = []json.RawMessage{}
	}
	return values, true
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func isJSONString(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 1 && trimmed[0] == '"' && json.Valid(trimmed)
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

// canonicalPromptJSON produces a stable comparison key independent of object
// key order. Message comparisons ignore cache_control because the frontend
// strips it before rendering and equality checks.
func canonicalPromptJSON(raw json.RawMessage, stripCacheControl bool) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return string(raw)
	}
	if stripCacheControl {
		value = withoutCacheControl(value)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return string(raw)
	}
	return string(encoded)
}

func withoutCacheControl(value any) any {
	switch value := value.(type) {
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = withoutCacheControl(child)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			if key != "cache_control" {
				out[key] = withoutCacheControl(child)
			}
		}
		return out
	default:
		return value
	}
}
