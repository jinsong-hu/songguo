package api

import (
	"testing"
	"time"
)

func TestResolveBucketAutoSelectUnchanged(t *testing.T) {
	// Omitting the bucket must keep behaving exactly as it did before minute
	// sizes existed: day beyond 48h, hour at or under it. Callers that never
	// pass a bucket (MCP tools, ad-hoc curl) should see no change in point count.
	base := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		span  time.Duration
		want  time.Duration
		label string
	}{
		{"one hour", time.Hour, time.Hour, "hour"},
		{"exactly 48h stays hourly", 48 * time.Hour, time.Hour, "hour"},
		{"just over 48h flips to day", 48*time.Hour + time.Minute, 24 * time.Hour, "day"},
		{"thirty days", 30 * 24 * time.Hour, 24 * time.Hour, "day"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, label, err := resolveBucket("", base, base.Add(tc.span))
			if err != nil {
				t.Fatalf("resolveBucket: %v", err)
			}
			if got != tc.want || label != tc.label {
				t.Fatalf("got (%v, %q), want (%v, %q)", got, label, tc.want, tc.label)
			}
		})
	}
}

func TestResolveBucketSizes(t *testing.T) {
	base := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	until := base.Add(time.Hour)

	tests := []struct {
		in    string
		want  time.Duration
		label string
	}{
		// Legacy spellings keep their exact old labels.
		{"hour", time.Hour, "hour"},
		{"day", 24 * time.Hour, "day"},
		// Minute sizes, the point of the change.
		{"1m", time.Minute, "1m"},
		{"5m", 5 * time.Minute, "5m"},
		{"15m", 15 * time.Minute, "15m"},
		{"90m", 90 * time.Minute, "90m"},
		// Sizes that land on an hour or a day collapse onto the legacy label,
		// so the wire never carries two names for one size.
		{"60m", time.Hour, "hour"},
		{"1h", time.Hour, "hour"},
		{"24h", 24 * time.Hour, "day"},
		{"1d", 24 * time.Hour, "day"},
		{"60s", time.Minute, "1m"},
		// Multi-unit sizes.
		{"6h", 6 * time.Hour, "6h"},
		{"7d", 7 * 24 * time.Hour, "7d"},
		{"365d", 365 * 24 * time.Hour, "365d"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, label, err := resolveBucket(tc.in, base, until)
			if err != nil {
				t.Fatalf("resolveBucket(%q): %v", tc.in, err)
			}
			if got != tc.want || label != tc.label {
				t.Fatalf("resolveBucket(%q) = (%v, %q), want (%v, %q)",
					tc.in, got, label, tc.want, tc.label)
			}
		})
	}
}

func TestResolveBucketRejects(t *testing.T) {
	base := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	until := base.Add(time.Hour)

	tests := []struct {
		in     string
		reason string
	}{
		{"minute", "unit must be a single trailing letter"},
		{"5x", "unknown unit"},
		{"5", "no unit"},
		{"m", "no count"},
		{"0m", "zero count"},
		{"-5m", "negative count"},
		{"5.5m", "fractional count"},
		{"30s", "below the 1m floor"},
		{"1s", "below the 1m floor"},
		{"90s", "not a whole number of minutes"},
		{"366d", "above the 365d ceiling"},
		{"99999999999999999999d", "count overflows"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			_, _, err := resolveBucket(tc.in, base, until)
			if err == nil {
				t.Fatalf("resolveBucket(%q) succeeded, want error (%s)", tc.in, tc.reason)
			}
		})
	}
}

func TestResolveBucketFloorAndCeilingMessages(t *testing.T) {
	// The floor and ceiling say what is wrong; a generic parse failure would
	// leave the caller guessing which knob to turn.
	base := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	until := base.Add(time.Hour)

	if _, _, err := resolveBucket("30s", base, until); err == nil ||
		err.Error() != "bucket must be at least 1m" {
		t.Fatalf("30s error = %v, want the 1m floor message", err)
	}
	if _, _, err := resolveBucket("400d", base, until); err == nil ||
		err.Error() != "bucket must be at most 365d" {
		t.Fatalf("400d error = %v, want the 365d ceiling message", err)
	}
}
