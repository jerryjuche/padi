package research

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jerryjuche/padi/internal/egress/config"
	"github.com/jerryjuche/padi/internal/egress/proxy"
	"github.com/jerryjuche/padi/internal/egress/search"
)

type testResolver struct {
	mu    sync.Mutex
	addrs []net.IPAddr
	calls int
}

func (r *testResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return append([]net.IPAddr(nil), r.addrs...), nil
}

type testDialer struct {
	mu     sync.Mutex
	target string
	ips    []string
}

func (d *testDialer) DialIP(ctx context.Context, _ string, ip net.IP, _ string) (net.Conn, error) {
	d.mu.Lock()
	d.ips = append(d.ips, ip.String())
	target := d.target
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, "tcp", target)
}

func testConfig() *config.Config {
	nets, _ := config.ParseDenyRanges(nil)
	return &config.Config{
		Ports:    []int{80, 443},
		DenyNets: nets,
		Research: config.ResearchCfg{
			MaxBodyMB: 1, MaxRedirects: 5, MaxChars: 40000,
			URLMaxLen: 512, QueryValueMaxLen: 100, QueryMaxLen: 200,
			EntropyThresholdBits: 4.2, EntropyMinLen: 32,
			RatePerMinSession: 30, MaxPerTask: 150,
			Search: config.SearchCfg{MaxResults: 8, Timeout: 8 * time.Second},
		},
	}
}

func newFetcherFor(t *testing.T, upstream string, capBytes int) (*Fetcher, *testResolver, *testDialer) {
	t.Helper()
	cfg := testConfig()
	cfg.Research.MaxBodyMB = capBytes
	resolver := &testResolver{addrs: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
	dialer := &testDialer{target: strings.TrimPrefix(upstream, "http://")}
	gd := &proxy.GuardedDialer{
		Resolver: resolver, DialIP: dialer.DialIP,
		Ports: cfg.Ports, DenyNets: cfg.DenyNets,
	}
	return NewFetcher(cfg, gd), resolver, dialer
}

func TestExtractTextDropsUnsafeContent(t *testing.T) {
	input := `<html><head><title>  A &amp; B  </title><style>SECRET_STYLE{}</style></head>` +
		`<body><!-- COMMENT --><script>SECRET_SCRIPT</script><noscript>SECRET_NOSCRIPT</noscript>` +
		`<template>SECRET_TEMPLATE</template><p>VISIBLE &lt;TEXT&gt;</p>` +
		`<div style="display:none">SECRET_HIDDEN_STYLE</div><div hidden>SECRET_HIDDEN_ATTR</div>` +
		`<span aria-hidden="true">SECRET_ARIA</span><p>TAIL</p></body></html>`
	title, text, err := ExtractText(strings.NewReader(input), 40000)
	if err != nil {
		t.Fatal(err)
	}
	if title != "A & B" {
		t.Fatalf("title = %q", title)
	}
	for _, forbidden := range []string{"SECRET_STYLE", "SECRET_SCRIPT", "SECRET_NOSCRIPT", "SECRET_TEMPLATE", "SECRET_HIDDEN_STYLE", "SECRET_HIDDEN_ATTR", "SECRET_ARIA", "COMMENT"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("text contains %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "VISIBLE <TEXT>") || !strings.Contains(text, "TAIL") {
		t.Fatalf("visible text missing: %s", text)
	}
}

func TestExtractTextBounds(t *testing.T) {
	input := strings.Repeat("word ", 5000)
	_, text, truncated, err := extractText(strings.NewReader(input), 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(text)); got > 50 {
		t.Fatalf("text length = %d, want <= 50", got)
	}
	if !truncated {
		t.Fatal("truncated flag not set")
	}
}

func TestResearchURLGuards(t *testing.T) {
	lim := testConfig().URLLimits()
	high := "xK9mQ2vL7pR4tN8bZ1yC5wA3dF6hJ0sU"
	tests := []struct {
		name   string
		url    string
		reason string
	}{
		{"valid", "https://example.com/page", ""},
		{"too_long", "https://example.com/" + strings.Repeat("a", 600), "url_too_long"},
		{"query_value_too_long", "https://example.com/?q=" + strings.Repeat("a", 101), "query_value_too_long"},
		{"entropy", "https://example.com/?q=" + high, "high_entropy_value"},
		{"private_ip", "http://169.254.169.254/latest/meta-data/", "ip_literal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := config.CheckURL(tt.url, lim)
			if tt.reason == "" && !v.Allow {
				t.Fatalf("CheckURL denied: %+v", v)
			}
			if tt.reason != "" && (v.Allow || v.Reason != tt.reason) {
				t.Fatalf("CheckURL = %+v, want %q", v, tt.reason)
			}
		})
	}
}

func TestFetchBodyCapAndCleanText(t *testing.T) {
	headersCh := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headersCh <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><script>SECRET_SCRIPT</script><p>"+strings.Repeat("A", (1<<20)+50)+"</p></body></html>")
	}))
	defer upstream.Close()
	fetcher, _, _ := newFetcherFor(t, upstream.URL, 1)
	fetcher.cfg.Research.MaxChars = 40
	page, err := fetcher.Fetch(context.Background(), "session-1", "http://public.example/page?token=notlogged")
	if err != nil {
		t.Fatal(err)
	}
	if !page.Truncated {
		t.Fatal("page.Truncated = false for oversized body")
	}
	if got := len([]rune(page.Text)); got != 40 {
		t.Fatalf("text length = %d, want 40", got)
	}
	if strings.Contains(page.Text, "SECRET_SCRIPT") {
		t.Fatal("script text leaked into page")
	}
	if page.Status != 200 || page.FinalURL != "http://public.example/page?token=notlogged" {
		t.Fatalf("unexpected page metadata: %+v", page)
	}
	seenHeaders := <-headersCh
	if len(seenHeaders.Values("Cookie")) != 0 {
		t.Fatalf("cookies sent upstream: %v", seenHeaders.Values("Cookie"))
	}
}

