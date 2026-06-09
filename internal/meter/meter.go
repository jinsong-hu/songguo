// Package meter extracts token usage from proxied AI responses.
//
// All of meter is read-only sniffing: it observes request paths/bodies and
// response bodies to classify a call's modality and recover its usage. It never
// blocks traffic — every parse failure yields a zero value, never an error.
package meter

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/songguo/songguo/internal/calls"
)

// Result is the outcome of classifying a request.
type Result struct {
	Modality calls.Modality
	Model    string
}

// Classify determines the modality of a call from its URL path (suffix match,
// case-insensitive) and best-effort extracts the model from the JSON request
// body. Model is empty if the body is not JSON or has no "model" field.
//
// The method argument is currently unused but is part of the proxy-facing API,
// as future modalities (e.g. MCP) may key off HTTP method.
func Classify(method, path string, body []byte) Result {
	return Result{
		Modality: modalityFromPath(path),
		Model:    modelFromBody(body),
	}
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
	default:
		return calls.ModalityUnknown
	}
}

// modelFromBody pulls the "model" string from a JSON body, returning "" on any
// failure. It decodes only the model field to avoid materializing large bodies.
func modelFromBody(body []byte) string {
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

// ExtractUsage parses a non-streaming JSON response body and returns its
// "usage" object as a map, or nil if it is absent, null, or unparseable.
func ExtractUsage(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var env struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	return env.Usage
}

// maxLineBytes caps the partial-line buffer so a single oversized SSE line
// cannot grow memory without bound. A line longer than this is dropped.
const maxLineBytes = 1 << 20 // 1 MiB

// StreamUsageScanner is an io.Writer the proxy tees a streaming (SSE) response
// into. It reassembles "data:" lines across arbitrary Write boundaries, parses
// each chunk's JSON, and remembers the latest non-null "usage" object seen.
//
// It never errors and always reports consuming every byte, so teeing into it
// can never disrupt the proxied stream.
type StreamUsageScanner struct {
	buf   []byte         // partial line carried across writes
	usage map[string]any // latest non-null usage seen
	// overflow indicates the current line exceeded maxLineBytes and the rest of
	// it (up to the next newline) must be discarded.
	overflow bool
}

// NewStreamUsageScanner returns a ready scanner.
func NewStreamUsageScanner() *StreamUsageScanner {
	return &StreamUsageScanner{}
}

// Write consumes all of p, parsing any complete lines it completes. It always
// returns (len(p), nil).
func (s *StreamUsageScanner) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			// No newline: the rest is a partial line to carry forward.
			s.appendPartial(p)
			break
		}
		line := p[:i]
		p = p[i+1:]

		if s.overflow {
			// We were discarding an oversized line; this newline ends it.
			s.overflow = false
			s.buf = s.buf[:0]
			continue
		}
		if len(s.buf) > 0 {
			s.appendPartial(line)
			if s.overflow {
				continue
			}
			s.processLine(s.buf)
			s.buf = s.buf[:0]
		} else {
			s.processLine(line)
		}
	}
	return n, nil
}

// appendPartial accumulates bytes of an in-progress line, enforcing the size
// cap. On overflow it sets the overflow flag and clears the buffer; the
// remaining bytes of the line will be discarded until the next newline.
func (s *StreamUsageScanner) appendPartial(b []byte) {
	if s.overflow {
		return
	}
	if len(s.buf)+len(b) > maxLineBytes {
		s.overflow = true
		s.buf = s.buf[:0]
		return
	}
	s.buf = append(s.buf, b...)
}

// processLine inspects one complete SSE line and records its usage if present.
func (s *StreamUsageScanner) processLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var env struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	if env.Usage != nil {
		s.usage = env.Usage
	}
}

// Usage returns the latest non-null usage object seen, or nil.
func (s *StreamUsageScanner) Usage() map[string]any {
	return s.usage
}
