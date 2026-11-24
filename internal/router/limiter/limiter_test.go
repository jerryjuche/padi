package limiter

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestTryReturnsRetryAfter(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	s := NewSliding(2, clock)
	for i := 0; i < 2; i++ {
		if wait, ok := s.Try(); !ok || wait != 0 {
			t.Fatalf("call %d wait=%v ok=%v", i, wait, ok)
		}
	}
	wait, ok := s.Try()
	if ok || wait != time.Minute {
		t.Fatalf("wait=%v ok=%v want 1m/false", wait, ok)
	}
}

func TestSlidingNeverExceedsRPMUnderConcurrency(t *testing.T) {
	const rpm = 5
	const workers = 40
	clock := NewFakeClock(time.Unix(0, 0))
	s := NewSliding(rpm, clock)

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Wait(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	stamps := s.Snapshot()
	for i, start := range stamps {
		n := 0
		for _, ts := range stamps[i:] {
			if !ts.Before(start) && ts.Before(start.Add(time.Minute)) {
				n++
			}
		}
		if n > rpm {
			t.Fatalf("window from %v contains %d requests, want <= %d", start, n, rpm)
		}
	}
}

func TestSemAcquireWaitsForRelease(t *testing.T) {
	sem := NewSem(1)
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sem.Acquire(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("acquired before release: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	sem.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("still blocked after release")
	}
}

func TestSemConcurrency(t *testing.T) {
	sem := NewSem(2)
	if !sem.TryAcquire() || !sem.TryAcquire() || sem.TryAcquire() {
		t.Fatal("semaphore should allow exactly two holders")
	}
	sem.Release()
	if !sem.TryAcquire() {
		t.Fatal("release should free one slot")
	}
}
