// Package sessiontitle derives durable coding-session titles from captured
// request and response bodies.
package sessiontitle

import (
	"encoding/json"
	"strings"
)

const (
	maxTitleRunes   = 120
	codexTitleWords = 8
)

// FromClaude extracts the title returned by Claude Code's dedicated
// title-generation request. Ordinary Anthropic messages return an empty title.
func FromClaude(reqBody, respBody []byte) string {
	if !looksLikeClaudeTitleRequest(reqBody) {
		return ""
	}
	text := extractAnthropicText(respBody)
	if text == "" {
		text = string(respBody)
	}
	return cleanClaudeTitle(text)
}

// FromCodex uses the first user message in an OpenAI chat or Responses request.
func FromCodex(reqBody []byte) string {
	var req struct {
		Messages json.RawMessage `json:"messages"`
		Input    json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return ""
	}

	text := firstUserText(req.Messages)
	if text == "" {
		text = firstUserText(req.Input)
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	if len(words) > codexTitleWords {
		words = words[:codexTitleWords]
	}
	return limitTitle(strings.Join(words, " "))
}

func firstUserText(raw json.RawMessage) string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil {
		return strings.TrimSpace(direct)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		items = []json.RawMessage{raw}
	}
	for _, item := range items {
		var message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(item, &message); err != nil ||
			!strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		if text := textFromContent(message.Content); text != "" {
			return text
		}
	}
	return ""
}

func textFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(textFromJSONValue(value))
}

func looksLikeClaudeTitleRequest(body []byte) bool {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}

	systemText := textFromJSONValue(req["system"])
	return strings.Contains(systemText, "Generate a concise, sentence-case") &&
		hasTitleOutputSchema(req["output_config"])
}

func extractAnthropicText(body []byte) string {
	if text := extractAnthropicStreamText(string(body)); text != "" {
		return text
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	switch content := obj["content"].(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := block["type"].(string); typ != "" && typ != "text" {
				continue
			}
			if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func hasTitleOutputSchema(value any) bool {
	outputConfig, ok := value.(map[string]any)
	if !ok {
		return false
	}
	format, ok := outputConfig["format"].(map[string]any)
	if !ok || format["type"] != "json_schema" {
		return false
	}
	schema, ok := format["schema"].(map[string]any)
	if !ok || schema["type"] != "object" {
		return false
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return false
	}
	title, ok := properties["title"].(map[string]any)
	if !ok || title["type"] != "string" {
		return false
	}
	for _, item := range asAnySlice(schema["required"]) {
		if item == "title" {
			return true
		}
	}
	return false
}

func asAnySlice(value any) []any {
	items, _ := value.([]any)
	return items
}

func extractAnthropicStreamText(body string) string {
	var parts []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if typ, _ := event["type"].(string); typ != "content_block_delta" {
			continue
		}
		delta, _ := event["delta"].(map[string]any)
		if text, ok := delta["text"].(string); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}

func textFromJSONValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if text := textFromJSONValue(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, ok := v["text"].(string); ok {
			return text
		}
		if content, ok := v["content"]; ok {
			return textFromJSONValue(content)
		}
		return ""
	default:
		return ""
	}
}

func cleanClaudeTitle(raw string) string {
	title := strings.TrimSpace(raw)
	if title == "" {
		return ""
	}
	var jsonString string
	if err := json.Unmarshal([]byte(title), &jsonString); err == nil {
		title = strings.TrimSpace(jsonString)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(title), &obj); err == nil {
		if value, ok := obj["title"].(string); ok {
			title = strings.TrimSpace(value)
		} else {
			return ""
		}
	}
	title = strings.Trim(title, " \t\r\n\"'`")
	title = strings.Join(strings.Fields(title), " ")
	return limitTitle(title)
}

func limitTitle(title string) string {
	runes := []rune(strings.TrimSpace(title))
	if len(runes) > maxTitleRunes {
		runes = runes[:maxTitleRunes]
	}
	return strings.TrimSpace(string(runes))
}
