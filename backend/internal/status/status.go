// Package status samples what the admin status page shows: how loaded the host
// is, how loaded songguo itself is, and whether it is degrading — with the
// readings behind that and the lines they are measured against.
//
// # Nothing here touches the database
//
// Every reading is an atomic, a mutex-guarded snapshot, a /proc file or a stat
// of the database file. The page is polled exactly when an operator suspects the
// gateway is struggling, and on this deployment the database is the thing that
// struggles: on 2026-09-15 one unfiltered feed query pushed host I/O pressure to
// 50% and shed capture. A status check must never be the cause of the status.
//
// # Absent is not zero
//
// A reading that cannot be taken — no /proc on a dev laptop, a rate before its
// second sample — is nil in the JSON, and the page shows "—". A zero would claim
// an idle CPU nobody measured.
package status

import (
	"context"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/songguo/songguo/internal/janitor"
	"github.com/songguo/songguo/internal/ledger"
	"github.com/songguo/songguo/internal/pressure"
)

// Defaults: a sample every 5s, kept for 15 minutes (180 points) — enough to
// tell a spike from a plateau, small enough to send on every poll.
const (
	DefaultInterval = 5 * time.Second
	DefaultHistory  = 15 * time.Minute
)

// GatewayCounters is the proxy's live load, as the proxy counts it.
type GatewayCounters struct {
	InFlight      int64
	Started       int64
	BufferedBytes int64
}

// Sources are the in-memory readings the sampler does not own. Every field is
// optional; a nil source leaves its block out.
type Sources struct {
	Pressure func() pressure.Stats
	Ledger   func() ledger.Stats
	Gateway  func() GatewayCounters
	// Waiting counts requests queued for a provider slot (max_concurrency).
	Waiting func() int
	DBPath  string
}

// Snapshot is the GET /api/status response.
type Snapshot struct {
	SampledAtMS int64 `json:"sampled_at_ms"`
	IntervalMS  int64 `json:"interval_ms"`
	StartedAtMS int64 `json:"started_at_ms"`

	// Degrade is the pressure monitor: level, cause, readings, lines, counters.
	Degrade  *pressure.Stats      `json:"degrade,omitempty"`
	Host     Host                 `json:"host"`
	Gateway  Gateway              `json:"gateway"`
	Process  Process              `json:"process"`
	Network  Network              `json:"network"`
	Database Database             `json:"database"`
	Drain    *janitor.DrainStatus `json:"drain,omitempty"`
	History  History              `json:"history"`
}

// Host is whole-machine load. /proc/loadavg, /proc/stat and /proc/pressure are
// not namespaced, so inside a container these are still the host's figures —
// which is the point: songguo shares the box. Memory and disk space are on
// Degrade, measured the way shedding measures them.
type Host struct {
	CPUs int `json:"cpus"`
	// Load averages: runnable plus uninterruptible (mostly I/O-blocked) tasks.
	Load1  *float64 `json:"load1,omitempty"`
	Load5  *float64 `json:"load5,omitempty"`
	Load15 *float64 `json:"load15,omitempty"`
	// CPUPct is the busy share of all CPUs over the last interval.
	CPUPct *float64 `json:"cpu_pct,omitempty"`
	// PSI `some`: the share of time at least one task was stalled on the resource.
	CPUPressurePct     *float64 `json:"cpu_pressure_pct,omitempty"`
	MemoryPressurePct  *float64 `json:"memory_pressure_pct,omitempty"`
	IOPressurePct      *float64 `json:"io_pressure_pct,omitempty"`
	IOPressureAvg60Pct *float64 `json:"io_pressure_avg60_pct,omitempty"`
}

// Gateway is songguo's own traffic load.
type Gateway struct {
	InFlight       int64    `json:"in_flight"`
	Waiting        int      `json:"waiting"`
	RequestsPerMin *float64 `json:"requests_per_min,omitempty"`
	// BufferedRequestBytes is request bodies held in memory by in-flight calls.
	BufferedRequestBytes int64         `json:"buffered_request_bytes"`
	Ledger               *ledger.Stats `json:"ledger,omitempty"`
}

// Process is the songguo process's own resource use.
type Process struct {
	// CPUPct is one core = 100, so a busy process on 4 cores can read 400.
	CPUPct     *float64 `json:"cpu_pct,omitempty"`
	RSSBytes   *uint64  `json:"rss_bytes,omitempty"`
	Goroutines int      `json:"goroutines"`
	// Disk bytes per second this process read and wrote at the storage layer.
	DiskReadBps  *float64 `json:"disk_read_bps,omitempty"`
	DiskWriteBps *float64 `json:"disk_write_bps,omitempty"`
}

// Network is traffic on the interfaces in songguo's network namespace, in bits
// per second. In a container that is songguo's own traffic — clients in, and
// the same bodies out to providers; on the host network it is the whole host.
type Network struct {
	RxBps *float64 `json:"rx_bps,omitempty"`
	TxBps *float64 `json:"tx_bps,omitempty"`
}

