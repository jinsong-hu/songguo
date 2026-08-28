package parse

import (
	"encoding/json"
	"fmt"
	"sort"
)

// --- request shapes (shared by chat/completions) ---

type oaiReq struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	Tools    []oaiTool    `json:"tools"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content"` // string or []part
	Name       string        `json:"name"`
	ToolCallID string        `json:"tool_call_id"`
	ToolCalls  []oaiToolCall `json:"tool_calls"`
}

type oaiTool struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (m oaiMessage) toMessage() Message {
	msg := Message{Role: m.Role, Text: flexText(m.Content), Name: m.Name, ToolCallID: m.ToolCallID}
	for _, tc := range m.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return msg
}

// parseOpenAIChat parses a chat/completions call (request always; response
// either buffered JSON or reassembled from SSE).
func parseOpenAIChat(in Input) (Call, error) {
	c := Call{Format: "openai-chat"}

	var req oaiReq
	if err := json.Unmarshal(in.ReqBody, &req); err == nil {
		c.Model = req.Model
		for _, m := range req.Messages {
			if m.Role == "system" || m.Role == "developer" {
				if c.System == "" {
					c.System = flexText(m.Content)
				}
			}
			c.Input = append(c.Input, m.toMessage())
		}
		for _, t := range req.Tools {
			if t.Function.Name != "" {
				c.Tools = append(c.Tools, t.Function.Name)
			}
		}
	}

	var perr error
	if in.Stream {
		perr = c.openAIChatFromStream(in.RespBody)
	} else {
		perr = c.openAIChatFromJSON(in.RespBody)
	}
	c.finalize(in)
	return c, perr
}

func (c *Call) openAIChatFromJSON(body []byte) error {
	var resp struct {
		Choices []struct {
			Message      oaiMessage `json:"message"`
			FinishReason string     `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse openai-chat response: %w", err)
	}
	for _, ch := range resp.Choices {
		c.Output = append(c.Output, ch.Message.toMessage())
		if ch.FinishReason != "" {
			c.FinishReason = ch.FinishReason
		}
	}
	if resp.Usage != nil {
		c.Tokens = tokensFrom(resp.Usage)
	}
	return nil
}

// openAIChatFromStream reassembles the assistant message from SSE deltas:
// content text is concatenated and tool calls are accumulated by their index
// (id/name arrive once, arguments stream in fragments). Usage rides the final
// chunk when stream_options.include_usage was set.
func (c *Call) openAIChatFromStream(body []byte) error {
	type tcAccum struct {
		id, name string
		args     string
	}
	var (
		text   string
		role   = "assistant"
		finish string
		usage  map[string]any
		tcs    = map[int]*tcAccum{}
		seen   bool
	)

	for _, ev := range scanSSE(body) {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Role      string `json:"role"`
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(ev.data), &chunk); err != nil {
			continue
		}
		seen = true
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Role != "" {
				role = ch.Delta.Role
			}
			text += ch.Delta.Content
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc := tcs[tc.Index]
				if acc == nil {
					acc = &tcAccum{}
					tcs[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				acc.args += tc.Function.Arguments
			}
		}
	}

	if !seen {
		return fmt.Errorf("parse openai-chat stream: no data events")
	}

	msg := Message{Role: role, Text: text}
	for _, idx := range sortedIntKeys(tcs) {
		acc := tcs[idx]
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: acc.id, Name: acc.name, Arguments: acc.args})
	}
	c.Output = append(c.Output, msg)
	c.FinishReason = finish
	if usage != nil {
		c.Tokens = tokensFrom(usage)
	}
	return nil
}

// parseOpenAIResponses parses the OpenAI Responses API. Its output is an array
// of typed items; we pull text and function-call items into one assistant
// message. Streaming is not delta-reassembled here — a streamed Responses body
// records request + usage best-effort.
func parseOpenAIResponses(in Input) (Call, error) {
	c := Call{Format: "openai-responses"}
	c.Model = modelFromBody(in.ReqBody)
	c.Input = responsesInput(in.ReqBody)

	var perr error
	if in.Stream {
		c.Tokens = lastSSEUsage(in.RespBody, "response")
		if c.Tokens == (Tokens{}) {
			perr = fmt.Errorf("parse openai-responses stream: no usage")
		}
	} else {
		var resp struct {
			Output []struct {
				Type    string `json:"type"`
				Name    string `json:"name"`      // function_call
				Args    string `json:"arguments"` // function_call
				CallID  string `json:"call_id"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal(in.RespBody, &resp); err != nil {
			perr = fmt.Errorf("parse openai-responses response: %w", err)
		} else {
			msg := Message{Role: "assistant"}
			for _, item := range resp.Output {
				switch item.Type {
				case "function_call":
					msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Args})
				default:
					for _, ct := range item.Content {
						if ct.Text != "" {
							if msg.Text != "" {
								msg.Text += "\n"
							}
							msg.Text += ct.Text
						}
					}
				}
			}
			if msg.Text != "" || len(msg.ToolCalls) > 0 {
				c.Output = append(c.Output, msg)
			}
			if resp.Usage != nil {
				c.Tokens = tokensFrom(resp.Usage)
			}
		}
	}
	c.finalize(in)
	return c, perr
}

