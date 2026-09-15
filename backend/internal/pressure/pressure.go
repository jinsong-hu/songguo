// Package pressure decides when songguo sheds its own bookkeeping so that
// forwarding keeps working on a machine that is running out of room.
//
// # Degrade the gateway's extras, never the caller's traffic
//
// A request passing through songguo costs two very different kinds of work. The
// forward itself — buffer, route, relay, meter the usage, record the call row —
// is what the caller is paying for. Everything else is songguo looking at the
// traffic for the operator's benefit: the captured request/response bodies in
// `raw`, the async parse that fingerprints them, the local o200k context
// composition. For an agent resending a 20 MB history every few seconds, that
// second kind is most of the memory and nearly all of the disk writes, and on a
// box without swap it is what takes the whole host down with it.
//
// So the order of sacrifice is fixed and it runs one way:
//
//	Normal        everything on
//	ShedCapture   stop writing raw bodies (and the parse that reads them)
//	ShedAnalysis  also stop local context composition and tool-turn estimates
//
// Metering, the call row, spend, routing and health are never shed: a missing
// call row is a hole in the cost history, while a missing trace is a trace. And
// nothing here refuses, delays or reroutes a request — making callers wait is the
// provider concurrency queue's job, a knob an operator sets on purpose, not
// something songguo reaches for on its own.
//
// # Signals
//
// The level is the worst of independent readings, sampled on a ticker:
//
//   - available memory as a share of the tighter of the cgroup limit and the
//     host (MemAvailable, which already counts reclaimable page cache as free);
//   - free space on the filesystem that holds the database;
//   - host I/O pressure (PSI `some avg10`: the share of the last ten seconds in
//     which some task was stalled on I/O);
//   - ledger write lag — how long the slowest write since the last sample took
//     from Submit to done, or how long the write in progress has been running;
//   - bytes of captured bodies queued in the ledger but not yet on disk.
//
// Disk I/O is the bottleneck more often than memory: a captured agent turn is
// thousands of overflow pages written twice, and on a cloud disk that is what
// stalls everything else on the box. So four of the five signals shed capture,
// and the defaults shed it early and restore it slowly — losing traces for a few
// minutes costs nothing a caller can see.
//
// A signal that cannot be read (no /proc on a dev laptop) is simply absent; it
// never escalates on its own.
//
// Escalation is immediate. Stepping back down waits for the readings to stay
// clear for a cooldown, so a load that hovers at a threshold does not flip
// capture on and off every few seconds.
package pressure

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Level is how much of songguo's own bookkeeping is currently shed. Higher
// levels include everything the lower ones shed.
type Level int32

const (
	// Normal runs every feature.
	Normal Level = iota
	// ShedCapture skips writing captured bodies to `raw` and the parse that
	// depends on them. Call rows and metering are untouched.
	ShedCapture
	// ShedAnalysis additionally skips local context composition and tool-turn
	// token estimates, which decode and tokenize the whole request body.
	ShedAnalysis
)

// String names the level for logs and the admin API.
func (l Level) String() string {
	switch l {
	case Normal:
		return "normal"
	case ShedCapture:
		return "shed_capture"
	case ShedAnalysis:
		return "shed_analysis"
	}
	return "unknown"
}

// Defaults, chosen for a small single-box deployment (8 GB, no swap, one cloud
// disk shared with other services). Capture is shed early and restored slowly.
const (
	DefaultInterval       = time.Second
	DefaultCooldown       = 5 * time.Minute
	DefaultMemCapturePct  = 30
	DefaultMemAnalysisPct = 10
	DefaultDiskFreeMin    = 10 << 30 // 10 GiB
	DefaultIOPressurePct  = 10
	DefaultWriteLag       = time.Second
)

// Options configures a Monitor. Zero thresholds disable that signal.
type Options struct {
	// Interval is the sampling period; <= 0 uses DefaultInterval.
	Interval time.Duration
	// Cooldown is how long readings must stay below the current level before
	// stepping down; <= 0 uses DefaultCooldown.
	Cooldown time.Duration

	// MemCapturePct sheds capture when available memory falls below this share
	// of total. MemAnalysisPct additionally sheds analysis. 0 disables either.
	MemCapturePct  float64
	MemAnalysisPct float64

	// DiskDir is a directory on the filesystem holding the database. DiskFreeMin
	// sheds capture when that filesystem has fewer free bytes. 0 disables.
	DiskDir     string
	DiskFreeMin uint64

	// IOPressurePct sheds capture when host I/O pressure (PSI some avg10)
	// reaches this percentage. 0 disables.
	IOPressurePct float64
	// WriteLag sheds capture when a ledger write takes this long from Submit to
	// done. 0 disables.
	WriteLag time.Duration

	Logger *slog.Logger

	// Probes, replaced in tests.
	memProbe  func() (avail, total uint64, ok bool)
	diskProbe func(dir string) (free, total uint64, ok bool)
	ioProbe   func() (someAvg10 float64, ok bool)
	now       func() time.Time
}

