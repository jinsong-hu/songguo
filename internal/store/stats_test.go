package store

import (
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

func TestOverviewStats(t *testing.T) {
	s := openTestStore(t)

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// Latencies: 10,20,30,40,50,60,70,80,90,100 (10 rows).
	// Mark some as errors: status 0 (transport), 500, 404 are errors; 200/201 are ok.
	rows := []struct {
		latency int64
		status  int
	}{
		{10, 200},
		{20, 200},
		{30, 500}, // error
		{40, 200},
		{50, 0}, // error (transport)
		{60, 200},
		{70, 404}, // error
		{80, 200},
		{90, 201},
		{100, 200},
	}
	for i, r := range rows {
		if _, err := s.AppendCall(calls.Entry{
			TS: base.Add(time.Duration(i) * time.Minute), TokenID: "t", Model: "m",
			Vendor: "v", Status: r.status, LatencyMS: r.latency,
		}); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	st, err := s.OverviewStats(nil, nil)
	if err != nil {
		t.Fatalf("OverviewStats: %v", err)
	}
	if st.Requests != 10 {
		t.Errorf("Requests = %d, want 10", st.Requests)
	}
	if st.Errors != 3 {
		t.Errorf("Errors = %d, want 3 (status 500, 0, 404)", st.Errors)
	}
	// Nearest-rank over sorted [10..100]:
	// p50 -> rank ceil(0.5*10)=5 -> sorted[4]=50
	// p95 -> rank ceil(0.95*10)=10 -> sorted[9]=100
	// p99 -> rank ceil(0.99*10)=10 -> sorted[9]=100
	if st.P50 != 50 {
		t.Errorf("P50 = %d, want 50", st.P50)
	}
	if st.P95 != 100 {
		t.Errorf("P95 = %d, want 100", st.P95)
	}
	if st.P99 != 100 {
		t.Errorf("P99 = %d, want 100", st.P99)
	}
}

func TestOverviewStatsEmpty(t *testing.T) {
	s := openTestStore(t)
	st, err := s.OverviewStats(nil, nil)
	if err != nil {
		t.Fatalf("OverviewStats: %v", err)
	}
	if st.Requests != 0 || st.Errors != 0 || st.P50 != 0 || st.P95 != 0 || st.P99 != 0 {
		t.Errorf("empty stats = %+v, want all zero", st)
	}
}

func TestOverviewStatsWindow(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if _, err := s.AppendCall(calls.Entry{
			TS: base.Add(time.Duration(i) * time.Minute), Vendor: "v",
			Status: 200, LatencyMS: int64((i + 1) * 10),
		}); err != nil {
			t.Fatalf("AppendCall: %v", err)
		}
	}
	since := base.Add(1 * time.Minute)
	until := base.Add(4 * time.Minute)
	st, err := s.OverviewStats(&since, &until)
	if err != nil {
		t.Fatalf("OverviewStats(window): %v", err)
	}
	if st.Requests != 3 { // minutes 1,2,3
		t.Errorf("windowed Requests = %d, want 3", st.Requests)
	}
}

func TestVendorStats(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// openai: 3 rows, 1 error (500); latencies 100,200,300 -> avg 200; last status 500.
	// anthropic: 2 rows, 0 errors; latencies 50,150 -> avg 100; last status 200.
	entries := []calls.Entry{
		{TS: base.Add(0), Vendor: "openai", Status: 200, LatencyMS: 100},
		{TS: base.Add(1 * time.Minute), Vendor: "openai", Status: 200, LatencyMS: 200},
		{TS: base.Add(2 * time.Minute), Vendor: "openai", Status: 500, LatencyMS: 300},
		{TS: base.Add(3 * time.Minute), Vendor: "anthropic", Status: 200, LatencyMS: 50},
		{TS: base.Add(4 * time.Minute), Vendor: "anthropic", Status: 200, LatencyMS: 150},
	}
	for i, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	stats, err := s.VendorStats(nil, nil)
	if err != nil {
		t.Fatalf("VendorStats: %v", err)
	}
	oa, ok := stats["openai"]
	if !ok {
		t.Fatal("missing openai vendor stat")
	}
	if oa.Requests != 3 || oa.Errors != 1 {
		t.Errorf("openai = %+v, want 3 req / 1 err", oa)
	}
	if !approx(oa.AvgLatency, 200) {
		t.Errorf("openai AvgLatency = %v, want 200", oa.AvgLatency)
	}
	if oa.LastStatus != 500 {
		t.Errorf("openai LastStatus = %d, want 500", oa.LastStatus)
	}
	an, ok := stats["anthropic"]
	if !ok {
		t.Fatal("missing anthropic vendor stat")
	}
	if an.Requests != 2 || an.Errors != 0 {
		t.Errorf("anthropic = %+v, want 2 req / 0 err", an)
	}
	if !approx(an.AvgLatency, 100) {
		t.Errorf("anthropic AvgLatency = %v, want 100", an.AvgLatency)
	}
	if an.LastStatus != 200 {
		t.Errorf("anthropic LastStatus = %d, want 200", an.LastStatus)
	}
}

func TestVendorStatsEmpty(t *testing.T) {
	s := openTestStore(t)
	stats, err := s.VendorStats(nil, nil)
	if err != nil {
		t.Fatalf("VendorStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("empty VendorStats len = %d, want 0", len(stats))
	}
}

func TestPercentileNearestRank(t *testing.T) {
	cases := []struct {
		in   []int64
		p    int
		want int64
	}{
		{nil, 50, 0},
		{[]int64{}, 95, 0},
		{[]int64{5}, 50, 5},
		{[]int64{5}, 99, 5},
		{[]int64{1, 2, 3, 4}, 50, 2},  // ceil(0.5*4)=2 -> idx1=2
		{[]int64{1, 2, 3, 4}, 100, 4}, // rank 4 -> idx3=4
		{[]int64{10, 20, 30}, 95, 30}, // ceil(0.95*3)=3 -> idx2=30
		{[]int64{30, 10, 20}, 50, 20}, // unsorted input handled defensively
	}
	for _, c := range cases {
		if got := percentileNearestRank(c.in, c.p); got != c.want {
			t.Errorf("percentileNearestRank(%v, %d) = %d, want %d", c.in, c.p, got, c.want)
		}
	}
}
