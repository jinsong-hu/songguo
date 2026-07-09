// Package bodycodec decodes HTTP bodies according to Content-Encoding.
package bodycodec

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

var ErrUnsupportedEncoding = errors.New("unsupported content encoding")

// Decode returns a decoded copy of body when Content-Encoding contains only
// supported encodings. The boolean reports whether decoding was attempted.
func Decode(body []byte, contentEncoding string) ([]byte, bool, error) {
	if len(body) == 0 {
		return nil, false, nil
	}
	r, ok, err := NewReader(bytes.NewReader(body), contentEncoding)
	if !ok || err != nil {
		return nil, ok, err
	}
	defer r.Close()
	decoded, err := io.ReadAll(r)
	if err != nil {
		return nil, true, err
	}
	return decoded, true, nil
}

// NewReader wraps r in decoders for Content-Encoding. Encodings are decoded in
// reverse order, matching RFC 9110's representation encoding semantics.
func NewReader(r io.Reader, contentEncoding string) (io.ReadCloser, bool, error) {
	encodings, err := parseEncodings(contentEncoding)
	if err != nil {
		return nil, len(encodings) > 0, err
	}
	if len(encodings) == 0 {
		return nil, false, nil
	}

	var closers []io.Closer
	current := r
	for i := len(encodings) - 1; i >= 0; i-- {
		switch encodings[i] {
		case "gzip", "x-gzip":
			zr, err := gzip.NewReader(current)
			if err != nil {
				closeAll(closers)
				return nil, true, err
			}
			closers = append(closers, zr)
			current = zr
		case "zstd":
			zr, err := zstd.NewReader(current)
			if err != nil {
				closeAll(closers)
				return nil, true, err
			}
			closers = append(closers, zstdCloser{zr})
			current = zr
		case "br":
			current = brotli.NewReader(current)
		default:
			closeAll(closers)
			return nil, true, fmt.Errorf("%w: %s", ErrUnsupportedEncoding, encodings[i])
		}
	}
	return decoderReader{Reader: current, closers: closers}, true, nil
}

func parseEncodings(contentEncoding string) ([]string, error) {
	var encodings []string
	for _, part := range strings.Split(contentEncoding, ",") {
		enc := strings.ToLower(strings.TrimSpace(part))
		if enc == "" || enc == "identity" {
			continue
		}
		encodings = append(encodings, enc)
	}
	return encodings, nil
}

type decoderReader struct {
	io.Reader
	closers []io.Closer
}

type zstdCloser struct {
	*zstd.Decoder
}

func (c zstdCloser) Close() error {
	c.Decoder.Close()
	return nil
}

func (r decoderReader) Close() error {
	return closeAll(r.closers)
}

func closeAll(closers []io.Closer) error {
	var err error
	for i := len(closers) - 1; i >= 0; i-- {
		if closeErr := closers[i].Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}
