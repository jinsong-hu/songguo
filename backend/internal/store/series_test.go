package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

func TestUsageSeriesDayBuckets(t *testing.T) {
	s := openTestStore(t)

	// Three days with traffic, plus a gap day with none. Use UTC midnights so
	// the aligned day buckets line up with the calendar.
	day0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day1 := day0.AddDate(0, 0, 1)
	day2 := day0.AddDate(0, 0, 2)
	day3 := day0.AddDate(0, 0, 3)

	entries := []calls.Entry{
		// day0: 2 rows, 1 error (500); cost 0.10 + 0.20 = 0.30.
		{TS: day0.Add(1 * time.Hour), Vendor: "v", Status: 200, Cost: 0.10, LatencyMS: 10},
		{TS: day0.Add(5 * time.Hour), Vendor: "v", Status: 500, Cost: 0.20, LatencyMS: 20},
		// day1: gap (no rows).
		// day2: 3 rows, 2 errors (status 0 transport + 404); cost 1.0+2.0+3.0=6.0.
		{TS: day2.Add(2 * time.Hour), Vendor: "v", Status: 200, Cost: 1.0, LatencyMS: 30},
		{TS: day2.Add(3 * time.Hour), Vendor: "v", Status: 0, Cost: 2.0, LatencyMS: 40},
		{TS: day2.Add(4 * time.Hour), Vendor: "v", Status: 404, Cost: 3.0, LatencyMS: 50},
	}
	for i, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	pts, err := s.UsageSeries(day0, day3, 24*time.Hour)
	if err != nil {
		t.Fatalf("UsageSeries: %v", err)
	}
	// [day0, day3) -> 3 contiguous day buckets, gap-filled.
	if len(pts) != 3 {
		t.Fatalf("len(points) = %d, want 3", len(pts))
	}

	// Buckets are contiguous, ascending, aligned to UTC midnight.
	wantStarts := []time.Time{day0, day1, day2}
	for i, p := range pts {
		if !p.Bucket.Equal(wantStarts[i]) {
			t.Errorf("bucket[%d] = %v, want %v", i, p.Bucket, wantStarts[i])
		}
		if p.Bucket.Location() != time.UTC {
			t.Errorf("bucket[%d] not UTC: %v", i, p.Bucket.Location())
		}
	}

	// day0 sums/counts.
	if !approx(pts[0].Cost, 0.30) || pts[0].Requests != 2 || pts[0].Errors != 1 {
		t.Errorf("day0 = %+v, want cost 0.30 / 2 req / 1 err", pts[0])
	}
	// day1 gap-filled with zeroes.
	if pts[1].Cost != 0 || pts[1].Requests != 0 || pts[1].Errors != 0 {
		t.Errorf("day1 (gap) = %+v, want all zero", pts[1])
	}
	// day2 sums/counts.
	if !approx(pts[2].Cost, 6.0) || pts[2].Requests != 3 || pts[2].Errors != 2 {
		t.Errorf("day2 = %+v, want cost 6.0 / 3 req / 2 err", pts[2])
	}
}

