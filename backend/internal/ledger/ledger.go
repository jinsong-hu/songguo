// Package ledger writes call records to the store from a single background
// goroutine, so the gateway's request path never waits on the database.
//
// The proxy used to make nine synchronous SQLite round-trips per request —
// three of them before the vendor was even dialled, so they sat directly in
// front of the client's first byte. docs/arch-gateway.md already promised that
// "a slow or failing write never delays or corrupts the response"; this is what
// makes that true.
//
// # Never drops, blocks only in a catastrophe
//
// The other two background forks in the gateway (parse, insights) drop their
// work when saturated, because a stale session rollup is expendable. A call
// record is not: a missing row is an unexplained hole in the dashboard and in
// the cost history, and nothing downstream can tell it apart from traffic that
// never happened. So this queue does not drop. When it is full, submit blocks.
//
// That is a deliberate ordering of the three properties you cannot have at
// once — non-blocking, bounded memory, no loss. Dropping buys non-blocking at
// the cost of silent holes; blocking keeps the record complete and makes the
// backpressure visible instead. The ceiling is sized so that the choice is
// theoretical:
//
//	depth = ops_per_second × seconds_of_database_stall_to_absorb
//
// A gateway proxying LLM requests cannot outrun a local SQLite writer. Each
// turn occupies the upstream for seconds, so the completion rate is
// concurrency ÷ duration — on the order of 200 requests and 600 ops per second
// even at a concurrency of 1000. A single writer on WAL with
// synchronous=NORMAL sustains 5k–20k ops/sec, an order of magnitude more. The
// queue therefore drains continuously and sits near empty; the ceiling exists
// only to ride out a hiccup (a janitor sweep, an fsync stall). At roughly 500
// bytes an op, DefaultQueue is ~16 MB and absorbs about a minute of total
// database unavailability.
//
// Note that a bound on CONCURRENCY does not bound queue depth — depth is
// arrival rate minus drain rate, sustained over time, and requests complete and
// are replaced. Size this by how long a stall you want to survive, not by
// max_concurrency.
//
// # Captured bodies are the exception, and they do drop
//
// The 500-bytes-an-op model holds for call rows. It does not hold for
// KindPayload: a captured agent turn carries its whole request body, and a
// codex-style client resends a 20 MB history every few seconds. Those ops are
// also the slowest to write — thousands of overflow pages each — so they are
// exactly the ones that back up, and 32k slots of them is the host's memory,
// not 16 MB.
//
// So payloads get a byte budget of their own (SetPayloadBudget). Past it a
// payload is dropped at Submit and counted, and nothing else changes: the call
// row, its finalize and its metering still go through, never dropped, exactly
// as above. A missing trace is a trace; a missing call row is a hole in the
// cost history. That asymmetry is the whole reason the rule differs.
//
// # One writer, because ordering is the whole game
//
// SQLite permits exactly one writer, so a single goroutine matches the
// database's real concurrency rather than contending with itself for the WAL
// write lock. More importantly it makes ordering free, and ordering here is
// load-bearing:
//
//   - FinalizeCall is an UPDATE keyed by id. A finalize that overtakes its
//     create matches no row, and the entire outcome of the call is lost.
//   - raw and context_composition are FOREIGN KEYs onto
//     calls(id), so a payload or composition written before its parent row
//     fails outright.
//
// A worker pool would reintroduce both. If this ever genuinely needs more
// throughput, shard by call id — so every op for one call lands on one
// worker — rather than adding workers to a shared queue.
package ledger

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/compose"
	"github.com/songguo/songguo/internal/store"
)

// Kind selects which store write an Op performs.
type Kind uint8

const (
	// KindCreate opens a call row (phase 1). Must precede everything else for
	// that call id.
	KindCreate Kind = iota
	// KindFinalize records the outcome onto an already-open row (phase 2).
	KindFinalize
	// KindUpsert is create+finalize as one ordered unit, for calls whose whole
	// life is known at once (gateway denials, WebSocket sessions).
	KindUpsert
	// KindPayload stores the captured request/response bodies.
	KindPayload
	// KindComposition stores the context-composition breakdown.
	KindComposition
	// KindBarrier writes nothing. Because the queue is strictly ordered, a
	// barrier that has been applied proves every op submitted before it has
	// been too — which is what Flush waits on.
	KindBarrier
)

