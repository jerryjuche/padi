package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDisablesPlaceholderAndMissingKey(t *testing.T) {
	path := writeConfig(t, `
listen: ":8090"
providers:
  primary:
    name: Primary
    base_url: "https://primary.invalid/v1"
    api_key_env: "ROUTER_TEST_KEY"
    rpm: 10
    concurrency: 1
    supports_stream: true
    supports_tools: true
    supports_images: false
    max_context: 1000
  spare:
    name: Spare
    base_url: "https://spare.invalid/v1"
    api_key_env: "MISSING_ROUTER_TEST_KEY"
    rpm: 10
    concurrency: 1
routes:
  chat:
    - provider: primary
      model: "real/model"
      priority: 1
    - provider: primary
      model: "<placeholder>"
      priority: 2
    - provider: spare
      model: "spare/model"
      priority: 3
retry: {attempts: 2, backoff: 10ms}
breaker: {consecutive_failures: 2, initial_cooldown: 1s, max_cooldown: 5s}
rl_cooldown: [1s, 2s]
queue_max_wait: 1s
first_byte_timeout: 1s
total_timeout: 5s
`)
	env := map[string]string{"ROUTER_TOKEN": "router-token", "ROUTER_TEST_KEY": "not-a-real-secret"}
	cfg, err := Load(path, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers["primary"].Enabled {
		t.Fatal("primary provider should be enabled")
	}
	if cfg.Providers["spare"].Enabled {
		t.Fatal("provider with missing key should be disabled")
	}
	if !cfg.Routes["chat"][0].Enabled || cfg.Routes["chat"][1].Enabled || cfg.Routes["chat"][2].Enabled {
		t.Fatalf("unexpected target enablement: %+v", cfg.Routes["chat"])
	}
	if got := cfg.EnabledAliases(); len(got) != 1 || got[0] != "chat" {
		t.Fatalf("aliases=%v", got)
	}
	if len(cfg.Warnings) < 2 {
		t.Fatalf("warnings=%v", cfg.Warnings)
	}
	if cfg.Retry.Backoff != 10*time.Millisecond {
		t.Fatalf("backoff=%v", cfg.Retry.Backoff)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, "listen: \":8090\"\nunknown_field: true\n")
	if _, err := Load(path, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("err=%v", err)
	}
}

func TestFullAliasesWithOnlyNvidiaKey(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "router.yaml")
	env := map[string]string{
		"ROUTER_TOKEN":        "router-token",
		"NVIDIA_API_KEY":      "only-key-on-this-host",
		"ROUTER_DATABASE_URL": "postgres://example.invalid/agent",
	}
	cfg, err := Load(path, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{
		"agent-build", "agent-plan", "agent-chat", "agent-small",
		"agent-think", "agent-vision", "agent-embed", "agent-eval",
	} {
		if _, ok := cfg.Routes[alias]; !ok {
			t.Fatalf("alias %s missing", alias)
		}
		foundPlaceholder := false
		for _, target := range cfg.Routes[alias] {
			if strings.Contains(target.Model, "<placeholder>") {
				foundPlaceholder = true
				if target.Enabled {
					t.Fatalf("alias %s placeholder target enabled", alias)
				}
			}
		}
		if (alias == "agent-build" || alias == "agent-eval") && !foundPlaceholder {
			t.Fatalf("alias %s should retain keyless placeholders", alias)
		}
	}
	enabled := cfg.EnabledAliases()
	sort.Strings(enabled)
	want := []string{"agent-build", "agent-chat", "agent-plan", "agent-small", "agent-think", "agent-vision"}
	if strings.Join(enabled, ",") != strings.Join(want, ",") {
		t.Fatalf("enabled=%v want %v", enabled, want)
	}
	if cfg.AliasEnabled("agent-eval") || cfg.AliasEnabled("agent-embed") {
		t.Fatal("eval and embed must stay disabled without independent keys or embeddings handler")
	}
	if !cfg.Providers["nvidia"].Enabled || cfg.Providers["groq"].Enabled || cfg.Providers["openrouter"].Enabled || cfg.Providers["deepseek"].Enabled {
		t.Fatalf("provider enablement=%+v", cfg.Providers)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "<placeholder>") {
		t.Fatalf("warnings=%v", cfg.Warnings)
	}
}
