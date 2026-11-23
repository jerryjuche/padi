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
	if got := readState(t, statePath); got.ResidentMB != 128 {
		t.Fatalf("resident after idle tick = %d, want 128", got.ResidentMB)
	}

	// A test process allocates memory: MemAvailable drops and the keeper yields.
	reader.info = MemInfo{TotalKB: testTotalKB, AvailableKB: 1024 * 1024}
	if err := k.tick(context.Background()); err != nil {
		t.Fatalf("busy tick: %v", err)
	}
	if got := readState(t, statePath); got.ResidentMB != 0 {
		t.Fatalf("resident after busy tick = %d, want 0", got.ResidentMB)
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
	return state
}