// Op is one queued write.
type Op struct {
	Kind        Kind
	Entry       calls.Entry
	Payload     *store.Payload
	CallID      string
	Composition *compose.Composition

	// After runs on the writer goroutine after the write SUCCEEDS, and is
	// skipped when it fails. This is how work that depends on the row already
	// existing — the parse pipeline UPDATEs the calls row with a message
	// fingerprint — is sequenced behind it without this package having to know
	// what that work is. A payload dropped over budget never runs its After.
	After func()

	// payloadBytes is what this op counted against the payload budget, released
	// once it has been applied.
	payloadBytes int64
	// submitted is when Submit took the op, so the write lag includes both the
	// queue wait and the write itself.
	submitted time.Time
}

// Store is the subset of *store.Store the writer needs. An interface so tests
// can substitute a slow or failing store without a real database.
type Store interface {
	CreateCall(calls.Entry) error
	FinalizeCall(calls.Entry) error
	SavePayload(store.Payload) error
	SaveComposition(string, compose.Composition) error
}

// DefaultQueue is the ceiling. See the package comment for the sizing model:
// roughly a minute of total database unavailability at a realistic op rate.
const DefaultQueue = 32768

// blockLogInterval rate-limits the "queue full" warning. If the ceiling is ever
// reached it is reached at hundreds of ops per second, and a log line per op
// would bury the signal it is trying to raise.
const blockLogInterval = time.Second

// busyRetryAttempts is deliberately small and bounded. The store serializes
// all in-process mutators, so this is for an external SQLite writer or a
// transient driver-level lock that still escapes the gate. Retries happen
// in-place at the queue head: requeueing would let a finalize or child write
// overtake a failed create.
const (
	busyRetryAttempts = 4
	busyRetryDelay    = 25 * time.Millisecond
)

// Stats is a snapshot of queue occupancy and lifetime counters, for the admin
// API. Depth is the live signal; HighWater and Blocked are what reveal a
// problem that has already passed.
type Stats struct {
	Capacity  int   `json:"capacity"`
	Depth     int   `json:"depth"`
	HighWater int   `json:"high_water"`
	Written   int64 `json:"written"`
	Failed    int64 `json:"failed"`
	Blocked   int64 `json:"blocked"`
	BlockedMS int64 `json:"blocked_ms"`

	// PayloadBytes is the size of captured bodies queued but not yet written;
	// PayloadBudget is the ceiling past which they drop (0 = unlimited);
	// PayloadsShed counts the ones that did.
	PayloadBytes  int64 `json:"payload_bytes"`
	PayloadBudget int64 `json:"payload_budget"`
	PayloadsShed  int64 `json:"payloads_shed"`
}

// Writer owns the queue and its single writer goroutine.
type Writer struct {
	ops    chan Op
	store  Store
	logger *slog.Logger
	wg     sync.WaitGroup

	highWater    atomic.Int64
	written      atomic.Int64
	failed       atomic.Int64
	blocked      atomic.Int64
	blockedNanos atomic.Int64
	lastBlockLog atomic.Int64 // unix nanos, for rate-limiting the warning
	now          func() time.Time

	payloadBudget  atomic.Int64 // bytes; 0 = unlimited
	payloadPending atomic.Int64
	payloadsShed   atomic.Int64
	lastShedLog    atomic.Int64 // unix nanos

	lagPeak  atomic.Int64 // nanos; slowest Submit-to-done since the last WriteLag
	applying atomic.Int64 // unix nanos the op being applied was submitted; 0 = idle
}

// New starts the writer goroutine. A non-positive queue uses DefaultQueue.
func New(st Store, logger *slog.Logger, queue int) *Writer {
	if logger == nil {
		logger = slog.Default()
	}
	if queue <= 0 {
		queue = DefaultQueue
	}
	w := &Writer{
		ops:    make(chan Op, queue),
		store:  st,
		logger: logger,
		now:    time.Now,
	}
	w.wg.Add(1)
	go w.run()
	return w
}

// SetPayloadBudget caps the bytes of captured bodies that may sit in the queue
// at once. 0 (the default) is unlimited. See the package comment.
func (w *Writer) SetPayloadBudget(bytes int64) {
	if w == nil {
		return
	}
	if bytes < 0 {
		bytes = 0
	}
	w.payloadBudget.Store(bytes)
}