func TestUsageSeriesHourBuckets(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Rows in hour 0 and hour 2; hour 1 is a gap.
	rows := []calls.Entry{
		{TS: base.Add(10 * time.Minute), Vendor: "v", Status: 200, Cost: 1, LatencyMS: 1},
		{TS: base.Add(2*time.Hour + 5*time.Minute), Vendor: "v", Status: 200, Cost: 2, LatencyMS: 1},
	}
	for i, e := range rows {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}
	pts, err := s.UsageSeries(base, base.Add(3*time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("UsageSeries: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("len = %d, want 3", len(pts))
	}
	if pts[0].Requests != 1 || pts[1].Requests != 0 || pts[2].Requests != 1 {
		t.Errorf("requests = [%d %d %d], want [1 0 1]", pts[0].Requests, pts[1].Requests, pts[2].Requests)
	}
}

func TestUsageSeriesAlignsToEpoch(t *testing.T) {
	s := openTestStore(t)
	// since at 03:30 should align down to the 03:00 hour bucket.
	since := time.Date(2026, 6, 1, 3, 30, 0, 0, time.UTC)
	until := time.Date(2026, 6, 1, 5, 0, 0, 0, time.UTC)
	pts, err := s.UsageSeries(since, until, time.Hour)
	if err != nil {
		t.Fatalf("UsageSeries: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("len = %d, want 2", len(pts))
	}
	want0 := time.Date(2026, 6, 1, 3, 0, 0, 0, time.UTC)
	if !pts[0].Bucket.Equal(want0) {
		t.Errorf("first bucket = %v, want %v (aligned to epoch hour)", pts[0].Bucket, want0)
	}
}

func TestUsageSeriesTooManyBuckets(t *testing.T) {
	s := openTestStore(t)
	since := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	// ~26 years of hourly buckets is far over the 10000 cap.
	until := since.AddDate(26, 0, 0)
	_, err := s.UsageSeries(since, until, time.Hour)
	if err == nil {
		t.Fatal("expected an error for absurd bucket count")
	}
	if !errors.Is(err, ErrTooManyBuckets) {
		t.Errorf("err = %v, want ErrTooManyBuckets", err)
	}
}

func TestUsageSeriesEmptyRange(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// until <= aligned start yields no buckets.
	pts, err := s.UsageSeries(base, base, time.Hour)
	if err != nil {
		t.Fatalf("UsageSeries: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("len = %d, want 0 for empty range", len(pts))
	}
}

// flushShaped is the failure this whole median business exists for: a relay
// that buffers the whole answer and flushes the SSE at once. TTFT absorbs the
// entire call, generation_ms collapses to the millisecond floor, and the row
// scores 2,000,000 tok/s. One of these used to drag a bucket's mean to ~181,900.
func flushShaped(ts time.Time, model string) calls.Entry {
	return calls.Entry{
		TS: ts, Vendor: "v", Model: model, Status: 200,
		OutputTokens: 2000, TTFTMS: 80_000, GenerationMS: 1, LatencyMS: 80_001,
	}
}

// normalStream is an ordinary streamed call: 200 ms to the first token, then
// 100 tokens over a second, so 100 tok/s.
func normalStream(ts time.Time, model string) calls.Entry {
	return calls.Entry{
		TS: ts, Vendor: "v", Model: model, Status: 200,
		OutputTokens: 100, TTFTMS: 200, GenerationMS: 1000, LatencyMS: 1200,
	}
}

func TestUsageSeriesUsesMedianNotMean(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	var entries []calls.Entry
	for i := 0; i < 10; i++ {
		entries = append(entries, normalStream(base.Add(time.Duration(i)*time.Minute), "m"))
	}
	entries = append(entries, flushShaped(base.Add(30*time.Minute), "m"))
	for i, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	pts, err := s.UsageSeries(base, base.Add(time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("UsageSeries: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("len = %d, want 1", len(pts))
	}
	// 11 samples: ten at 100 tok/s and one at 2,000,000. Nearest-rank p50 picks
	// the 6th, which is 100. The mean was ~181,909.
	if !approx(pts[0].OutputTokensSecP50, 100) {
		t.Errorf("output TPS p50 = %v, want 100 (mean would be ~181909)", pts[0].OutputTokensSecP50)
	}
	if !approx(pts[0].TTFTMSP50, 200) {
		t.Errorf("TTFT p50 = %v, want 200 (mean would be ~7454)", pts[0].TTFTMSP50)
	}
}

func TestTokensByModelSeriesUsesMedianNotMean(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	var entries []calls.Entry
	for i := 0; i < 10; i++ {
		entries = append(entries, normalStream(base.Add(time.Duration(i)*time.Minute), "m"))
	}
	entries = append(entries, flushShaped(base.Add(30*time.Minute), "m"))
	for i, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	models, pts, err := s.TokensByModelSeries(Scope{}, BreakdownByModel, base, base.Add(time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("TokensByModelSeries: %v", err)
	}
	if len(models) != 1 || models[0] != "m" {
		t.Fatalf("models = %v, want [m]", models)
	}
	if len(pts) != 1 {
		t.Fatalf("len = %d, want 1", len(pts))
	}
	if !approx(pts[0].TPSByModel["m"], 100) {
		t.Errorf("TPS p50 = %v, want 100 (mean would be ~181909)", pts[0].TPSByModel["m"])
	}
	if !approx(pts[0].TTFTByModel["m"], 200) {
		t.Errorf("TTFT p50 = %v, want 200 (mean would be ~7454)", pts[0].TTFTByModel["m"])
	}
}

func TestTokensByModelSeriesEmptyBucketOmitsKey(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	entries := []calls.Entry{
		// hour0: one ordinary streamed call.
		normalStream(base.Add(10*time.Minute), "m"),
		// hour1: traffic, but nothing streamed — no TTFT, no generation window.
		{TS: base.Add(1*time.Hour + 10*time.Minute), Vendor: "v", Model: "m", Status: 200,
			InputTokens: 10, OutputTokens: 5, Cost: 0.5, LatencyMS: 40},
		// hour2: no traffic at all.
	}
	for i, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	_, pts, err := s.TokensByModelSeries(Scope{}, BreakdownByModel, base, base.Add(3*time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("TokensByModelSeries: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("len = %d, want 3", len(pts))
	}

	if !approx(pts[0].TTFTByModel["m"], 200) || !approx(pts[0].TPSByModel["m"], 100) {
		t.Errorf("hour0 = TTFT %v / TPS %v, want 200 / 100", pts[0].TTFTByModel["m"], pts[0].TPSByModel["m"])
	}

	// hour1 has tokens and cost, so those keys stay present; the performance
	// maps have nothing to report and omit the key rather than claim a zero.
	if !approx(pts[1].Tokens["m"], 15) || !approx(pts[1].CostByModel["m"], 0.5) {
		t.Errorf("hour1 tokens/cost = %v / %v, want 15 / 0.5", pts[1].Tokens["m"], pts[1].CostByModel["m"])
	}
	for i, p := range pts[1:] {
		if _, ok := p.TTFTByModel["m"]; ok {
			t.Errorf("hour%d TTFTByModel has key m = %v, want absent", i+1, p.TTFTByModel["m"])
		}
		if _, ok := p.TPSByModel["m"]; ok {
			t.Errorf("hour%d TPSByModel has key m = %v, want absent", i+1, p.TPSByModel["m"])
		}
	}

	// hour2 is empty, but tokens/cost are still gap-filled with a real zero.
	for _, k := range []string{"Tokens", "CostByModel"} {
		var m map[string]float64
		if k == "Tokens" {
			m = pts[2].Tokens
		} else {
			m = pts[2].CostByModel
		}
		v, ok := m["m"]
		if !ok || v != 0 {
			t.Errorf("hour2 %s[m] = %v (present %v), want 0 present", k, v, ok)
		}
	}
}

func TestTokensByModelSeriesOtherGroupMedianFoldsSamples(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	var entries []calls.Entry
	// Five heavyweights fill the top-N slots on token volume alone.
	for i := 0; i < tokensByModelTopN; i++ {
		entries = append(entries, calls.Entry{
			TS: base.Add(time.Duration(i) * time.Minute), Vendor: "v",
			Model: fmt.Sprintf("big%d", i), Status: 200, InputTokens: 100_000, LatencyMS: 10,
		})
	}
	// "slow" contributes one sample at 10 tok/s; "fast" contributes three at
	// 1000. Both land in "Other".
	entries = append(entries, calls.Entry{
		TS: base.Add(20 * time.Minute), Vendor: "v", Model: "slow", Status: 200,
		OutputTokens: 10, TTFTMS: 10, GenerationMS: 1000, LatencyMS: 1010,
	})
	for i := 0; i < 3; i++ {
		entries = append(entries, calls.Entry{
			TS: base.Add(time.Duration(30+i) * time.Minute), Vendor: "v", Model: "fast", Status: 200,
			OutputTokens: 100, TTFTMS: 5000, GenerationMS: 100, LatencyMS: 5100,
		})
	}
	for i, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	models, pts, err := s.TokensByModelSeries(Scope{}, BreakdownByModel, base, base.Add(time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("TokensByModelSeries: %v", err)
	}
	if got := models[len(models)-1]; got != otherModelKey {
		t.Fatalf("last key = %q, want %q", got, otherModelKey)
	}
	if len(pts) != 1 {
		t.Fatalf("len = %d, want 1", len(pts))
	}

	// "Other" pools four samples — [10, 1000, 1000, 1000] — and the median over
	// that pooled set is 1000. A median of the two per-model medians
	// ([10, 1000]) would be 10, which is the mistake this guards.
	if !approx(pts[0].TPSByModel[otherModelKey], 1000) {
		t.Errorf("Other TPS p50 = %v, want 1000 (median-of-medians would be 10)",
			pts[0].TPSByModel[otherModelKey])
	}
	// Same fold for TTFT: [10, 5000, 5000, 5000] -> 5000, not 10.
	if !approx(pts[0].TTFTByModel[otherModelKey], 5000) {
		t.Errorf("Other TTFT p50 = %v, want 5000 (median-of-medians would be 10)",
			pts[0].TTFTByModel[otherModelKey])
	}
	// The heavyweights streamed nothing, so they are absent from both maps.
	if _, ok := pts[0].TPSByModel["big0"]; ok {
		t.Errorf("big0 in TPSByModel, want absent (it never streamed)")
	}
}