// Database is the SQLite files' size on disk — os.Stat, never a query.
type Database struct {
	SizeBytes *int64 `json:"size_bytes,omitempty"`
	WALBytes  *int64 `json:"wal_bytes,omitempty"`
}

// History is the recent samples, oldest first, as parallel series. A nil entry
// is a sample where that reading was absent.
type History struct {
	T             []int64    `json:"t"`
	CPUPct        []*float64 `json:"cpu_pct"`
	IOPressurePct []*float64 `json:"io_pressure_pct"`
	NetRxBps      []*float64 `json:"net_rx_bps"`
	NetTxBps      []*float64 `json:"net_tx_bps"`
	InFlight      []int64    `json:"in_flight"`
	ProcessCPUPct []*float64 `json:"process_cpu_pct"`
	DiskWriteBps  []*float64 `json:"disk_write_bps"`
	// Level is the degrade level at each sample: 0 normal, 1 shed_capture,
	// 2 shed_analysis.
	Level []int `json:"level"`
}

// counters are the cumulative readings rates are computed from.
type counters struct {
	at                  time.Time
	cpuBusy, cpuTotal   uint64
	cpuOK               bool
	procTicks           uint64
	procOK              bool
	rx, tx              uint64
	netOK               bool
	diskRead, diskWrite uint64
	diskOK              bool
	started             int64
	gatewayOK           bool
}

type point struct {
	t                  int64
	cpu, io, rx, tx    *float64
	inFlight           int64
	procCPU, diskWrite *float64
	level              int
}

// Sampler takes a reading every interval and serves the latest with history.
type Sampler struct {
	src       Sources
	interval  time.Duration
	startedAt time.Time
	now       func() time.Time
	read      func(path string) (string, bool)
	pageSize  int

	mu    sync.Mutex
	drain func() janitor.DrainStatus
	prev  counters
	last  Snapshot // Host, Process, Network, Database, SampledAtMS
	ring  []point
	next  int
	full  bool
}

// New builds a Sampler and takes its first reading, so levels are meaningful at
// once; rates need a second sample and stay absent until Run's first tick.
func New(src Sources, interval, history time.Duration) *Sampler {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if history < interval {
		history = DefaultHistory
	}
	s := &Sampler{
		src:      src,
		interval: interval,
		now:      time.Now,
		read:     readFile,
		pageSize: os.Getpagesize(),
		ring:     make([]point, int(history/interval)),
	}
	s.startedAt = s.now()
	s.sample()
	return s
}

// WatchDrain supplies the parsed_calls drain's progress. The janitor is built
// after the admin API, so it is wired in here rather than through Sources.
func (s *Sampler) WatchDrain(f func() janitor.DrainStatus) {
	s.mu.Lock()
	s.drain = f
	s.mu.Unlock()
}

// Run samples on the interval until ctx is done.
func (s *Sampler) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sample()
		}
	}
}

// Snapshot returns the last sample with history. The in-memory sources —
// degrade level, ledger, in-flight, drain — are read now rather than at the
// last tick: they cost nothing, and the level is the one thing an operator
// wants to the second.
func (s *Sampler) Snapshot() Snapshot {
	s.mu.Lock()
	out := s.last
	out.History = s.historyLocked()
	drain := s.drain
	s.mu.Unlock()

	out.IntervalMS = s.interval.Milliseconds()
	out.StartedAtMS = s.startedAt.UnixMilli()
	if s.src.Pressure != nil {
		p := s.src.Pressure()
		out.Degrade = &p
	}
	if s.src.Ledger != nil {
		l := s.src.Ledger()
		out.Gateway.Ledger = &l
	}
	if s.src.Gateway != nil {
		g := s.src.Gateway()
		out.Gateway.InFlight, out.Gateway.BufferedRequestBytes = g.InFlight, g.BufferedBytes
	}
	if s.src.Waiting != nil {
		out.Gateway.Waiting = s.src.Waiting()
	}
	out.Process.Goroutines = runtime.NumGoroutine()
	if drain != nil {
		// A drain that finished without deleting anything had no table to
		// drain: nothing to report, so no block.
		if d := drain(); d.State != janitor.DrainDone || d.Rows > 0 {
			out.Drain = &d
		}
	}
	return out
}

