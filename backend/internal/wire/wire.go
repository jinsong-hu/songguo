// Package wire is the flat registry of wire-protocol entries the gateway can
// meter. A Wire pairs a URL path pattern with a usage extractor and a stream
// scanner; the proxy resolves each request against a service's enabled wires
// and denies paths that match none (unless the service allows unmatched
// passthrough). There is no protocol-family hierarchy at runtime — the "/"
// prefix in wire names ("openai/chat") is naming convention only.
package wire

import (
	"io"
	"sort"
	"strings"

	"github.com/songguo/songguo/internal/calls"
)

// Normalized is the canonical usage view fed to pricing. Wires translate
// vendor-specific usage fields into it; raw fields are preserved separately
// for logging.
type Normalized struct {
	InputTokens       float64
	OutputTokens      float64
	CachedInputTokens float64 // subset of InputTokens billed at the cached rate

	// Non-token quantities for media/tool wires (priced via per_call,
	// per_image, per_second, per_char units).
	Calls   float64
	Images  float64
	Seconds float64
	Chars   float64
}

// Extraction is the outcome of metering one response.
type Extraction struct {
	Raw        map[string]any // as reported by the upstream, logged verbatim
	Norm       Normalized     // canonical view for pricing
	Confidence calls.Confidence
}

// StreamScanner consumes a streaming response as an io.Writer tee. Writes
// must never fail or block; Result is called once after the stream ends.
type StreamScanner interface {
	io.Writer
	Result() Extraction
}

// FirstTokenNotifier is implemented by stream scanners that can identify the
// first meaningful generated output event. The proxy uses it to measure TTFT
// without coupling transport code to vendor-specific SSE shapes.
type FirstTokenNotifier interface {
	SetFirstTokenCallback(func())
}

// Quirks are per-service data flags that parameterize an extractor without
// forking the wire (e.g. {"cache_tokens": "deepseek"}).
type Quirks map[string]string

// Wire is one registry entry: a path pattern plus the logic to meter
// responses that flow through it.
type Wire struct {
	Name     string   // e.g. "openai/chat"
	Suffixes []string // case-insensitive path suffixes, e.g. "/chat/completions"
	Modality calls.Modality
	// Extract meters a buffered non-streaming response body.
	Extract func(body []byte, q Quirks) Extraction
	// NewScanner returns a scanner for a streaming response; nil means the
	// wire never streams.
	NewScanner func(q Quirks) StreamScanner
	// ZeroCost marks management endpoints (model listings) that are metered
	// as free without parsing.
	ZeroCost bool
}

// registry holds all known wires, populated by the per-family files' init().
var registry = map[string]Wire{}

func register(w Wire) {
	registry[w.Name] = w
}

// Get returns the wire registered under name.
func Get(name string) (Wire, bool) {
	w, ok := registry[name]
	return w, ok
}

// Names returns all registered wire names, sorted.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Resolve matches a request path against a service's enabled wires and
// returns the best match. The longest matching suffix wins, so "/chat/completions"
// beats "/completions"; ties break lexicographically by wire name for
// determinism. Unknown names in enabled are ignored. The method argument is
// reserved for future method-sensitive wires.
func Resolve(enabled []string, method, path string) (Wire, bool) {
	p := normalizePath(path)
	var (
		best    Wire
		bestLen = -1
		found   bool
	)
	for _, name := range enabled {
		w, ok := registry[name]
		if !ok {
			continue
		}
		for _, suf := range w.Suffixes {
			if !strings.HasSuffix(p, suf) {
				continue
			}
			if len(suf) > bestLen || (len(suf) == bestLen && w.Name < best.Name) {
				best, bestLen, found = w, len(suf), true
			}
		}
	}
	return best, found
}

// normalizePath lowercases and strips the query string and trailing slashes,
// mirroring the proxy's tolerant path handling.
func normalizePath(path string) string {
	p := strings.ToLower(path)
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return strings.TrimRight(p, "/")
}

// num coerces a JSON-decoded numeric value to float64.
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

// numAt walks nested maps by key path and coerces the leaf to float64.
func numAt(m map[string]any, path ...string) float64 {
	cur := m
	for i, key := range path {
		v, ok := cur[key]
		if !ok {
			return 0
		}
		if i == len(path)-1 {
			return num(v)
		}
		next, ok := v.(map[string]any)
		if !ok {
			return 0
		}
		cur = next
	}
	return 0
}

// perCallExtract meters media-generation endpoints (image/video) that report
// no token usage object: the request itself is the billable unit. Confidence
// is measured (the call completed) and Calls=1 drives per_call pricing.
func perCallExtract(_ []byte, _ Quirks) Extraction {
	return Extraction{Norm: Normalized{Calls: 1}, Confidence: calls.ConfidenceMeasured}
}

// confidenceFor grades an extraction: usage present means measured.
func confidenceFor(raw map[string]any) calls.Confidence {
	if raw == nil {
		return calls.ConfidenceUnknown
	}
	return calls.ConfidenceMeasured
}
