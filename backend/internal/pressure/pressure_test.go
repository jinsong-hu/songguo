package pressure

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// rig drives a Monitor with readings and a clock the test controls.
type rig struct {
	m *Monitor

	now             time.Time
	memAvail        uint64
	memTotal        uint64
	memOK           bool
	diskFree        uint64
	diskOK          bool
	ioSome          float64
	ioOK            bool
	lag             time.Duration
	pending, budget int64
}

func newRig(t *testing.T, o Options) *rig {
	t.Helper()
	r := &rig{now: time.Unix(1_700_000_000, 0), memAvail: 80, memTotal: 100, memOK: true, diskFree: 1 << 40, diskOK: true, ioOK: true}
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	o.now = func() time.Time { return r.now }
	o.memProbe = func() (uint64, uint64, bool) { return r.memAvail, r.memTotal, r.memOK }
	o.diskProbe = func(string) (uint64, uint64, bool) { return r.diskFree, 1 << 41, r.diskOK }
	o.ioProbe = func() (float64, bool) { return r.ioSome, r.ioOK }
	r.m = New(o)
	r.m.WatchBacklog(func() (int64, int64) { return r.pending, r.budget })
	r.m.WatchWriteLag(func() time.Duration { return r.lag })
	return r
}

func (r *rig) at(d time.Duration) *rig { r.now = r.now.Add(d); r.m.sample(); return r }

func (r *rig) want(t *testing.T, l Level) {
	t.Helper()
	if got := r.m.Level(); got != l {
		t.Fatalf("level = %s, want %s (stats %+v)", got, l, r.m.Stats())
	}
}

func memOpts() Options {
	return Options{MemCapturePct: 20, MemAnalysisPct: 10, Cooldown: time.Minute}
}

// Escalation must be immediate: the reading that says memory is gone is the
// last one before the host stops answering, so there is no waiting for a
// second opinion.
func TestEscalatesAtOnce(t *testing.T) {
	r := newRig(t, memOpts())
	r.want(t, Normal)

	r.memAvail = 15
	r.at(0).want(t, ShedCapture)
	if s := r.m.Stats(); s.Reason != "memory" {
		t.Errorf("reason = %q, want memory", s.Reason)
	}

	r.memAvail = 5
	r.at(0).want(t, ShedAnalysis)
}

// Stepping down waits for the readings to stay clear for the whole cooldown,
// and a relapse restarts the wait — otherwise a load hovering at the threshold
// turns capture on and off every second.
func TestStepsDownOnlyAfterCooldown(t *testing.T) {
	r := newRig(t, memOpts())
	r.memAvail = 15
	r.at(0).want(t, ShedCapture)

	r.memAvail = 80
	r.at(time.Second).want(t, ShedCapture)
	r.at(30*time.Second).want(t, ShedCapture)

	r.memAvail = 15 // relapse
	r.at(time.Second).want(t, ShedCapture)

	r.memAvail = 80
	r.at(time.Second).want(t, ShedCapture)
	r.at(59*time.Second).want(t, ShedCapture)
	r.at(time.Second).want(t, Normal)
}

// The status page answers three questions from Stats alone: what is over its
// line right now, where the lines are, and when a shed level lets go. Reason
// cannot answer the first — it keeps naming the cause through the cooldown.
func TestStatsSayWhatIsOverAndWhenItRestores(t *testing.T) {
	r := newRig(t, Options{MemCapturePct: 20, MemAnalysisPct: 10, IOPressurePct: 10, WriteLag: time.Second, Cooldown: time.Minute})
	if s := r.m.Stats(); s.Over != nil || s.RestoreAtMS != 0 {
		t.Fatalf("at normal: over %v restore %d, want neither", s.Over, s.RestoreAtMS)
	}
	want := Thresholds{MemCapturePct: 20, MemAnalysisPct: 10, IOPressurePct: 10, WriteLagMS: 1000}
	if s := r.m.Stats(); s.Thresholds != want || s.CooldownMS != 60_000 {
		t.Errorf("thresholds %+v cooldown %d, want %+v and 60000", s.Thresholds, s.CooldownMS, want)
	}

	r.memAvail, r.ioSome = 15, 12
	r.at(0).want(t, ShedCapture)
	if s := r.m.Stats(); len(s.Over) != 2 || s.Over[0] != "memory" || s.Over[1] != "io_pressure" || s.RestoreAtMS != 0 {
		t.Errorf("both over: over %v restore %d, want [memory io_pressure] and no restore time", s.Over, s.RestoreAtMS)
	}

	r.memAvail, r.ioSome = 80, 0
	r.at(10 * time.Second)
	clear := r.now
	r.at(20*time.Second).want(t, ShedCapture)
	s := r.m.Stats()
	if s.Over != nil || s.Reason != "memory" {
		t.Errorf("cooling down: over %v reason %q, want none over and the original reason", s.Over, s.Reason)
	}
	if got, want := s.RestoreAtMS, clear.Add(time.Minute).UnixMilli(); got != want {
		t.Errorf("restore_at = %d, want %d (first clear reading + cooldown)", got, want)
	}

	r.ioSome = 11 // relapse: no restore time until it clears again
	r.at(time.Second)
	if s := r.m.Stats(); s.RestoreAtMS != 0 {
		t.Errorf("relapsed: restore_at = %d, want 0", s.RestoreAtMS)
	}
}

