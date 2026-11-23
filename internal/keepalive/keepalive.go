package keepalive

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"
)

const (
	// StepMB is the granularity at which the keeper grows or shrinks its resident floor.
	StepMB = 64
	// HysteresisMB is the dead band that stops oscillation at the boundary.
	HysteresisMB = 128
	pageSize     = 4096
)

// Config holds the keeper settings passed as flags by deploy/host/keepalive.service.
type Config struct {
	TargetUsedMB   int
	MinAvailableMB int
	Tick           time.Duration
	StatePath      string
	CPUDuty        CPUDuty
}

// CPUDuty is an optional, disabled-by-default busy loop for the OCI idle rule.
type CPUDuty struct {
	Enabled       bool
	Percent       int
	WindowMinutes int
	StartHourUTC  int
}

// State is the atomically written status file read later by the gateway queue.
type State struct {
	ResidentMB int    `json:"resident_mb"`
	TargetMB   int    `json:"target_mb"`
	Updated    string `json:"updated"`
}

// Keeper holds a genuine resident-memory floor and yields it on demand.
type Keeper struct {
	cfg    Config
	reader MemReader
	heldMB int
	blocks [][]byte
}

// New constructs a Keeper. A nil reader defaults to reading /proc/meminfo.
func New(cfg Config, reader MemReader) *Keeper {
	if reader == nil {
		reader = ProcMemInfo{}
	}
	return &Keeper{cfg: cfg, reader: reader}
}

// Desired returns how many MiB the keeper itself should hold.
// It is pure so it can be exhaustively unit-tested.
func Desired(info MemInfo, ownMB int, cfg Config) (holdMB int) {
	totalMB := int(info.TotalKB / 1024)
	availableMB := int(info.AvailableKB / 1024)

	usedByOthers := totalMB - availableMB - ownMB
	if usedByOthers < 0 {
		usedByOthers = 0
	}

	want := cfg.TargetUsedMB - usedByOthers
	if want < 0 {
		want = 0
	}

	// availableMB already excludes our resident blocks; adding ownMB gives the
	// memory that would be available if we released everything. Never push that
	// below MinAvailableMB because of ourselves.
	capMB := availableMB + ownMB - cfg.MinAvailableMB
	if capMB < 0 {
		capMB = 0
	}
	if want > capMB {
		want = capMB
	}

	// A 128 MiB dead band avoids allocating a few MiB when the host is close to
	// the target; those tiny allocations would otherwise oscillate every tick.
	if want < HysteresisMB {
		want = 0
	}
	return want
}

// Run holds the floor until ctx is cancelled.
func (k *Keeper) Run(ctx context.Context) error {
	if k.cfg.Tick <= 0 {
		return fmt.Errorf("keepalive: tick must be positive")
	}
	if k.cfg.StatePath == "" {
		return fmt.Errorf("keepalive: state path is required")
	}
	k.runCPUDuty(ctx)

	ticker := time.NewTicker(k.cfg.Tick)
	defer ticker.Stop()
	for {
		if err := k.tick(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// tick performs one read/adjust/write cycle. It is unexported so tests can
// drive it directly without waiting on a real clock.
func (k *Keeper) tick(ctx context.Context) error {
	info, err := k.reader.Read()
	if err != nil {
		return fmt.Errorf("keepalive: read memory: %w", err)
	}

	target := Desired(info, k.heldMB, k.cfg)

	// Hysteresis against the current holding: small changes around the current
	// level do not cause a churn of one 64 MiB block per tick.
	if k.heldMB > 0 && target > 0 && abs(target-k.heldMB) < HysteresisMB {
		target = k.heldMB
	}
	target -= target % StepMB

	grew, shrunk := false, false
	for k.heldMB < target {
		block := make([]byte, StepMB*1024*1024)
		for i := 0; i < len(block); i += pageSize {
			block[i] = 1 // touch every page so the memory is genuinely resident
		}
		k.blocks = append(k.blocks, block)
		k.heldMB += StepMB
		grew = true
	}
	for k.heldMB > target && len(k.blocks) > 0 {
		k.blocks = k.blocks[:len(k.blocks)-1]
		k.heldMB -= StepMB
		shrunk = true
	}
	if shrunk {
		runtime.GC()
		debug.FreeOSMemory()
	}
	if grew || shrunk {
		slog.Info("memory floor adjusted",
			"resident_mb", k.heldMB,
			"desired_mb", target,
			"available_mb", int(info.AvailableKB/1024),
		)
	}

	if err := k.writeState(target); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	return nil
}

func (k *Keeper) writeState(target int) error {
	state := State{
		ResidentMB: k.heldMB,
		TargetMB:   target,
		Updated:    time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("keepalive: encode state: %w", err)
	}
	dir := filepath.Dir(k.cfg.StatePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("keepalive: create state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".keepalive-*.tmp")
	if err != nil {
		return fmt.Errorf("keepalive: create temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("keepalive: write state: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("keepalive: chmod state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("keepalive: close state: %w", err)
	}
	if err := os.Rename(tmpName, k.cfg.StatePath); err != nil {
		return fmt.Errorf("keepalive: install state: %w", err)
	}
	return nil
}

func (k *Keeper) runCPUDuty(ctx context.Context) {
	if !k.cfg.CPUDuty.Enabled {
		return
	}
	duty := k.cfg.CPUDuty
	if duty.Percent < 1 || duty.Percent > 100 {
		duty.Percent = 20
	}
	if duty.WindowMinutes < 1 {
		duty.WindowMinutes = 60
	}

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			now := time.Now().UTC()
			inWindow := now.Hour() == duty.StartHourUTC &&
				now.Minute() < duty.WindowMinutes
			if inWindow {
				// Percent ms of work, then the remainder of a 100 ms slice idle.
				burn(time.Duration(duty.Percent)*time.Millisecond, ctx)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(100-duty.Percent) * time.Millisecond):
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func burn(d time.Duration, ctx context.Context) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		for i := 0; i < 10000; i++ {
			_ = i * i
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
