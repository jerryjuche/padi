package allowlist

import (
	"testing"
	"time"
)

func TestAllowedStaticAndWildcard(t *testing.T) {
	l := New([]string{"registry.npmjs.org", "*.githubusercontent.com"})
	now := time.Unix(1700000000, 0).UTC()
	tests := []struct {
		name  string
		host  string
		allow bool
	}{
		{"exact", "registry.npmjs.org", true},
		{"exact_case_and_dot", "REGISTRY.NPMJS.ORG.", true},
		{"wildcard_subdomain", "raw.githubusercontent.com", true},
		{"wildcard_deep", "a.b.githubusercontent.com", true},
		{"wildcard_apex_excluded", "githubusercontent.com", false},
		{"suffix_lookalike", "evilgithubusercontent.com", false},
		{"unknown", "example.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l.Allowed(tt.host, "task-1", now); got != tt.allow {
				t.Fatalf("Allowed(%q) = %v, want %v", tt.host, got, tt.allow)
			}
		})
	}
}

func TestRuntimeApprovalExpiryAndRevoke(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	l := New(nil)
	if l.Allowed("private.example.com", "task-1", now) {
		t.Fatal("unapproved host allowed")
	}
	l.Approve("private.example.com", "task-1", now.Add(time.Minute))
	if !l.Allowed("PRIVATE.EXAMPLE.COM.", "task-1", now.Add(30*time.Second)) {
		t.Fatal("approved host not allowed")
	}
	if l.Allowed("private.example.com", "task-2", now.Add(30*time.Second)) {
		t.Fatal("approval leaked to another task")
	}
	if l.Allowed("private.example.com", "task-1", now.Add(2*time.Minute)) {
		t.Fatal("expired approval still allowed")
	}
	l.Approve("private.example.com", "task-1", now.Add(time.Hour))
	l.Revoke("private.example.com", "task-1")
	if l.Allowed("private.example.com", "task-1", now.Add(time.Minute)) {
		t.Fatal("revoked approval still allowed")
	}
}

func TestCounts(t *testing.T) {
	l := New([]string{"a.example", "b.example"})
	if l.StaticCount() != 2 {
		t.Fatalf("StaticCount = %d", l.StaticCount())
	}
	now := time.Unix(1700000000, 0).UTC()
	l.Approve("c.example", "task", now.Add(time.Hour))
	l.Approve("d.example", "task", now.Add(time.Second))
	if got := l.RuntimeCount(now); got != 2 {
		t.Fatalf("RuntimeCount = %d, want 2", got)
	}
	if got := l.RuntimeCount(now.Add(time.Minute)); got != 1 {
		t.Fatalf("RuntimeCount after expiry = %d, want 1", got)
	}
}
