// Package breaker implements the minimal circuit breaker used by the router.
// Only retryable failures open it; 429 uses the router's cooldown map and 4xx
// client errors never affect breaker state.
package breaker

import (
	"sync"
	"time"
)

// State is one of closed, open or half-open.
type State string

const (
	Closed   State = "closed"
	Open     State = "open"
	HalfOpen State = "half-open"
)

// Breaker allows one probe after its cooldown expires. A failed probe re-opens
// with a doubled cooldown, capped at max.
type Breaker struct {
	mu        sync.Mutex
	state     State
	failures  int
	until     time.Time
	cooldown  time.Duration
	probeOpen bool
}

func New() *Breaker { return &Breaker{state: Closed} }

// State reports the effective state at now. An expired open breaker is shown
// as half-open and is eligible for exactly one probe via Allow.
func (b *Breaker) State(now time.Time) State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && !now.Before(b.until) {
		return HalfOpen
	}
	return b.state
}

// Allow returns whether a request may pass at now.
func (b *Breaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true
	case Open:
		if now.Before(b.until) {
			return false
		}
		b.state = HalfOpen
		b.probeOpen = true
		return true
	default:
		return false
	}
}

// Success closes the breaker and resets all counters.
func (b *Breaker) Success() {
	b.mu.Lock()
	b.state, b.failures, b.until, b.cooldown, b.probeOpen = Closed, 0, time.Time{}, 0, false
	b.mu.Unlock()
}

// Fail records a retryable failure. Consecutive failures below threshold leave
// the breaker closed. A failure while half-open re-opens it immediately.
func (b *Breaker) Fail(now time.Time, threshold int, initial, max time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.state != HalfOpen && b.failures < threshold {
		return
	}
	b.state = Open
	b.failures = 0
	b.probeOpen = false
	if b.cooldown == 0 {
		b.cooldown = initial
	} else {
		b.cooldown *= 2
	}
	if b.cooldown > max {
		b.cooldown = max
	}
	b.until = now.Add(b.cooldown)
}

// Cooldown is the current cooldown duration, exposed for deterministic tests.
func (b *Breaker) Cooldown() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cooldown
}
