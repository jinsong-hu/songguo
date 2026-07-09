package compose

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"testing"
)

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
      {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADggGOSHzRgAAAAABJRU5ErkJggg=="}},
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
    {"type": "reasoning", "summary": [{"text": "Need inspect the repo first."}], "encrypted_content": "gAAAA-test"},
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
			var blockTokens, blockCached int64
			for _, b := range comp.Blocks {
				blockTokens += b.Tokens
				blockCached += b.Cached
				if b.Hash == "" || b.Source == "" || b.Type == "" {
					t.Errorf("block missing metadata: %+v", b)
				}
			}
			// INVARIANT 1: per-source tokens partition Total exactly.
			if gotTokens != comp.Total {
				t.Errorf("sum(sources.tokens) = %d, want Total %d", gotTokens, comp.Total)
			}
			if blockTokens != comp.Total {
				t.Errorf("sum(blocks.tokens) = %d, want Total %d", blockTokens, comp.Total)
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
			if blockCached != comp.Cached {
				t.Errorf("sum(blocks.cached) = %d, want Cached %d", blockCached, comp.Cached)
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

func TestComposeAnthropicThinkingSignatureIgnored(t *testing.T) {
	withSignatureOnly := `{
	  "model": "claude-x",
	  "messages": [
	    {"role": "assistant", "content": [
	      {"type": "thinking", "thinking": "", "signature": "EuUGCmTIDxgCKKa5uioxLargeOpaqueSignature"},
	      {"type": "text", "text": "Done."}
	    ]}
	  ]
	}`
	comp, ok := Compose("anthropic-messages", []byte(withSignatureOnly), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	for _, s := range comp.Sources {
		if s.Key == "reasoning" {
			t.Fatalf("signature-only thinking produced reasoning tokens: %+v", s)
		}
	}

	withThinking := `{
	  "model": "claude-x",
	  "messages": [
	    {"role": "assistant", "content": [
	      {"type": "thinking", "thinking": "Need answer directly.", "signature": "EuUGCmTIDxgCKKa5uioxLargeOpaqueSignature"},
	      {"type": "text", "text": "Done."}
	    ]}
	  ]
	}`
	comp, ok = Compose("anthropic-messages", []byte(withThinking), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	var reasoning int64
	for _, s := range comp.Sources {
		if s.Key == "reasoning" {
			reasoning = s.Tokens
		}
	}
	if reasoning <= 0 {
		t.Fatal("thinking text did not produce reasoning tokens")
	}
}

func TestVisualTokensForSizeClaudeTiers(t *testing.T) {
	cases := []struct {
		name     string
		width    int
		height   int
		standard int64
		high     int64
	}{
		{"200-square", 200, 200, 64, 64},
		{"1000-square", 1000, 1000, 1296, 1296},
		{"1092-square", 1092, 1092, 1521, 1521},
		{"hd", 1920, 1080, 1560, 2691},
		{"3mp", 2000, 1500, 1564, 3888},
		{"4k", 3840, 2160, 1560, 4784},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := visualTokensForSize(tc.width, tc.height, standardImageTier); got != tc.standard {
				t.Errorf("standard tokens = %d, want %d", got, tc.standard)
			}
			if got := visualTokensForSize(tc.width, tc.height, highImageTier); got != tc.high {
				t.Errorf("high tokens = %d, want %d", got, tc.high)
			}
		})
	}
}

func TestComposeAnthropicImageUsesVisualTokensNotBase64(t *testing.T) {
	pngBytes := testPNG(t, 200, 200)
	body := fmt.Sprintf(`{
	  "model": "claude-haiku-4.5",
	  "tools": [{"name": "Read", "input_schema": {"type": "object"}}],
	  "messages": [
	    {"role": "assistant", "content": [{"type": "tool_use", "id": "tu_1", "name": "Read", "input": {"path": "shot.png"}}]},
	    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "tu_1", "content": [
	      {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": %q}}
	    ]}]}
	  ]
	}`, base64.StdEncoding.EncodeToString(pngBytes))

	comp, ok := Compose("anthropic-messages", []byte(body), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	var readToolResults int64
	for _, src := range comp.Sources {
		if src.Key != "tool_results" {
			continue
		}
		for _, child := range src.Children {
			if child.Key == "read" {
				readToolResults = child.Tokens
			}
		}
	}
	if readToolResults != 64 {
		t.Fatalf("read tool result tokens = %d, want 64 visual tokens", readToolResults)
	}
}

func TestOpenAIPatchCountExamples(t *testing.T) {
	if got := openAIPatchCount(1024, 1024, 1536); got != 1024 {
		t.Fatalf("1024 square patches = %d, want 1024", got)
	}
	if got := openAIPatchCount(1800, 2400, 1536); got != 1452 {
		t.Fatalf("1800x2400 patches = %d, want 1452", got)
	}
}

func TestComposeOpenAIPatchImageUsesVisualTokensNotBase64(t *testing.T) {
	body := fmt.Sprintf(`{
	  "model": "gpt-5-mini",
	  "messages": [
	    {"role": "user", "content": [
	      {"type": "input_text", "text": "what is this?"},
	      {"type": "image_url", "image_url": {"url": %q, "detail": "high"}}
	    ]}
	  ]
	}`, testPNGDataURL(t, 1024, 1024))

	comp, ok := Compose("openai-chat", []byte(body), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	var attachments int64
	for _, src := range comp.Sources {
		if src.Key == "attachments" {
			attachments = src.Tokens
		}
	}
	// 1024 patches * gpt-5-mini's 1.62 multiplier, rounded up.
	if attachments != 1659 {
		t.Fatalf("attachment tokens = %d, want 1659", attachments)
	}
}

func TestComposeOpenAITileImageDetail(t *testing.T) {
	dataURL := testPNGDataURL(t, 1024, 1024)
	highBody := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":%q,"detail":"high"}}]}]}`, dataURL)
	lowBody := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":%q,"detail":"low"}}]}]}`, dataURL)

	high, ok := Compose("openai-chat", []byte(highBody), 0)
	if !ok {
		t.Fatal("high ok=false")
	}
	low, ok := Compose("openai-chat", []byte(lowBody), 0)
	if !ok {
		t.Fatal("low ok=false")
	}
	if high.Total != 765 {
		t.Fatalf("high total = %d, want 765", high.Total)
	}
	if low.Total != 85 {
		t.Fatalf("low total = %d, want 85", low.Total)
	}
}

func testPNGDataURL(t *testing.T, width, height int) string {
	t.Helper()
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(testPNG(t, width, height))
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var pngBuf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 40, B: 30, A: 255})
		}
	}
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	return pngBuf.Bytes()
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

func TestComposeOpenAIResponsesEncryptedReasoningIgnored(t *testing.T) {
	body := `{
	  "model": "gpt-x",
	  "input": [
	    {"type": "reasoning", "summary": [], "encrypted_content": "gAAAA-test"},
	    {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Done."}]}
	  ]
	}`
	comp, ok := Compose("openai/responses", []byte(body), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	for _, s := range comp.Sources {
		if s.Key == "reasoning" {
			t.Fatalf("encrypted-only reasoning produced reasoning tokens: %+v", s)
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
