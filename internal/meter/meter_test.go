package meter

import (
	"strings"
	"testing"

	"github.com/songguo/songguo/internal/calls"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		body     string
		wantMod  calls.Modality
		wantModl string
	}{
		{"chat completions", "/v1/chat/completions", `{"model":"gpt-4o"}`, calls.ModalityChat, "gpt-4o"},
		{"legacy completions", "/v1/completions", `{"model":"gpt-3.5-turbo-instruct"}`, calls.ModalityChat, "gpt-3.5-turbo-instruct"},
		{"embeddings", "/v1/embeddings", `{"model":"text-embedding-3-small"}`, calls.ModalityEmbedding, "text-embedding-3-small"},
		{"tts", "/v1/audio/speech", `{"model":"tts-1"}`, calls.ModalityTTS, "tts-1"},
		{"stt transcriptions", "/v1/audio/transcriptions", `{"model":"whisper-1"}`, calls.ModalitySTT, "whisper-1"},
		{"stt translations", "/v1/audio/translations", `{"model":"whisper-1"}`, calls.ModalitySTT, "whisper-1"},
		{"image generations", "/v1/images/generations", `{"model":"dall-e-3"}`, calls.ModalityImage, "dall-e-3"},
		{"image edits", "/v1/images/edits", `{"model":"dall-e-2"}`, calls.ModalityImage, "dall-e-2"},
		{"unknown path", "/v1/moderations", `{"model":"omni-moderation"}`, calls.ModalityUnknown, "omni-moderation"},
		{"case insensitive", "/V1/Chat/Completions", `{"model":"gpt-4o"}`, calls.ModalityChat, "gpt-4o"},
		{"trailing slash and query", "/v1/chat/completions/?stream=true", `{"model":"gpt-4o"}`, calls.ModalityChat, "gpt-4o"},
		{"non-json body modality only", "/v1/chat/completions", "not json at all", calls.ModalityChat, ""},
		{"empty body", "/v1/embeddings", "", calls.ModalityEmbedding, ""},
		{"json without model", "/v1/chat/completions", `{"messages":[]}`, calls.ModalityChat, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify("POST", tt.path, []byte(tt.body))
			if got.Modality != tt.wantMod {
				t.Errorf("Modality = %q, want %q", got.Modality, tt.wantMod)
			}
			if got.Model != tt.wantModl {
				t.Errorf("Model = %q, want %q", got.Model, tt.wantModl)
			}
		})
	}
}

func TestExtractUsage(t *testing.T) {
	chat := `{"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`
	u := ExtractUsage([]byte(chat))
	if u == nil {
		t.Fatal("chat usage nil")
	}
	if got, ok := u["prompt_tokens"].(float64); !ok || got != 10 {
		t.Errorf("prompt_tokens = %v", u["prompt_tokens"])
	}

	emb := `{"data":[],"usage":{"prompt_tokens":5,"total_tokens":5}}`
	if u := ExtractUsage([]byte(emb)); u == nil || u["total_tokens"].(float64) != 5 {
		t.Errorf("embedding usage = %v", u)
	}

	if u := ExtractUsage([]byte(`{"id":"x"}`)); u != nil {
		t.Errorf("missing usage should be nil, got %v", u)
	}
	if u := ExtractUsage([]byte(`{"usage":null}`)); u != nil {
		t.Errorf("null usage should be nil, got %v", u)
	}
	if u := ExtractUsage([]byte("not json")); u != nil {
		t.Errorf("bad json should be nil, got %v", u)
	}
	if u := ExtractUsage(nil); u != nil {
		t.Errorf("nil body should be nil, got %v", u)
	}
}

// realisticStream is a multi-chunk SSE body where only the final data chunk
// carries a non-null usage (as OpenAI does with stream_options).
const realisticStream = "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}],\"usage\":null}\n\n" +
	"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}],\"usage\":null}\n\n" +
	"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\" world\"}}],\"usage\":null}\n\n" +
	"data: {\"id\":\"1\",\"choices\":[{\"delta\":{}, \"finish_reason\":\"stop\"}],\"usage\":null}\n\n" +
	"data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22,\"total_tokens\":33}}\n\n" +
	"data: [DONE]\n\n"

func TestStreamUsageScannerByteByByte(t *testing.T) {
	s := NewStreamUsageScanner()
	for i := 0; i < len(realisticStream); i++ {
		n, err := s.Write([]byte{realisticStream[i]})
		if err != nil || n != 1 {
			t.Fatalf("Write returned (%d, %v)", n, err)
		}
	}
	u := s.Usage()
	if u == nil {
		t.Fatal("usage not recovered from byte-by-byte stream")
	}
	if u["prompt_tokens"].(float64) != 11 || u["completion_tokens"].(float64) != 22 {
		t.Fatalf("usage = %v", u)
	}
}

func TestStreamUsageScannerChunked(t *testing.T) {
	// Split at arbitrary, irregular boundaries.
	s := NewStreamUsageScanner()
	data := []byte(realisticStream)
	for i := 0; i < len(data); {
		end := i + 7
		if end > len(data) {
			end = len(data)
		}
		s.Write(data[i:end])
		i = end
	}
	u := s.Usage()
	if u == nil || u["total_tokens"].(float64) != 33 {
		t.Fatalf("chunked usage = %v", u)
	}
}

func TestStreamUsageScannerDoneOnly(t *testing.T) {
	s := NewStreamUsageScanner()
	s.Write([]byte("data: [DONE]\n\n"))
	if u := s.Usage(); u != nil {
		t.Fatalf("DONE-only stream should yield nil usage, got %v", u)
	}
}

func TestStreamUsageScannerKeepsLatestNonNull(t *testing.T) {
	s := NewStreamUsageScanner()
	s.Write([]byte("data: {\"usage\":{\"prompt_tokens\":1}}\n"))
	s.Write([]byte("data: {\"usage\":null}\n")) // must not clobber the prior non-null
	s.Write([]byte("data: {\"usage\":{\"prompt_tokens\":9}}\n"))
	u := s.Usage()
	if u == nil || u["prompt_tokens"].(float64) != 9 {
		t.Fatalf("latest usage = %v", u)
	}
}

func TestStreamUsageScannerOversizedLine(t *testing.T) {
	s := NewStreamUsageScanner()
	// A single line far larger than the 1 MiB cap must not panic and must be
	// dropped without retaining the whole thing.
	huge := "data: {\"x\":\"" + strings.Repeat("A", 2<<20) + "\"}\n"
	if n, err := s.Write([]byte(huge)); err != nil || n != len(huge) {
		t.Fatalf("oversized Write returned (%d, %v)", n, err)
	}
	if len(s.buf) > maxLineBytes {
		t.Fatalf("buffer not capped: %d bytes", len(s.buf))
	}
	// A valid usage line after the oversized one is still recovered.
	s.Write([]byte("data: {\"usage\":{\"prompt_tokens\":7}}\n"))
	if u := s.Usage(); u == nil || u["prompt_tokens"].(float64) != 7 {
		t.Fatalf("usage after oversized line = %v", u)
	}
}