// responsesInput normalizes a Responses request's `input` into the same
// []Message the chat and anthropic parsers produce.
//
// This exists for Call.Fingerprint, which is the sole consumer of Input, and
// that fixes what it has to guarantee: ONE Message per input item, in order.
// The fingerprint compares a session's requests to find the ones whose bodies
// are redundant prefixes of a later one (see store/messagecover.go), so the
// mapping has to be positional. Collapsing items — folding a tool result into
// the message before it, dropping an additional_tools preamble — would make
// Count stop tracking the array's length and let two different conversations
// agree on it.
//
// > History: this returned nothing at all until 2026-08-28. Input was never
// > populated for openai/responses, so Fingerprint returned its zero value,
// > SaveMessageFingerprint's `count <= 0` guard dropped the write, and every
// > call kept msg_count = 0 — which messageCover reads as "unknown", never
// > redundant. The cover therefore selected every request in the session and
// > the Messages panel read all of them. On the production gateway that was
// > 0 of 4882 openai/responses calls fingerprinted in a day against 542 of 668
// > for anthropic/messages, and opening a 2290-call session stalled the host
// > for 13 minutes.
func responsesInput(body []byte) []Message {
	var req struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Input) == 0 {
		return nil
	}

	var items []json.RawMessage
	if err := json.Unmarshal(req.Input, &items); err != nil {
		// `input` accepts a bare string prompt in place of the array.
		var s string
		if err := json.Unmarshal(req.Input, &s); err == nil && s != "" {
			return []Message{{Role: "user", Text: s}}
		}
		return nil
	}

	out := make([]Message, 0, len(items))
	for _, raw := range items {
		out = append(out, responsesMessage(raw))
	}
	return out
}

// responsesMessage normalizes one `input` item.
//
// Decoded into a map rather than a struct, deliberately. The item types share
// field NAMES without sharing field TYPES — `arguments` is a JSON string on a
// function_call and a JSON object on a tool_search_call — so a struct with
// `Arguments string` fails to unmarshal the whole item the moment a client
// sends the other shape, discarding the type and id along with it. A map
// degrades one field at a time, which is the same reason anthropicMessage
// walks content blocks as map[string]any.
//
// The default branch carries the item's type and id through Role and Name when
// the item has no role of its own, and falls back to the item's raw bytes when
// neither survives. That is not cosmetic. A reasoning item keeps its payload in
// `encrypted_content` and a tool_search_call keeps its in `arguments`, so
// nothing this function reads distinguishes one from another — and two items
// that hash alike are what license the cover to DROP a body. An id a client
// varies between turns costs a false run boundary instead, which the cover pays
// for with one redundant read. Of the two failure modes only the second is
// recoverable, so every path here is arranged to fail into it.
func responsesMessage(raw json.RawMessage) Message {
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil {
		// Not an object — a bare string item, or something unmodelled. Its bytes
		// still have to distinguish it from its neighbours.
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return Message{Role: "user", Text: s}
		}
		return Message{Text: string(raw)}
	}

	typ, _ := item["type"].(string)
	role, _ := item["role"].(string)
	id, _ := item["id"].(string)
	callID, _ := item["call_id"].(string)

	switch typ {
	case "function_call":
		name, _ := item["name"].(string)
		return Message{
			Role:      "assistant",
			Name:      id,
			ToolCalls: []ToolCall{{ID: callID, Name: name, Arguments: jsonText(item["arguments"])}},
		}
	case "function_call_output":
		return Message{Role: "tool", Name: id, ToolCallID: callID, Text: flexText(item["output"])}
	}

	msg := Message{Role: role, Text: flexText(item["content"]), Name: id}
	if msg.Role == "" {
		msg.Role = typ
	}
	if msg.Name == "" {
		msg.Name = callID
	}
	if msg.Text == "" && msg.Name == "" {
		msg.Text = string(raw)
	}
	return msg
}

// jsonText renders a value that is a JSON string in one item type and a JSON
// object in another (`arguments`) as comparable text.
func jsonText(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case string:
		return v
	}
	if raw, err := json.Marshal(v); err == nil {
		return string(raw)
	}
	return ""
}

// lastSSEUsage scans an SSE body for the last event whose JSON carries a usage
// object (optionally nested under a named field, e.g. "response").
func lastSSEUsage(body []byte, nestUnder string) Tokens {
	var t Tokens
	for _, ev := range scanSSE(body) {
		var top struct {
			Usage    map[string]any `json:"usage"`
			Response struct {
				Usage map[string]any `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(ev.data), &top); err != nil {
			continue
		}
		if top.Usage != nil {
			t = tokensFrom(top.Usage)
		} else if nestUnder == "response" && top.Response.Usage != nil {
			t = tokensFrom(top.Response.Usage)
		}
	}
	return t
}

func sortedIntKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
