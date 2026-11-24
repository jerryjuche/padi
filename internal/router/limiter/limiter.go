// Package limiter implements sliding-window RPM limits and concurrency semaphores.
package limiter

import (
	"context"
	"sync"
	"time"
)

// Clock is injectable so tests can advance time without sleeping.
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

// RealClock is the production clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }
func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// FakeClock advances virtual time immediately when Sleep is called. It is safe
// for concurrent use and makes sliding-window tests deterministic.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func NewFakeClock(now time.Time) *FakeClock { return &FakeClock{now: now} }

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *FakeClock) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	f.Advance(d)
	return nil
}

// Sliding keeps the timestamps of the last rpm requests in a one-minute window.
type Sliding struct {
	mu         sync.Mutex
	timestamps []time.Time
	rpm        int
	clock      Clock
}

func NewSliding(rpm int, clock Clock) *Sliding {
	if rpm <= 0 {
		rpm = 1
	}
	if clock == nil {
		clock = RealClock{}
	}
	return &Sliding{rpm: rpm, clock: clock}
}

// Try records one request if a slot is available.
func (s *Sliding) Try() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	s.prune(now)
	if len(s.timestamps) < s.rpm {
		s.timestamps = append(s.timestamps, now)
		return 0, true
	}
	wait := time.Minute - now.Sub(s.timestamps[0])
	if wait <= 0 {
		s.prune(now.Add(time.Nanosecond))
		if len(s.timestamps) < s.rpm {
			s.timestamps = append(s.timestamps, now)
			return 0, true
		}
		wait = time.Minute - now.Sub(s.timestamps[0])
	}
	if wait <= 0 {
		wait = time.Nanosecond
	}
	return wait, false
}

// Wait blocks until a slot is available or ctx is done.
func (s *Sliding) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		wait, ok := s.Try()
		if ok {
			return nil
		}
		if err := s.clock.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// Used reports the current window usage.
func (s *Sliding) Used() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(s.clock.Now())
	return len(s.timestamps)
}

// Snapshot returns a copy of the current window for deterministic tests.
func (s *Sliding) Snapshot() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(s.clock.Now())
	return append([]time.Time(nil), s.timestamps...)
}

func (s *Sliding) prune(now time.Time) {
	cutoff := now.Add(-time.Minute)
	i := 0
	for i < len(s.timestamps) && !s.timestamps[i].After(cutoff) {
		i++
	}
	if i > 0 {
		s.timestamps = append([]time.Time(nil), s.timestamps[i:]...)
	}
}

// Sem is a per-provider concurrency semaphore.
type Sem struct {
	ch chan struct{}
}

func NewSem(n int) *Sem {
	if n <= 0 {
		n = 1
	}
	return &Sem{ch: make(chan struct{}, n)}
}

// Acquire waits for a free slot or ctx cancellation. Hammer tests require
// parallel requests to be delayed, not rejected, when concurrency is full.
func (s *Sem) Acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.ch <- struct{}{}:
		return nil
	}
}

func (s *Sem) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Sem) Release() {
	select {
	case <-s.ch:
	default:
	}
}

func (s *Sem) InUse() int { return len(s.ch) }
