package janitor

import (
	"context"
	"errors"
	"time"
)

// The retired parsed_calls table is emptied a few rows at a time, continuously,
// until it can be dropped (store.DrainParsedCalls says why it cannot simply be
// dropped). The pace is set by two things that must never be traded for speed:
//
//   - The write lock. Each batch holds it, and freeing a multi-MB row means
//     reading its whole overflow chain, so a batch can take seconds. The pause
//     after a batch is at least twice the batch's own duration, so the drain
//     holds the lock at most a third of the time and ledger writes interleave.
//   - The disk. The drain's reads land on the disk capture writes to, and the
//     pressure monitor sheds capture when that disk is busy. The drain therefore
//     waits whenever busy says so — which main sets below the shedding
//     threshold — so the cleanup never becomes the reason traces are dropped.
//
// It takes as long as it takes; hours to days on the production database. A
// restart resumes where it stopped, since there is no state beyond the table.
const (
	drainBatch     = 4
	drainMinPause  = 500 * time.Millisecond
	drainBusyPause = 30 * time.Second
	drainErrPause  = time.Minute
	drainLogEvery  = 10 * time.Minute
)

func (j *Janitor) drainParsedCalls(ctx context.Context) {
	var total int64
	started := j.now()
	lastLog := started
	for {
		if j.busy != nil && j.busy() {
			if !sleep(ctx, drainBusyPause) {
				return
			}
			continue
		}
		batchStart := time.Now()
		n, done, err := j.store.DrainParsedCalls(ctx, drainBatch)
		elapsed := time.Since(batchStart)
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return
		case err != nil:
			j.logger.Error("parsed_calls drain failed; retrying", "err", err, "rows_so_far", total)
			if !sleep(ctx, drainErrPause) {
				return
			}
			continue
		case done:
			if total > 0 {
				j.logger.Info("parsed_calls drained and dropped", "rows", total, "took", j.now().Sub(started).Round(time.Second).String())
			}
			return
		}
		total += n
		if now := j.now(); now.Sub(lastLog) >= drainLogEvery {
			j.logger.Info("draining parsed_calls", "rows_so_far", total, "last_batch_ms", elapsed.Milliseconds())
			lastLog = now
		}
		if !sleep(ctx, max(drainMinPause, 2*elapsed)) {
			return
		}
	}
}

// sleep waits d or until ctx is done, reporting whether to carry on.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
