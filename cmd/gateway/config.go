package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the M1 subset of config/gateway.yaml. Secrets are never read from
// this file; their environment variable names are configurable instead.
type Config struct {
	HTTP struct {
		Listen       string `yaml:"listen"`
		UIOrigin     string `yaml:"ui_origin"`
		CookieDomain string `yaml:"cookie_domain"`
	} `yaml:"http"`
	Telegram struct {
		PollTimeoutS    int `yaml:"poll_timeout_s"`
		EditIntervalMS  int `yaml:"edit_interval_ms"`
		MaxMessageChars int `yaml:"max_message_chars"`
		PhotoMaxMB      int `yaml:"photo_max_mb"`
	} `yaml:"telegram"`
	Sessions struct {
		MaxSessions            int `yaml:"max_sessions"`
		MinFreeMemMB           int `yaml:"min_free_mem_mb"`
		WorkspaceRetentionDays int `yaml:"workspace_retention_days"`
		WorkspaceMaxMB         int `yaml:"workspace_max_mb"`
	} `yaml:"sessions"`
	Container struct {
		Image           string  `yaml:"image"`
		Runtime         string  `yaml:"runtime"`
		InternalNetwork bool    `yaml:"internal_network"`
		MemoryMB        int     `yaml:"memory_mb"`
		CPUs            float64 `yaml:"cpus"`
		Pids            int     `yaml:"pids"`
		Network         string  `yaml:"network"`
		Proxy           string  `yaml:"proxy"`
		BundleMaxMB     int     `yaml:"bundle_max_mb"`
	} `yaml:"container"`
	OpenCode struct {
		Model          string `yaml:"model"`
		RouterBaseURL  string `yaml:"router_base_url"`
		RouterTokenEnv string `yaml:"router_token_env"`
		LSP            bool   `yaml:"lsp"`
		Snapshots      bool   `yaml:"snapshots"`
	} `yaml:"opencode"`
	Git struct {
		CacheDir      string `yaml:"cache_dir"`
		WorkspacesDir string `yaml:"workspaces_dir"`
		QuarantineDir string `yaml:"quarantine_dir"`
		VerifyDir     string `yaml:"verify_dir"`
		DefaultRepo   string `yaml:"default_repo"`
	} `yaml:"git"`
	Store struct {
		DSNEnv string `yaml:"dsn_env"`
	} `yaml:"store"`
}

func defaults() Config {
	var c Config
	c.HTTP.Listen = ":8080"
	c.Telegram.PollTimeoutS = 30
	c.Telegram.EditIntervalMS = 1200
	c.Telegram.MaxMessageChars = 3800
	c.Telegram.PhotoMaxMB = 10
	c.Sessions.MaxSessions = 1
	c.Sessions.MinFreeMemMB = 3000
	c.Sessions.WorkspaceRetentionDays = 7
	c.Sessions.WorkspaceMaxMB = 2048
	c.Container.Image = "sandbox-agent-session:latest"
	c.Container.Runtime = "runc"
	c.Container.MemoryMB = 2560
	c.Container.CPUs = 1
	c.Container.Pids = 512
	c.Container.Network = "agent-session-net"
	c.Container.BundleMaxMB = 200
	c.OpenCode.Model = "padi/agent-chat"
	c.OpenCode.RouterBaseURL = "http://router:8090/v1"
	c.OpenCode.RouterTokenEnv = "ROUTER_TOKEN"
	c.OpenCode.LSP = false
	c.OpenCode.Snapshots = false
	c.Git.CacheDir = "/srv/agent/cache/repos"
	c.Git.WorkspacesDir = "/srv/agent/workspaces"
	c.Git.QuarantineDir = "/srv/agent/quarantine"
	c.Git.VerifyDir = "/srv/agent/verify"
	c.Store.DSNEnv = "DATABASE_URL"
	return c
}

func loadConfig(path string) (Config, error) {
	c := defaults()
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("gateway: read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("gateway: parse config %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	if c.Sessions.MaxSessions != 1 {
		return fmt.Errorf("gateway: M1 supports max_sessions=1 only, got %d", c.Sessions.MaxSessions)
	}
	if c.Container.MemoryMB <= 0 || c.Container.CPUs <= 0 || c.Container.Pids <= 0 {
		return fmt.Errorf("gateway: container limits must be positive")
	}
	if c.Container.Network == "" {
		return fmt.Errorf("gateway: container.network is required")
	}
	if c.OpenCode.Model == "" || !strings.Contains(c.OpenCode.Model, "/") {
		return fmt.Errorf("gateway: opencode.model must be provider/id")
	}
	if c.OpenCode.RouterBaseURL == "" || !strings.Contains(c.OpenCode.RouterBaseURL, "://") {
		return fmt.Errorf("gateway: opencode.router_base_url must be an http(s) URL")
	}
	if c.OpenCode.RouterTokenEnv == "" {
		return fmt.Errorf("gateway: opencode.router_token_env is required")
	}
	if c.Git.CacheDir == "" || c.Git.WorkspacesDir == "" {
		return fmt.Errorf("gateway: git cache/workspace dirs are required")
	}
	if c.Store.DSNEnv == "" {
		return fmt.Errorf("gateway: store.dsn_env is required")
	}
	return nil
}

// secretFromEnv returns a required environment value without ever logging it.
func secretFromEnv(name, label string) (string, error) {
	return secretFromEnvAny(label, name)
}

// secretFromEnvAny accepts the first non-empty name, so a documented env var
// and its earlier alias can both be used without duplicating configuration.
func secretFromEnvAny(label string, names ...string) (string, error) {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("gateway: %s missing (set %s)", label, strings.Join(names, " or "))
}
