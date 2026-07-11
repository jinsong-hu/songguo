package wire

import (
	"testing"

	"github.com/songguo/songguo/internal/calls"
)

func TestResolveLongestSuffixWins(t *testing.T) {
	enabled := []string{"openai/chat", "openai/completions", "openai/embeddings", "openai/models"}

	cases := []struct {
		path string
		want string
	}{
		{"/chat/completions", "openai/chat"},
		{"/v1/chat/completions", "openai/chat"},
		{"/completions", "openai/completions"},
		{"/beta/completions", "openai/completions"},
		{"/embeddings?x=1", "openai/embeddings"},
		{"/models/", "openai/models"},
		{"/CHAT/COMPLETIONS", "openai/chat"},
	}
	for _, c := range cases {
		w, ok := Resolve(enabled, "POST", c.path)
		if !ok {
			t.Fatalf("Resolve(%q): no match", c.path)
		}
		if w.Name != c.want {
			t.Errorf("Resolve(%q) = %q, want %q", c.path, w.Name, c.want)
		}
	}
}

// count_tokens shares the /messages stem, so its longer suffix must win and the
// two must never resolve to each other's wire when both are enabled.
func TestResolveCountTokensVsMessages(t *testing.T) {
	enabled := []string{"anthropic/messages", "anthropic/count_tokens"}
	cases := []struct {
		path string
		want string
	}{
		{"/v1/messages", "anthropic/messages"},
		{"/v1/messages/", "anthropic/messages"},
		{"/v1/messages/count_tokens", "anthropic/count_tokens"},
		{"/v1/messages/count_tokens?beta=1", "anthropic/count_tokens"},
	}
	for _, c := range cases {
		w, ok := Resolve(enabled, "POST", c.path)
		if !ok {
			t.Fatalf("Resolve(%q): no match", c.path)
		}
		if w.Name != c.want {
			t.Errorf("Resolve(%q) = %q, want %q", c.path, w.Name, c.want)
		}
	}
	// count_tokens is billed free.
	if w, _ := Get("anthropic/count_tokens"); !w.ZeroCost {
		t.Error("anthropic/count_tokens must be zero-cost")
	}
}

func TestResolveNoMatch(t *testing.T) {
	if _, ok := Resolve([]string{"openai/chat"}, "POST", "/v1/rerank"); ok {
		t.Error("expected no match for /v1/rerank")
	}
	if _, ok := Resolve([]string{"bogus/wire"}, "POST", "/chat/completions"); ok {
		t.Error("unknown wire names must be ignored")
	}
	if _, ok := Resolve(nil, "POST", "/chat/completions"); ok {
		t.Error("empty allowlist must match nothing")
	}
}

func TestOpenAIExtract(t *testing.T) {
	body := []byte(`{"id":"x","usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40}}}`)
	got := openAIExtract(body, nil)
	if got.Confidence != calls.ConfidenceMeasured {
		t.Fatalf("confidence = %q", got.Confidence)
	}
	want := Normalized{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 40}
	if got.Norm != want {
		t.Errorf("norm = %+v, want %+v", got.Norm, want)
	}
	if got.Raw["prompt_tokens"] == nil {
		t.Error("raw usage not preserved")
	}
}

func TestOpenAIExtractDeepSeekQuirk(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":60,"prompt_cache_miss_tokens":40}}`)
	got := openAIExtract(body, Quirks{"cache_tokens": "deepseek"})
	want := Normalized{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 60}
	if got.Norm != want {
		t.Errorf("norm = %+v, want %+v", got.Norm, want)
	}
}

func TestOpenAIExtractMiniMaxQuirk(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":50,"completion_tokens":5,"cached_tokens":30}}`)
	got := openAIExtract(body, Quirks{"cache_tokens": "minimax"})
	want := Normalized{InputTokens: 50, OutputTokens: 5, CachedInputTokens: 30}
	if got.Norm != want {
		t.Errorf("norm = %+v, want %+v", got.Norm, want)
	}
}

func TestOpenAIExtractEmbeddings(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":8,"total_tokens":8}}`)
	got := openAIExtract(body, nil)
	want := Normalized{InputTokens: 8}
	if got.Norm != want {
		t.Errorf("norm = %+v, want %+v", got.Norm, want)
	}
}

func TestOpenAIExtractNoUsage(t *testing.T) {
	for _, body := range [][]byte{nil, []byte(`{}`), []byte(`not json`)} {
		got := openAIExtract(body, nil)
		if got.Confidence != calls.ConfidenceUnknown {
			t.Errorf("body %q: confidence = %q, want unknown", body, got.Confidence)
		}
	}
}

