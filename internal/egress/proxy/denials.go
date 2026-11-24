package proxy

import (
	"sync"
	"time"
)

// Denial is one blocked request, safe to expose on the admin endpoint.
type Denial struct {
	Time      time.Time `json:"time"`
	Host      string    `json:"host"`
	Kind      string    `json:"kind"`
	Reason    string    `json:"reason"`
	SessionID string    `json:"session_id,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
}

// Denials is a bounded in-memory ring of recent denials for the gateway's
// approval poller. It never stores URLs, queries or bodies.
type Denials struct {
	mu    sync.Mutex
	items []Denial
	limit int
	now   func() time.Time
}

// NewDenials creates a store with a bounded number of entries.
func NewDenials(limit int, now func() time.Time) *Denials {
	if limit <= 0 {
		limit = 4096
	}
	if now == nil {
		now = time.Now
	}
	return &Denials{limit: limit, now: now, items: make([]Denial, 0, limit)}
}

// Record adds one denial.
func (d *Denials) Record(kind, host, reason, sessionID, taskID string) {
	if d == nil {
		return
	}
	item := Denial{
		Time:      d.now().UTC(),
		Host:      host,
		Kind:      kind,
		Reason:    reason,
		SessionID: sessionID,
		TaskID:    taskID,
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.items) >= d.limit {
		copy(d.items, d.items[1:])
		d.items[len(d.items)-1] = item
		return
	}
	d.items = append(d.items, item)
}

// Since returns denials at or after the supplied time, oldest first.
func (d *Denials) Since(since time.Time) []Denial {
	if d == nil {
		return []Denial{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Denial, 0, len(d.items))
	for _, item := range d.items {
		if !since.IsZero() && item.Time.Before(since) {
			continue
		}
		out = append(out, item)
	}
	return out
}

// Count returns the number of stored denials.
func (d *Denials) Count() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.items)
}
