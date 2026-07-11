package wire

import "bytes"

// maxLineBytes caps the partial-line buffer so a single oversized SSE line
// cannot grow memory without bound. A line longer than this is dropped.
const maxLineBytes = 1 << 20 // 1 MiB

// lineScanner is the shared SSE plumbing: an io.Writer that reassembles lines
// across arbitrary Write boundaries and hands each complete line to onLine.
// It never errors and always reports consuming every byte, so teeing into it
// can never disrupt the proxied stream. Per-wire scanners embed it and parse
// lines in their onLine callback.
type lineScanner struct {
	buf            []byte // partial line carried across writes
	onLine         func(line []byte)
	overflow       bool // current line exceeded maxLineBytes; discard until newline
	firstToken     func()
	firstTokenSeen bool
}

// SetFirstTokenCallback registers a callback invoked once when a wire-specific
// scanner identifies its first generated output delta.
func (s *lineScanner) SetFirstTokenCallback(fn func()) {
	s.firstToken = fn
}

func (s *lineScanner) markFirstToken() {
	if s.firstTokenSeen {
		return
	}
	s.firstTokenSeen = true
	if s.firstToken != nil {
		s.firstToken()
	}
}

// Write consumes all of p, dispatching any complete lines. It always returns
// (len(p), nil).
func (s *lineScanner) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
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
			s.onLine(s.buf)
			s.buf = s.buf[:0]
		} else {
			s.onLine(line)
		}
	}
	return n, nil
}

// appendPartial accumulates bytes of an in-progress line, enforcing the size
// cap. On overflow the rest of the line is discarded until the next newline.
func (s *lineScanner) appendPartial(b []byte) {
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

// ssePayload extracts the JSON payload from an SSE "data:" line, returning
// ok=false for non-data lines, blank payloads, and the [DONE] sentinel.
func ssePayload(line []byte) ([]byte, bool) {
	line = bytes.TrimRight(line, "\r")
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, false
	}
	return payload, true
}

func nonEmptyJSONValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	default:
		return true
	}
}