func TestOpenAIScannerFinalChunkUsage(t *testing.T) {
	s := newOpenAIScanner(nil)
	firstTokens := 0
	s.(FirstTokenNotifier).SetFirstTokenCallback(func() { firstTokens++ })
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	// Byte-by-byte to exercise line reassembly across writes.
	for i := 0; i < len(stream); i++ {
		if _, err := s.Write([]byte{stream[i]}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	got := s.Result()
	want := Normalized{InputTokens: 11, OutputTokens: 7}
	if got.Norm != want {
		t.Errorf("norm = %+v, want %+v", got.Norm, want)
	}
	if got.Confidence != calls.ConfidenceMeasured {
		t.Errorf("confidence = %q", got.Confidence)
	}
	if firstTokens != 1 {
		t.Errorf("first-token callbacks = %d, want 1", firstTokens)
	}
}

func TestOpenAIScannerNoUsage(t *testing.T) {
	s := newOpenAIScanner(nil)
	s.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	if got := s.Result(); got.Confidence != calls.ConfidenceUnknown {
		t.Errorf("confidence = %q, want unknown", got.Confidence)
	}
}

func TestAnthropicExtract(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":10,"output_tokens":25,"cache_read_input_tokens":90,"cache_creation_input_tokens":5}}`)
	got := anthropicExtract(body, nil)
	want := Normalized{InputTokens: 105, OutputTokens: 25, CachedInputTokens: 90}
	if got.Norm != want {
		t.Errorf("norm = %+v, want %+v", got.Norm, want)
	}
}

func TestAnthropicScannerMergesStartAndDelta(t *testing.T) {
	s := newAnthropicScanner(nil)
	firstTokens := 0
	s.(FirstTokenNotifier).SetFirstTokenCallback(func() { firstTokens++ })
	stream := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":120,\"cache_read_input_tokens\":30,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":42}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	s.Write([]byte(stream))
	got := s.Result()
	// input from message_start must survive the message_delta merge.
	want := Normalized{InputTokens: 150, OutputTokens: 42, CachedInputTokens: 30}
	if got.Norm != want {
		t.Errorf("norm = %+v, want %+v", got.Norm, want)
	}
	if got.Confidence != calls.ConfidenceMeasured {
		t.Errorf("confidence = %q", got.Confidence)
	}
	if firstTokens != 1 {
		t.Errorf("first-token callbacks = %d, want 1", firstTokens)
	}
}

func TestResponsesExtract(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":200,"output_tokens":30,"input_tokens_details":{"cached_tokens":150}}}`)
	got := responsesExtract(body, nil)
	want := Normalized{InputTokens: 200, OutputTokens: 30, CachedInputTokens: 150}
	if got.Norm != want {
		t.Errorf("norm = %+v, want %+v", got.Norm, want)
	}
}

func TestResponsesScannerCompletedEvent(t *testing.T) {
	s := newResponsesScanner(nil)
	firstTokens := 0
	s.(FirstTokenNotifier).SetFirstTokenCallback(func() { firstTokens++ })
	stream := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"usage\":{\"input_tokens\":77,\"output_tokens\":9,\"input_tokens_details\":{\"cached_tokens\":50}}}}\n\n"
	s.Write([]byte(stream))
	got := s.Result()
	want := Normalized{InputTokens: 77, OutputTokens: 9, CachedInputTokens: 50}
	if got.Norm != want {
		t.Errorf("norm = %+v, want %+v", got.Norm, want)
	}
	if firstTokens != 1 {
		t.Errorf("first-token callbacks = %d, want 1", firstTokens)
	}
}

func TestZeroCostWire(t *testing.T) {
	w, ok := Get("openai/models")
	if !ok || !w.ZeroCost {
		t.Fatal("openai/models must be registered and zero-cost")
	}
	got := w.Extract(nil, nil)
	if got.Confidence != calls.ConfidenceMeasured {
		t.Errorf("confidence = %q, want measured", got.Confidence)
	}
}

func TestScannerOversizedLineDropped(t *testing.T) {
	s := newOpenAIScanner(nil)
	huge := make([]byte, maxLineBytes+10)
	for i := range huge {
		huge[i] = 'a'
	}
	s.Write(append([]byte("data: "), huge...))
	s.Write([]byte("\n"))
	s.Write([]byte("data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n"))
	got := s.Result()
	want := Normalized{InputTokens: 3, OutputTokens: 1}
	if got.Norm != want {
		t.Errorf("norm after oversized line = %+v, want %+v", got.Norm, want)
	}
}
