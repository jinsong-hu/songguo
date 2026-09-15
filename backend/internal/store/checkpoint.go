package store

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// Checkpoints run on a background goroutine, never inside a commit.
//
// SQLite's default is an automatic checkpoint once the WAL passes 1000 pages
// (4 MiB), run synchronously by whichever connection just committed. On this
// gateway that connection is the ledger writer, and one captured agent turn is
// a multi-MB body — so nearly every payload commit crossed the threshold and
// then, before the next ledger op could start, fsynced the WAL, copied its pages
// into scattered overflow pages of a 150 GB file on a shared cloud disk, and
// fsynced that too. The ledger's write lag and capture backlog — two of the
// signals that shed capture — include every one of those waits.
//
// So wal_autocheckpoint is 0 on every connection (dsnPragmas), and this loop
// runs a PASSIVE checkpoint every checkpointEvery, or sooner once the WAL holds
// checkpointWALBytes. PASSIVE never takes the write lock: writers keep
// appending while it copies, and it stops short of any frame a live reader
// still needs instead of waiting for it. What the ledger commit does is now one
// sequential WAL append. Pages rewritten several times inside one interval —
// the calls and session rows, their indexes, the freelist — reach the database
// once instead of once per commit.
//
// Durability is unchanged in kind: with synchronous=NORMAL a commit was never
// fsynced by itself, and the kernel's own writeback already bounds how long a
// commit can sit unflushed to about the same window.
//
// journal_size_limit (dsnPragmas) truncates the WAL file back to that size when
// it restarts, so one burst does not leave a multi-GB file on disk for good.

const (
	checkpointEvery    = 15 * time.Second
	checkpointWALBytes = 256 << 20
	checkpointPoll     = time.Second
	walSizeLimit       = 64 << 20

	// A WAL this large that a checkpoint could not fully copy means a long read
	// is pinning it; worth an operator's attention, at most every walWarnEvery.
	walWarnBytes = 1 << 30
	walWarnEvery = 10 * time.Minute
)

type checkpointConfig struct {
	every, poll time.Duration
	walBytes    int64
}

var defaultCheckpointConfig = checkpointConfig{every: checkpointEvery, poll: checkpointPoll, walBytes: checkpointWALBytes}

// checkpointer owns the background loop for one Store.
type checkpointer struct {
	cfg    checkpointConfig
	path   string
	stop   context.CancelFunc
	done   chan struct{}
	logger atomic.Pointer[slog.Logger]

	runs     atomic.Int64
	lastWarn time.Time
}

// SetLogger gives the checkpointer somewhere to report a WAL that is not being
// checkpointed and checkpoint errors. Without one they are discarded.
func (s *Store) SetLogger(l *slog.Logger) {
	if l != nil {
		s.ckpt.logger.Store(l)
	}
}

func (s *Store) startCheckpointer(path string, cfg checkpointConfig) {
	ctx, cancel := context.WithCancel(context.Background())
	s.ckpt = &checkpointer{cfg: cfg, path: path, stop: cancel, done: make(chan struct{})}
	s.ckpt.logger.Store(slog.New(slog.NewTextHandler(io.Discard, nil)))
	go s.checkpointLoop(ctx)
}

func (s *Store) stopCheckpointer() {
	if s.ckpt == nil {
		return
	}
	s.ckpt.stop()
	<-s.ckpt.done
}

func (s *Store) checkpointLoop(ctx context.Context) {
	c := s.ckpt
	defer close(c.done)
	t := time.NewTicker(c.cfg.poll)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if time.Since(last) < c.cfg.every && s.walBytes() < c.cfg.walBytes {
			continue
		}
		last = time.Now()
		s.checkpoint(ctx)
	}
}

// checkpoint runs one PASSIVE checkpoint and reports what it could not copy.
func (s *Store) checkpoint(ctx context.Context) {
	c := s.ckpt
	var busy, logFrames, copied int
	err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &copied)
	c.runs.Add(1)
	logger := c.logger.Load()
	switch {
	case ctx.Err() != nil:
	case err != nil:
		logger.Error("wal checkpoint failed", "err", err)
	case copied < logFrames:
		if size := s.walBytes(); size >= walWarnBytes && time.Since(c.lastWarn) >= walWarnEvery {
			c.lastWarn = time.Now()
			logger.Warn("wal checkpoint held back by a long read; the WAL keeps growing until it ends",
				"wal_mb", size>>20, "frames", logFrames, "copied", copied)
		}
	}
}

// walBytes is the WAL file's size on disk, 0 when it does not exist.
func (s *Store) walBytes() int64 {
	fi, err := os.Stat(s.ckpt.path + "-wal")
	if err != nil {
		return 0
	}
	return fi.Size()
}