// Stats is a snapshot for the admin API: the level, what put it there, the last
// readings, and how much has been shed since start.
type Stats struct {
	Level          string  `json:"level"`
	Reason         string  `json:"reason,omitempty"`
	SinceMS        int64   `json:"since_ms"`
	MemAvailable   uint64  `json:"mem_available_bytes,omitempty"`
	MemTotal       uint64  `json:"mem_total_bytes,omitempty"`
	DiskFree       uint64  `json:"disk_free_bytes,omitempty"`
	DiskTotal      uint64  `json:"disk_total_bytes,omitempty"`
	IOPressure     float64 `json:"io_pressure_pct"`
	WriteLagMS     int64   `json:"write_lag_ms"`
	CaptureBacklog int64   `json:"capture_backlog_bytes"`
	CaptureBudget  int64   `json:"capture_budget_bytes"`
	ShedCaptures   int64   `json:"shed_captures"`
	ShedAnalyses   int64   `json:"shed_analyses"`

	// Over names every signal past its line in the last sample. Reason is only
	// the one that set the level; while the level waits out its cooldown, Over
	// is empty and Reason still says what caused it.
	Over []string `json:"over,omitempty"`
	// Thresholds are the configured lines, so a reading can be shown against
	// the one it would cross.
	Thresholds Thresholds `json:"thresholds"`
	CooldownMS int64      `json:"cooldown_ms"`
	// RestoreAtMS is when the level steps down if the readings stay clear. Zero
	// at normal, and while any signal is still over its line.
	RestoreAtMS int64 `json:"restore_at_ms,omitempty"`
}

// Thresholds are the shedding lines in force. Zero means that signal is
// disabled. Capture backlog has no line of its own: it sheds at half of
// Stats.CaptureBudget.
type Thresholds struct {
	MemCapturePct  float64 `json:"mem_capture_pct"`
	MemAnalysisPct float64 `json:"mem_analysis_pct"`
	DiskFreeMin    uint64  `json:"disk_free_min_bytes"`
	IOPressurePct  float64 `json:"io_pressure_pct"`
	WriteLagMS     int64   `json:"write_lag_ms"`
}

// Monitor samples the signals and holds the current level. A nil *Monitor is
// valid and always reports Normal, so tests and embedders that never construct
// one keep every feature on.
type Monitor struct {
	opts  Options
	level atomic.Int32

	shedCaptures atomic.Int64
	shedAnalyses atomic.Int64

	mu        sync.Mutex
	backlog   func() (pending, budget int64)
	writeLag  func() time.Duration
	reason    string
	changedAt time.Time
	clearAt   time.Time // when readings first dropped below the current level
	last      Stats
}

// New builds a Monitor and takes one reading, so the level is meaningful before
// Run's first tick.
func New(o Options) *Monitor {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Cooldown <= 0 {
		o.Cooldown = DefaultCooldown
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.memProbe == nil {
		o.memProbe = probeMemory
	}
	if o.diskProbe == nil {
		o.diskProbe = probeDisk
	}
	if o.ioProbe == nil {
		o.ioProbe = probeIO
	}
	if o.now == nil {
		o.now = time.Now
	}
	m := &Monitor{opts: o, changedAt: o.now()}
	m.sample()
	return m
}

// WatchBacklog supplies the capture backlog reading: bytes of captured bodies
// queued for the database, and the budget above which the queue drops them. The
// proxy wires its ledger in here, since the ledger is built after the Monitor.
func (m *Monitor) WatchBacklog(f func() (pending, budget int64)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.backlog = f
	m.mu.Unlock()
}

// WatchWriteLag supplies the ledger write-lag reading. The function is called
// once per sample and may reset its peak on each call; nothing else should read
// it.
func (m *Monitor) WatchWriteLag(f func() time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.writeLag = f
	m.mu.Unlock()
}

// Run samples on the configured interval until ctx is done.
func (m *Monitor) Run(ctx context.Context) {
	if m == nil {
		return
	}
	t := time.NewTicker(m.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sample()
		}
	}
}

// Level reports the current level.
func (m *Monitor) Level() Level {
	if m == nil {
		return Normal
	}
	return Level(m.level.Load())
}

// Capture filters a capture decision: it returns want unless capture is being
// shed, and counts the calls it turned off.
func (m *Monitor) Capture(want bool) bool {
	if !want || m.Level() < ShedCapture {
		return want
	}
	m.shedCaptures.Add(1)
	return false
}

