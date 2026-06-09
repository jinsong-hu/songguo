package store

import (
	"errors"
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
