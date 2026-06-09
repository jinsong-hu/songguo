package proxy

import (
	"sync"
	"time"
)

// rateWindow is the fixed window over which per-token RPM is counted.
const rateWindow = time.Minute

// rateLimiter is a per-token fixed-window request counter. It is safe for
// concurrent use.
type rateLimiter struct {
	now func() time.Time

	mu      sync.Mutex
	windows map[string]*window
}

// window tracks the count of requests in the current fixed window for one key.
type window struct {
	start time.Time
	count int
}

// newRateLimiter constructs a limiter using clock now (defaults to time.Now).
func newRateLimiter(now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{now: now, windows: make(map[string]*window)}
}

// allow records an attempt for key and reports whether it is within limit.
// A limit <= 0 means unlimited (always allowed).
func (l *rateLimiter) allow(key string, limit int) bool {
	if limit <= 0 {
		return true
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	w := l.windows[key]
	if w == nil || now.Sub(w.start) >= rateWindow {
		l.windows[key] = &window{start: now, count: 1}
		return true
	}
	if w.count >= limit {
		return false
	}
	w.count++
	return true
}