func TestFetchRedirectRechecksEachHop(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/start":
			w.Header().Set("Location", "http://127.0.0.1:80/private")
			w.WriteHeader(http.StatusFound)
		case "/ok":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "FINAL")
		default:
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "SHOULD_NOT_BE_READ")
		}
	}))
	defer upstream.Close()
	fetcher, _, _ := newFetcherFor(t, upstream.URL, 1)

	_, err := fetcher.Fetch(context.Background(), "s", "http://public.example/start")
	if err == nil || !strings.Contains(err.Error(), "ip_literal") {
		t.Fatalf("redirect to private address error = %v, want ip_literal", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (second hop must be blocked before connection)", hits.Load())
	}
}

func TestFetchFollowsValidRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "http://public.example/final")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><div hidden>NOPE</div><p>FINAL_TEXT</p></body></html>")
	}))
	defer upstream.Close()
	fetcher, _, _ := newFetcherFor(t, upstream.URL, 1)
	page, err := fetcher.Fetch(context.Background(), "s", "http://public.example/start")
	if err != nil {
		t.Fatal(err)
	}
	if page.FinalURL != "http://public.example/final" || page.Status != 200 || !strings.Contains(page.Text, "FINAL_TEXT") || strings.Contains(page.Text, "NOPE") {
		t.Fatalf("unexpected page after redirect: %+v", page)
	}
}

func TestFetchRejectsContentType(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, "raw")
	}))
	defer upstream.Close()
	fetcher, _, _ := newFetcherFor(t, upstream.URL, 1)
	_, err := fetcher.Fetch(context.Background(), "s", "http://public.example/file")
	if err == nil || !strings.Contains(err.Error(), "content_type_not_allowed") {
		t.Fatalf("content type error = %v", err)
	}
}

func TestResearchHandlerAuthSearchUnavailableAndAudit(t *testing.T) {
	logs := &bytes.Buffer{}
	counters := &config.Counters{}
	cfg := testConfig()
	h := NewHandler(nil, search.None{}, "router-secret", config.NewAudit(logs), counters, nil,
		NewLimits(30, 150, time.Now), cfg.URLLimits())

	body := `{"query":"SUPERSECRETQUERY","n":5,"session_id":"s-1","task_id":"t-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/research/search", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer router-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var problem map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil || problem["code"] != "search_unavailable" {
		t.Fatalf("problem = %v, err = %v", problem, err)
	}
	if strings.Contains(logs.String(), "SUPERSECRETQUERY") || strings.Contains(logs.String(), "router-secret") {
		t.Fatalf("audit log leaked query or token: %s", logs.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/research/search", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer wrong")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", rec2.Code)
	}
}

func TestResearchLimits(t *testing.T) {
	now := time.Now()
	l := NewLimits(2, 3, func() time.Time { return now })
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("s", "task"); !ok {
			t.Fatalf("call %d unexpectedly limited", i)
		}
	}
	if ok, why := l.Allow("s", "task"); ok || why != "session_rate_limited" {
		t.Fatalf("rate limit not enforced: ok=%v why=%s", ok, why)
	}
	l2 := NewLimits(100, 2, func() time.Time { return now })
	for i := 0; i < 2; i++ {
		if ok, _ := l2.Allow("s", "task"); !ok {
			t.Fatalf("task call %d unexpectedly limited", i)
		}
	}
	if ok, why := l2.Allow("s", "task"); ok || why != "task_cap_reached" {
		t.Fatalf("task cap not enforced: ok=%v why=%s", ok, why)
	}
}