// Analysis reports whether local body analysis may run, counting the calls it
// skips. Call it only where the analysis would otherwise have run.
func (m *Monitor) Analysis() bool {
	if m.Level() < ShedAnalysis {
		return true
	}
	m.shedAnalyses.Add(1)
	return false
}

// Stats returns a snapshot for the admin API.
func (m *Monitor) Stats() Stats {
	if m == nil {
		return Stats{Level: Normal.String()}
	}
	o := m.opts
	m.mu.Lock()
	s := m.last
	s.Level = m.Level().String()
	s.Reason = m.reason
	s.SinceMS = m.changedAt.UnixMilli()
	if !m.clearAt.IsZero() {
		s.RestoreAtMS = m.clearAt.Add(o.Cooldown).UnixMilli()
	}
	m.mu.Unlock()
	s.Thresholds = Thresholds{
		MemCapturePct:  o.MemCapturePct,
		MemAnalysisPct: o.MemAnalysisPct,
		DiskFreeMin:    o.DiskFreeMin,
		IOPressurePct:  o.IOPressurePct,
		WriteLagMS:     o.WriteLag.Milliseconds(),
	}
	s.CooldownMS = o.Cooldown.Milliseconds()
	s.ShedCaptures = m.shedCaptures.Load()
	s.ShedAnalyses = m.shedAnalyses.Load()
	return s
}

// sample takes one reading and moves the level: up at once, down only after
// the readings have stayed clear for the cooldown.
func (m *Monitor) sample() {
	m.mu.Lock()
	defer m.mu.Unlock()

	o := m.opts
	var r Stats
	target, reason := Normal, ""
	raise := func(l Level, why string) {
		r.Over = append(r.Over, why)
		if l > target {
			target, reason = l, why
		}
	}

	if avail, total, ok := o.memProbe(); ok && total > 0 {
		r.MemAvailable, r.MemTotal = avail, total
		pct := float64(avail) / float64(total) * 100
		switch {
		case o.MemAnalysisPct > 0 && pct < o.MemAnalysisPct:
			raise(ShedAnalysis, "memory")
		case o.MemCapturePct > 0 && pct < o.MemCapturePct:
			raise(ShedCapture, "memory")
		}
	}
	if o.DiskDir != "" {
		if free, total, ok := o.diskProbe(o.DiskDir); ok {
			r.DiskFree, r.DiskTotal = free, total
			if o.DiskFreeMin > 0 && free < o.DiskFreeMin {
				raise(ShedCapture, "disk")
			}
		}
	}
	if o.IOPressurePct > 0 {
		if some, ok := o.ioProbe(); ok {
			r.IOPressure = some
			if some >= o.IOPressurePct {
				raise(ShedCapture, "io_pressure")
			}
		}
	}
	if m.writeLag != nil {
		lag := m.writeLag()
		r.WriteLagMS = lag.Milliseconds()
		if o.WriteLag > 0 && lag >= o.WriteLag {
			raise(ShedCapture, "write_lag")
		}
	}
	if m.backlog != nil {
		pending, budget := m.backlog()
		r.CaptureBacklog, r.CaptureBudget = pending, budget
		// Shed at half the budget: past that the ledger is already behind, and the
		// budget itself is the hard stop that catches a burst between samples.
		if budget > 0 && pending*2 >= budget {
			raise(ShedCapture, "capture_backlog")
		}
	}
	m.last = r

	now := o.now()
	cur := Level(m.level.Load())
	switch {
	case target > cur:
		m.set(cur, target, reason, now)
	case target < cur:
		if m.clearAt.IsZero() {
			m.clearAt = now
		}
		if now.Sub(m.clearAt) >= o.Cooldown {
			m.set(cur, target, reason, now)
		}
	default:
		m.clearAt = time.Time{}
		if target != Normal {
			m.reason = reason
		}
	}
}

func (m *Monitor) set(from, to Level, reason string, now time.Time) {
	m.level.Store(int32(to))
	m.reason = reason
	m.changedAt = now
	m.clearAt = time.Time{}
	r := m.last
	attrs := []any{
		"from", from.String(), "to", to.String(), "reason", reason,
		"mem_available_mb", r.MemAvailable >> 20, "mem_total_mb", r.MemTotal >> 20,
		"disk_free_mb", r.DiskFree >> 20,
		"io_pressure_pct", r.IOPressure, "write_lag_ms", r.WriteLagMS,
		"capture_backlog_mb", r.CaptureBacklog >> 20, "capture_budget_mb", r.CaptureBudget >> 20,
	}
	if to > from {
		m.opts.Logger.Warn("pressure: shedding bookkeeping to keep forwarding", attrs...)
	} else {
		m.opts.Logger.Info("pressure: restoring bookkeeping", attrs...)
	}
}