// PayloadBacklog reports the captured bytes queued and the budget, for the
// pressure monitor.
func (w *Writer) PayloadBacklog() (pending, budget int64) {
	if w == nil {
		return 0, 0
	}
	return w.payloadPending.Load(), w.payloadBudget.Load()
}

// WriteLag reports how far behind the writer is: the slowest Submit-to-done time
// since the previous call, or the age of the write still in progress if that is
// longer. The peak resets on every call, so it has exactly one reader — the
// pressure monitor. It is the direct reading of disk I/O as the bottleneck: the
// database is not slow in general, this write is slow now.
func (w *Writer) WriteLag() time.Duration {
	if w == nil {
		return 0
	}
	lag := w.lagPeak.Swap(0)
	if started := w.applying.Load(); started != 0 {
		if cur := w.now().UnixNano() - started; cur > lag {
			lag = cur
		}
	}
	return time.Duration(lag)
}

func (w *Writer) observeLag(submitted time.Time) {
	lag := int64(w.now().Sub(submitted))
	for {
		peak := w.lagPeak.Load()
		if lag <= peak || w.lagPeak.CompareAndSwap(peak, lag) {
			return
		}
	}
}

// Submit enqueues an op. It does not block in normal operation; when the queue
// is at its ceiling it blocks until there is room rather than discarding the
// record, and counts how long it waited so the stall is visible in Stats.
//
// The one exception is a payload over the byte budget, which is dropped here
// rather than queued (see the package comment).
//
// Callers must treat this as potentially blocking and therefore must not hold
// a lock across it.
func (w *Writer) Submit(op Op) {
	if w == nil {
		return
	}
	if op.Kind == KindPayload && op.Payload != nil && !w.admitPayload(&op) {
		return
	}
	op.submitted = w.now()
	select {
	case w.ops <- op:
		w.observeDepth()
		return
	default:
	}

	// Ceiling reached. The database is not keeping up, which after Phase 0's
	// busy_timeout fix should mean it is genuinely unwell. Wait rather than
	// lose the record.
	start := w.now()
	w.ops <- op
	waited := w.now().Sub(start)
	w.blocked.Add(1)
	w.blockedNanos.Add(int64(waited))
	w.observeDepth()

	last := w.lastBlockLog.Load()
	nowNanos := start.UnixNano()
	if nowNanos-last >= int64(blockLogInterval) && w.lastBlockLog.CompareAndSwap(last, nowNanos) {
		w.logger.Warn("ledger queue full; request blocked waiting to record the call",
			"waited_ms", waited.Milliseconds(),
			"capacity", cap(w.ops),
			"blocked_total", w.blocked.Load())
	}
}

// admitPayload charges a payload against the byte budget, or drops it. An empty
// backlog always admits one, so a single body larger than the whole budget is
// still captured when nothing else is waiting.
func (w *Writer) admitPayload(op *Op) bool {
	size := int64(len(op.Payload.ReqBody) + len(op.Payload.RespBody))
	for {
		pending := w.payloadPending.Load()
		if budget := w.payloadBudget.Load(); budget > 0 && pending > 0 && pending+size > budget {
			w.shedPayload(op, pending, budget)
			return false
		}
		if w.payloadPending.CompareAndSwap(pending, pending+size) {
			op.payloadBytes = size
			return true
		}
	}
}

func (w *Writer) shedPayload(op *Op, pending, budget int64) {
	total := w.payloadsShed.Add(1)
	nowNanos := w.now().UnixNano()
	last := w.lastShedLog.Load()
	if nowNanos-last >= int64(blockLogInterval) && w.lastShedLog.CompareAndSwap(last, nowNanos) {
		w.logger.Warn("ledger payload budget full; captured body dropped, call row kept",
			"call_id", op.callID(),
			"body_bytes", len(op.Payload.ReqBody)+len(op.Payload.RespBody),
			"pending_bytes", pending, "budget_bytes", budget, "shed_total", total)
	}
}

// observeDepth keeps the high-water mark, which is the only way to see that a
// burst happened after it has already drained.
func (w *Writer) observeDepth() {
	d := int64(len(w.ops))
	for {
		hi := w.highWater.Load()
		if d <= hi || w.highWater.CompareAndSwap(hi, d) {
			return
		}
	}
}

