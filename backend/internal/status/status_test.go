package status

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/janitor"
	"github.com/songguo/songguo/internal/pressure"
)

// fakeProc is a /proc whose counters the test advances between samples.
type fakeProc struct {
	files map[string]string
}

func (f *fakeProc) read(path string) (string, bool) {
	s, ok := f.files[path]
	return s, ok
}

func (f *fakeProc) set(cpuBusy, cpuIdle, procTicks, rx, tx, diskW uint64) {
	f.files["/proc/stat"] = fmt.Sprintf("cpu  %d 0 0 %d 0 0 0 0 0 0\n", cpuBusy, cpuIdle)
	f.files["/proc/self/stat"] = fmt.Sprintf("1 (songguo) S 1 1 1 0 -1 0 0 0 0 0 %d 0 0 0 20 0 15 0 1 1 1\n", procTicks)
	f.files["/proc/net/dev"] = fmt.Sprintf("Inter-|\n face |\n  eth0: %d 0 0 0 0 0 0 0 %d 0 0 0 0 0 0 0\n", rx, tx)
	f.files["/proc/self/io"] = fmt.Sprintf("read_bytes: 0\nwrite_bytes: %d\n", diskW)
}

type rig struct {
	s       *Sampler
	proc    *fakeProc
	now     time.Time
	started int64
	level   string
}

func newRig(t *testing.T, history time.Duration) *rig {
	t.Helper()
	r := &rig{
		proc:  &fakeProc{files: map[string]string{"/proc/pressure/io": "some avg10=12.50 avg60=4.00 avg300=1.00 total=1\n"}},
		now:   time.Unix(1_700_000_000, 0),
		level: "normal",
	}
	r.proc.set(0, 0, 0, 0, 0, 0)
	src := Sources{
		Gateway:  func() GatewayCounters { return GatewayCounters{InFlight: 3, Started: r.started} },
		Pressure: func() pressure.Stats { return pressure.Stats{Level: r.level} },
	}
	// Built by hand so the first sample uses the fakes, not the real /proc.
	r.s = &Sampler{src: src, interval: 5 * time.Second, now: func() time.Time { return r.now }, read: r.proc.read, pageSize: 4096, ring: make([]point, int(history/(5*time.Second)))}
	r.s.startedAt = r.now
	r.s.sample()
	return r
}

func (r *rig) tick() { r.now = r.now.Add(5 * time.Second); r.s.sample() }

// Rates come from the difference between two samples, in the units the page
// shows: CPU as a share of all ticks, process CPU with one core = 100, network
// in bits, disk in bytes, requests per minute.
func TestRatesFromTwoSamples(t *testing.T) {
	r := newRig(t, time.Minute)
	if got := r.s.Snapshot(); got.Host.CPUPct != nil || got.Network.TxBps != nil || got.Gateway.RequestsPerMin != nil {
		t.Fatalf("first sample has rates: %+v", got)
	}

	// Over 5s: 250 of 1000 CPU ticks busy; 250 process ticks (2.5 CPU-seconds);
	// 5 MB received, 7.5 MB sent; 10 MB written; 20 requests.
	r.proc.set(250, 750, 250, 5_000_000, 7_500_000, 10_000_000)
	r.started = 20
	r.tick()

	got := r.s.Snapshot()
	check := func(name string, p *float64, want float64) {
		t.Helper()
		if p == nil {
			t.Errorf("%s absent, want %v", name, want)
		} else if *p != want {
			t.Errorf("%s = %v, want %v", name, *p, want)
		}
	}
	check("host cpu", got.Host.CPUPct, 25)
	check("process cpu", got.Process.CPUPct, 50)
	check("net rx", got.Network.RxBps, 8_000_000)
	check("net tx", got.Network.TxBps, 12_000_000)
	check("disk write", got.Process.DiskWriteBps, 2_000_000)
	check("requests/min", got.Gateway.RequestsPerMin, 240)
	check("io psi", got.Host.IOPressurePct, 12.5)
	check("io psi avg60", got.Host.IOPressureAvg60Pct, 4)
	if got.Gateway.InFlight != 3 {
		t.Errorf("in flight = %d, want 3", got.Gateway.InFlight)
	}
}

// A reading that cannot be taken is left out of the JSON rather than sent as 0.
func TestAbsentReadingsAreOmitted(t *testing.T) {
	r := newRig(t, time.Minute)
	delete(r.proc.files, "/proc/stat")
	delete(r.proc.files, "/proc/pressure/io")
	r.tick()
	snap := r.s.Snapshot()
	for name, block := range map[string]any{"host": snap.Host, "process": snap.Process, "database": snap.Database} {
		b, err := json.Marshal(block)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{`"cpu_pct":`, `"io_pressure_pct":`, `"load1":`, `"rss_bytes":`, `"size_bytes":`} {
			// Process CPU was readable in this rig; host CPU, PSI, load, RSS and the
			// database file were not.
			if name == "process" && key == `"cpu_pct":` {
				continue
			}
			if strings.Contains(string(b), key) {
				t.Errorf("%s block %s carries %s for a reading that was never taken", name, b, key)
			}
		}
	}
	// The history keeps the slot, as null, so the series stay aligned in time.
	if h := r.s.Snapshot().History; len(h.CPUPct) != 2 || h.CPUPct[1] != nil {
		t.Errorf("history cpu = %v, want two slots with the second absent", h.CPUPct)
	}
}

// History is oldest first and holds the configured window, dropping the oldest
// samples once full; the degrade level rides along to shade shed periods.
func TestHistoryWindow(t *testing.T) {
	r := newRig(t, 20*time.Second) // 4 slots
	for i := 1; i <= 5; i++ {
		if i == 5 {
			r.level = "shed_capture"
		}
		r.tick()
	}
	h := r.s.Snapshot().History
	if len(h.T) != 4 {
		t.Fatalf("history length = %d, want 4", len(h.T))
	}
	for i := 1; i < len(h.T); i++ {
		if h.T[i]-h.T[i-1] != 5000 {
			t.Fatalf("history not oldest-first at 5s steps: %v", h.T)
		}
	}
	if last := r.now.UnixMilli(); h.T[3] != last {
		t.Errorf("newest = %d, want %d", h.T[3], last)
	}
	if want := []int{0, 0, 0, 1}; fmt.Sprint(h.Level) != fmt.Sprint(want) {
		t.Errorf("levels = %v, want %v", h.Level, want)
	}
}

// A drain that is running is reported; one that found no table is not a
// finding worth a block on the page.
func TestDrainBlock(t *testing.T) {
	r := newRig(t, time.Minute)
	if r.s.Snapshot().Drain != nil {
		t.Error("drain block without a drain source")
	}
	d := janitor.DrainStatus{State: janitor.DrainDone}
	r.s.WatchDrain(func() janitor.DrainStatus { return d })
	if r.s.Snapshot().Drain != nil {
		t.Error("drain block for a table that never existed")
	}
	d = janitor.DrainStatus{State: janitor.DrainRunning, Rows: 4160, LastBatchMS: 101}
	if got := r.s.Snapshot().Drain; got == nil || *got != d {
		t.Errorf("drain = %+v, want %+v", got, d)
	}
}
