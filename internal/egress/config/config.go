// Package config loads and validates the M3a egress-proxy configuration.
// Tokens are never read from the YAML file; they come from the environment.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the validated egress-proxy configuration.
type Config struct {
	Listen         ListenCfg      `yaml:"listen"`
	BuildAllowlist []string       `yaml:"build_allowlist"`
	DenyRanges     []string       `yaml:"deny_ranges"`
	Ports          []int          `yaml:"ports"`
	Research       ResearchCfg    `yaml:"research"`
	Limits         LimitsCfg      `yaml:"limits"`
	Credentials    CredentialsCfg `yaml:"credentials"`
	LogPath        string         `yaml:"log_path"`

	DenyNets    []*net.IPNet `yaml:"-"`
	RouterToken string       `yaml:"-"`
	AdminToken  string       `yaml:"-"`
	Warnings    []string     `yaml:"-"`
}

// ListenCfg holds the four listener addresses.
type ListenCfg struct {
	Proxy      string `yaml:"proxy"`
	Research   string `yaml:"research"`
	Admin      string `yaml:"admin"`
	Credential string `yaml:"credential"`
}

// ResearchCfg controls the read-only research API.
type ResearchCfg struct {
	Enabled              bool      `yaml:"enabled"`
	MaxBodyMB            int       `yaml:"max_body_mb"`
	MaxRedirects         int       `yaml:"max_redirects"`
	MaxChars             int       `yaml:"max_chars"`
	URLMaxLen            int       `yaml:"url_max_len"`
	QueryValueMaxLen     int       `yaml:"query_value_max_len"`
	QueryMaxLen          int       `yaml:"query_max_len"`
	EntropyThresholdBits float64   `yaml:"entropy_threshold_bits"`
	EntropyMinLen        int       `yaml:"entropy_min_len"`
	RatePerMinSession    int       `yaml:"rate_per_min_session"`
	MaxPerTask           int       `yaml:"max_per_task"`
	Search               SearchCfg `yaml:"search"`
}

// SearchCfg selects the M12 search provider. "none" is the M3a default.
type SearchCfg struct {
	Provider   string        `yaml:"provider"`
	URL        string        `yaml:"url"`
	Engines    []string      `yaml:"engines"`
	MaxResults int           `yaml:"max_results"`
	Timeout    time.Duration `yaml:"timeout_s"`
}

// LimitsCfg holds build-proxy per-session limits.
type LimitsCfg struct {
	SessionBytesMB     int `yaml:"session_bytes_mb"`
	SessionConnsPerMin int `yaml:"session_conns_per_min"`
}

// CredentialsCfg is the M16 reverse-proxy route configuration. Routes stay
// disabled until services are configured; this milestone only owns the stub.
type CredentialsCfg struct {
	Listen   string              `yaml:"listen"`
	Services []CredentialService `yaml:"services"`
}

// CredentialService describes one future M16 route. Values are never stored
// here from the environment; ValueEnv is only the name of the variable.
type CredentialService struct {
	Name         string        `yaml:"name"`
	Upstream     string        `yaml:"upstream"`
	PathPrefixes []string      `yaml:"path_prefixes"`
	Methods      []string      `yaml:"methods"`
	Header       string        `yaml:"header"`
	ValueEnv     string        `yaml:"value_env"`
	TTL          time.Duration `yaml:"ttl"`
}

func defaults() *Config {
	return &Config{
		Listen: ListenCfg{
			Proxy:      ":3128",
			Research:   ":3129",
			Admin:      ":3130",
			Credential: ":3131",
		},
		BuildAllowlist: []string{},
		DenyRanges:     []string{},
		Ports:          []int{80, 443},
		Research: ResearchCfg{
			Enabled:              true,
			MaxBodyMB:            2,
			MaxRedirects:         5,
			MaxChars:             40000,
			URLMaxLen:            512,
			QueryValueMaxLen:     100,
			QueryMaxLen:          200,
			EntropyThresholdBits: 4.2,
			EntropyMinLen:        32,
			RatePerMinSession:    30,
			MaxPerTask:           150,
			Search: SearchCfg{
				Provider:   "none",
				MaxResults: 8,
				Timeout:    8 * time.Second,
			},
		},
		Limits: LimitsCfg{SessionBytesMB: 500, SessionConnsPerMin: 300},
		Credentials: CredentialsCfg{
			Listen:   ":3131",
			Services: []CredentialService{},
		},
		LogPath: "/var/log/agent/egress.ndjson",
	}
}

