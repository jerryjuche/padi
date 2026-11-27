package telegram

import (
	"strings"
	"testing"
	"time"
)

func TestLiveTracePhasesAndEvents(t *testing.T) {
	trace := NewLiveTrace("task-12345678", "owner/repo", "build")
	trace.SetPhase("contract", "Contract approval")
	trace.SetPhase("clone", "Cloning repository")
	trace.SetPhase("sandbox", "Starting sandbox")
	trace.SetPhase("execute", "Agent executing")
	trace.Add("tool", "shell", "read README.md")
	trace.Add("file", "README.md", "changed")
	trace.SetPhase("done", "Done")
	trace.AddTokens(42)

	snap := trace.Snapshot()
	if snap.Phase != "done" || snap.PhaseIndex != len(livePhases)-1 {
		t.Fatalf("phase=%q idx=%d", snap.Phase, snap.PhaseIndex)
	}
	if snap.Tokens != 42 || len(snap.Events) < 7 {
		t.Fatalf("tokens=%d events=%d", snap.Tokens, len(snap.Events))
	}

	card := RenderLiveCard(snap, "answer <b>text</b>", 3)
	for _, want := range []string{"PADI / LIVE TRACE", "DONE", "EXECUTE", "README.md", "42 tokens"} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q: %s", want, card)
		}
	}
	if strings.Contains(card, "<b>text</b>") {
		t.Fatalf("draft was not escaped: %s", card)
	}
	if strings.Count(card, "✓") < 6 || !strings.Contains(card, "▶ DONE") {
		t.Fatalf("phase track missing: %s", card)
	}

	// Regression: a stale earlier phase cannot move the track backwards.
	trace.SetPhase("clone", "Cloning repository")
	if got := trace.Snapshot().Phase; got != "done" {
		t.Fatalf("phase moved backwards to %q", got)
	}
}

func TestLiveTracePulseShowsRetryError(t *testing.T) {
	trace := NewLiveTrace("t", "owner/repo", "build")
	trace.SetPulse(31, "getaddrinfo EAI_AGAIN egress-proxy")
	card := RenderLiveCard(trace.Snapshot(), "", 1)
	for _, want := range []string{"♥ alive", "retry 31", "EAI_AGAIN"} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q: %s", want, card)
		}
	}
	if got := len(trace.Snapshot().Events); got != 0 {
		t.Fatalf("pulse added %d events", got)
	}
}

func TestLiveTraceBoundsMemory(t *testing.T) {
	trace := NewLiveTrace("t", "r", "plan")
	for i := 0; i < liveMaxEvents+50; i++ {
		trace.Add("status", "tick", "")
	}
	if got := len(trace.Snapshot().Events); got != liveMaxEvents {
		t.Fatalf("events=%d want %d", got, liveMaxEvents)
	}
	// A zero time must render safely rather than producing a huge elapsed value.
	card := RenderLiveCard(LiveSnapshot{Started: time.Time{}, Repo: "r", Mode: "plan"}, "", 1)
	if !strings.Contains(card, "LIVE TRACE") {
		t.Fatalf("unexpected card: %s", card)
	}
}
