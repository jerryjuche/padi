package contract

import (
	"strings"
	"sync"
)

// Supported Meter kinds. Wall time is not metered here: the watchdog handles
// Budget.WallMinutes.
const (
	UsageTokens    = "tokens"
	UsageToolCalls = "tool_calls"
	UsageFiles     = "files"
	UsageFixLoops  = "fix_loops"
)

// BudgetState is the budget status reported after each Add and by State.
type BudgetState string

const (
	BudgetOK       BudgetState = "ok"
	BudgetWarn80   BudgetState = "warn80"
	BudgetExceeded BudgetState = "exceeded"

	// State* aliases are provided for callers that prefer that prefix.
	StateOK       = BudgetOK
	StateWarn80   = BudgetWarn80
	StateExceeded = BudgetExceeded
)

// Meter accumulates task usage. It is safe for concurrent use: the gateway
// polls events and usage from different goroutines.
type Meter struct {
	mu       sync.Mutex
	limits   map[string]int
	used     map[string]int
	exceeded bool
}

// NewMeter builds a meter for the four countable budget fields. Zero and
// negative limits mean unlimited.
func NewMeter(b Budget) *Meter {
	limits := map[string]int{
		UsageTokens:    nonNegative(b.MaxTokens),
		UsageToolCalls: nonNegative(b.MaxToolCalls),
		UsageFiles:     nonNegative(b.MaxFilesChanged),
		UsageFixLoops:  nonNegative(b.MaxFixLoops),
	}
	used := make(map[string]int, len(limits))
	for k := range limits {
		used[k] = 0
	}
	return &Meter{limits: limits, used: used}
}

func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// Add records n units of kind and returns the resulting state. n <= 0 and
// unknown kinds are no-ops; once exceeded, the state stays exceeded.
func (m *Meter) Add(kind string, n int) BudgetState {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()

	kind = strings.TrimSpace(kind)
	_, supported := m.limits[kind]
	if !supported {
		return m.stateLocked()
	}
	if n > 0 {
		const maxInt = int(^uint(0) >> 1)
		if m.used[kind] > maxInt-n {
			m.used[kind] = maxInt
		} else {
			m.used[kind] += n
		}
	}
	if state := m.stateLocked(); state == BudgetExceeded {
		m.exceeded = true
	}
	return m.stateLocked()
}

// State reports the current budget state without changing usage.
func (m *Meter) State() BudgetState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked()
}

// Used returns a copy of the per-kind usage map, so callers cannot mutate the
// meter's internal state.
func (m *Meter) Used() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	out := make(map[string]int, len(m.used))
	for k, v := range m.used {
		out[k] = v
	}
	return out
}

func (m *Meter) ensureLocked() {
	if m.limits == nil {
		m.limits = map[string]int{
			UsageTokens:    0,
			UsageToolCalls: 0,
			UsageFiles:     0,
			UsageFixLoops:  0,
		}
	}
	if m.used == nil {
		m.used = make(map[string]int, len(m.limits))
		for k := range m.limits {
			m.used[k] = 0
		}
	}
}

func (m *Meter) stateLocked() BudgetState {
	if m.exceeded {
		return BudgetExceeded
	}
	state := BudgetOK
	for kind, limit := range m.limits {
		if limit <= 0 {
			continue
		}
		used := m.used[kind]
		// 100% is exceeded. The 80% threshold is ceil(4*limit/5),
		// written as limit - floor(limit/5) to avoid overflow.
		if used >= limit {
			return BudgetExceeded
		}
		if used >= limit-limit/5 {
			state = BudgetWarn80
		}
	}
	return state
}
