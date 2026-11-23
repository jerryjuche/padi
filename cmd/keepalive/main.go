package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jerryjuche/padi/internal/keepalive"
)

func main() {
	targetUsedMB := flag.Int("target-used-mb", 4096, "total used memory to maintain, in MiB")
	minAvailableMB := flag.Int("min-available-mb", 2500, "never let MemAvailable fall below this because of us")
	tick := flag.Duration("tick", 30*time.Second, "adjustment interval")
	statePath := flag.String("state", "/srv/agent/state/keepalive.json", "atomic state file path")
	cpuDuty := flag.Bool("cpu-duty", false, "enable the optional CPU duty cycle (off by default)")
	cpuPercent := flag.Int("cpu-duty-percent", 20, "percent of one core during the duty window")
	cpuWindow := flag.Int("cpu-duty-window-minutes", 60, "length of the duty window")
	cpuStartHour := flag.Int("cpu-duty-start-hour-utc", 3, "UTC start hour of the duty window")
	flag.Parse()

	if *targetUsedMB <= 0 {
		fmt.Fprintln(os.Stderr, "--target-used-mb must be positive")
		os.Exit(2)
	}
	if *minAvailableMB < 0 {
		fmt.Fprintln(os.Stderr, "--min-available-mb must not be negative")
		os.Exit(2)
	}
	if *tick <= 0 {
		fmt.Fprintln(os.Stderr, "--tick must be positive")
		os.Exit(2)
	}
	if *statePath == "" {
		fmt.Fprintln(os.Stderr, "--state is required")
		os.Exit(2)
	}
	if *cpuPercent < 1 || *cpuPercent > 100 {
		fmt.Fprintln(os.Stderr, "--cpu-duty-percent must be between 1 and 100")
		os.Exit(2)
	}
	if *cpuWindow < 1 || *cpuWindow > 60 {
		fmt.Fprintln(os.Stderr, "--cpu-duty-window-minutes must be between 1 and 60")
		os.Exit(2)
	}
	if *cpuStartHour < 0 || *cpuStartHour > 23 {
		fmt.Fprintln(os.Stderr, "--cpu-duty-start-hour-utc must be between 0 and 23")
		os.Exit(2)
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg := keepalive.Config{
		TargetUsedMB:   *targetUsedMB,
		MinAvailableMB: *minAvailableMB,
		Tick:           *tick,
		StatePath:      *statePath,
		CPUDuty: keepalive.CPUDuty{
			Enabled:       *cpuDuty,
			Percent:       *cpuPercent,
			WindowMinutes: *cpuWindow,
			StartHourUTC:  *cpuStartHour,
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("keepalive starting",
		"target_used_mb", cfg.TargetUsedMB,
		"min_available_mb", cfg.MinAvailableMB,
		"tick", cfg.Tick.String(),
		"state", cfg.StatePath,
	)
	if err := keepalive.New(cfg, nil).Run(ctx); err != nil {
		slog.Error("keepalive stopped", "error", err)
		os.Exit(1)
	}
	slog.Info("keepalive stopped cleanly")
}
