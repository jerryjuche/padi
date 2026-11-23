package keepalive

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	testTotalKB     = 12 * 1024 * 1024
	testAvailableKB = 12 * 1024 * 1024
)

type fakeReader struct{ info MemInfo }

func (f *fakeReader) Read() (MemInfo, error) { return f.info, nil }

func TestDesiredTable(t *testing.T) {
	tests := []struct {
		name        string
		totalKB     int64
		availableKB int64
		ownMB       int
		cfg         Config
		want        int
	}{
		{
			name:        "idle host holds the target",
			totalKB:     testTotalKB,
			availableKB: testAvailableKB,
			cfg:         Config{TargetUsedMB: 4096, MinAvailableMB: 2500},
			want:        4096,
		},
		{
			name:        "busy host holds zero",
			totalKB:     testTotalKB,
			availableKB: 2 * 1024 * 1024,
			cfg:         Config{TargetUsedMB: 4096, MinAvailableMB: 2500},
			want:        0,
		},
		{
			name:        "low MemAvailable shrinks below the cap",
			totalKB:     6000 * 1024,
			availableKB: 2600 * 1024,
			ownMB:       2000,
			cfg:         Config{TargetUsedMB: 4096, MinAvailableMB: 2500},
			want:        2100,
		},
		{
			name:        "tiny shortfall is inside the dead band",
			totalKB:     testTotalKB,
			availableKB: (12*1024 - 4096 + 96) * 1024,
			cfg:         Config{TargetUsedMB: 4096, MinAvailableMB: 2500},
			want:        0,
		},
		{
			name:        "own memory counts as available if released",
			totalKB:     testTotalKB,
			availableKB: (12*1024 - 2048) * 1024,
			ownMB:       2048,
			cfg:         Config{TargetUsedMB: 4096, MinAvailableMB: 2500},
			want:        4096,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Desired(MemInfo{TotalKB: tc.totalKB, AvailableKB: tc.availableKB}, tc.ownMB, tc.cfg)
			if got != tc.want {
				t.Fatalf("Desired() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDesiredNoOscillationNearTarget(t *testing.T) {
	cfg := Config{TargetUsedMB: 4096, MinAvailableMB: 2500}
	for usedByOthers := cfg.TargetUsedMB - 64; usedByOthers <= cfg.TargetUsedMB+64; usedByOthers++ {
		available := int64(testTotalKB/1024-usedByOthers) * 1024
		got := Desired(MemInfo{TotalKB: testTotalKB, AvailableKB: available}, 0, cfg)
		if got != 0 {
			t.Fatalf("usedByOthers=%d: hold=%d, want 0 (dead band)", usedByOthers, got)
		}
	}
}

func TestKeeperGrowsThenYields(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state", "keepalive.json")
	reader := &fakeReader{info: MemInfo{TotalKB: testTotalKB, AvailableKB: testAvailableKB}}
	cfg := Config{
		TargetUsedMB:   128,
		MinAvailableMB: 100,
		Tick:           time.Hour,
		StatePath:      statePath,
	}
	k := New(cfg, reader)

	if err := k.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if got := readState(t, statePath); got.ResidentMB != 128 || got.TargetMB != 128 {
		t.Fatalf("state after idle tick = %+v, want resident/target 128", got)
	}

	// A test process allocates memory: MemAvailable drops and the keeper yields.
	reader.info = MemInfo{TotalKB: testTotalKB, AvailableKB: 1024 * 1024}
	if err := k.tick(context.Background()); err != nil {
		t.Fatalf("busy tick: %v", err)
	}
	if got := readState(t, statePath); got.ResidentMB != 0 || got.TargetMB != 0 {
		t.Fatalf("state after busy tick = %+v, want resident/target 0", got)
	}
	if k.heldMB != 0 || len(k.blocks) != 0 {
		t.Fatalf("keeper still holds %d MB in %d blocks", k.heldMB, len(k.blocks))
	}
}

func TestKeeperHysteresisAvoidsOneBlockChurn(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "keepalive.json")
	reader := &fakeReader{info: MemInfo{TotalKB: testTotalKB, AvailableKB: testAvailableKB}}
	cfg := Config{TargetUsedMB: 256, MinAvailableMB: 100, Tick: time.Hour, StatePath: statePath}
	k := New(cfg, reader)

	if err := k.tick(context.Background()); err != nil {
		t.Fatalf("initial tick: %v", err)
	}
	// Drop MemAvailable by 64 MiB: the new desire is 192 MiB, only one step
	// away, so the keeper must not churn a block.
	reader.info = MemInfo{TotalKB: testTotalKB, AvailableKB: (12*1024 - 64) * 1024}
	if err := k.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := k.heldMB; got != 256 {
		t.Fatalf("held = %d MB, want 256 (hysteresis)", got)
	}
}

func TestHysteresisRespectsSafetyCap(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "keepalive.json")
	// Desired is capped at 4032 MiB; held is 4096 MiB, so hysteresis would
	// otherwise round target back up and push MemAvailable below the floor.
	info := MemInfo{TotalKB: 6400 * 1024, AvailableKB: 2436 * 1024}
	cfg := Config{TargetUsedMB: 4096, MinAvailableMB: 2500, Tick: time.Hour, StatePath: statePath}
	k := New(cfg, &fakeReader{info: info})
	k.heldMB = 4096
	k.blocks = make([][]byte, 4096/StepMB)
	if got := Desired(info, k.heldMB, cfg); got != 4032 {
		t.Fatalf("Desired() = %d, want 4032", got)
	}

	if err := k.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := k.heldMB; got != 4032 {
		t.Fatalf("held = %d MB, want 4032 (cap beats hysteresis)", got)
	}
	if got := readState(t, statePath); got.TargetMB != 4032 {
		t.Fatalf("state target = %d MB, want 4032", got.TargetMB)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "keepalive.json")
	cfg := Config{TargetUsedMB: 128, MinAvailableMB: 100, Tick: time.Hour, StatePath: statePath}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(cfg, &fakeReader{info: MemInfo{TotalKB: testTotalKB, AvailableKB: testAvailableKB}}).Run(ctx); err != nil {
		t.Fatalf("Run after cancel: %v", err)
	}
}

func TestProcMemInfoParsesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	body := "MemTotal:       12582912 kB\nMemFree:         1234567 kB\nMemAvailable:   11534336 kB\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FileMemInfo{Path: path}.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.TotalKB != 12582912 || got.AvailableKB != 11534336 {
		t.Fatalf("got %+v", got)
	}
}

