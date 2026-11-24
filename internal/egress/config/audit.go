package config

import (
	"io"
	"log/slog"
	"sync/atomic"
)

// Audit writes one structured JSON line per request. It never receives bodies,
// query values or credential values.
type Audit struct{ log *slog.Logger }

// NewAudit creates a JSON-line audit logger writing to w.
func NewAudit(w io.Writer) *Audit {
	if w == nil {
		return &Audit{}
	}
	return &Audit{log: slog.New(slog.NewJSONHandler(w, nil))}
}

// Line records one request. Keep fields limited to stable identifiers and
// reason codes; host is a DNS name without path or query.
func (a *Audit) Line(kind, host, sessionID, taskID, verdict, reason string, bytes int64) {
	if a == nil || a.log == nil {
		return
	}
	attrs := []any{
		"kind", kind,
		"host", host,
		"session", sessionID,
		"task", taskID,
		"bytes", bytes,
		"verdict", verdict,
	}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	a.log.Info("egress_request", attrs...)
}

// Counters are process-lifetime counters exposed by /status. They contain no
// secrets and are safe for concurrent use.
type Counters struct {
	ProxyAllow     atomic.Uint64
	ProxyDeny      atomic.Uint64
	ResearchFetch  atomic.Uint64
	ResearchSearch atomic.Uint64
	ResearchDeny   atomic.Uint64
	AdminRequests  atomic.Uint64
	Credential501  atomic.Uint64
}

// Snapshot returns a plain map for JSON encoding.
func (c *Counters) Snapshot() map[string]uint64 {
	if c == nil {
		return map[string]uint64{}
	}
	return map[string]uint64{
		"proxy_allow":     c.ProxyAllow.Load(),
		"proxy_deny":      c.ProxyDeny.Load(),
		"research_fetch":  c.ResearchFetch.Load(),
		"research_search": c.ResearchSearch.Load(),
		"research_deny":   c.ResearchDeny.Load(),
		"admin_requests":  c.AdminRequests.Load(),
		"credential_501":  c.Credential501.Load(),
	}
}
