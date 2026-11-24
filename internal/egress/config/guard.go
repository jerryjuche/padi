package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"
)

// Verdict is the result of a guard check. Reason is a short stable code, never
// a raw URL or query value.
type Verdict struct {
	Allow  bool
	Reason string
}

var (
	// ErrDenied is returned when a guard check blocks an address or URL.
	ErrDenied = errors.New("egress denied")
	// ErrResolve is returned when in-proxy DNS resolution fails.
	ErrResolve = errors.New("egress dns resolution failed")
)

// DenyError carries the stable reason for a blocked connection.
type DenyError struct{ Reason string }

func (e *DenyError) Error() string { return "egress denied: " + e.Reason }

// Unwrap makes errors.Is(err, ErrDenied) work.
func (e *DenyError) Unwrap() error { return ErrDenied }

func deny(reason string) Verdict { return Verdict{Allow: false, Reason: reason} }

func allow() Verdict { return Verdict{Allow: true} }

// unmapIP converts IPv4-mapped IPv6 addresses to plain IPv4.
func unmapIP(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

// CheckIP applies the always-on SSRF deny rules to one address.
func CheckIP(ip net.IP) Verdict { return CheckIPIn(ip, nil) }

// CheckIPIn applies the built-in rules plus any configured deny ranges.
// IPv4-mapped IPv6 addresses are checked as IPv4.
func CheckIPIn(ip net.IP, extra []*net.IPNet) Verdict {
	if ip == nil {
		return deny("invalid_address")
	}
	ip = unmapIP(ip)
	if ip.To4() == nil && ip.To16() == nil {
		return deny("invalid_address")
	}
	if ip.IsUnspecified() {
		return deny("unspecified")
	}
	if ip.Equal(net.ParseIP("169.254.169.254")) {
		return deny("metadata")
	}
	if ip.IsLoopback() {
		return deny("loopback")
	}
	if ip.IsMulticast() {
		return deny("multicast")
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return deny("link_local")
	}
	if _, cgnat, _ := net.ParseCIDR("100.64.0.0/10"); cgnat.Contains(ip) {
		return deny("cgnat")
	}
	if _, ula, _ := net.ParseCIDR("fc00::/7"); ula.Contains(ip) {
		return deny("ula")
	}
	if ip.IsPrivate() {
		return deny("private")
	}
	builtin, _ := ParseDenyRanges(nil)
	for _, n := range append(builtin, extra...) {
		if n != nil && n.Contains(ip) {
			return deny("deny_range")
		}
	}
	return allow()
}

// EntropyBitsPerChar returns Shannon entropy in bits per character.
func EntropyBitsPerChar(s string) float64 {
	if s == "" {
		return 0
	}
	counts := make(map[rune]int, len(s))
	total := 0
	for _, r := range s {
		counts[r]++
		total++
	}
	if total == 0 {
		return 0
	}
	bits := 0.0
	for _, n := range counts {
		p := float64(n) / float64(total)
		bits -= p * math.Log2(p)
	}
	if bits < 0 {
		return 0
	}
	return bits
}

// CheckURL enforces http/https, allowed ports, length, query and entropy
// limits before any connection is made. It deliberately rejects IP literals
// and userinfo: all outbound hosts must be DNS names checked in-proxy.
func CheckURL(raw string, lim URLLimits) Verdict {
	if lim.MaxURLLen > 0 && len(raw) > lim.MaxURLLen {
		return deny("url_too_long")
	}
	if strings.ContainsAny(raw, "\r\n\x00") {
		return deny("bad_url")
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return deny("bad_url")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return deny("scheme_not_allowed")
	}
	if u.Opaque != "" || u.User != nil {
		return deny("userinfo_not_allowed")
	}
	host := u.Hostname()
	port := u.Port()
	if host == "" {
		return deny("missing_host")
	}
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	n, err := ParsePort(port)
	if err != nil || !PortAllowed(n, lim.Ports) {
		return deny("port_not_allowed")
	}
	if err := ValidateHost(host); err != nil {
		return deny("bad_host")
	}
	if net.ParseIP(host) != nil {
		return deny("ip_literal")
	}
	if lim.MaxQueryLen > 0 && len(u.RawQuery) > lim.MaxQueryLen {
		return deny("query_too_long")
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return deny("bad_query")
	}
	for _, vs := range values {
		for _, v := range vs {
			if lim.MaxQueryValueLen > 0 && len(v) > lim.MaxQueryValueLen {
				return deny("query_value_too_long")
			}
			if highEntropy(v, lim) {
				return deny("high_entropy_value")
			}
		}
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if highEntropy(segment, lim) {
			return deny("high_entropy_path")
		}
	}
	return allow()
}

// CheckQuery applies the same value/entropy rules to a search query.
func CheckQuery(q string, lim URLLimits) Verdict {
	if lim.MaxQueryLen > 0 && len(q) > lim.MaxQueryLen {
		return deny("query_too_long")
	}
	for _, field := range strings.Fields(q) {
		if lim.MaxQueryValueLen > 0 && len(field) > lim.MaxQueryValueLen {
			return deny("query_value_too_long")
		}
		if highEntropy(field, lim) {
			return deny("high_entropy_value")
		}
	}
	return allow()
}

func highEntropy(s string, lim URLLimits) bool {
	if lim.EntropyMinLen <= 0 || len([]rune(s)) < lim.EntropyMinLen {
		return false
	}
	return EntropyBitsPerChar(s) >= lim.EntropyThreshold
}

// ParseHostPort strictly parses a CONNECT authority or dial address.
func ParseHostPort(raw string) (host, port string, err error) {
	if raw == "" || strings.ContainsAny(raw, " \t\r\n/\\@?#") {
		return "", "", fmt.Errorf("invalid host:port %q", raw)
	}
	host, port, err = net.SplitHostPort(raw)
	if err != nil {
		return "", "", fmt.Errorf("invalid host:port %q: %w", raw, err)
	}
	if err := ValidateHost(host); err != nil {
		return "", "", err
	}
	if _, err := ParsePort(port); err != nil {
		return "", "", fmt.Errorf("invalid host:port %q: %w", raw, err)
	}
	return host, port, nil
}

// ValidateHost accepts DNS names and IP literals, but never empty labels,
// userinfo, paths or whitespace. A trailing dot is tolerated and stripped by
// callers that need a canonical host.
func ValidateHost(host string) error {
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " \t\r\n/\\@?#[]") {
		return fmt.Errorf("invalid host %q", host)
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	h := strings.TrimSuffix(host, ".")
	if h == "" {
		return fmt.Errorf("invalid host %q", host)
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("invalid host %q", host)
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return fmt.Errorf("invalid host %q", host)
		}
	}
	return nil
}

// CanonicalHost lowercases a host and strips a trailing dot for comparisons.
func CanonicalHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return strings.TrimSuffix(host, ".")
}

// DefaultPort returns the implicit port for an http/https URL.
func DefaultPort(scheme string) string {
	if strings.EqualFold(scheme, "https") {
		return "443"
	}
	return "80"
}
