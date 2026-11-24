// Command egress-proxy is the single outbound boundary for session containers:
// an allowlisting build proxy, a read-only research API and a locked-down admin
// API. Credential routes are a listener stub until M16.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jerryjuche/padi/internal/egress/allowlist"
	"github.com/jerryjuche/padi/internal/egress/config"
	"github.com/jerryjuche/padi/internal/egress/proxy"
	"github.com/jerryjuche/padi/internal/egress/research"
	"github.com/jerryjuche/padi/internal/egress/search"
)

func main() {
	path := os.Getenv("EGRESS_CONFIG")
	if path == "" {
		path = "config/egress.yaml"
	}
	cfg, err := config.Load(path, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "egress-proxy:", err)
		os.Exit(1)
	}
	startup := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	for _, warning := range cfg.Warnings {
		startup.Warn("config warning", "detail", warning)
	}

	logWriter, closeLog, err := openAudit(cfg.LogPath)
	if err != nil {
		startup.Warn("audit log unavailable; using stderr", "detail", err.Error())
		logWriter = os.Stderr
		closeLog = func() {}
	}
	defer closeLog()
	audit := config.NewAudit(logWriter)
	counters := &config.Counters{}
	denials := proxy.NewDenials(4096, time.Now)
	allow := allowlist.New(cfg.BuildAllowlist)

	buildProxy := proxy.New(cfg, allow, denials, counters, audit)
	researchDialer := &proxy.GuardedDialer{Ports: cfg.Ports, DenyNets: cfg.DenyNets}
	fetcher := research.NewFetcher(cfg, researchDialer)
	provider := search.Provider(search.None{})
	if cfg.Research.Search.Provider == "searxng" {
		provider = &research.SearxNG{
			BaseURL:    cfg.Research.Search.URL,
			Engines:    cfg.Research.Search.Engines,
			MaxResults: cfg.Research.Search.MaxResults,
			Limits:     cfg.URLLimits(),
			Timeout:    cfg.Research.Search.Timeout,
		}
	}
	researchLimits := research.NewLimits(cfg.Research.RatePerMinSession, cfg.Research.MaxPerTask, time.Now)
	researchHandler := research.NewHandler(fetcher, provider, cfg.RouterToken, audit, counters, denials, researchLimits, cfg.URLLimits())
	admin := newAdminHandler(cfg, allow, denials, counters, audit)
	credentialStubHandler := credentialStub(cfg, audit, counters)

	servers := []struct {
		name string
		srv  *http.Server
	}{
		{"build_proxy", newServer(cfg.Listen.Proxy, buildProxy)},
		{"research", newServer(cfg.Listen.Research, researchHandler)},
		{"admin", newServer(cfg.Listen.Admin, admin)},
		{"credentials", newServer(cfg.Credentials.Listen, credentialStubHandler)},
	}
	errCh := make(chan error, len(servers))
	for _, s := range servers {
		go func(name string, srv *http.Server) {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s listener: %w", name, err)
			}
		}(s.name, s.srv)
	}
	startup.Info("egress-proxy ready",
		"proxy", cfg.Listen.Proxy,
		"research", cfg.Listen.Research,
		"admin", cfg.Listen.Admin,
		"credentials", cfg.Credentials.Listen,
		"allowlist_entries", allow.StaticCount(),
		"search_provider", cfg.Research.Search.Provider,
		"credential_services", len(cfg.Credentials.Services),
	)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		startup.Info("shutting down", "signal", sig.String())
	case err := <-errCh:
		startup.Error("listener failed", "detail", err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.srv.Shutdown(ctx); err != nil {
			startup.Warn("shutdown failed", "listener", s.name, "detail", err.Error())
		}
	}
}

func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
}

