package config

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLimits() URLLimits {
	return URLLimits{
		Ports:            []int{80, 443},
		MaxURLLen:        512,
		MaxQueryValueLen: 100,
		MaxQueryLen:      200,
		EntropyThreshold: 4.2,
		EntropyMinLen:    32,
	}
}

func TestCheckIPDenyRanges(t *testing.T) {
	tests := []struct {
		name   string
		ip     string
		allow  bool
		reason string
	}{
		{"public_v4", "93.184.216.34", true, ""},
		{"public_v6", "2606:4700::1111", true, ""},
		{"loopback", "127.0.0.1", false, "loopback"},
		{"loopback_v6", "::1", false, "loopback"},
		{"private_10", "10.1.2.3", false, "private"},
		{"private_172", "172.16.0.1", false, "private"},
		{"private_192", "192.168.1.1", false, "private"},
		{"metadata", "169.254.169.254", false, "metadata"},
		{"link_local_v4", "169.254.10.10", false, "link_local"},
		{"link_local_v6", "fe80::1", false, "link_local"},
		{"ula", "fd00::1", false, "ula"},
		{"cgnat", "100.64.0.1", false, "cgnat"},
		{"multicast_v4", "224.0.0.1", false, "multicast"},
		{"multicast_v6", "ff02::1", false, "multicast"},
		{"unspecified", "0.0.0.0", false, "unspecified"},
		{"reserved_0", "0.1.2.3", false, "deny_range"},
		{"documentation", "198.51.100.7", false, "deny_range"},
		{"mapped_loopback", "::ffff:127.0.0.1", false, "loopback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckIP(net.ParseIP(tt.ip))
			if got.Allow != tt.allow || got.Reason != tt.reason {
				t.Fatalf("CheckIP(%s) = %+v, want allow=%v reason=%q", tt.ip, got, tt.allow, tt.reason)
			}
		})
	}
}

func TestCheckURLGuards(t *testing.T) {
	lim := testLimits()
	lim.MaxURLLen = 60
	lim.MaxQueryValueLen = 8
	lim.MaxQueryLen = 20
	lim.EntropyMinLen = 8
	lim.EntropyThreshold = 4.0
	high := "xK9mQ2vL7pR4tN8bZ1yC5wA3dF6hJ0sU"
	tests := []struct {
		name   string
		url    string
		allow  bool
		reason string
	}{
		{"https", "https://example.com/docs?page=1", true, ""},
		{"http_default_port", "http://example.com:80/", true, ""},
		{"ftp", "ftp://example.com/file", false, "scheme_not_allowed"},
		{"ip_literal", "https://127.0.0.1/", false, "ip_literal"},
		{"private_ip", "http://10.0.0.1/", false, "ip_literal"},
		{"bad_port", "https://example.com:22/", false, "port_not_allowed"},
		{"userinfo", "https://user@example.com/", false, "userinfo_not_allowed"},
		{"too_long", "https://example.com/" + strings.Repeat("a", 60), false, "url_too_long"},
		{"query_too_long", "https://example.com/?q=" + strings.Repeat("a", 21), false, "query_too_long"},
		{"query_value_too_long", "https://example.com/?q=abcdefghi", false, "query_value_too_long"},
		{"high_entropy_query", "https://example.com/?q=" + high, false, "query_too_long"},
		{"bad_host", "https://bad_host.example/", false, "bad_host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckURL(tt.url, lim)
			if got.Allow != tt.allow || (tt.reason != "" && got.Reason != tt.reason) {
				t.Fatalf("CheckURL(%q) = %+v, want allow=%v reason=%q", tt.url, got, tt.allow, tt.reason)
			}
		})
	}

	wide := testLimits()
	if v := CheckURL("https://example.com/?q="+high, wide); v.Allow || v.Reason != "high_entropy_value" {
		t.Fatalf("high entropy value: got %+v", v)
	}
	if v := CheckURL("https://example.com/"+high, wide); v.Allow || v.Reason != "high_entropy_path" {
		t.Fatalf("high entropy path: got %+v", v)
	}
}

func TestEntropyBitsPerChar(t *testing.T) {
	if got := EntropyBitsPerChar("aaaaaaaa"); got != 0 {
		t.Fatalf("constant string entropy = %f, want 0", got)
	}
	if got := EntropyBitsPerChar("ab"); got != 1 {
		t.Fatalf("two-symbol entropy = %f, want 1", got)
	}
}

func TestParseHostPortStrict(t *testing.T) {
	tests := []struct {
		raw     string
		host    string
		port    string
		wantErr bool
	}{
		{"example.com:443", "example.com", "443", false},
		{"[2001:db8::1]:80", "2001:db8::1", "80", false},
		{"example.com", "", "", true},
		{"example.com:0", "", "", true},
		{"example.com:+443", "", "", true},
		{"user@example.com:443", "", "", true},
		{"example.com:443/path", "", "", true},
		{" bad.example:443", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			host, port, err := ParseHostPort(tt.raw)
			if (err != nil) != tt.wantErr || (!tt.wantErr && (host != tt.host || port != tt.port)) {
				t.Fatalf("ParseHostPort(%q) = (%q,%q,%v)", tt.raw, host, port, err)
			}
		})
	}
}

func TestLoadConfigAndStrictKeys(t *testing.T) {
	cfg, err := Load("../../../config/egress.yaml", func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load config/egress.yaml: %v", err)
	}
	if cfg.Listen.Proxy != ":3128" || cfg.Credentials.Listen != ":3131" {
		t.Fatalf("unexpected listeners: %+v %+v", cfg.Listen, cfg.Credentials)
	}
	if cfg.Research.Search.Provider != "none" {
		t.Fatalf("search provider = %q, want none", cfg.Research.Search.Provider)
	}
	if cfg.Research.Search.Timeout.Seconds() != 8 {
		t.Fatalf("search timeout = %v, want 8s", cfg.Research.Search.Timeout)
	}
	if !PortAllowed(443, cfg.Ports) || PortAllowed(22, cfg.Ports) {
		t.Fatalf("unexpected ports %v", cfg.Ports)
	}

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("listen: {proxy: ':1'}\nunknown_key: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad, nil); err == nil || !strings.Contains(err.Error(), "field unknown_key not found") {
		t.Fatalf("strict unknown key error = %v", err)
	}
}
