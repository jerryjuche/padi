package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jerryjuche/padi/internal/egress/allowlist"
	"github.com/jerryjuche/padi/internal/egress/config"
	"github.com/jerryjuche/padi/internal/egress/proxy"
)

func adminTestHandler() (*adminHandler, *allowlist.List, *proxy.Denials, *bytes.Buffer) {
	cfg := &config.Config{
		RouterToken: "router-secret",
		AdminToken:  "admin-secret",
		Research:    config.ResearchCfg{Search: config.SearchCfg{Provider: "none"}},
		Credentials: config.CredentialsCfg{Listen: ":3131"},
	}
	allow := allowlist.New(nil)
	denials := proxy.NewDenials(20, time.Now)
	logs := &bytes.Buffer{}
	return newAdminHandler(cfg, allow, denials, &config.Counters{}, config.NewAudit(logs)), allow, denials, logs
}

func TestAdminAuthRejectsRouterToken(t *testing.T) {
	h, _, _, _ := adminTestHandler()
	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"router_token", "Bearer router-secret", http.StatusUnauthorized},
		{"admin_token", "Bearer admin-secret", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

func TestAdminAllowDeniedAndStatus(t *testing.T) {
	h, allow, denials, logs := adminTestHandler()
	auth := func(req *http.Request) *http.Request {
		req.Header.Set("Authorization", "Bearer admin-secret")
		return req
	}

	put := httptest.NewRequest(http.MethodPut, "/admin/allow", strings.NewReader(`{"host":"private.example","task_id":"task-1","ttl_s":60}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, auth(put))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /admin/allow status = %d: %s", rec.Code, rec.Body.String())
	}
	if !allow.Allowed("private.example", "task-1", time.Now().Add(time.Second)) {
		t.Fatal("runtime approval not recorded")
	}

	del := httptest.NewRequest(http.MethodDelete, "/admin/allow", strings.NewReader(`{"host":"private.example","task_id":"task-1"}`))
	recDel := httptest.NewRecorder()
	h.ServeHTTP(recDel, auth(del))
	if recDel.Code != http.StatusOK || allow.Allowed("private.example", "task-1", time.Now().Add(time.Second)) {
		t.Fatalf("DELETE /admin/allow failed: status=%d", recDel.Code)
	}

	denials.Record("build_proxy", "blocked.example", "not_allowlisted", "s", "task-2")
	req := httptest.NewRequest(http.MethodGet, "/admin/denied?since=2000-01-01T00:00:00Z", nil)
	recDenied := httptest.NewRecorder()
	h.ServeHTTP(recDenied, auth(req))
	var items []map[string]any
	if err := json.Unmarshal(recDenied.Body.Bytes(), &items); err != nil || len(items) != 1 || items[0]["host"] != "blocked.example" {
		t.Fatalf("denials body = %s, err = %v", recDenied.Body.String(), err)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/status", nil)
	recStatus := httptest.NewRecorder()
	h.ServeHTTP(recStatus, auth(statusReq))
	for _, secret := range []string{"admin-secret", "router-secret"} {
		if strings.Contains(recStatus.Body.String(), secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("secret %s leaked in status or audit log", secret)
		}
	}
}

func TestCredentialListenerStub(t *testing.T) {
	cfg := &config.Config{Credentials: config.CredentialsCfg{Listen: ":3131"}}
	logs := &bytes.Buffer{}
	counters := &config.Counters{}
	h := credentialStub(cfg, config.NewAudit(logs), counters)
	req := httptest.NewRequest(http.MethodGet, "/svc/npm-private/packages?token=SUPERSECRET", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("credential stub status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "credential_denied") {
		t.Fatalf("credential stub body = %s", rec.Body.String())
	}
	if strings.Contains(logs.String(), "SUPERSECRET") {
		t.Fatalf("credential log leaked query value: %s", logs.String())
	}
	if counters.Credential501.Load() != 1 {
		t.Fatalf("credential_501 counter = %d", counters.Credential501.Load())
	}
}
