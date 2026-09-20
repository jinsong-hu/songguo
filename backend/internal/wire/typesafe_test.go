package wire

import (
	"testing"

	"github.com/songguo/songguo/internal/calls"
)

// TestSystemOneResolve pins the suffix match and, more importantly, that adding
// it stole nothing: /systemone shares no tail with any registered suffix, so the
// wires it is co-enabled with must still win their own paths.
func TestSystemOneResolve(t *testing.T) {
	enabled := []string{"typesafe/systemone", "openai/chat", "openai/completions", "openai/models"}
	cases := map[string]string{
		"/v1/systemone":        "typesafe/systemone",
		"/v1/chat/completions": "openai/chat",
		"/v1/completions":      "openai/completions",
		"/v1/models":           "openai/models",
		"/systemone":           "typesafe/systemone",
		"/v1/systemone/":       "typesafe/systemone",
		"/v1/systemone?x=1":    "typesafe/systemone",
		"/v1/SystemOne":        "typesafe/systemone",
	}
	for path, want := range cases {
		w, ok := Resolve(enabled, "POST", path)
		if !ok || w.Name != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q, true", path, w.Name, ok, want)
		}
	}
}

// TestSystemOneNeverStreams guards the nil scanner. The System One API returns
// one JSON document — there is no streaming mode to scan, and registering a
// scanner would invent a protocol the vendor does not speak.
func TestSystemOneNeverStreams(t *testing.T) {
	w, ok := Get("typesafe/systemone")
	if !ok {
		t.Fatal("typesafe/systemone is not registered")
	}
	if w.NewScanner != nil {
		t.Error("typesafe/systemone declares a stream scanner; the wire never streams")
	}
	if w.ZeroCost {
		t.Error("typesafe/systemone is billed per token, not zero-cost")
	}
}

// TestSystemOneExtract meters a real response body from the TypeSafe docs. The
// zero assertions are the point: TypeSafe reports two token counts and nothing
// else, so every cache and reasoning axis must stay untouched rather than pick
// up a stray number from a sibling normalizer.
func TestSystemOneExtract(t *testing.T) {
	body := []byte(`{
		"model": "jev-1.13.0",
		"answers": {
			"is_urgent": {"type": "noul", "value": true, "probability": 0.94, "confidence": 0.88}
		},
		"usage": {"input_tokens": 296, "output_tokens": 20}
	}`)
	got := systemOneExtract(body, nil)
	if got.Norm.InputTokens != 296 {
		t.Errorf("InputTokens = %v, want 296", got.Norm.InputTokens)
	}
	if got.Norm.OutputTokens != 20 {
		t.Errorf("OutputTokens = %v, want 20", got.Norm.OutputTokens)
	}
	if got.Norm.CachedInputTokens != 0 || got.Norm.CacheCreationTokens != 0 || got.Norm.ThinkingTokens != 0 {
		t.Errorf("cache/thinking axes = %+v, want all zero — TypeSafe publishes none", got.Norm)
	}
	if got.Confidence != calls.ConfidenceMeasured {
		t.Errorf("Confidence = %q, want measured", got.Confidence)
	}
	if got.Raw == nil {
		t.Error("Raw usage was dropped; it is logged verbatim beside the canonical view")
	}
}

// TestSystemOneExtractWithoutUsage covers the rule every normalizer follows:
// parsing never blocks traffic, so a body with no usage meters zero and says so
// rather than claiming a measurement it does not have.
func TestSystemOneExtractWithoutUsage(t *testing.T) {
	for name, body := range map[string]string{
		"no usage key": `{"model":"jev-1.13.0","answers":{}}`,
		"not json":     `<html>502 Bad Gateway</html>`,
		"empty":        ``,
	} {
		got := systemOneExtract([]byte(body), nil)
		if got.Confidence != calls.ConfidenceUnknown {
			t.Errorf("%s: Confidence = %q, want unknown", name, got.Confidence)
		}
		if got.Norm.InputTokens != 0 || got.Norm.OutputTokens != 0 {
			t.Errorf("%s: tokens = %+v, want zero", name, got.Norm)
		}
	}
}
