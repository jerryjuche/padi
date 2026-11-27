// Package config loads the M2 router configuration. Secrets are referenced by
// environment-variable name and never stored in the YAML file.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const placeholder = "<placeholder>"

// Config is the validated router configuration.
type Config struct {
	Listen           string              `yaml:"listen"`
	RouterTokenEnv   string              `yaml:"router_token_env"`
	RouterToken      string              `yaml:"-"`
	UsageDSNEnv      string              `yaml:"usage_dsn_env"`
	UsageDSN         string              `yaml:"-"`
	Providers        map[string]Provider `yaml:"providers"`
	Routes           map[string][]Target `yaml:"routes"`
	Retry            RetryCfg            `yaml:"retry"`
	Breaker          BreakerCfg          `yaml:"breaker"`
	RLCooldown       []time.Duration     `yaml:"rl_cooldown"`
	QueueMaxWait     time.Duration       `yaml:"queue_max_wait"`
	FirstByteTimeout time.Duration       `yaml:"first_byte_timeout"`
	TotalTimeout     time.Duration       `yaml:"total_timeout"`
	Warnings         []string            `yaml:"-"`
}

// Provider is one upstream provider. APIKey is resolved from the environment
// at load time and is never serialised.
type Provider struct {
	Name               string `yaml:"name"`
	BaseURL            string `yaml:"base_url"`
	APIKeyEnv          string `yaml:"api_key_env"`
	APIKey             string `yaml:"-"`
	Package            string `yaml:"package"`
	RPM                int    `yaml:"rpm"`
	Concurrency        int    `yaml:"concurrency"`
	SupportsStream     bool   `yaml:"supports_stream"`
	SupportsTools      bool   `yaml:"supports_tools"`
	SupportsImages     bool   `yaml:"supports_images"`
	MaxContext         int    `yaml:"max_context"`
	DailyTokenCap      int    `yaml:"daily_token_cap"`
	CostPer1MInMicros  int64  `yaml:"cost_per_1m_in_micros"`
	CostPer1MOutMicros int64  `yaml:"cost_per_1m_out_micros"`
	Enabled            bool   `yaml:"-"`
}

// Target is one ordered candidate for an alias.
type Target struct {
	Provider       string `yaml:"provider"`
	Model          string `yaml:"model"`
	Priority       int    `yaml:"priority"`
	SupportsImages bool   `yaml:"supports_images"`
	Embeddings     bool   `yaml:"embeddings"`
	Dims           int    `yaml:"dims"`
	Enabled        bool   `yaml:"-"`
}

// RetryCfg controls retries inside the same target before failover.
type RetryCfg struct {
	Attempts int           `yaml:"attempts"`
	Backoff  time.Duration `yaml:"backoff"`
}

// BreakerCfg controls the minimal circuit-breaker used by the handler.
type BreakerCfg struct {
	ConsecutiveFailures int           `yaml:"consecutive_failures"`
	InitialCooldown     time.Duration `yaml:"initial_cooldown"`
	MaxCooldown         time.Duration `yaml:"max_cooldown"`
}

func defaults() *Config {
	return &Config{
		Listen:           ":8090",
		RouterTokenEnv:   "ROUTER_TOKEN",
		UsageDSNEnv:      "ROUTER_DATABASE_URL",
		Retry:            RetryCfg{Attempts: 3, Backoff: 150 * time.Millisecond},
		Breaker:          BreakerCfg{ConsecutiveFailures: 3, InitialCooldown: 10 * time.Second, MaxCooldown: 2 * time.Minute},
		RLCooldown:       []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 15 * time.Second, time.Minute},
		QueueMaxWait:     20 * time.Second,
		FirstByteTimeout: 15 * time.Second,
		TotalTimeout:     120 * time.Second,
	}
}