// sample takes one reading, computes rates against the previous one and appends
// a history point.
func (s *Sampler) sample() {
	now := s.now()
	var c counters
	c.at = now
	var snap Snapshot
	snap.SampledAtMS = now.UnixMilli()

	h := &snap.Host
	h.CPUs = runtime.NumCPU()
	if txt, ok := s.read("/proc/loadavg"); ok {
		if l1, l5, l15, ok := parseLoadavg(txt); ok {
			h.Load1, h.Load5, h.Load15 = &l1, &l5, &l15
		}
	}
	if txt, ok := s.read("/proc/stat"); ok {
		c.cpuBusy, c.cpuTotal, c.cpuOK = parseCPUTimes(txt)
	}
	h.CPUPressurePct, _ = s.psi("cpu")
	h.MemoryPressurePct, _ = s.psi("memory")
	h.IOPressurePct, h.IOPressureAvg60Pct = s.psi("io")

	if txt, ok := s.read("/proc/net/dev"); ok {
		c.rx, c.tx, c.netOK = parseNetDev(txt)
	}
	if txt, ok := s.read("/proc/self/stat"); ok {
		c.procTicks, c.procOK = parseSelfCPU(txt)
	}
	if txt, ok := s.read("/proc/self/statm"); ok {
		if rss, ok := parseStatmRSS(txt, s.pageSize); ok {
			snap.Process.RSSBytes = &rss
		}
	}
	if txt, ok := s.read("/proc/self/io"); ok {
		c.diskRead, c.diskWrite, c.diskOK = parseSelfIO(txt)
	}
	if s.src.Gateway != nil {
		c.started, c.gatewayOK = s.src.Gateway().Started, true
	}
	if s.src.DBPath != "" {
		snap.Database.SizeBytes = fileSize(s.src.DBPath)
		snap.Database.WALBytes = fileSize(s.src.DBPath + "-wal")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.prev; !p.at.IsZero() {
		if dt := now.Sub(p.at).Seconds(); dt > 0 {
			if c.cpuOK && p.cpuOK && c.cpuTotal > p.cpuTotal && c.cpuBusy >= p.cpuBusy {
				h.CPUPct = ptr(float64(c.cpuBusy-p.cpuBusy) / float64(c.cpuTotal-p.cpuTotal) * 100)
			}
			if c.procOK && p.procOK && c.procTicks >= p.procTicks {
				snap.Process.CPUPct = ptr(float64(c.procTicks-p.procTicks) / clockTicks / dt * 100)
			}
			if c.netOK && p.netOK && c.rx >= p.rx && c.tx >= p.tx {
				snap.Network.RxBps = ptr(float64(c.rx-p.rx) * 8 / dt)
				snap.Network.TxBps = ptr(float64(c.tx-p.tx) * 8 / dt)
			}
			if c.diskOK && p.diskOK && c.diskRead >= p.diskRead && c.diskWrite >= p.diskWrite {
				snap.Process.DiskReadBps = ptr(float64(c.diskRead-p.diskRead) / dt)
				snap.Process.DiskWriteBps = ptr(float64(c.diskWrite-p.diskWrite) / dt)
			}
			if c.gatewayOK && p.gatewayOK && c.started >= p.started {
				snap.Gateway.RequestsPerMin = ptr(float64(c.started-p.started) / dt * 60)
			}
		}
	}
	s.prev = c
	s.last = snap

	pt := point{
		t: snap.SampledAtMS, cpu: h.CPUPct, io: h.IOPressurePct,
		rx: snap.Network.RxBps, tx: snap.Network.TxBps,
		procCPU: snap.Process.CPUPct, diskWrite: snap.Process.DiskWriteBps,
	}
	if s.src.Gateway != nil {
		pt.inFlight = s.src.Gateway().InFlight
	}
	if s.src.Pressure != nil {
		pt.level = levelIndex(s.src.Pressure().Level)
	}
	s.ring[s.next] = pt
	s.next = (s.next + 1) % len(s.ring)
	if s.next == 0 {
		s.full = true
	}
}

func (s *Sampler) psi(resource string) (avg10, avg60 *float64) {
	txt, ok := s.read("/proc/pressure/" + resource)
	if !ok {
		return nil, nil
	}
	a10, a60, ok := parsePSI(txt)
	if !ok {
		return nil, nil
	}
	return &a10, &a60
}

func (s *Sampler) historyLocked() History {
	n, start := s.next, 0
	if s.full {
		n, start = len(s.ring), s.next
	}
	h := History{
		T: make([]int64, n), CPUPct: make([]*float64, n), IOPressurePct: make([]*float64, n),
		NetRxBps: make([]*float64, n), NetTxBps: make([]*float64, n), InFlight: make([]int64, n),
		ProcessCPUPct: make([]*float64, n), DiskWriteBps: make([]*float64, n), Level: make([]int, n),
	}
	for i := 0; i < n; i++ {
		p := s.ring[(start+i)%len(s.ring)]
		h.T[i], h.CPUPct[i], h.IOPressurePct[i] = p.t, p.cpu, p.io
		h.NetRxBps[i], h.NetTxBps[i], h.InFlight[i] = p.rx, p.tx, p.inFlight
		h.ProcessCPUPct[i], h.DiskWriteBps[i], h.Level[i] = p.procCPU, p.diskWrite, p.level
	}
	return h
}

func levelIndex(level string) int {
	switch level {
	case pressure.ShedCapture.String():
		return 1
	case pressure.ShedAnalysis.String():
		return 2
	}
	return 0
}

func fileSize(path string) *int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	n := fi.Size()
	return &n
}

func ptr[T any](v T) *T { return &v }
