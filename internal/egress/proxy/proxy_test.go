package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryjuche/padi/internal/egress/allowlist"
	"github.com/jerryjuche/padi/internal/egress/config"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fakeResolver struct {
	mu       sync.Mutex
	answers  [][]net.IPAddr
	seq      int
	calls    int
	fallback []net.IPAddr
}

func (f *fakeResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.answers) > 0 {
		i := f.seq
		if i >= len(f.answers) {
			i = len(f.answers) - 1
		}
		f.seq++
		return append([]net.IPAddr(nil), f.answers[i]...), nil
	}
	return append([]net.IPAddr(nil), f.fallback...), nil
}

func (f *fakeResolver) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type recordingDialer struct {
	mu     sync.Mutex
	target string
	ips    []string
	conns  int
}

func (d *recordingDialer) DialIP(ctx context.Context, _ string, ip net.IP, _ string) (net.Conn, error) {
	d.mu.Lock()
	d.ips = append(d.ips, ip.String())
	d.conns++
	target := d.target
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, "tcp", target)
}

func (d *recordingDialer) Snapshot() ([]string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.ips...), d.conns
}

func ipaddr(s string) net.IPAddr { return net.IPAddr{IP: net.ParseIP(s)} }

func testConfig() *config.Config {
	nets, _ := config.ParseDenyRanges(nil)
	return &config.Config{
		Ports:    []int{80, 443},
		DenyNets: nets,
		Limits:   config.LimitsCfg{SessionBytesMB: 10, SessionConnsPerMin: 100},
		Research: config.ResearchCfg{
			URLMaxLen: 512, QueryValueMaxLen: 100, QueryMaxLen: 200,
			EntropyThresholdBits: 4.2, EntropyMinLen: 32,
		},
	}
}

type testEnv struct {
	proxy    *Proxy
	server   *httptest.Server
	resolver *fakeResolver
	dialer   *recordingDialer
	denials  *Denials
	logs     *lockedBuffer
	counters *config.Counters
	upstream string
}

func newTestEnv(t *testing.T, resolver *fakeResolver, dialer *recordingDialer, allowHosts ...string) *testEnv {
	t.Helper()
	logs := &lockedBuffer{}
	denials := NewDenials(100, time.Now)
	counters := &config.Counters{}
	audit := config.NewAudit(logs)
	p := New(testConfig(), allowlist.New(allowHosts), denials, counters, audit,
		WithResolver(resolver), WithDialIP(dialer.DialIP))
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return &testEnv{proxy: p, server: srv, resolver: resolver, dialer: dialer, denials: denials, logs: logs, counters: counters}
}

func echoListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func connectRequest(t *testing.T, proxyURL, authority string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	addr := strings.TrimPrefix(proxyURL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nX-Session-Id: s-1\r\nX-Task-Id: task-1\r\n\r\n", authority, authority)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return resp, conn, br
}

func TestConnectHostPortParsing(t *testing.T) {
	tests := []struct {
		raw     string
		wantErr bool
	}{
		{"public.example:443", false},
		{"[2001:db8::1]:80", false},
		{"public.example:22", false}, // parse is strict; proxy policy rejects the port later
		{"public.example", true},
		{"public.example:0", true},
		{"user@public.example:443", true},
		{"public.example:443/x", true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			_, _, err := ParseHostPort(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseHostPort(%q) err = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
		})
	}
}

func TestConnectSSRFAndAllowlist(t *testing.T) {
	public := []net.IPAddr{ipaddr("93.184.216.34")}
	private := []net.IPAddr{ipaddr("93.184.216.34"), ipaddr("10.0.0.5")}
	tests := []struct {
		name       string
		authority  string
		answers    [][]net.IPAddr
		allow      []string
		wantStatus int
		wantReason string
		wantDial   bool
	}{
		{"allowed", "public.example:443", [][]net.IPAddr{public}, []string{"public.example"}, 200, "", true},
		{"rebinding_second_answer_private", "public.example:443", [][]net.IPAddr{private}, []string{"public.example"}, 403, "private", false},
		{"ip_literal", "127.0.0.1:443", nil, []string{"public.example"}, 403, "ip_literal", false},
		{"metadata_literal", "169.254.169.254:80", nil, []string{"public.example"}, 403, "ip_literal", false},
		{"bad_port", "public.example:22", [][]net.IPAddr{public}, []string{"public.example"}, 403, "port_not_allowed", false},
		{"not_allowlisted", "other.example:443", [][]net.IPAddr{public}, []string{"public.example"}, 403, "not_allowlisted", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := echoListener(t)
			resolver := &fakeResolver{answers: tt.answers, fallback: public}
			dialer := &recordingDialer{target: upstream}
			env := newTestEnv(t, resolver, dialer, tt.allow...)
			resp, conn, br := connectRequest(t, env.server.URL, tt.authority)
			defer conn.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantReason != "" {
				if got := resp.Header.Get("X-Egress-Deny"); got == "" {
					t.Fatal("missing X-Egress-Deny header")
				}
				items := env.denials.Since(time.Time{})
				if len(items) == 0 || items[len(items)-1].Reason != tt.wantReason {
					t.Fatalf("denials = %+v, want reason %q", items, tt.wantReason)
				}
			}
			_, dials := dialer.Snapshot()
			if (dials > 0) != tt.wantDial {
				t.Fatalf("dial attempts = %d, wantDial=%v", dials, tt.wantDial)
			}
			if tt.wantStatus == 200 {
				if _, err := io.WriteString(conn, "ping\n"); err != nil {
					t.Fatal(err)
				}
				line, err := br.ReadString('\n')
				if err != nil || strings.TrimSpace(line) != "ping" {
					t.Fatalf("tunnel response = %q, %v", line, err)
				}
			}
			if strings.Contains(env.logs.String(), "SUPERSECRET") {
				t.Fatal("audit log leaked a secret")
			}
		})
	}
}

