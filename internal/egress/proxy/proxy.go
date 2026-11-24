package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jerryjuche/padi/internal/egress/allowlist"
	"github.com/jerryjuche/padi/internal/egress/config"
)

// requestInfo carries only identifier fields safe for audit logging.
type requestInfo struct {
	Host      string
	SessionID string
	TaskID    string
}

// Proxy serves the build proxy on port 3128.
type Proxy struct {
	cfg      *config.Config
	allow    *allowlist.List
	denials  *Denials
	counters *config.Counters
	audit    *config.Audit
	limits   *Limits
	resolver Resolver
	dialIP   DialIPFunc
	now      func() time.Time
}

// Option customises dependencies, primarily for tests.
type Option func(*Proxy)

// WithResolver injects a DNS resolver.
func WithResolver(r Resolver) Option { return func(p *Proxy) { p.resolver = r } }

// WithDialIP injects a dialer that receives an already checked IP.
func WithDialIP(d DialIPFunc) Option { return func(p *Proxy) { p.dialIP = d } }

// WithClock injects a clock for TTL and rate-limit tests.
func WithClock(now func() time.Time) Option { return func(p *Proxy) { p.now = now } }

// New builds a proxy. All state is instance-local.
func New(cfg *config.Config, allow *allowlist.List, denials *Denials, counters *config.Counters, audit *config.Audit, opts ...Option) *Proxy {
	p := &Proxy{
		cfg:      cfg,
		allow:    allow,
		denials:  denials,
		counters: counters,
		audit:    audit,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}
	p.limits = NewLimits(cfg.Limits.SessionBytesMB, cfg.Limits.SessionConnsPerMin, p.now)
	return p
}

// ParseHostPort is the strict CONNECT authority parser required by the spec.
func ParseHostPort(raw string) (host, port string, err error) {
	return config.ParseHostPort(raw)
}

func infoFor(r *http.Request) requestInfo {
	host := r.Host
	if r.URL != nil && r.URL.Host != "" {
		host = r.URL.Host
	}
	host, _, err := net.SplitHostPort(host)
	if err != nil {
		host = r.Host
	}
	return requestInfo{
		Host:      safeField(host, 253),
		SessionID: safeField(r.Header.Get("X-Session-Id"), 128),
		TaskID:    safeField(r.Header.Get("X-Task-Id"), 128),
	}
}

func safeField(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		return s[:max]
	}
	return s
}

// ServeHTTP implements both CONNECT and absolute-form plain forwarding.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	info := infoFor(r)
	verdict, reason := "deny", "bad_request"
	var n int64
	defer func() {
		if verdict == "allow" {
			p.counters.ProxyAllow.Add(1)
		} else {
			p.counters.ProxyDeny.Add(1)
		}
		p.audit.Line("build_proxy", info.Host, info.SessionID, info.TaskID, verdict, reason, n)
	}()

	if r.Method == http.MethodConnect {
		n, verdict, reason = p.connect(w, r, info)
		return
	}
	n, verdict, reason = p.forward(w, r, info)
}

func (p *Proxy) guarded(info requestInfo) *GuardedDialer {
	return &GuardedDialer{
		Resolver: p.resolver,
		DialIP:   p.dialIP,
		Ports:    p.cfg.Ports,
		DenyNets: p.cfg.DenyNets,
		OnDeny: func(host, reason string) {
			p.recordDenial("build_proxy", host, reason, info)
		},
	}
}

func (p *Proxy) recordDenial(kind, host, reason string, info requestInfo) {
	if host != "" {
		info.Host = safeField(host, 253)
	}
	p.denials.Record(kind, info.Host, reason, info.SessionID, info.TaskID)
}

func writeProblem(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"code":%q,"message":%q}`, safeField(code, 64), safeField(message, 256))
}

func writeDeny(w http.ResponseWriter, info requestInfo, status int, reason string) {
	w.Header().Set("X-Egress-Deny", safeField(info.Host, 253))
	code := "egress_denied"
	message := "Host blocked by egress policy."
	if status == http.StatusTooManyRequests {
		code = "rate_limited"
		message = "Session egress rate limit reached."
	}
	writeProblem(w, status, code, message)
}

func denyReason(err error) string {
	var de *config.DenyError
	if errors.As(err, &de) {
		return de.Reason
	}
	return "upstream_error"
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, info requestInfo) (int64, string, string) {
	if r.URL == nil || r.URL.Scheme == "" || r.URL.Host == "" {
		p.recordDenial("build_proxy", info.Host, "bad_url", info)
		writeDeny(w, info, http.StatusForbidden, "bad_url")
		return 0, "deny", "bad_url"
	}
	info.Host = config.CanonicalHost(r.URL.Hostname())
	raw := r.URL.String()
	v := config.CheckURL(raw, p.cfg.URLLimits())
	if !v.Allow {
		p.recordDenial("build_proxy", info.Host, v.Reason, info)
		writeDeny(w, info, http.StatusForbidden, v.Reason)
		return 0, "deny", v.Reason
	}
	port := r.URL.Port()
	if port == "" {
		port = config.DefaultPort(r.URL.Scheme)
	}
	if !p.allow.Allowed(info.Host, info.TaskID, p.now()) {
		p.recordDenial("build_proxy", info.Host, "not_allowlisted", info)
		writeDeny(w, info, http.StatusForbidden, "not_allowlisted")
		return 0, "deny", "not_allowlisted"
	}
	if !p.limits.AllowConn(info.SessionID) {
		p.recordDenial("build_proxy", info.Host, "session_rate_limited", info)
		writeDeny(w, info, http.StatusTooManyRequests, "session_rate_limited")
		return 0, "deny", "session_rate_limited"
	}
	gd := p.guarded(info)
	checked, err := gd.CheckedAddresses(r.Context(), info.Host, port)
	if err != nil {
		if errors.Is(err, config.ErrDenied) {
			reason := denyReason(err)
			writeDeny(w, info, http.StatusForbidden, reason)
			return 0, "deny", reason
		}
		writeProblem(w, http.StatusBadGateway, "upstream_error", "Could not resolve upstream host.")
		return 0, "deny", "dns_failed"
	}

	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return gd.DialChecked(ctx, network, checked, port)
	}
	transport := &http.Transport{
		Proxy:             nil,
		DialContext:       dial,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: false,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 2 * time.Minute,
	}

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = r.URL.Scheme
	out.URL.Host = net.JoinHostPort(info.Host, port)
	out.Host = r.Host
	out.Header = r.Header.Clone()
	stripHopByHop(out.Header)
	out.Header.Del("Cookie")
	out.Header.Del("Proxy-Authorization")

	resp, err := client.Do(out)
	if err != nil {
		transport.CloseIdleConnections()
		writeProblem(w, http.StatusBadGateway, "upstream_error", "Could not reach upstream host.")
		return 0, "deny", "dial_failed"
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	cw := &countWriter{w: w}
	cw.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(cw, resp.Body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	n := cw.n
	p.limits.AddBytes(info.SessionID, n)
	transport.CloseIdleConnections()
	return n, "allow", ""
}

func stripHopByHop(h http.Header) {
	for _, key := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		h.Del(key)
	}
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		if strings.EqualFold(key, "Set-Cookie") || strings.EqualFold(key, "Connection") ||
			strings.EqualFold(key, "Proxy-Authenticate") || strings.EqualFold(key, "Proxy-Authorization") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

type countWriter struct {
	w http.ResponseWriter
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (c *countWriter) WriteHeader(code int) { c.w.WriteHeader(code) }
