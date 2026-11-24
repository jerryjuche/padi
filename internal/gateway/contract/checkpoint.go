package contract

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Runner is the subset of the container runner needed for checkpoints. It is
// deliberately small so checkpointing can be tested without Docker.
type Runner interface {
	Exec(ctx context.Context, id string, cmd ...string) ([]byte, error)
}

// checkpointScript lives in the session image, never in the agent-writable
// workspace. Checkpoints and rollbacks pass only a validated integer to it.
const checkpointScript = "/usr/local/bin/padi-checkpoint"

const (
	checkpointSave     = "save"
	checkpointRollback = "rollback"
)

var (
	ErrInvalidCheckpoint = errors.New("contract: invalid checkpoint")
	ErrCheckpointFailed  = errors.New("contract: checkpoint failed")
)

// Checkpoint asks the fixed container-side script to capture the working tree
// under refs/agent/checkpoints/<n>. It does not move the branch or touch a
// gateway-owned git workspace. The returned ref is trimmed stdout.
func Checkpoint(ctx context.Context, r Runner, containerID string, n int) (ref string, err error) {
	if err := validateCheckpoint(n, r); err != nil {
		return "", err
	}
	out, err := r.Exec(ctx, containerID, checkpointScript, checkpointSave, strconv.Itoa(n))
	if err != nil {
		return "", fmt.Errorf("%w: save %d: %w", ErrCheckpointFailed, n, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Rollback restores the working tree from checkpoint n. No user-controlled
// command text is ever passed to the runner.
func Rollback(ctx context.Context, r Runner, containerID string, n int) error {
	if err := validateCheckpoint(n, r); err != nil {
		return err
	}
	if _, err := r.Exec(ctx, containerID, checkpointScript, checkpointRollback, strconv.Itoa(n)); err != nil {
		return fmt.Errorf("%w: rollback %d: %w", ErrCheckpointFailed, n, err)
	}
	return nil
}

func validateCheckpoint(n int, r Runner) error {
	if n < 0 {
		return fmt.Errorf("%w: n must be non-negative, got %d", ErrInvalidCheckpoint, n)
	}
	if r == nil {
		return fmt.Errorf("%w: no runner", ErrInvalidCheckpoint)
	}
	return nil
}

// Save is an alias for Checkpoint.
func Save(ctx context.Context, r Runner, containerID string, n int) (string, error) {
	return Checkpoint(ctx, r, containerID, n)
}

// Resume is an alias for Rollback.
func Resume(ctx context.Context, r Runner, containerID string, n int) error {
	return Rollback(ctx, r, containerID, n)
}

// Rewind is an alias for Rollback.
func Rewind(ctx context.Context, r Runner, containerID string, n int) error {
	return Rollback(ctx, r, containerID, n)
}
