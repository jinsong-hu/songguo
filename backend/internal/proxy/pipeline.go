package proxy

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/songguo/songguo/internal/parse"
	"github.com/songguo/songguo/internal/store"
)

// The parse pipeline runs the content parse OFF the request hot path. The
// synchronous proxy does only routing + metering and records the call row; this
// pipeline then parses a captured request's messages and stamps their
// message-shape fingerprint onto the call row, which is what lets the session
// Messages view skip redundant bodies (store/messagecover.go). It is
// best-effort: a saturated queue drops jobs (the call is already metered) and a
// call without a fingerprint only costs that view a redundant read.
//
// > History: it also persisted the whole parse.Call to parsed_calls — a second
// > copy of every captured conversation, kept 90 days, read by nothing, and
// > about a third of each captured call's disk writes. The table is retired and
// > drained (store.DrainParsedCalls); the fingerprint is all that is kept.

// parseJob is one unit of async post-processing. It carries the request only:
// the fingerprint is computed from the request's messages.
type parseJob struct {
	callID string
	in     parse.Input
}

type parsePipeline struct {
	jobs   chan parseJob
	store  *store.Store
	logger *slog.Logger
	wg     sync.WaitGroup

	// Each job holds a whole request body, so 256 slots of agent turns is
	// gigabytes. queuedBytes bounds that alongside the slot count.
	budget      int64
	queuedBytes atomic.Int64
}

const (
	defaultParseWorkers = 2
	defaultParseQueue   = 256
	defaultParseBudget  = 32 << 20
)

func (j parseJob) size() int64 { return int64(len(j.in.ReqBody) + len(j.in.RespBody)) }

// newParsePipeline starts the worker pool. workers/queue <= 0 use defaults.
func newParsePipeline(st *store.Store, logger *slog.Logger, workers, queue int) *parsePipeline {
	if logger == nil {
		logger = slog.Default()
	}
	if workers <= 0 {
		workers = defaultParseWorkers
	}
	if queue <= 0 {
		queue = defaultParseQueue
	}
	p := &parsePipeline{jobs: make(chan parseJob, queue), store: st, logger: logger, budget: defaultParseBudget}
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go p.worker()
	}
	return p
}

func (p *parsePipeline) worker() {
	defer p.wg.Done()
	for job := range p.jobs {
		p.process(job)
		p.queuedBytes.Add(-job.size())
	}
}

// submit enqueues a job without ever blocking the caller. A full queue — by
// slots or by bytes — means analysis is backed up; the job is dropped (and
// logged) rather than slowing the proxy — the call's metering row was already
// written synchronously. An empty queue always admits one job, however large.
func (p *parsePipeline) submit(job parseJob) {
	if p == nil {
		return
	}
	size := job.size()
	queued := p.queuedBytes.Add(size)
	if p.budget > 0 && queued > p.budget && queued != size {
		p.queuedBytes.Add(-size)
		p.logger.Warn("parse queue over byte budget; dropping parse job",
			"call_id", job.callID, "queued_bytes", queued-size, "budget_bytes", p.budget)
		return
	}
	select {
	case p.jobs <- job:
	default:
		p.queuedBytes.Add(-size)
		p.logger.Warn("parse queue full; dropping parse job", "call_id", job.callID)
	}
}

func (p *parsePipeline) process(job parseJob) {
	c, err := parse.Parse(job.in)
	if err != nil {
		// Non-fatal: the job carries no response, which some parsers flag; the
		// request's messages are what the fingerprint needs.
		p.logger.Debug("parse incomplete", "err", err, "call_id", job.callID, "format", c.Format)
	}

	// Stamp the message-shape fingerprint onto the calls row. A failure is
	// logged and dropped: the columns stay unknown, and an unknown fingerprint
	// costs the session view a redundant body read rather than a missing
	// conversation.
	f := c.Fingerprint()
	if ferr := p.store.SaveMessageFingerprint(job.callID, f.Count, f.Head, f.Tail); ferr != nil {
		p.logger.Error("save message fingerprint failed", "err", ferr, "call_id", job.callID)
	}
}

// Close stops accepting jobs and waits for in-flight ones to finish. Tests use
// it as a drain barrier; in production it is invoked on shutdown.
func (p *parsePipeline) Close() {
	if p == nil {
		return
	}
	close(p.jobs)
	p.wg.Wait()
}
