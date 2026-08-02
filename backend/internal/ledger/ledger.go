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
// # One writer, because ordering is the whole game
//
// SQLite permits exactly one writer, so a single goroutine matches the
// database's real concurrency rather than contending with itself for the WAL
// write lock. More importantly it makes ordering free, and ordering here is
// load-bearing:
//
//   - FinalizeCall is an UPDATE keyed by id. A finalize that overtakes its
//     create matches no row, and the entire outcome of the call is lost.
//   - raw, parsed_calls and context_composition are FOREIGN KEYs onto
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
	// existing — the parse pipeline writes parsed_calls, a FOREIGN KEY onto
	// calls(id) — is sequenced behind it without this package having to know
	// what that work is.
	After func()
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

// Submit enqueues an op. It does not block in normal operation; when the queue
// is at its ceiling it blocks until there is room rather than discarding the
// record, and counts how long it waited so the stall is visible in Stats.
//
// Callers must treat this as potentially blocking and therefore must not hold
// a lock across it.
func (w *Writer) Submit(op Op) {
	if w == nil {
		return
	}
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
	}
}

func (w *Writer) run() {
	defer w.wg.Done()
	for op := range w.ops {
		w.apply(op)
	}
}

// apply performs one write. A failure is logged and counted, never retried:
// the ops behind it in the queue belong to other calls and must not wait on
// this one, and a retry of a create whose finalize is already queued would
// reorder them.
func (w *Writer) apply(op Op) {
	var err error
	switch op.Kind {
	case KindCreate:
		err = w.store.CreateCall(op.Entry)
	case KindFinalize:
		err = w.store.FinalizeCall(op.Entry)
	case KindUpsert:
		if err = w.store.CreateCall(op.Entry); err == nil {
			err = w.store.FinalizeCall(op.Entry)
		}
	case KindPayload:
		if op.Payload != nil {
			err = w.store.SavePayload(*op.Payload)
		}
	case KindComposition:
		if op.Composition != nil {
			err = w.store.SaveComposition(op.CallID, *op.Composition)
		}
	case KindBarrier:
		// Nothing to write; reaching it is the signal.
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