// Load reads strict YAML, resolves tokens from the environment and validates
// every security-relevant field before any listener starts.
func Load(path string, lookup func(string) string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("egress config: read %s: %w", path, err)
	}
	c := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("egress config: parse %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("egress config: %s has more than one YAML document", path)
		}
		return nil, fmt.Errorf("egress config: parse %s: %w", path, err)
	}
	if lookup == nil {
		lookup = os.Getenv
	}
	c.RouterToken = lookup("ROUTER_TOKEN")
	c.AdminToken = lookup("EGRESS_ADMIN_TOKEN")
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	listens := map[string]string{}
	for name, addr := range map[string]string{
		"proxy": c.Listen.Proxy, "research": c.Listen.Research,
		"admin": c.Listen.Admin, "credential": c.Credentials.Listen,
	} {
		if addr == "" {
			return fmt.Errorf("egress config: %s listen is required", name)
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("egress config: %s listen %q is not host:port: %w", name, addr, err)
		}
		if other, ok := listens[addr]; ok {
			return fmt.Errorf("egress config: %s and %s cannot share listen address %q", other, name, addr)
		}
		listens[addr] = name
	}
	if len(c.Ports) == 0 {
		return errors.New("egress config: ports must not be empty")
	}
	seenPorts := map[int]bool{}
	for _, p := range c.Ports {
		if p != 80 && p != 443 {
			return fmt.Errorf("egress config: port %d is not allowed; only 80 and 443 are permitted", p)
		}
		seenPorts[p] = true
	}
	for _, host := range c.BuildAllowlist {
		if err := ValidateAllowlistEntry(host); err != nil {
			return fmt.Errorf("egress config: build_allowlist: %w", err)
		}
	}
	nets, err := ParseDenyRanges(c.DenyRanges)
	if err != nil {
		return fmt.Errorf("egress config: deny_ranges: %w", err)
	}
	c.DenyNets = nets

	r := &c.Research
	if r.MaxBodyMB <= 0 || r.MaxChars <= 0 || r.URLMaxLen <= 0 ||
		r.QueryValueMaxLen <= 0 || r.QueryMaxLen <= 0 || r.EntropyMinLen <= 0 ||
		r.EntropyThresholdBits <= 0 || r.RatePerMinSession <= 0 || r.MaxPerTask <= 0 {
		return errors.New("egress config: research limits must be positive")
	}
	if r.MaxRedirects < 0 {
		return errors.New("egress config: research.max_redirects must not be negative")
	}
	if r.MaxRedirects > 5 {
		c.Warnings = append(c.Warnings, "research.max_redirects exceeds 5; clamped to 5")
		r.MaxRedirects = 5
	}
	switch r.Search.Provider {
	case "none":
	case "searxng":
		if r.Search.URL == "" || hasPlaceholder(r.Search.Engines) {
			c.Warnings = append(c.Warnings, "search provider searxng is not configured; using none")
			r.Search.Provider = "none"
		}
	default:
		return fmt.Errorf("egress config: unknown search provider %q", r.Search.Provider)
	}
	if r.Search.MaxResults <= 0 || r.Search.Timeout <= 0 {
		return errors.New("egress config: search max_results and timeout_s must be positive")
	}
	if c.Limits.SessionBytesMB <= 0 || c.Limits.SessionConnsPerMin <= 0 {
		return errors.New("egress config: limits must be positive")
	}
	for i, svc := range c.Credentials.Services {
		if svc.Name == "" || svc.Upstream == "" {
			return fmt.Errorf("egress config: credentials service %d needs name and upstream", i)
		}
	}
	if c.LogPath == "" {
		return errors.New("egress config: log_path is required")
	}
	return nil
}

func hasPlaceholder(values []string) bool {
	for _, v := range values {
		if strings.Contains(v, "<") || strings.Contains(v, ">") {
			return true
		}
	}
	return false
}

// URLLimits is the subset of configuration used by URL validation.
type URLLimits struct {
	Ports            []int
	MaxURLLen        int
	MaxQueryValueLen int
	MaxQueryLen      int
	EntropyThreshold float64
	EntropyMinLen    int
}

// URLLimits returns the limits used by the URL guard.
func (c *Config) URLLimits() URLLimits {
	return URLLimits{
		Ports:            append([]int(nil), c.Ports...),
		MaxURLLen:        c.Research.URLMaxLen,
		MaxQueryValueLen: c.Research.QueryValueMaxLen,
		MaxQueryLen:      c.Research.QueryMaxLen,
		EntropyThreshold: c.Research.EntropyThresholdBits,
		EntropyMinLen:    c.Research.EntropyMinLen,
	}
}

// ValidateAllowlistEntry accepts an exact host or a "*.suffix" pattern.
func ValidateAllowlistEntry(entry string) error {
	host := entry
	if strings.HasPrefix(host, "*.") {
		host = strings.TrimPrefix(host, "*.")
	}
	if host == "" || strings.ContainsAny(host, "*/:@?# ") {
		return fmt.Errorf("invalid allowlist host %q", entry)
	}
	return ValidateHost(host)
}

// ParseDenyRanges parses configured CIDRs and returns them alongside the
// always-on deny ranges. Built-in ranges cannot be removed by configuration.
func ParseDenyRanges(extra []string) ([]*net.IPNet, error) {
	builtins := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
		"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128", "fc00::/7",
		"fe80::/10", "ff00::/8",
	}
	out := make([]*net.IPNet, 0, len(builtins)+len(extra))
	for _, cidr := range append(builtins, extra...) {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// ParsePort expects a plain decimal port with no sign or whitespace.
func ParsePort(raw string) (int, error) {
	if raw == "" {
		return 0, errors.New("empty port")
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid port %q", raw)
		}
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("invalid port %q", raw)
	}
	return n, nil
}

// PortAllowed reports whether port is in the configured forward-port set.
func PortAllowed(port int, allowed []int) bool {
	for _, p := range allowed {
		if p == port {
			return true
		}
	}
	return false
}
