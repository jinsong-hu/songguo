package parse

import (
	"encoding/json"
	"fmt"
)

// parseAnthropic parses an Anthropic /messages call. Request: system (string or
// blocks) + messages whose content is text and/or tool_use/tool_result blocks.
// Response: buffered JSON, or reassembled from the event-typed SSE stream.
func parseAnthropic(in Input) (Call, error) {
	c := Call{Format: "anthropic-messages"}

	var req struct {
		Model    string `json:"model"`
		System   any    `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(in.ReqBody, &req); err == nil {
		c.Model = req.Model
		c.System = flexText(req.System)
		for _, m := range req.Messages {
			c.Input = append(c.Input, anthropicMessage(m.Role, m.Content))
		}
		for _, t := range req.Tools {
			if t.Name != "" {
				c.Tools = append(c.Tools, t.Name)
			}
		}
	}

	var perr error
	if in.Stream {
		perr = c.anthropicFromStream(in.RespBody)
	} else {
		perr = c.anthropicFromJSON(in.RespBody)
	}
	c.finalize(in)
	return c, perr
}

// anthropicMessage flattens one request message's content blocks into a
// Message: text blocks concatenate into Text, tool_use blocks become ToolCalls,
// tool_result blocks set ToolCallID + their text.
func anthropicMessage(role string, content any) Message {
	msg := Message{Role: role}
	blocks, ok := content.([]any)
	if !ok {
		msg.Text = flexText(content)
		return msg
	}
	for _, b := range blocks {
		m, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "text":
			if txt, _ := m["text"].(string); txt != "" {
				if msg.Text != "" {
					msg.Text += "\n"
				}
				msg.Text += txt
			}
		case "tool_use":
			name, _ := m["name"].(string)
			id, _ := m["id"].(string)
			args := ""
			if input, ok := m["input"]; ok {
				if raw, err := json.Marshal(input); err == nil {
					args = string(raw)
				}
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: id, Name: name, Arguments: args})
		case "tool_result":
			if id, ok := m["tool_use_id"].(string); ok {
				msg.ToolCallID = id
			}
			if txt := flexText(m["content"]); txt != "" {
				if msg.Text != "" {
					msg.Text += "\n"
				}
				msg.Text += txt
			}
		}
	}
	return msg
}

func (c *Call) anthropicFromJSON(body []byte) error {
	var resp struct {
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse anthropic response: %w", err)
	}
	role := resp.Role
	if role == "" {
		role = "assistant"
	}
	msg := Message{Role: role}
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			if msg.Text != "" {
				msg.Text += "\n"
			}
			msg.Text += b.Text
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: string(b.Input)})
		}
	}
	c.Output = append(c.Output, msg)
	c.FinishReason = resp.StopReason
	if resp.Usage != nil {
		c.Tokens = tokensFrom(resp.Usage)
	}
	return nil
}

// anthropicFromStream reassembles the response from the event-typed stream:
// message_start carries the role + input/cache usage; content_block_start opens
// a text or tool_use block at an index; *_delta appends text or partial tool
// JSON; message_delta carries stop_reason + output token count.
func (c *Call) anthropicFromStream(body []byte) error {
	type blockAccum struct {
		kind, id, name string
		text, args     string
	}
	var (
		role   = "assistant"
		stop   string
		blocks = map[int]*blockAccum{}
		usage  = map[string]any{}
		seen   bool
	)

	for _, ev := range scanSSE(body) {
		seen = true
		switch ev.event {
		case "message_start":
			var d struct {
				Message struct {
					Role  string         `json:"role"`
					Usage map[string]any `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(ev.data), &d) == nil {
				if d.Message.Role != "" {
					role = d.Message.Role
				}
				for k, v := range d.Message.Usage {
					usage[k] = v
				}
			}
		case "content_block_start":
			var d struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if json.Unmarshal([]byte(ev.data), &d) == nil {
				blocks[d.Index] = &blockAccum{kind: d.ContentBlock.Type, id: d.ContentBlock.ID, name: d.ContentBlock.Name}
			}
		case "content_block_delta":
			var d struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if json.Unmarshal([]byte(ev.data), &d) == nil {
				acc := blocks[d.Index]
				if acc == nil {
					acc = &blockAccum{}
					blocks[d.Index] = acc
				}
				acc.text += d.Delta.Text
				acc.args += d.Delta.PartialJSON
			}
		case "message_delta":
			var d struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage map[string]any `json:"usage"`
			}
			if json.Unmarshal([]byte(ev.data), &d) == nil {
				if d.Delta.StopReason != "" {
					stop = d.Delta.StopReason
				}
				for k, v := range d.Usage {
					usage[k] = v
				}
			}
		}
	}

	if !seen {
		return fmt.Errorf("parse anthropic stream: no events")
	}

	msg := Message{Role: role}
	for _, idx := range sortedIntKeys(blocks) {
		b := blocks[idx]
		switch b.kind {
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: b.id, Name: b.name, Arguments: b.args})
		default:
			if b.text != "" {
				if msg.Text != "" {
					msg.Text += "\n"
				}
				msg.Text += b.text
			}
		}
	}
	c.Output = append(c.Output, msg)
	c.FinishReason = stop
	if len(usage) > 0 {
		c.Tokens = tokensFrom(usage)
	}
	return nil
}
