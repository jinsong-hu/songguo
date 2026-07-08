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

const sampleOpenAIResponses = `{
  "model": "gpt-x",
  "instructions": "You are a coding agent.",
  "tools": [
    {"type": "function", "name": "exec_command", "description": "Run a command", "parameters": {"type": "object"}},
    {"type": "function", "name": "mcp__github__list_prs", "description": "List PRs", "parameters": {"type": "object"}}
  ],
  "input": [
    {"type": "message", "role": "developer", "content": [{"type": "input_text", "text": "Follow sandbox rules."}]},
    {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Inspect this repo."}]},
    {"type": "reasoning", "summary": [], "encrypted_content": "gAAAA-test"},
    {"type": "function_call", "name": "exec_command", "arguments": "{\"cmd\":\"pwd\"}", "call_id": "call_1"},
    {"type": "function_call_output", "call_id": "call_1", "output": "Output:\n/Users/example/project\n"},
    {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "I found the project root."}]}
  ]
}`

func sum(src []Source) (tokens, cached int64) {
	for _, s := range src {
		tokens += s.Tokens
		cached += s.Cached
	}
	return
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func TestComposeInvariants(t *testing.T) {
	cases := []struct {
		name         string
		wire         string
		body         string
		cachedTokens int64
	}{
		{"anthropic", "anthropic-messages", sampleAnthropic, 640},
		{"anthropic-no-cache", "anthropic-messages", sampleAnthropic, 0},
		{"openai", "openai-chat", sampleOpenAI, 41},
		{"openai-responses", "openai/responses", sampleOpenAIResponses, 60},
		{"openai-over-cache", "openai-chat", sampleOpenAI, 100000}, // exceeds total → clamps
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			comp, ok := Compose(tc.wire, []byte(tc.body), tc.cachedTokens)
			if !ok {
				t.Fatalf("Compose returned ok=false")
			}

			// Total is our own local token count: positive and non-anchored.
			if comp.Total <= 0 {
				t.Errorf("Total = %d, want > 0", comp.Total)
			}

			gotTokens, gotCached := sum(comp.Sources)
			// INVARIANT 1: per-source tokens partition Total exactly.
			if gotTokens != comp.Total {
				t.Errorf("sum(sources.tokens) = %d, want Total %d", gotTokens, comp.Total)
			}
			// INVARIANT 2: cached is the official cache-read total front-filled and
			// clamped to Total; per-source cached sums back to it.
			wantCached := min64(tc.cachedTokens, comp.Total)
			if comp.Cached != wantCached {
				t.Errorf("Cached = %d, want min(official, total) = %d", comp.Cached, wantCached)
			}
			if gotCached != comp.Cached {
				t.Errorf("sum(sources.cached) = %d, want Cached %d", gotCached, comp.Cached)
			}

			// Producer tokens never exceed their source's tokens; cached ≤ tokens.
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

// TestComposeDeterministic is the whole point of self-counting: an unchanged
// body always yields the same counts, so a caller tuning prompt-cache reuse
// sees a stable number for a fixed prefix.
func TestComposeDeterministic(t *testing.T) {
	a, ok1 := Compose("anthropic-messages", []byte(sampleAnthropic), 300)
	b, ok2 := Compose("anthropic-messages", []byte(sampleAnthropic), 300)
	if !ok1 || !ok2 {
		t.Fatal("ok=false")
	}
	if a.Total != b.Total || a.Cached != b.Cached {
		t.Fatalf("non-deterministic totals: %+v vs %+v", a, b)
	}
	if len(a.Sources) != len(b.Sources) {
		t.Fatalf("source count differs: %d vs %d", len(a.Sources), len(b.Sources))
	}
	for i := range a.Sources {
		if a.Sources[i].Key != b.Sources[i].Key || a.Sources[i].Tokens != b.Sources[i].Tokens {
			t.Errorf("source %d differs: %+v vs %+v", i, a.Sources[i], b.Sources[i])
		}
	}
}

func TestComposeAnthropicSources(t *testing.T) {
	comp, ok := Compose("anthropic-messages", []byte(sampleAnthropic), 0)
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

func TestComposeOpenAIResponsesSources(t *testing.T) {
	comp, ok := Compose("openai/responses", []byte(sampleOpenAIResponses), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	want := map[string]bool{
		"system": true, "tool_schemas": true, "user": true,
		"reasoning": true, "actions": true, "tool_results": true,
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

	for _, s := range comp.Sources {
		switch s.Key {
		case "tool_schemas":
			prods := map[string]bool{}
			for _, p := range s.Children {
				prods[p.Key] = true
			}
			if !prods["builtin"] || !prods["mcp:github"] {
				t.Errorf("tool_schemas producers = %v, want builtin + mcp:github", prods)
			}
		case "tool_results":
			if len(s.Children) != 1 || s.Children[0].Key != "exec_command" {
				t.Errorf("tool_results producers = %+v, want exec_command", s.Children)
			}
		}
	}
}

func TestComposeUnsupportedWire(t *testing.T) {
	if _, ok := Compose("volc-speech-tts", []byte(`{}`), 0); ok {
		t.Error("expected ok=false for unsupported wire")
	}
}

func TestComposeBadBody(t *testing.T) {
	if _, ok := Compose("anthropic-messages", []byte(`not json`), 0); ok {
		t.Error("expected ok=false for unparseable body")
	}
}
