package pricing

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/songguo/songguo/internal/config"
)

func approx(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestCost(t *testing.T) {
	tests := []struct {
		name  string
		price config.Price
		usage map[string]any
		want  float64
	}{
		{
			name:  "per_1m_tokens openai naming",
			price: config.Price{Input: 3, Output: 15, Unit: "per_1m_tokens"},
			usage: map[string]any{"prompt_tokens": 1_000_000.0, "completion_tokens": 2_000_000.0},
			want:  3*1 + 15*2,
		},
		{
			name:  "per_1m_tokens alt naming",
			price: config.Price{Input: 3, Output: 15, Unit: "per_1m_tokens"},
			usage: map[string]any{"input_tokens": 500_000.0, "output_tokens": 100_000.0},
			want:  3*0.5 + 15*0.1,
		},
		{
			name:  "per_1k_tokens",
			price: config.Price{Input: 0.5, Output: 1.5, Unit: "per_1k_tokens"},
			usage: map[string]any{"prompt_tokens": 1000.0, "completion_tokens": 2000.0},
			want:  0.5*1 + 1.5*2,
		},
		{
			name:  "per_token",
			price: config.Price{Input: 0.001, Output: 0.002, Unit: "per_token"},
			usage: map[string]any{"prompt_tokens": 10.0, "completion_tokens": 20.0},
			want:  0.001*10 + 0.002*20,
		},
		{
			name:  "per_call default 1",
			price: config.Price{Input: 0.25, Unit: "per_call"},
			usage: map[string]any{},
			want:  0.25,
		},
		{
			name:  "per_call explicit",
			price: config.Price{Input: 0.25, Unit: "per_call"},
			usage: map[string]any{"calls": 4.0},
			want:  1.0,
		},
		{
			name:  "per_image",
			price: config.Price{Input: 0.04, Unit: "per_image"},
			usage: map[string]any{"images": 3.0},
			want:  0.12,
		},
		{
			name:  "per_second",
			price: config.Price{Input: 0.006, Unit: "per_second"},
			usage: map[string]any{"seconds": 10.0},
			want:  0.06,
		},
		{
			name:  "per_char characters",
			price: config.Price{Input: 0.00002, Unit: "per_char"},
			usage: map[string]any{"characters": 1000.0},
			want:  0.02,
		},
		{
			name:  "per_char chars alias",
			price: config.Price{Input: 0.00002, Unit: "per_char"},
			usage: map[string]any{"chars": 500.0},
			want:  0.01,
		},
		{
			name:  "int values",
			price: config.Price{Input: 3, Output: 15, Unit: "per_1m_tokens"},
			usage: map[string]any{"prompt_tokens": 1_000_000, "completion_tokens": 1_000_000},
			want:  18,
		},
		{
			name:  "json.Number values",
			price: config.Price{Input: 3, Output: 15, Unit: "per_1m_tokens"},
			usage: map[string]any{"prompt_tokens": json.Number("1000000"), "completion_tokens": json.Number("0")},
			want:  3,
		},
		{
			name:  "missing keys contribute zero",
			price: config.Price{Input: 3, Output: 15, Unit: "per_1m_tokens"},
			usage: map[string]any{},
			want:  0,
		},
		{
			name:  "non-numeric value ignored",
			price: config.Price{Input: 3, Output: 15, Unit: "per_1m_tokens"},
			usage: map[string]any{"prompt_tokens": "lots", "completion_tokens": 1_000_000.0},
			want:  15,
		},
		{
			name:  "unknown unit zero",
			price: config.Price{Input: 3, Output: 15, Unit: "per_furlong"},
			usage: map[string]any{"prompt_tokens": 1_000_000.0},
			want:  0,
		},
		{
			name:  "empty unit zero",
			price: config.Price{Input: 3, Output: 15, Unit: ""},
			usage: map[string]any{"prompt_tokens": 1_000_000.0},
			want:  0,
		},
		{
			name:  "nil usage no panic",
			price: config.Price{Input: 3, Output: 15, Unit: "per_1m_tokens"},
			usage: nil,
			want:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Cost(tt.price, tt.usage)
			if !approx(got, tt.want) {
				t.Fatalf("Cost() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAsFloat(t *testing.T) {
	tests := []struct {
		in   any
		want float64
		ok   bool
	}{
		{float64(1.5), 1.5, true},
		{float32(2), 2, true},
		{int(3), 3, true},
		{int64(4), 4, true},
		{uint(5), 5, true},
		{json.Number("6.5"), 6.5, true},
		{json.Number("nope"), 0, false},
		{"string", 0, false},
		{nil, 0, false},
		{true, 0, false},
	}
	for _, tt := range tests {
		got, ok := asFloat(tt.in)
		if ok != tt.ok || (ok && !approx(got, tt.want)) {
			t.Fatalf("asFloat(%#v) = (%v, %v), want (%v, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
