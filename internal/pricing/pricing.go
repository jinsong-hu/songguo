// Package pricing computes true cost from vendor price tables and token counts.
package pricing

import (
	"encoding/json"

	"github.com/songguo/songguo/internal/config"
)

// Cost computes the USD cost for a single call from a vendor price entry and an
// extracted usage map. It is deliberately defensive: missing or non-numeric
// keys contribute zero, and an unknown or empty Unit yields zero. It never
// panics.
//
// Token counts are read under both OpenAI naming (prompt_tokens /
// completion_tokens) and the alternate naming (input_tokens / output_tokens),
// taking whichever is present. Counts for non-token units are read from calls,
// images, seconds, and characters (also "chars").
func Cost(p config.Price, usage map[string]any) float64 {
	switch p.Unit {
	case "per_1m_tokens":
		in, out := tokens(usage)
		return p.Input*in/1e6 + p.Output*out/1e6
	case "per_1k_tokens":
		in, out := tokens(usage)
		return p.Input*in/1e3 + p.Output*out/1e3
	case "per_token":
		in, out := tokens(usage)
		return p.Input*in + p.Output*out
	case "per_call":
		calls, ok := count(usage, "calls")
		if !ok {
			calls = 1
		}
		return p.Input * calls
	case "per_image":
		images, _ := count(usage, "images")
		return p.Input * images
	case "per_second":
		seconds, _ := count(usage, "seconds")
		return p.Input * seconds
	case "per_char":
		chars, _ := count(usage, "characters", "chars")
		return p.Input * chars
	default:
		return 0
	}
}

// tokens extracts input and output token counts, accepting both naming styles.
func tokens(usage map[string]any) (in, out float64) {
	in, _ = count(usage, "prompt_tokens", "input_tokens")
	out, _ = count(usage, "completion_tokens", "output_tokens")
	return in, out
}

// count returns the first numeric value found among keys, and whether one was
// found.
func count(usage map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := usage[k]; ok {
			if f, ok := asFloat(v); ok {
				return f, true
			}
		}
	}
	return 0, false
}

// asFloat coerces a value decoded from JSON (float64, json.Number) or a plain
// Go integer into a float64. It reports false for anything non-numeric.
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}