// Disk space, disk I/O, write lag and capture backlog are about writes, so they
// shed capture and never analysis — analysis costs memory and CPU, not disk.
func TestWriteSignalsShedCaptureOnly(t *testing.T) {
	t.Run("io pressure", func(t *testing.T) {
		r := newRig(t, Options{IOPressurePct: 10})
		r.ioSome = 9.9
		r.at(0).want(t, Normal)
		r.ioSome = 10
		r.at(0).want(t, ShedCapture)
		if s := r.m.Stats(); s.Reason != "io_pressure" || s.IOPressure != 10 {
			t.Errorf("stats = %+v, want reason io_pressure and the reading", s)
		}
	})
	t.Run("write lag", func(t *testing.T) {
		r := newRig(t, Options{WriteLag: time.Second})
		r.lag = 999 * time.Millisecond
		r.at(0).want(t, Normal)
		r.lag = 1500 * time.Millisecond
		r.at(0).want(t, ShedCapture)
		if s := r.m.Stats(); s.Reason != "write_lag" || s.WriteLagMS != 1500 {
			t.Errorf("stats = %+v, want reason write_lag and 1500ms", s)
		}
	})
	t.Run("disk", func(t *testing.T) {
		r := newRig(t, Options{DiskDir: "/db", DiskFreeMin: 10 << 30})
		r.diskFree = 9 << 30
		r.at(0).want(t, ShedCapture)
		if s := r.m.Stats(); s.Reason != "disk" || s.DiskFree != 9<<30 {
			t.Errorf("stats = %+v, want reason disk and the reading", s)
		}
	})
	t.Run("capture backlog", func(t *testing.T) {
		r := newRig(t, Options{})
		r.budget = 100
		r.pending = 49
		r.at(0).want(t, Normal)
		r.pending = 50
		r.at(0).want(t, ShedCapture)
		if s := r.m.Stats(); s.Reason != "capture_backlog" {
			t.Errorf("reason = %q, want capture_backlog", s.Reason)
		}
	})
}

// A signal that cannot be read is absent, not alarming: a dev laptop has no
// /proc/meminfo and must not come up with capture shed.
func TestUnreadableSignalsNeverEscalate(t *testing.T) {
	r := newRig(t, Options{MemCapturePct: 20, MemAnalysisPct: 10, DiskDir: "/db", DiskFreeMin: 10 << 30, IOPressurePct: 10})
	r.memOK, r.diskOK, r.ioOK = false, false, false
	r.memAvail, r.diskFree, r.ioSome = 0, 0, 100
	r.at(0).want(t, Normal)
}

// Zero thresholds disable their signal entirely.
func TestZeroThresholdsDisable(t *testing.T) {
	r := newRig(t, Options{DiskDir: "/db"})
	r.memAvail, r.diskFree, r.ioSome, r.lag = 1, 1, 100, time.Hour
	r.at(0).want(t, Normal)
}

func TestParsePSI(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{"some avg10=12.34 avg60=1.00 avg300=0.10 total=249126552\nfull avg10=3.00 avg60=0.00 avg300=0.00 total=239926891\n", 12.34, true},
		{"full avg10=3.00 avg60=0.00 avg300=0.00 total=1\nsome avg10=0.00 avg60=0.00 avg300=0.00 total=1\n", 0, true},
		{"", 0, false},
		{"some avg60=1.00\n", 0, false},
	} {
		got, ok := parsePSISomeAvg10(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parsePSISomeAvg10(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestCaptureAndAnalysisFilters(t *testing.T) {
	var none *Monitor
	if !none.Capture(true) || !none.Analysis() || none.Level() != Normal {
		t.Fatal("a nil Monitor must keep every feature on")
	}
	none.WatchBacklog(nil)
	if s := none.Stats(); s.Level != "normal" {
		t.Errorf("nil Stats level = %q", s.Level)
	}

	r := newRig(t, memOpts())
	r.memAvail = 15
	r.at(0).want(t, ShedCapture)

	if r.m.Capture(false) {
		t.Error("Capture(false) must stay false")
	}
	if r.m.Capture(true) {
		t.Error("Capture(true) must be shed at ShedCapture")
	}
	if !r.m.Analysis() {
		t.Error("analysis must still run at ShedCapture")
	}
	if s := r.m.Stats(); s.ShedCaptures != 1 || s.ShedAnalyses != 0 {
		t.Errorf("counters = %d/%d, want 1/0 (a user with capture off is not a shed)", s.ShedCaptures, s.ShedAnalyses)
	}

	r.memAvail = 5
	r.at(0).want(t, ShedAnalysis)
	if r.m.Analysis() {
		t.Error("analysis must be shed at ShedAnalysis")
	}
	if s := r.m.Stats(); s.ShedAnalyses != 1 {
		t.Errorf("ShedAnalyses = %d, want 1", s.ShedAnalyses)
	}
}
