package sessiontitle

import "testing"

func TestFromClaude(t *testing.T) {
	req := []byte(`{
		"messages":[{"role":"user","content":[{"type":"text","text":"<session>\nPlease polish the session detail page.\n</session>"}]}],
		"system":[{"type":"text","text":"Generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session. Return JSON with a single \"title\" field."}],
		"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}}
	}`)
	resp := []byte("event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"{\\\"title\\\": \\\"Session\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" detail polish\\\"}\"}}\n\n")

	if got := FromClaude(req, resp); got != "Session detail polish" {
		t.Fatalf("FromClaude = %q, want Session detail polish", got)
	}
}

func TestFromClaudeRejectsOrdinaryRequest(t *testing.T) {
	req := []byte(`{
		"messages":[{"role":"user","content":"Perform a web search"}],
		"system":"You are an assistant for performing a web search tool use"
	}`)
	if got := FromClaude(req, []byte(`{"content":[{"type":"text","text":"Not a title"}]}`)); got != "" {
		t.Fatalf("FromClaude ordinary request = %q, want empty", got)
	}
}

func TestFromCodexResponsesUsesFirstUserMessage(t *testing.T) {
	req := []byte(`{
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"System instructions"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Add persistent session titles to the recent activity table today"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Ignore this later message"}]}
		]
	}`)
	if got := FromCodex(req); got != "Add persistent session titles to the recent activity" {
		t.Fatalf("FromCodex = %q", got)
	}
}

func TestFromCodexChatAndStringInput(t *testing.T) {
	chat := []byte(`{"messages":[{"role":"system","content":"system"},{"role":"user","content":"Fix the session title column"}]}`)
	if got := FromCodex(chat); got != "Fix the session title column" {
		t.Fatalf("FromCodex chat = %q", got)
	}

	input := []byte(`{"input":"Summarize this captured coding session"}`)
	if got := FromCodex(input); got != "Summarize this captured coding session" {
		t.Fatalf("FromCodex string input = %q", got)
	}
}