func TestConnectDNSRebindingDoesNotReResolve(t *testing.T) {
	upstream := echoListener(t)
	resolver := &fakeResolver{answers: [][]net.IPAddr{
		{ipaddr("93.184.216.34")},
		{ipaddr("10.0.0.9")},
	}}
	dialer := &recordingDialer{target: upstream}
	env := newTestEnv(t, resolver, dialer, "public.example")

	resp, conn, _ := connectRequest(t, env.server.URL, "public.example:443")
	_ = resp.Body.Close()
	_ = conn.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("first CONNECT status = %d", resp.StatusCode)
	}
	if resolver.Calls() != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1", resolver.Calls())
	}
	ips, dials := dialer.Snapshot()
	if dials != 1 || len(ips) != 1 || ips[0] != "93.184.216.34" {
		t.Fatalf("dialed IPs = %v, dials = %d; want the checked public IP only", ips, dials)
	}

	resp2, conn2, _ := connectRequest(t, env.server.URL, "public.example:443")
	_ = resp2.Body.Close()
	_ = conn2.Close()
	if resp2.StatusCode != 403 || resp2.Header.Get("X-Egress-Deny") == "" {
		t.Fatalf("second CONNECT status = %d header=%q", resp2.StatusCode, resp2.Header.Get("X-Egress-Deny"))
	}
	if resolver.Calls() != 2 {
		t.Fatalf("resolver calls = %d, want 2", resolver.Calls())
	}
	_, dials = dialer.Snapshot()
	if dials != 1 {
		t.Fatalf("dial attempts = %d; private rebind must not be dialed", dials)
	}
}

func TestPlainForwardChecksURLsAndLogsNoQueryValues(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "upstream-ok")
	}))
	defer upstream.Close()
	resolver := &fakeResolver{fallback: []net.IPAddr{ipaddr("93.184.216.34")}}
	dialer := &recordingDialer{target: strings.TrimPrefix(upstream.URL, "http://")}
	env := newTestEnv(t, resolver, dialer, "public.example")

	proxyURL, _ := url.Parse(env.server.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := client.Get("http://public.example/path?token=SUPERSECRETVALUE")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "upstream-ok" {
		t.Fatalf("forward status=%d body=%q", resp.StatusCode, body)
	}
	if strings.Contains(env.logs.String(), "SUPERSECRETVALUE") {
		t.Fatalf("audit log leaked query value: %s", env.logs.String())
	}

	resp2, err := client.Get("http://public.example:22/path")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != 403 || resp2.Header.Get("X-Egress-Deny") == "" {
		t.Fatalf("bad-port forward status=%d header=%q", resp2.StatusCode, resp2.Header.Get("X-Egress-Deny"))
	}
}
