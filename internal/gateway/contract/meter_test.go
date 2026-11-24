package contract

import (
	"sync"
	"testing"
)

func TestMeterBudgetStates(t *testing.T) {
	type step struct {
		kind string
		n    int
		want BudgetState
	}
	tests := []struct {
		name   string
		budget Budget
		steps  []step
	}{
		{
			name:   "tokens warn at 80 and stick at exceeded",
			budget: Budget{MaxTokens: 100},
			steps: []step{
				{kind: UsageTokens, n: 79, want: BudgetOK},
				{kind: UsageTokens, n: 1, want: BudgetWarn80},
				{kind: UsageTokens, n: 19, want: BudgetWarn80},
				{kind: UsageTokens, n: 1, want: BudgetExceeded},
				{kind: UsageTokens, n: -5, want: BudgetExceeded},
				{kind: "wall_minutes", n: 100, want: BudgetExceeded},
			},
		},
		{
			name:   "tool calls",
			budget: Budget{MaxToolCalls: 200},
			steps: []step{
				{kind: UsageToolCalls, n: 159, want: BudgetOK},
				{kind: UsageToolCalls, n: 1, want: BudgetWarn80},
				{kind: UsageToolCalls, n: 40, want: BudgetExceeded},
			},
		},
		{
			name:   "files",
			budget: Budget{MaxFilesChanged: 40},
			steps: []step{
				{kind: UsageFiles, n: 31, want: BudgetOK},
				{kind: UsageFiles, n: 1, want: BudgetWarn80},
				{kind: UsageFiles, n: 8, want: BudgetExceeded},
			},
		},
		{
			name:   "fix loops",
			budget: Budget{MaxFixLoops: 3},
			steps: []step{
				{kind: UsageFixLoops, n: 2, want: BudgetOK},
				{kind: UsageFixLoops, n: 1, want: BudgetExceeded},
			},
		},
		{
			name:   "one exceeded kind poisons the whole meter",
			budget: Budget{MaxTokens: 100, MaxFixLoops: 3},
			steps: []step{
				{kind: UsageTokens, n: 80, want: BudgetWarn80},
				{kind: UsageFixLoops, n: 3, want: BudgetExceeded},
				{kind: UsageTokens, n: 0, want: BudgetExceeded},
			},
		},
		{
			name:   "zero budgets are unlimited",
			budget: Budget{},
			steps: []step{
				{kind: UsageTokens, n: 1000000, want: BudgetOK},
				{kind: UsageFiles, n: 1000000, want: BudgetOK},
				{kind: "unknown", n: 1000000, want: BudgetOK},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMeter(tc.budget)
			for i, s := range tc.steps {
				if got := m.Add(s.kind, s.n); got != s.want {
					t.Fatalf("step %d: Add(%q, %d) = %q, want %q", i, s.kind, s.n, got, s.want)
				}
				if got := m.State(); got != s.want {
					t.Fatalf("step %d: State() = %q, want %q", i, got, s.want)
				}
			}
			if got := string(m.State()); got != string(tc.steps[len(tc.steps)-1].want) {
				t.Fatalf("final state = %q", got)
			}
		})
	}
}

func TestMeterUsedReturnsCopy(t *testing.T) {
	m := NewMeter(Budget{MaxTokens: 100})
	m.Add(UsageTokens, 3)
	snapshot := m.Used()
	snapshot[UsageTokens] = 99
	snapshot["injected"] = 7
	if got := m.Used()[UsageTokens]; got != 3 {
		t.Fatalf("Used() must be a copy, got %d", got)
	}
	if _, ok := m.Used()["injected"]; ok {
		t.Fatal("Used() leaked a caller-added key")
	}
}

func TestMeterConcurrentUse(t *testing.T) {
	const (
		workers = 8
		adds    = 100
	)
	m := NewMeter(Budget{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < adds; j++ {
				m.Add(UsageTokens, 1)
				_ = m.State()
				_ = m.Used()
			}
		}()
	}
	wg.Wait()
	if got := m.Used()[UsageTokens]; got != workers*adds {
		t.Fatalf("used tokens = %d, want %d", got, workers*adds)
	}
	if got := m.State(); got != BudgetOK {
		t.Fatalf("state = %q, want %q", got, BudgetOK)
	}
}