// Load reads strict YAML, resolves secrets and disables unusable targets.
func Load(path string, lookup func(string) string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("router config: read %s: %w", path, err)
	}
	c := defaults()
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("router config: parse %s: %w", path, err)
	}
	if lookup == nil {
		lookup = os.Getenv
	}
	if c.RouterTokenEnv == "" {
		c.RouterTokenEnv = "ROUTER_TOKEN"
	}
	c.RouterToken = lookup(c.RouterTokenEnv)
	if c.RouterToken == "" {
		c.Warnings = append(c.Warnings, c.RouterTokenEnv+" is empty; /v1 and /status will refuse requests")
	}
	if c.UsageDSNEnv == "" {
		c.UsageDSNEnv = "ROUTER_DATABASE_URL"
	}
	c.UsageDSN = lookup(c.UsageDSNEnv)
	if c.UsageDSN == "" {
		c.Warnings = append(c.Warnings, c.UsageDSNEnv+" is empty; llm_usage recording disabled")
	}
	for name, p := range c.Providers {
		if p.Name == "" {
			p.Name = name
		}
		if p.APIKeyEnv != "" {
			p.APIKey = lookup(p.APIKeyEnv)
			if p.APIKey == "" {
				c.Warnings = append(c.Warnings, "provider "+name+": "+p.APIKeyEnv+" is empty; provider disabled")
			}
		}
		p.Enabled = p.APIKey != "" && p.BaseURL != ""
		if p.Enabled {
			p.Enabled = true
		}
		c.Providers[name] = p
	}
	for alias, targets := range c.Routes {
		enabled := 0
		for i, t := range targets {
			t.Enabled = true
			p, ok := c.Providers[t.Provider]
			if !ok {
				return nil, fmt.Errorf("router config: alias %s target %s references undefined provider %s", alias, t.Model, t.Provider)
			}
			if !p.Enabled {
				t.Enabled = false
				continue
			}
			if strings.Contains(strings.ToLower(t.Model), placeholder) {
				t.Enabled = false
				c.Warnings = append(c.Warnings, "alias "+alias+" target "+t.Provider+" has "+placeholder+"; target disabled")
				continue
			}
			targets[i] = t
			enabled++
		}
		c.Routes[alias] = targets
		if len(targets) == 0 {
			return nil, fmt.Errorf("router config: alias %s has no targets", alias)
		}
		if enabled == 0 {
			c.Warnings = append(c.Warnings, "alias "+alias+" has no enabled targets")
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.Listen == "" {
		return fmt.Errorf("router config: listen is required")
	}
	if len(c.Providers) == 0 || len(c.Routes) == 0 {
		return fmt.Errorf("router config: providers and routes are required")
	}
	for name, p := range c.Providers {
		if p.RPM <= 0 || p.Concurrency <= 0 {
			return fmt.Errorf("router config: provider %s rpm and concurrency must be positive", name)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("router config: provider %s base_url is required", name)
		}
	}
	if c.Retry.Attempts <= 0 || c.Retry.Backoff <= 0 {
		return fmt.Errorf("router config: retry attempts and backoff must be positive")
	}
	if c.Breaker.ConsecutiveFailures <= 0 || c.Breaker.InitialCooldown <= 0 || c.Breaker.MaxCooldown <= 0 {
		return fmt.Errorf("router config: breaker thresholds and cooldowns must be positive")
	}
	if len(c.RLCooldown) == 0 {
		return fmt.Errorf("router config: rl_cooldown must not be empty")
	}
	for _, d := range c.RLCooldown {
		if d <= 0 {
			return fmt.Errorf("router config: rl_cooldown values must be positive")
		}
	}
	if c.QueueMaxWait <= 0 || c.FirstByteTimeout <= 0 || c.TotalTimeout <= 0 {
		return fmt.Errorf("router config: queue/first-byte/total timeouts must be positive")
	}
	return nil
}

// EnabledAliases returns aliases with at least one enabled target.
func (c *Config) EnabledAliases() []string {
	out := make([]string, 0, len(c.Routes))
	for alias, targets := range c.Routes {
		for _, t := range targets {
			if t.Enabled && c.Providers[t.Provider].Enabled {
				out = append(out, alias)
				break
			}
		}
	}
	return out
}

// TargetFor returns a target by provider/model pair, used by tests and models.
func (c *Config) TargetFor(provider, model string) (Target, bool) {
	for _, targets := range c.Routes {
		for _, t := range targets {
			if t.Provider == provider && t.Model == model {
				return t, true
			}
		}
	}
	return Target{}, false
}

// AliasEnabled reports whether an alias has at least one callable target.
func (c *Config) AliasEnabled(alias string) bool {
	for _, t := range c.Routes[alias] {
		if t.Enabled && c.Providers[t.Provider].Enabled {
			return true
		}
	}
	return false
}

// EnabledTargetCount is the number of callable targets for an alias.
func (c *Config) EnabledTargetCount(alias string) int {
	n := 0
	for _, t := range c.Routes[alias] {
		if t.Enabled && c.Providers[t.Provider].Enabled {
			n++
		}
	}
	return n
}
