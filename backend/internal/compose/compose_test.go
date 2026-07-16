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

// childTokens returns the token count of one producer child, or 0.
func childTokens(comp Composition, srcKey, prodKey string) int64 {
	for _, s := range comp.Sources {
		if s.Key != srcKey {
			continue
		}
		for _, p := range s.Children {
			if p.Key == prodKey {
				return p.Tokens
			}
		}
	}
	return 0
}

func TestComposeAnthropicSources(t *testing.T) {
	comp, ok := Compose("anthropic-messages", []byte(sampleAnthropic), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	want := map[string]bool{
		"system": true, "tool_schemas": true, "user": true,
		"assistant": true, "tool_calls": true, "tool_results": true,
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
	// Producers are the verbatim tool names — no builtin lump, no mcp: split.
	if childTokens(comp, "tool_schemas", "Read") <= 0 {
		t.Error("tool_schemas missing verbatim producer Read")
	}
	if childTokens(comp, "tool_schemas", "mcp__github__list_prs") <= 0 {
		t.Error("tool_schemas missing verbatim producer mcp__github__list_prs")
	}
	if childTokens(comp, "tool_calls", "Read") <= 0 {
		t.Error("tool_calls missing verbatim producer Read")
	}
	if childTokens(comp, "tool_results", "Read") <= 0 {
		t.Error("tool_results missing verbatim producer Read")
	}
	// Thinking is a child of assistant; the image is a child of user.
	if childTokens(comp, "assistant", "reasoning") <= 0 {
		t.Error("assistant missing reasoning child")
	}
	if childTokens(comp, "assistant", "text") <= 0 {
		t.Error("assistant missing text child")
	}
	if childTokens(comp, "user", "attachments") <= 0 {
		t.Error("user missing attachments child")
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
	if got := childTokens(comp, "assistant", "reasoning"); got != 0 {
		t.Fatalf("signature-only thinking produced reasoning tokens: %d", got)
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
	if childTokens(comp, "assistant", "reasoning") <= 0 {
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
	if got := childTokens(comp, "tool_results", "Read"); got != 64 {
		t.Fatalf("Read tool result tokens = %d, want 64 visual tokens", got)
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
	// 1024 patches * gpt-5-mini's 1.62 multiplier, rounded up. The image is a
	// child of the carrying user turn, not a separate source.
	if got := childTokens(comp, "user", "attachments"); got != 1659 {
		t.Fatalf("user attachment tokens = %d, want 1659", got)
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
		"assistant": true, "tool_calls": true, "tool_results": true,
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

	if childTokens(comp, "tool_schemas", "exec_command") <= 0 {
		t.Error("tool_schemas missing verbatim producer exec_command")
	}
	if childTokens(comp, "tool_schemas", "mcp__github__list_prs") <= 0 {
		t.Error("tool_schemas missing verbatim producer mcp__github__list_prs")
	}
	if childTokens(comp, "tool_calls", "exec_command") <= 0 {
		t.Error("tool_calls missing producer exec_command")
	}
	if childTokens(comp, "tool_results", "exec_command") <= 0 {
		t.Error("tool_results missing producer exec_command")
	}
	if childTokens(comp, "assistant", "reasoning") <= 0 {
		t.Error("assistant missing reasoning child")
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
	if got := childTokens(comp, "assistant", "reasoning"); got != 0 {
		t.Fatalf("encrypted-only reasoning produced reasoning tokens: %d", got)
	}
}

// TestComposeOpenAIResponsesCustomToolAttribution is the fix for the dominant
// 'unknown 51%' bucket: custom_tool_call (and other non-function call items)
// register in the call-id map, so their outputs attribute to the verbatim
// tool name.
func TestComposeOpenAIResponsesCustomToolAttribution(t *testing.T) {
	body := `{
	  "model": "gpt-x",
	  "input": [
	    {"type": "custom_tool_call", "name": "exec", "input": "ls", "call_id": "ctc_1"},
	    {"type": "custom_tool_call_output", "call_id": "ctc_1", "output": "main.go README.md"},
	    {"type": "local_shell_call", "call_id": "lsc_1", "action": {"command": ["pwd"]}},
	    {"type": "local_shell_call_output", "call_id": "lsc_1", "output": "/repo"}
	  ]
	}`
	comp, ok := Compose("openai/responses", []byte(body), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	if childTokens(comp, "tool_results", "exec") <= 0 {
		t.Error("custom_tool_call_output not attributed to exec")
	}
	// A name-less call attributes as its type — the identifier the request has.
	if childTokens(comp, "tool_results", "local_shell_call") <= 0 {
		t.Error("local_shell_call_output not attributed to local_shell_call")
	}
	if childTokens(comp, "tool_results", "unknown") != 0 {
		t.Error("attributable outputs still landed in unknown")
	}
}

// additional_tools items carry tool schemas; they weigh as tool_schemas per
// verbatim tool name, not as an opaque assistant action.
func TestComposeOpenAIResponsesAdditionalTools(t *testing.T) {
	body := `{
	  "model": "gpt-x",
	  "input": [
	    {"type": "additional_tools", "role": "developer", "tools": [
	      {"name": "exec", "description": "Run a command", "parameters": {"type": "object"}},
	      {"name": "wait", "description": "Wait", "parameters": {"type": "object"}}
	    ]},
	    {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]}
	  ]
	}`
	comp, ok := Compose("openai/responses", []byte(body), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	if childTokens(comp, "tool_schemas", "exec") <= 0 || childTokens(comp, "tool_schemas", "wait") <= 0 {
		t.Errorf("additional_tools not attributed as tool_schemas: %+v", comp.Sources)
	}
}

// Typeless shorthand message items ({role, content} with no type) route by
// role, and images weigh visually even for models without a published spec
// (fail-open default tile spec) — never as base64 text.
func TestComposeOpenAIResponsesTypelessMessageAndUnknownModelImage(t *testing.T) {
	body := fmt.Sprintf(`{
	  "model": "gpt-5.6-sol",
	  "input": [
	    {"role": "user", "content": [
	      {"type": "input_text", "text": "what is this?"},
	      {"type": "input_image", "image_url": %q}
	    ]}
	  ]
	}`, testPNGDataURL(t, 1024, 1024))
	comp, ok := Compose("openai/responses", []byte(body), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	got := childTokens(comp, "user", "attachments")
	if got <= 0 {
		t.Fatal("unknown-model image was dropped from the taxonomy")
	}
	// Far below what base64-as-text would produce (a 1024px PNG is >1KB of
	// base64 per few tokens); the default tile spec caps this in the hundreds.
	if got > 10000 {
		t.Fatalf("image weight %d looks like tokenized base64, not a visual estimate", got)
	}
}

// image_generation_call items weigh their base64 result visually, never as
// base64 text.
func TestComposeOpenAIResponsesImageGenerationCall(t *testing.T) {
	png := base64.StdEncoding.EncodeToString(testPNG(t, 200, 200))
	body := fmt.Sprintf(`{
	  "model": "gpt-image-2",
	  "input": [
	    {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "draw a cat"}]},
	    {"type": "image_generation_call", "id": "ig_1", "result": %q}
	  ]
	}`, png)
	comp, ok := Compose("openai/responses", []byte(body), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	got := childTokens(comp, "tool_results", "image_generation_call")
	if got <= 0 {
		t.Fatal("image_generation_call result not weighed")
	}
	if got > 10000 {
		t.Fatalf("result weight %d looks like tokenized base64, not a visual estimate", got)
	}
}

// Developer-role and legacy function-role chat messages must not vanish.
func TestComposeOpenAIChatDeveloperAndFunctionRoles(t *testing.T) {
	body := `{
	  "model": "gpt-x",
	  "messages": [
	    {"role": "developer", "content": "Follow the house style."},
	    {"role": "user", "content": "hi"},
	    {"role": "assistant", "content": "calling", "tool_calls": [
	      {"id": "c1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"SF\"}"}}
	    ]},
	    {"role": "function", "name": "get_weather", "content": "sunny"}
	  ]
	}`
	comp, ok := Compose("openai-chat", []byte(body), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	var system int64
	for _, s := range comp.Sources {
		if s.Key == "system" {
			system = s.Tokens
		}
	}
	if system <= 0 {
		t.Error("developer message did not weigh as system")
	}
	if childTokens(comp, "tool_results", "get_weather") <= 0 {
		t.Error("legacy function-role result not attributed via its name field")
	}
	if childTokens(comp, "tool_calls", "get_weather") <= 0 {
		t.Error("assistant tool_calls not attributed to tool_calls bucket")
	}
}

// Anthropic base64 PDF documents weigh per page, never as base64 text.
func TestComposeAnthropicDocumentWeighsPagesNotBase64(t *testing.T) {
	pdf := "%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n2 0 obj\n<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>\nendobj\n3 0 obj\n<< /Type /Page /Parent 2 0 R >>\nendobj\n4 0 obj\n<< /Type /Page /Parent 2 0 R >>\nendobj\n%%EOF"
	body := fmt.Sprintf(`{
	  "model": "claude-x",
	  "messages": [
	    {"role": "user", "content": [
	      {"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": %q}},
	      {"type": "text", "text": "summarize"}
	    ]}
	  ]
	}`, base64.StdEncoding.EncodeToString([]byte(pdf)))
	comp, ok := Compose("anthropic-messages", []byte(body), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	if got := childTokens(comp, "user", "attachments"); got != 2*2250 {
		t.Fatalf("document tokens = %d, want 2 pages x 2250", got)
	}
}

// Array-form system prompts weigh per block so the drill-down can itemize a
// multi-part prompt; text blocks weigh their text, without JSON framing.
func TestComposeAnthropicSystemArraySplitsBlocks(t *testing.T) {
	body := `{
	  "model": "claude-x",
	  "system": [
	    {"type": "text", "text": "Base prompt.", "cache_control": {"type": "ephemeral"}},
	    {"type": "text", "text": "Project instructions."}
	  ],
	  "messages": [{"role": "user", "content": "hi"}]
	}`
	comp, ok := Compose("anthropic-messages", []byte(body), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	var systemBlocks int
	for _, b := range comp.Blocks {
		if b.Source == "system" {
			systemBlocks++
		}
	}
	if systemBlocks != 2 {
		t.Fatalf("system blocks = %d, want 2", systemBlocks)
	}
}

// Unrecognized block types land in the explicit "other" bucket, not a guessed
// role bucket.
func TestComposeAnthropicUnknownTypeGoesToOther(t *testing.T) {
	body := `{
	  "model": "claude-x",
	  "messages": [
	    {"role": "user", "content": [
	      {"type": "container_upload", "file_id": "file_abc"},
	      {"type": "text", "text": "use the file"}
	    ]}
	  ]
	}`
	comp, ok := Compose("anthropic-messages", []byte(body), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	var other int64
	for _, s := range comp.Sources {
		if s.Key == "other" {
			other = s.Tokens
		}
	}
	if other <= 0 {
		t.Fatal("unknown block type did not land in other")
	}
}

// Chat tool messages fall back to their legacy name field for attribution
// when the tool_call_id lookup misses.
func TestComposeOpenAIChatToolNameFallback(t *testing.T) {
	body := `{
	  "model": "gpt-x",
	  "messages": [
	    {"role": "user", "content": "hi"},
	    {"role": "tool", "tool_call_id": "missing", "name": "search_docs", "content": "found 3 results"}
	  ]
	}`
	comp, ok := Compose("openai-chat", []byte(body), 0)
	if !ok {
		t.Fatal("ok=false")
	}
	if childTokens(comp, "tool_results", "search_docs") <= 0 {
		t.Error("tool message name field not used as attribution fallback")
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