// Stats returns a snapshot for the admin API.
func (w *Writer) Stats() Stats {
	if w == nil {
		return Stats{}
	}
	return Stats{
		Capacity:  cap(w.ops),
		Depth:     len(w.ops),
		HighWater: int(w.highWater.Load()),
		Written:   w.written.Load(),
		Failed:    w.failed.Load(),
		Blocked:   w.blocked.Load(),
		BlockedMS: w.blockedNanos.Load() / int64(time.Millisecond),

		PayloadBytes:  w.payloadPending.Load(),
		PayloadBudget: w.payloadBudget.Load(),
		PayloadsShed:  w.payloadsShed.Load(),
	}
}

func (w *Writer) run() {
	defer w.wg.Done()
	for op := range w.ops {
		w.apply(op)
	}
}

// apply performs one write. SQLite lock contention is retried in place at the
// queue head; all other failures are logged and counted immediately. Keeping
// a busy retry here, rather than requeueing the op, preserves create ->
// finalize -> children ordering.
func (w *Writer) apply(op Op) {
	if op.payloadBytes > 0 {
		// Released after the write, not before: the body is held until then.
		defer w.payloadPending.Add(-op.payloadBytes)
	}
	if !op.submitted.IsZero() {
		w.applying.Store(op.submitted.UnixNano())
		defer func() {
			w.applying.Store(0)
			w.observeLag(op.submitted)
		}()
	}
	var err error
	switch op.Kind {
	case KindUpsert:
		// These are two independent statements, but they remain one ordered
		// queue item. If finalize is busy after create committed, retry only
		// finalize; never repeat create.
		err = w.retryBusy(func() error { return w.store.CreateCall(op.Entry) })
		if err == nil {
			err = w.retryBusy(func() error { return w.store.FinalizeCall(op.Entry) })
		}
	default:
		err = w.retryBusy(func() error {
			switch op.Kind {
			case KindCreate:
				return w.store.CreateCall(op.Entry)
			case KindFinalize:
				return w.store.FinalizeCall(op.Entry)
			case KindPayload:
				if op.Payload != nil {
					return w.store.SavePayload(*op.Payload)
				}
			case KindComposition:
				if op.Composition != nil {
					return w.store.SaveComposition(op.CallID, *op.Composition)
				}
			case KindBarrier:
				// Nothing to write; reaching it is the signal.
			}
			return nil
		})
	}

	if err != nil {
		w.failed.Add(1)
		w.logger.Error("ledger write failed",
			"kind", op.Kind.String(), "call_id", op.callID(), "err", err)
		return
	}
	w.written.Add(1)
	if op.After != nil {
		op.After()
	}
}

func (w *Writer) retryBusy(write func() error) error {
	var err error
	for attempt := 0; attempt < busyRetryAttempts; attempt++ {
		err = write()
		if err == nil || !store.IsBusy(err) || attempt == busyRetryAttempts-1 {
			return err
		}
		time.Sleep(busyRetryDelay << attempt)
	}
	return err
}

// callID reports which call an op belongs to, for logging.
func (o Op) callID() string {
	if o.CallID != "" {
		return o.CallID
	}
	return o.Entry.ID
}

// String names the kind for logs.
func (k Kind) String() string {
	switch k {
	case KindCreate:
		return "create"
	case KindFinalize:
		return "finalize"
	case KindUpsert:
		return "upsert"
	case KindPayload:
		return "payload"
	case KindComposition:
		return "composition"
	case KindBarrier:
		return "barrier"
	}
	return "unknown"
}

// Flush blocks until every op submitted before the call has been applied. It
// leaves the writer running, which is what separates it from Close: tests read
// the store straight after a request, and the admin API could use it to answer
// a read-your-writes question without shutting anything down.
func (w *Writer) Flush() {
	if w == nil {
		return
	}
	done := make(chan struct{})
	w.Submit(Op{Kind: KindBarrier, After: func() { close(done) }})
	<-done
}

// Close stops accepting ops and drains everything already queued. On shutdown
// this must run before the store is closed, or the queued rows are lost — which
// for this fork means losing call records, not a derived rollup. Tests also use
// it as a drain barrier.
func (w *Writer) Close() {
	if w == nil {
		return
	}
	close(w.ops)
	w.wg.Wait()
}
