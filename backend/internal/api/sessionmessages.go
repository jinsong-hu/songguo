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

	seenSystem := map[string]struct{}{}
	seenTools := map[string]struct{}{}
	var messages []promptItem
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
		if view.Model == "" && prompt.Model != "" {
			view.Model = prompt.Model
		}

		system := prompt.System
		if !hasJSONValue(system) {
			system = prompt.Instructions
		}
		view.System = appendUniquePromptValue(view.System, seenSystem, system)

		if tools, ok := rawJSONArray(prompt.Tools); ok {
			for _, tool := range tools {
				view.Tools = appendUniquePromptValue(view.Tools, seenTools, tool)
			}
		}

		next := requestMessageItems(prompt.Messages, prompt.Input)
		messages = mergePromptItems(messages, next)
	}

	for _, item := range messages {
		view.Messages = append(view.Messages, item.raw)
	}
	return view, nil
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
