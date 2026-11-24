package breaker

import (
	"testing"
	"time"
)

func TestTransitionsAndCooldownDoubling(t *testing.T) {
	now := time.Unix(100, 0)
	b := New()
	if !b.Allow(now) {
		t.Fatal("closed breaker should allow")
	}
	for i := 0; i < 2; i++ {
		b.Fail(now.Add(time.Duration(i)*time.Second), 3, time.Second, 10*time.Second)
	}
	if got := b.State(now.Add(2 * time.Second)); got != Closed {
		t.Fatalf("state below threshold=%q", got)
	}
	b.Fail(now.Add(2*time.Second), 3, time.Second, 10*time.Second)
	if got := b.State(now.Add(2500 * time.Millisecond)); got != Open {
		t.Fatalf("state=%q", got)
	}
	if b.Allow(now.Add(2500 * time.Millisecond)) {
		t.Fatal("open breaker should refuse")
	}

	probeAt := now.Add(4 * time.Second) // cooldown 1s from last failure
	if !b.Allow(probeAt) || b.State(probeAt) != HalfOpen {
		t.Fatal("expired open breaker should allow exactly one half-open probe")
	}
	if b.Allow(probeAt) {
		t.Fatal("half-open breaker should refuse a second probe")
	}
	b.Fail(probeAt, 3, time.Second, 10*time.Second)
	if got := b.state; got != Open {
		t.Fatalf("after failed probe state=%q", got)
	}
	if got := b.Cooldown(); got != 2*time.Second {
		t.Fatalf("cooldown=%v want 2s", got)
	}
	if b.State(probeAt.Add(time.Second)) != Open {
		t.Fatal("reopened breaker should still be open before its cooldown ends")
	}
	if !b.Allow(probeAt.Add(3 * time.Second)) {
		t.Fatal("expired reopened breaker should allow a probe")
	}
	b.Success()
	if got := b.State(probeAt); got != Closed || b.Cooldown() != 0 {
		t.Fatalf("after success state=%q cooldown=%v", got, b.Cooldown())
	}
}

func TestCooldownIsCapped(t *testing.T) {
	now := time.Unix(0, 0)
	b := New()
	for i := 0; i < 3; i++ {
		b.Fail(now, 3, time.Second, 5*time.Second)
	}
	point := now.Add(time.Second)
	for _, want := range []time.Duration{2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second} {
		if !b.Allow(point) {
			t.Fatalf("probe at %v should be allowed", point)
		}
		b.Fail(point, 3, time.Second, 5*time.Second)
		if got := b.Cooldown(); got != want {
			t.Fatalf("cooldown=%v want %v", got, want)
		}
		point = point.Add(want)
	}
}
