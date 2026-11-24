// Package runner executes detected verification commands with a shared
// timeout, captures a redacted output tail, and aggregates the verdict.
package runner

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/jerryjuche/padi/internal/verify/detect"
	"github.com/jerryjuche/padi/internal/verify/secrets"
)

const (
	// DefaultTimeout bounds an entire verification run, not each step.
	DefaultTimeout = 10 * time.Minute
	// MaxTailBytes is the hard cap on a redacted OutputTail.
	MaxTailBytes = 4096
)

// The canonical types live in detect so every verifier package can share them.
type (
	Step    = detect.Step
	Verdict = detect.Verdict
	Runner  = detect.Runner
)

const (
	VerdictPass         = detect.VerdictPass
	VerdictFail         = detect.VerdictFail
	VerdictUnverifiable = detect.VerdictUnverifiable
)

// Detect is re-exported so callers can use the runner package alone.
func Detect(dir string) []Runner { return detect.Detect(dir) }

// Execute runs one runner in dir with the supplied context. The context may
// carry the total verification deadline; Execute does not add a second one.
func Execute(ctx context.Context, r Runner, dir string) Step {
	start := time.Now()
	step := Step{Name: r.Name}
	if ctx == nil {
		ctx = context.Background()
	}
	if r.Command == "" {
		step.OutputTail = "invalid runner: empty command"
		return step
	}

	tail := newTailWriter(MaxTailBytes)
	cmd := exec.CommandContext(ctx, r.Command, r.Args...)
	cmd.Dir = dir
	cmd.Stdout = tail
	cmd.Stderr = tail
	err := cmd.Run()
	step.DurationMS = time.Since(start).Milliseconds()

	out := tail.String()
	if err != nil {
		reason := err.Error()
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			reason = "canceled: " + reason
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			reason = "timeout: " + reason
		}
		if out == "" {
			out = reason
		} else {
			out += "\n" + reason
		}
	}
	step.OutputTail = secrets.CapTail(secrets.Redact(out), MaxTailBytes)
	step.OK = err == nil
	return step
}

// Run executes every detected runner under one timeout. An unrecognized
// project is always "unverifiable" and never "pass". A canceled parent context
// is also unverifiable; a command failure or total timeout is "fail".
func Run(ctx context.Context, dir string, timeout time.Duration) Verdict {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if dir == "" {
		dir = "."
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	runners := detect.Detect(dir)
	verdict := Verdict{
		Verdict: VerdictUnverifiable,
		Steps:   make([]Step, 0, len(runners)),
	}
	if len(runners) == 0 {
		verdict.Steps = append(verdict.Steps, Step{
			Name:       "detect",
			OK:         false,
			OutputTail: "no recognized project (expected go.mod, package.json, pyproject.toml, setup.py, requirements.txt or Cargo.toml)",
		})
		return verdict
	}

	failed := false
	for i, r := range runners {
		if i > 0 && runCtx.Err() != nil {
			verdict.Steps = append(verdict.Steps, Step{
				Name:       r.Name,
				OK:         false,
				OutputTail: "skipped: verification context ended before this step",
			})
			failed = true
			continue
		}
		step := Execute(runCtx, r, dir)
		verdict.Steps = append(verdict.Steps, step)
		if !step.OK {
			failed = true
		}
	}

	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		verdict.Verdict = VerdictUnverifiable
	case failed:
		verdict.Verdict = VerdictFail
	default:
		verdict.Verdict = VerdictPass
	}
	return verdict
}

// tailWriter retains only the last max bytes of everything written to it. It
// is safe for the concurrent stdout and stderr copiers used by os/exec.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailWriter(max int) *tailWriter {
	if max < 0 {
		max = 0
	}
	return &tailWriter{max: max}
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if w.max == 0 {
		return n, nil
	}
	if n >= w.max {
		w.buf = append(w.buf[:0], p[n-w.max:]...)
		return n, nil
	}
	if drop := len(w.buf) + n - w.max; drop > 0 {
		copy(w.buf, w.buf[drop:])
		w.buf = w.buf[:len(w.buf)-drop]
	}
	w.buf = append(w.buf, p...)
	return n, nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
