package store

import (
	"errors"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

func TestTokenTotalsAndSeries(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	entries := []calls.Entry{
		{TS: base.Add(10 * time.Minute), Vendor: "v", Model: "m", Status: 200, InputTokens: 100, OutputTokens: 20, CachedTokens: 40, LatencyMS: 10, TTFTMS: 5, GenerationMS: 1000},
		{TS: base.Add(1*time.Hour + 10*time.Minute), Vendor: "v", Model: "m", Status: 200, InputTokens: 50, OutputTokens: 5, CachedTokens: 0, LatencyMS: 30, TTFTMS: 15, GenerationMS: 500},
	}
	for i, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	tt, err := s.TokenTotals("", nil, nil)
	if err != nil {
		t.Fatalf("TokenTotals: %v", err)
	}
	if !approx(tt.Input, 150) || !approx(tt.Output, 25) || !approx(tt.Cached, 40) {
		t.Errorf("TokenTotals = %+v, want {150 25 40}", tt)
	}

	// Series over a 3-hour window: hour0 has row1, hour1 has row2, hour2 empty.
	pts, err := s.UsageSeries(base, base.Add(3*time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("UsageSeries: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("len = %d, want 3", len(pts))
	}
	if !approx(pts[0].InputTokens, 100) || !approx(pts[0].OutputTokens, 20) || !approx(pts[0].CachedTokens, 40) {
		t.Errorf("hour0 tokens = %+v", pts[0])
	}
	if !approx(pts[0].AvgLatencyMS, 10) {
		t.Errorf("hour0 avg latency = %v, want 10", pts[0].AvgLatencyMS)
	}
	if !approx(pts[0].AvgTTFTMS, 5) || !approx(pts[0].AvgOutputTokensSec, 20) {
		t.Errorf("hour0 performance = %+v, want TTFT 5 / output TPS 20", pts[0])
	}
	if !approx(pts[1].InputTokens, 50) || !approx(pts[1].AvgLatencyMS, 30) {
		t.Errorf("hour1 = %+v", pts[1])
	}
	if !approx(pts[1].AvgTTFTMS, 15) || !approx(pts[1].AvgOutputTokensSec, 10) {
		t.Errorf("hour1 performance = %+v, want TTFT 15 / output TPS 10", pts[1])
	}
	if pts[2].InputTokens != 0 || pts[2].AvgLatencyMS != 0 || pts[2].AvgTTFTMS != 0 || pts[2].AvgOutputTokensSec != 0 {
		t.Errorf("hour2 (gap) = %+v, want zero", pts[2])
	}
}

func TestTokenTotalsEmpty(t *testing.T) {
	s := openTestStore(t)
	tt, err := s.TokenTotals("", nil, nil)
	if err != nil {
		t.Fatalf("TokenTotals: %v", err)
	}
	if tt.Input != 0 || tt.Output != 0 || tt.Cached != 0 {
		t.Errorf("empty TokenTotals = %+v, want zero", tt)
	}
}

func TestDistinctUsers(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	entries := []calls.Entry{
		{TS: base, UserID: "a", Vendor: "v", Status: 200},
		{TS: base.Add(time.Minute), UserID: "a", Vendor: "v", Status: 200},
		{TS: base.Add(2 * time.Minute), UserID: "b", Vendor: "v", Status: 200},
		{TS: base.Add(3 * time.Minute), UserID: "", Vendor: "v", Status: 200}, // admin/unknown, excluded
	}
	for i, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}
	n, err := s.DistinctUsers(nil, nil)
	if err != nil {
		t.Fatalf("DistinctUsers: %v", err)
	}
	if n != 2 {
		t.Errorf("DistinctUsers = %d, want 2 (a, b; empty excluded)", n)
	}

	// Windowed: only b's row falls in [t+2m, t+3m).
	since := base.Add(2 * time.Minute)
	until := base.Add(3 * time.Minute)
	n, err = s.DistinctUsers(&since, &until)
	if err != nil {
		t.Fatalf("DistinctUsers(window): %v", err)
	}
	if n != 1 {
		t.Errorf("windowed DistinctUsers = %d, want 1", n)
	}
}

func TestBreakdown(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	entries := []calls.Entry{
		{TS: base, UserID: "u1", Model: "gpt-4o", Modality: calls.ModalityChat, Vendor: "openai", Status: 200, InputTokens: 100, OutputTokens: 10, Cost: 1, LatencyMS: 100},
		{TS: base.Add(time.Minute), UserID: "u1", Model: "gpt-4o", Modality: calls.ModalityChat, Vendor: "openai", Status: 500, InputTokens: 50, OutputTokens: 0, Cost: 0.5, LatencyMS: 300},
		{TS: base.Add(2 * time.Minute), UserID: "u2", Model: "claude", Modality: calls.ModalityChat, Vendor: "anthropic", Status: 200, InputTokens: 20, OutputTokens: 5, Cost: 0.2, LatencyMS: 50},
	}
	for i, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	rows, err := s.Breakdown(BreakdownByModel, nil, nil)
	if err != nil {
		t.Fatalf("Breakdown(model): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2 models", len(rows))
	}
	// Ordered by request count desc: gpt-4o (2) before claude (1).
	if rows[0].Key != "gpt-4o" || rows[0].Requests != 2 || rows[0].Errors != 1 {
		t.Errorf("rows[0] = %+v, want gpt-4o 2req/1err", rows[0])
	}
	if !approx(rows[0].InputTokens, 150) || !approx(rows[0].Cost, 1.5) || !approx(rows[0].AvgLatencyMS, 200) {
		t.Errorf("rows[0] sums = %+v", rows[0])
	}
	if rows[1].Key != "claude" || rows[1].Requests != 1 {
		t.Errorf("rows[1] = %+v, want claude 1req", rows[1])
	}

	// Vendor dimension, ordered by requests desc.
	vrows, err := s.Breakdown(BreakdownByVendor, nil, nil)
	if err != nil {
		t.Fatalf("Breakdown(vendor): %v", err)
	}
	if len(vrows) != 2 || vrows[0].Key != "openai" {
		t.Errorf("vendor rows = %+v", vrows)
	}

	// User dimension maps to the user_id column.
	urows, err := s.Breakdown(BreakdownByUser, nil, nil)
	if err != nil {
		t.Fatalf("Breakdown(user): %v", err)
	}
	if len(urows) != 2 {
		t.Errorf("user rows len = %d, want 2", len(urows))
	}

	// Unknown dimension is rejected.
	if _, err := s.Breakdown(BreakdownDimension("bogus"), nil, nil); !errors.Is(err, ErrBadDimension) {
		t.Errorf("bad dimension err = %v, want ErrBadDimension", err)
	}
}

func TestErrorClassCounts(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	statuses := []int{200, 429, 429, 404, 400, 500, 503, 0}
	for i, st := range statuses {
		if _, err := s.AppendCall(calls.Entry{TS: base.Add(time.Duration(i) * time.Minute), Vendor: "v", Status: st}); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}
	c, err := s.ErrorClassCounts(nil, nil)
	if err != nil {
		t.Fatalf("ErrorClassCounts: %v", err)
	}
	if c.RateLimited != 2 {
		t.Errorf("RateLimited = %d, want 2", c.RateLimited)
	}
	if c.ClientError != 2 { // 404, 400
		t.Errorf("ClientError = %d, want 2", c.ClientError)
	}
	if c.ServerError != 2 { // 500, 503
		t.Errorf("ServerError = %d, want 2", c.ServerError)
	}
	if c.Transport != 1 { // status 0
		t.Errorf("Transport = %d, want 1", c.Transport)
	}
}

func TestErrorClassCountsEmpty(t *testing.T) {
	s := openTestStore(t)
	c, err := s.ErrorClassCounts(nil, nil)
	if err != nil {
		t.Fatalf("ErrorClassCounts: %v", err)
	}
	if c != (ErrorClasses{}) {
		t.Errorf("empty = %+v, want zero", c)
	}
}
