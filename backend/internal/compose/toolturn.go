package compose

import (
	"encoding/json"
	"strings"
)

// ToolTurn reads the just-completed tool round-trip carried by a chat REQUEST
// body and returns two per-call metrics: the number of tool calls the assistant
// issued on that turn, and a local o200k token estimate of the tool results that
// came back.
//
// It reads ONLY the tail of the message history — the newest results and the
// assistant turn that produced them — never the cumulative tool history sitting
// in the rest of the window. That tail belongs to exactly ONE request (the one
// that carries those results as its final turn), so summing per-call values over
// a session double-counts nothing: SUM(tool_calls)/SUM(tool_tokens) across a
// session's calls equal the run's total tool invocations and result tokens.
// (A chat request resends the whole history, so the cumulative view — what
// Compose reports — would triangular-overcount on SUM; the tail view does not.)
//
// The final turn of a session, whose results are never sent in a follow-up
// request, is deliberately uncounted: that output never entered a model call.
// The two numbers are also attributed one call "late" — turn N's tools land on
// call N+1, the request that carries their results — which is exact in aggregate
// and consistent per call.
//
// Tokens are a local estimate (o200k, the same tokenizer as Compose), decoupled
// from the vendor's billed usage. This is pure read-only sniffing of the
// already-buffered request body — the same category as reading `model` or
// metering `usage` — and never mutates a byte.
//
// wireName selects the request shape (anthropic / openai chat / openai
// responses), matched exactly as Compose does. An unrecognized wire, an
// unparseable body, or a tail that is not a tool-result turn yields (0, 0).
func ToolTurn(wireName string, reqBody []byte) (toolCalls int, toolTokens int64) {
	switch {
	case strings.Contains(wireName, "anthropic"):
		return anthropicToolTurn(reqBody)
	case strings.Contains(wireName, "responses"):
		return responsesToolTurn(reqBody)
	case strings.Contains(wireName, "openai"), strings.Contains(wireName, "chat"):
		return openAIChatToolTurn(reqBody)
	default:
		return 0, 0
	}
}

// countTokensLocal is ToolTurn's o200k count for one string, lazily loading the
// shared encoder. If the encoder is unavailable the token estimate degrades to
// 0 while the structural call count is still returned.
func countTokensLocal(s string) int64 {
	if s == "" {
		return 0
	}
	e, err := encoder()
	if err != nil {
		return 0
	}
	return countTokens(e, s)
}

// anthropicToolTurn: results are the tool_result blocks of the LAST user
// message; the calls that produced them are the tool_use blocks of the nearest
// preceding assistant message.
func anthropicToolTurn(body []byte) (int, int64) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return 0, 0
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		return 0, 0 // tail is not a results turn
	}

	var tokens int64
	nResults := 0
	for _, raw := range anthBlocks(last.Content) {
		var b struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(raw, &b)
		if b.Type != "tool_result" {
			continue
		}
		nResults++
		text, visual := anthropicContentWeight(b.Content, req.Model)
		tokens += countTokensLocal(text) + visual
	}
	if nResults == 0 {
		return 0, 0 // last user turn is plain text, not tool results
	}

	calls := 0
	for i := len(req.Messages) - 2; i >= 0; i-- {
		if req.Messages[i].Role != "assistant" {
			continue
		}
		for _, raw := range anthBlocks(req.Messages[i].Content) {
			var b struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(raw, &b)
			if b.Type == "tool_use" || b.Type == "server_tool_use" {
				calls++
			}
		}
		break // nearest preceding assistant only
	}
	return calls, tokens
}

// openAIChatToolTurn: results are the trailing run of role:"tool" messages; the
// calls are the tool_calls array of the assistant message just before that run.
func openAIChatToolTurn(body []byte) (int, int64) {
	var req struct {
		Messages []struct {
			Role      string            `json:"role"`
			Content   json.RawMessage   `json:"content"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return 0, 0
	}

	i := len(req.Messages) - 1
	var tokens int64
	nResults := 0
	for ; i >= 0 && req.Messages[i].Role == "tool"; i-- {
		nResults++
		tokens += countTokensLocal(rawContentStr(req.Messages[i].Content))
	}
	if nResults == 0 {
		return 0, 0
	}

	calls := 0
	if i >= 0 && req.Messages[i].Role == "assistant" {
		calls = len(req.Messages[i].ToolCalls)
	}
	return calls, tokens
}

// responsesToolTurn: results are the trailing run of *_call_output items; the
// calls are the *_call items immediately preceding that run.
func responsesToolTurn(body []byte) (int, int64) {
	var req struct {
		Model string            `json:"model"`
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Input) == 0 {
		return 0, 0
	}
	typeOf := func(raw json.RawMessage) string {
		var it struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &it)
		return it.Type
	}

	i := len(req.Input) - 1
	var tokens int64
	nResults := 0
	for ; i >= 0 && isResponsesOutput(typeOf(req.Input[i])); i-- {
		nResults++
		text, visual := responsesOutputWeight(req.Input[i], req.Model)
		tokens += countTokensLocal(text) + visual
	}
	if nResults == 0 {
		return 0, 0
	}

	calls := 0
	for ; i >= 0 && isResponsesCall(typeOf(req.Input[i])); i-- {
		calls++
	}
	return calls, tokens
}

// isResponsesOutput / isResponsesCall mirror Compose's generalized item-type
// rules: any *_output item is a tool result, any *_call item invokes a tool.
func isResponsesOutput(t string) bool {
	return strings.HasSuffix(t, "_output")
}

func isResponsesCall(t string) bool {
	return strings.HasSuffix(t, "_call")
}