func TestInDutyWindow(t *testing.T) {
	duty := CPUDuty{StartHourUTC: 3, WindowMinutes: 15}
	tests := []struct {
		at   time.Time
		want bool
	}{
		{time.Date(2026, 11, 23, 2, 59, 0, 0, time.UTC), false},
		{time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC), true},
		{time.Date(2026, 11, 23, 3, 14, 59, 0, time.UTC), true},
		{time.Date(2026, 11, 23, 3, 15, 0, 0, time.UTC), false},
		{time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC), false},
		// Non-UTC input must still be evaluated in UTC (04:01+01:00 = 03:01Z).
		{time.Date(2026, 11, 23, 4, 1, 0, 0, time.FixedZone("X", 3600)), true},
	}
	for _, tc := range tests {
		if got := inDutyWindow(tc.at, duty); got != tc.want {
			t.Errorf("inDutyWindow(%s) = %v, want %v", tc.at, got, tc.want)
		}
	}
}

func TestBurnStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if burn(5*time.Second, ctx) {
		t.Fatal("burn ignored a cancelled context")
	}
}

func TestCapFor(t *testing.T) {
	info := MemInfo{TotalKB: 12288 * 1024, AvailableKB: 2404 * 1024}
	cfg := Config{MinAvailableMB: 2500}
	if got := capFor(info, 4096, cfg); got != 4000 {
		t.Fatalf("capFor() = %d, want 4000", got)
	}
	if got := capFor(info, 0, cfg); got != 0 {
		t.Fatalf("capFor() with no holding = %d, want 0", got)
	}
}

func TestWriteStateErrorPropagates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state-is-a-directory")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{TargetUsedMB: 128, MinAvailableMB: 100, Tick: time.Hour, StatePath: path}
	reader := &fakeReader{info: MemInfo{TotalKB: testTotalKB, AvailableKB: testAvailableKB}}
	if err := New(cfg, reader).tick(context.Background()); err == nil {
		t.Fatal("tick should fail when the state path cannot be replaced")
	}
}

func readState(t *testing.T, path string) State {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.Updated == "" {
		t.Fatal("state has no updated timestamp")
	}
	if _, err := time.Parse(time.RFC3339, state.Updated); err != nil {
		t.Fatalf("state timestamp %q is not RFC3339: %v", state.Updated, err)
	}
	if state.TargetMB < 0 {
		t.Fatalf("state target is negative: %d", state.TargetMB)
	}
	return state
}
