package router

import (
	"fmt"
	"sync"
	"testing"

	"github.com/jerryjuche/padi/internal/router/config"
)

func TestCostMicrosTable(t *testing.T) {
	p := config.Provider{CostPer1MInMicros: 2_000_000, CostPer1MOutMicros: 8_000_000}
	for _, tc := range []struct {
		name       string
		in, out    int
		wantMicros int64
	}{
		{"zero", 0, 0, 0},
		{"input only", 1_000_000, 0, 2_000_000},
		{"output only", 0, 1_000_000, 8_000_000},
		{"mixed", 500_000, 250_000, 3_000_000},
		{"unpriced", 100, 100, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := p
			if tc.name == "unpriced" {
				provider = config.Provider{}
			}
			if got := costMicros(tc.in, tc.out, provider); got != tc.wantMicros {
				t.Fatalf("cost=%d want %d", got, tc.wantMicros)
			}
		})
	}
}

func TestMemoryRecorderConcurrentRows(t *testing.T) {
	rec := &MemoryRecorder{}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			rec.Record(UsageRow{Alias: "agent-chat", Provider: "fake", Status: 200, QuotaTokens: n})
		}(i)
	}
	wg.Wait()
	rows := rec.Rows()
	if len(rows) != 32 {
		t.Fatalf("rows=%d want 32", len(rows))
	}
	for _, row := range rows {
		line := fmt.Sprintf("%+v", row)
		if line == "" {
			t.Fatal("empty row")
		}
	}
}
