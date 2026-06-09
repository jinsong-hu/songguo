package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxRequestBody bounds the JSON request body size for admin writes.
const maxRequestBody = 1 << 20 // 1 MiB

// decodeJSON decodes the (bounded) request body into v, rejecting unknown
// fields. An empty body is treated as an empty object so optional-field PATCH
// bodies are valid.
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	return nil
}

// serverError logs the underlying error (which may reference internals) and
// returns a generic 500 to the client so details never leak.
func (a *api) serverError(w http.ResponseWriter, op string, err error) {
	a.logger.Error("admin api error", "op", op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

// trimRightSlash drops a single trailing slash from a URL base.
func trimRightSlash(s string) string {
	return strings.TrimRight(s, "/")
}

// contextWithTimeout derives a timeout context from the request context.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// drain discards and closes a response body so the connection can be reused.
func drain(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
}