func openAudit(path string) (io.Writer, func(), error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// adminHandler serves denials, runtime allow approvals and status.
type adminHandler struct {
	cfg      *config.Config
	allow    *allowlist.List
	denials  *proxy.Denials
	counters *config.Counters
	audit    *config.Audit
	started  time.Time
}

func newAdminHandler(cfg *config.Config, allow *allowlist.List, denials *proxy.Denials, counters *config.Counters, audit *config.Audit) *adminHandler {
	return &adminHandler{cfg: cfg, allow: allow, denials: denials, counters: counters, audit: audit, started: time.Now()}
}

func (a *adminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	verdict, reason := "deny", "unauthorized"
	defer func() {
		a.counters.AdminRequests.Add(1)
		a.audit.Line("admin", r.URL.Path, "", "", verdict, reason, 0)
	}()
	token := bearer(r.Header.Get("Authorization"))
	if token == "" || !constantTimeEqual(token, a.cfg.AdminToken) ||
		(a.cfg.RouterToken != "" && constantTimeEqual(token, a.cfg.RouterToken)) {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "Admin bearer token required.")
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/admin/denied":
		a.denied(w, r)
		verdict, reason = "allow", ""
	case r.Method == http.MethodPut && r.URL.Path == "/admin/allow":
		a.putAllow(w, r)
		verdict, reason = "allow", ""
	case r.Method == http.MethodDelete && r.URL.Path == "/admin/allow":
		a.deleteAllow(w, r)
		verdict, reason = "allow", ""
	case r.Method == http.MethodGet && r.URL.Path == "/status":
		a.status(w)
		verdict, reason = "allow", ""
	default:
		reason = "not_found"
		writeProblem(w, http.StatusNotFound, "not_found", "Unknown admin endpoint.")
	}
}

func (a *adminHandler) denied(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339Nano, raw)
		}
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "bad_since", "since must be an RFC3339 timestamp.")
			return
		}
		since = parsed
	}
	writeJSON(w, http.StatusOK, a.denials.Since(since))
}

type allowRequest struct {
	Host   string `json:"host"`
	TaskID string `json:"task_id"`
	TTL    int    `json:"ttl_s"`
}

func (a *adminHandler) putAllow(w http.ResponseWriter, r *http.Request) {
	var req allowRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	host := config.CanonicalHost(req.Host)
	if err := config.ValidateHost(host); err != nil || netIP(host) {
		writeProblem(w, http.StatusBadRequest, "bad_host", "A DNS host is required.")
		return
	}
	if req.TaskID == "" || req.TTL <= 0 {
		writeProblem(w, http.StatusBadRequest, "bad_request", "task_id and positive ttl_s are required.")
		return
	}
	until := time.Now().Add(time.Duration(req.TTL) * time.Second)
	a.allow.Approve(host, req.TaskID, until)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *adminHandler) deleteAllow(w http.ResponseWriter, r *http.Request) {
	var req allowRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.TaskID == "" || req.Host == "" {
		writeProblem(w, http.StatusBadRequest, "bad_request", "host and task_id are required.")
		return
	}
	a.allow.Revoke(req.Host, req.TaskID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *adminHandler) status(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"counters":             a.counters.Snapshot(),
		"static_allow_entries": a.allow.StaticCount(),
		"runtime_approvals":    a.allow.RuntimeCount(time.Now()),
		"recent_denials":       a.denials.Count(),
		"search_provider":      a.cfg.Research.Search.Provider,
		"credential_services":  len(a.cfg.Credentials.Services),
		"uptime_s":             int64(time.Since(a.started).Seconds()),
		"listeners": map[string]string{
			"proxy":    a.cfg.Listen.Proxy,
			"research": a.cfg.Listen.Research,
			"admin":    a.cfg.Listen.Admin,
		},
	})
}

// credentialStub keeps :3131 reachable but refuses every route until the M16
// grant infrastructure exists. It never reads or logs credential values.
func credentialStub(cfg *config.Config, audit *config.Audit, counters *config.Counters) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reason := "credentials_disabled"
		if len(cfg.Credentials.Services) > 0 {
			reason = "credential_routes_not_implemented"
		}
		counters.Credential501.Add(1)
		audit.Line("credential_route", r.URL.Path, "", "", "deny", reason, 0)
		writeProblem(w, http.StatusNotImplemented, "credential_denied", "Credential routes are not available.")
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad_request", "Invalid JSON request.")
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "encode_failed", "Could not encode response.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeProblem(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"code": code, "message": message})
	_, _ = w.Write(body)
}

func bearer(header string) string {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func constantTimeEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func netIP(host string) bool { return net.ParseIP(host) != nil }
