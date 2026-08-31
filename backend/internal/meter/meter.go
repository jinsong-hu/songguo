// Package meter classifies proxied AI requests: it sniffs the modality from
// the URL path and the model from the request body. Usage extraction lives in
// the wire package (each wire owns its response shape); meter survives as the
// request-side classifier, including the fallback modality for
// allow-unmatched passthrough calls.
//
// All of meter is read-only sniffing: every parse failure yields a zero
// value, never an error.
package meter

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"strings"

	"github.com/songguo/songguo/internal/calls"
)

// Result is the outcome of classifying a request.
type Result struct {
	Modality calls.Modality
	Model    string
}

// Classify determines the modality of a call from its URL path (suffix match,
// case-insensitive) and best-effort extracts the model from the request body,
// reading it as JSON or as a multipart form per contentType. Model is empty
// when neither shape yields one.
//
// The method argument is currently unused but is part of the proxy-facing API,
// as future modalities (e.g. MCP) may key off HTTP method.
func Classify(method, path, contentType string, body []byte) Result {
	return Result{
		Modality: modalityFromPath(path),
		Model:    ModelFromBody(contentType, body),
	}
}

// ModelFromBody extracts the request's model from a JSON body or a
// multipart/form-data one. Both shapes are in use on a single endpoint: an
// OpenAI-style image edit is multipart (the image is a file part), while
// Volcengine Ark's equivalent is ordinary JSON. Routing has to see the model in
// either, or a multipart edit picks its provider by weight alone and can land
// on a vendor that only speaks JSON.
func ModelFromBody(contentType string, body []byte) string {
	if isMultipartForm(contentType) {
		return modelFromMultipart(contentType, body)
	}
	return modelFromJSON(body)
}

// modalityFromPath maps an upstream path to a modality using case-insensitive
// suffix matching. Trailing slashes and query strings are ignored.
func modalityFromPath(path string) calls.Modality {
	p := strings.ToLower(path)
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimRight(p, "/")

	switch {
	case strings.HasSuffix(p, "/chat/completions"):
		return calls.ModalityChat
	case strings.HasSuffix(p, "/completions"):
		return calls.ModalityChat
	case strings.HasSuffix(p, "/embeddings"):
		return calls.ModalityEmbedding
	case strings.HasSuffix(p, "/audio/speech"):
		return calls.ModalityTTS
	case strings.HasSuffix(p, "/audio/transcriptions"),
		strings.HasSuffix(p, "/audio/translations"):
		return calls.ModalitySTT
	case strings.HasSuffix(p, "/images/generations"),
		strings.HasSuffix(p, "/images/edits"):
		return calls.ModalityImage
	case strings.HasSuffix(p, "/contents/generations/tasks"):
		return calls.ModalityVideo
	default:
		return calls.ModalityUnknown
	}
}

// modelFromJSON pulls the "model" string from a JSON body, returning "" on any
// failure. It decodes only the model field to avoid materializing large bodies.
func modelFromJSON(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var shallow struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &shallow); err != nil {
		return ""
	}
	return shallow.Model
}

// Bounds on the multipart scan. A form part named "model" holds a model id, so
// anything past maxFieldValue is not one; maxParts stops a pathological form
// from walking forever. Both are sniffing guards, never limits on what is
// forwarded — the body is relayed verbatim whatever these decide.
const (
	maxFieldValue = 256
	maxParts      = 64
)

// isMultipartForm reports whether contentType is a multipart form. Anything
// unparseable is not, which just routes the sniff back to JSON.
func isMultipartForm(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "multipart/form-data"
}

// modelFromMultipart walks a multipart form for its "model" field. File parts
// are skipped by name (a file named "model" is an image to edit, not a model
// id) but still drained, since a part must be consumed before the reader can
// reach the next one — over an in-memory body that is a copy, not I/O.
//
// Read-only sniffing to the last line: a missing boundary, a truncated form or
// a malformed part all yield "", never an error, and the bytes forwarded
// upstream are untouched either way.
func modelFromMultipart(contentType string, body []byte) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	boundary := params["boundary"]
	if boundary == "" || len(body) == 0 {
		return ""
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for i := 0; i < maxParts; i++ {
		part, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() != "model" || part.FileName() != "" {
			_, _ = io.Copy(io.Discard, part)
			part.Close()
			continue
		}
		value, err := io.ReadAll(io.LimitReader(part, maxFieldValue))
		part.Close()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(value))
	}
	return ""
}
