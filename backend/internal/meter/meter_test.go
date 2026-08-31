package meter

import (
	"bytes"
	"io"
	"mime/multipart"
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
		{"video generation tasks", "/api/plan/v3/contents/generations/tasks", `{"model":"doubao-seedance-2.0"}`, calls.ModalityVideo, "doubao-seedance-2.0"},
		{"unknown path", "/v1/moderations", `{"model":"omni-moderation"}`, calls.ModalityUnknown, "omni-moderation"},
		{"case insensitive", "/V1/Chat/Completions", `{"model":"gpt-4o"}`, calls.ModalityChat, "gpt-4o"},
		{"trailing slash and query", "/v1/chat/completions/?stream=true", `{"model":"gpt-4o"}`, calls.ModalityChat, "gpt-4o"},
		{"non-json body modality only", "/v1/chat/completions", "not json at all", calls.ModalityChat, ""},
		{"empty body", "/v1/embeddings", "", calls.ModalityEmbedding, ""},
		{"json without model", "/v1/chat/completions", `{"messages":[]}`, calls.ModalityChat, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify("POST", tt.path, "application/json", []byte(tt.body))
			if got.Modality != tt.wantMod {
				t.Errorf("Modality = %q, want %q", got.Modality, tt.wantMod)
			}
			if got.Model != tt.wantModl {
				t.Errorf("Model = %q, want %q", got.Model, tt.wantModl)
			}
		})
	}
}

// An OpenAI-style image edit is multipart, not JSON. Routing has to find the
// model in it: without it the request picks its provider by weight alone and
// can land on a vendor that only speaks JSON on this endpoint.
func TestClassifyMultipartImageEdit(t *testing.T) {
	body, contentType := multipartForm(t,
		field{name: "image", filename: "panda.png", value: "\x89PNG\r\n\x1a\nbinary\x00bytes"},
		field{name: "model", value: "gpt-image-2"},
		field{name: "prompt", value: "put it in a snowy forest"},
	)
	got := Classify("POST", "/v1/images/edits", contentType, body)
	if got.Modality != calls.ModalityImage {
		t.Errorf("Modality = %q, want %q", got.Modality, calls.ModalityImage)
	}
	if got.Model != "gpt-image-2" {
		t.Errorf("Model = %q, want gpt-image-2", got.Model)
	}
}

func TestModelFromMultipart(t *testing.T) {
	// A file part named "model" is an image to edit, not a model id.
	fileNamed, ct := multipartForm(t,
		field{name: "model", filename: "model.png", value: "not-a-model-id"},
		field{name: "model", value: "gpt-image-2"},
	)
	if got := ModelFromBody(ct, fileNamed); got != "gpt-image-2" {
		t.Errorf("model = %q, want gpt-image-2 (a file part must not answer)", got)
	}

	// Values are trimmed, and a form without the field is simply empty.
	padded, ct := multipartForm(t, field{name: "model", value: "  gpt-image-2\n"})
	if got := ModelFromBody(ct, padded); got != "gpt-image-2" {
		t.Errorf("model = %q, want it trimmed", got)
	}
	noField, ct := multipartForm(t, field{name: "prompt", value: "hello"})
	if got := ModelFromBody(ct, noField); got != "" {
		t.Errorf("model = %q, want empty", got)
	}
}

// Sniffing never errors and never blocks: every malformed shape yields "".
func TestModelFromBodyDegradesQuietly(t *testing.T) {
	body, ct := multipartForm(t, field{name: "model", value: "gpt-image-2"})
	cases := []struct {
		name        string
		contentType string
		body        []byte
	}{
		{"no boundary", "multipart/form-data", body},
		{"unparseable content type", "multipart/form-data; boundary=", body},
		{"truncated form", ct, body[:len(body)/2]},
		{"empty body", ct, nil},
		{"json content type over a form", "application/json", body},
		{"multipart content type over json", ct, []byte(`{"model":"gpt-4o"}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ModelFromBody(c.contentType, c.body); got != "" {
				t.Errorf("model = %q, want empty", got)
			}
		})
	}
}

// A model field past the scanned part budget is not found — bounded on purpose,
// and the request still routes (by endpoint) and forwards untouched.
func TestModelFromMultipartPartBudget(t *testing.T) {
	fields := make([]field, 0, maxParts+1)
	for i := 0; i < maxParts; i++ {
		fields = append(fields, field{name: "filler", value: "x"})
	}
	fields = append(fields, field{name: "model", value: "gpt-image-2"})
	body, ct := multipartForm(t, fields...)
	if got := ModelFromBody(ct, body); got != "" {
		t.Errorf("model = %q, want empty past the part budget", got)
	}
}

type field struct{ name, filename, value string }

func multipartForm(t *testing.T, fields ...field) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		var (
			part io.Writer
			err  error
		)
		if f.filename != "" {
			part, err = w.CreateFormFile(f.name, f.filename)
		} else {
			part, err = w.CreateFormField(f.name)
		}
		if err != nil {
			t.Fatalf("create part %q: %v", f.name, err)
		}
		if _, err := io.WriteString(part, f.value); err != nil {
			t.Fatalf("write part %q: %v", f.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}
