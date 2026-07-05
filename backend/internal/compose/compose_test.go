package compose

import "testing"

// sampleAnthropic is a small but representative Claude Code request: a system
// prompt, two tool schemas (one builtin, one MCP), and a multi-turn message list
// with a tool_use / tool_result pair, an image, and a thinking block.
const sampleAnthropic = `{
  "model": "claude-x",
  "system": "You are a careful assistant. Follow the rules exactly.",
  "tools": [
    {"name": "Read", "description": "Read a file", "input_schema": {"type": "object"}},
    {"name": "mcp__github__list_prs", "description": "List PRs", "input_schema": {"type": "object"}}
  ],
  "messages": [
    {"role": "user", "content": "Please read main.go and summarize it."},
    {"role": "assistant", "content": [
      {"type": "thinking", "thinking": "I should read the file first."},
      {"type": "text", "text": "Reading now."},
      {"type": "tool_use", "id": "tu_1", "name": "Read", "input": {"path": "main.go"}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "tu_1", "content": "package main\nfunc main() {}"},
      {"type": "image", "source": {"type": "base64", "data": "AAAA"}},
      {"type": "text", "text": "Here is a screenshot too."}
    ]}
  ]
}`

const sampleOpenAI = `{
  "model": "gpt-x",
  "tools": [
    {"type": "function", "function": {"name": "get_weather", "parameters": {}}}
  ],
  "messages": [
    {"role": "system", "content": "You are helpful."},
    {"role": "user", "content": "What is the weather?"},
    {"role": "assistant", "content": "Let me check.", "tool_calls": [
      {"id": "c1", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}}
    ]},
    {"role": "tool", "tool_call_id": "c1", "content": "sunny, 25C"}
  ]
}`

func sum(src []Source) (tokens, cached int64) {
	for _, s := range src {
		tokens += s.Tokens
		cached += s.Cached
	}
	return
}

func TestComposeInvariants(t *testing.T) {
	cases := []struct {
		name         string
		wire         string
		body         string
		inputTokens  int64
		cachedTokens int64
	}{
		{"anthropic", "anthropic-messages", sampleAnthropic, 1000, 640},
		{"anthropic-no-cache", "anthropic-messages", sampleAnthropic, 733, 0},
		{"openai", "openai-chat", sampleOpenAI, 137, 41},
		{"openai-full-cache", "openai-chat", sampleOpenAI, 200, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			comp, ok := Compose(tc.wire, []byte(tc.body), tc.inputTokens, tc.cachedTokens)
			if !ok {
				t.Fatalf("Compose returned ok=false")
			}
			if comp.Total != tc.inputTokens {
				t.Errorf("Total = %d, want %d", comp.Total, tc.inputTokens)
			}
			if comp.Cached != tc.cachedTokens {
				t.Errorf("Cached = %d, want %d", comp.Cached, tc.cachedTokens)
			}

			gotTokens, gotCached := sum(comp.Sources)
			// INVARIANT 1: per-source tokens sum EXACTLY to the official input total.
			if gotTokens != tc.inputTokens {
				t.Errorf("sum(sources.tokens) = %d, want %d", gotTokens, tc.inputTokens)
			}
			// INVARIANT 2: per-source cached sum EXACTLY to the official cache-read total.
			if gotCached != tc.cachedTokens {
				t.Errorf("sum(sources.cached) = %d, want %d", gotCached, tc.cachedTokens)
			}

			// Producer tokens never exceed their source's tokens.
			for _, s := range comp.Sources {
				var pt int64
				for _, p := range s.Children {
					pt += p.Tokens
				}
				if pt > s.Tokens {
					t.Errorf("source %q: producer tokens %d exceed source tokens %d", s.Key, pt, s.Tokens)
				}
				if s.Cached > s.Tokens {
					t.Errorf("source %q: cached %d exceeds tokens %d", s.Key, s.Cached, s.Tokens)
				}
			}
		})
	}
}

func TestComposeAnthropicSources(t *testing.T) {
	comp, ok := Compose("anthropic-messages", []byte(sampleAnthropic), 1000, 0)
	if !ok {
		t.Fatal("ok=false")
	}
	want := map[string]bool{
		"system": true, "tool_schemas": true, "user": true,
		"reasoning": true, "actions": true, "tool_results": true, "attachments": true,
	}
	got := map[string]bool{}
	for _, s := range comp.Sources {
		got[s.Key] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing expected source %q", k)
		}
	}
	// tool_schemas must carry a builtin (Read) and an MCP (mcp:github) producer.
	for _, s := range comp.Sources {
		if s.Key != "tool_schemas" {
			continue
		}
		prods := map[string]bool{}
		for _, p := range s.Children {
			prods[p.Key] = true
		}
		if !prods["builtin"] || !prods["mcp:github"] {
			t.Errorf("tool_schemas producers = %v, want builtin + mcp:github", prods)
		}
	}
}

func TestComposeUnsupportedWire(t *testing.T) {
	if _, ok := Compose("volc-speech-tts", []byte(`{}`), 100, 0); ok {
		t.Error("expected ok=false for unsupported wire")
	}
}

func TestComposeBadBody(t *testing.T) {
	if _, ok := Compose("anthropic-messages", []byte(`not json`), 100, 0); ok {
		t.Error("expected ok=false for unparseable body")
	}
}
