package proxy

import (
	"sync"
	"time"
)

// Limits enforces the build-proxy per-session counters from config.
type Limits struct {
	mu         sync.Mutex
	maxBytes   int64
	maxConns   int
	now        func() time.Time
	bytes      map[string]int64
	connWindow map[string]window
}

type window struct {
	start time.Time
	count int
}

// NewLimits creates per-session byte and connection-rate limits.
func NewLimits(bytesMB, connsPerMin int, now func() time.Time) *Limits {
	if now == nil {
		now = time.Now
	}
	return &Limits{
		maxBytes:   int64(bytesMB) * 1024 * 1024,
		maxConns:   connsPerMin,
		now:        now,
		bytes:      make(map[string]int64),
		connWindow: make(map[string]window),
	}
}

// AllowConn records one connection attempt for a session.
func (l *Limits) AllowConn(session string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.connWindow[session]
	if w.start.IsZero() || now.Sub(w.start) >= time.Minute {
		w = window{start: now}
	}
	if w.count >= l.maxConns {
		return false
	}
	w.count++
	l.connWindow[session] = w
	return true
}

// AddBytes adds bytes to a session's cumulative counter and reports whether
// the session is still under its byte budget.
func (l *Limits) AddBytes(session string, n int64) bool {
	if l == nil || n <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bytes[session] += n
	return l.bytes[session] <= l.maxBytes
}
