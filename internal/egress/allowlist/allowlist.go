// Package allowlist holds the static build allowlist and runtime per-task
// approvals granted by the gateway admin endpoint.
package allowlist

import (
	"strings"
	"sync"
	"time"

	"github.com/jerryjuche/padi/internal/egress/config"
)

// List is a thread-safe allowlist. Entries are canonical DNS hosts; wildcard
// entries are "*.suffix" and match one or more subdomain labels, not the apex.
type List struct {
	mu      sync.RWMutex
	static  map[string]struct{}
	suffix  []string
	runtime map[string]map[string]time.Time // taskID -> host -> expiry
}

// New builds a list from exact hosts and "*.suffix" patterns.
func New(entries []string) *List {
	l := &List{
		static:  make(map[string]struct{}, len(entries)),
		suffix:  make([]string, 0),
		runtime: make(map[string]map[string]time.Time),
	}
	for _, e := range entries {
		e = config.CanonicalHost(e)
		if strings.HasPrefix(e, "*.") {
			l.suffix = append(l.suffix, strings.TrimPrefix(e, "*."))
			continue
		}
		l.static[e] = struct{}{}
	}
	return l
}

// Allowed reports whether host is statically allowed or has an unexpired
// runtime approval for taskID at the supplied time.
func (l *List) Allowed(host, taskID string, now time.Time) bool {
	if l == nil {
		return false
	}
	host = config.CanonicalHost(host)
	taskID = strings.TrimSpace(taskID)
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.static[host]; ok {
		return true
	}
	for _, suffix := range l.suffix {
		if strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	if taskID == "" {
		return false
	}
	byHost, ok := l.runtime[taskID]
	if !ok {
		return false
	}
	until, ok := byHost[host]
	return ok && now.Before(until)
}

// Approve grants host to taskID until the supplied time.
func (l *List) Approve(host, taskID string, until time.Time) {
	if l == nil {
		return
	}
	host = config.CanonicalHost(host)
	taskID = strings.TrimSpace(taskID)
	if host == "" || taskID == "" || until.IsZero() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	byHost := l.runtime[taskID]
	if byHost == nil {
		byHost = make(map[string]time.Time)
		l.runtime[taskID] = byHost
	}
	byHost[host] = until.UTC()
}

// Revoke removes a runtime approval. Static entries are never affected.
func (l *List) Revoke(host, taskID string) {
	if l == nil {
		return
	}
	host = config.CanonicalHost(host)
	taskID = strings.TrimSpace(taskID)
	l.mu.Lock()
	defer l.mu.Unlock()
	if byHost := l.runtime[taskID]; byHost != nil {
		delete(byHost, host)
		if len(byHost) == 0 {
			delete(l.runtime, taskID)
		}
	}
}

// RemoveTask drops every runtime approval for a finished task.
func (l *List) RemoveTask(taskID string) {
	if l == nil {
		return
	}
	taskID = strings.TrimSpace(taskID)
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.runtime, taskID)
}

// StaticCount returns the number of exact static allowlist entries.
func (l *List) StaticCount() int {
	if l == nil {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.static)
}

// RuntimeCount returns the number of approvals that are still valid at now.
func (l *List) RuntimeCount(now time.Time) int {
	if l == nil {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := 0
	for _, byHost := range l.runtime {
		for _, until := range byHost {
			if now.Before(until) {
				n++
			}
		}
	}
	return n
}
