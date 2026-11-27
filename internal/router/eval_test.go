package router

import (
	"reflect"
	"testing"

	"github.com/jerryjuche/padi/internal/router/config"
)

func evalConfig(buildProviders, evalProviders []string, enabled map[string]bool) *config.Config {
	cfg := &config.Config{
		Providers: map[string]config.Provider{},
		Routes: map[string][]config.Target{
			"agent-build": {},
			"agent-eval":  {},
		},
	}
	names := append(append([]string{}, buildProviders...), evalProviders...)
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		p := config.Provider{Name: name, BaseURL: "https://provider.invalid/v1", APIKey: "key", RPM: 10, Concurrency: 1, Enabled: enabled[name]}
		cfg.Providers[name] = p
	}
	for i, name := range buildProviders {
		cfg.Routes["agent-build"] = append(cfg.Routes["agent-build"], config.Target{
			Provider: name, Model: name + "/build", Priority: i + 1,
			Enabled: enabled[name] && cfg.Providers[name].Enabled,
		})
	}
	for i, name := range evalProviders {
		cfg.Routes["agent-eval"] = append(cfg.Routes["agent-eval"], config.Target{
			Provider: name, Model: name + "/eval", Priority: i + 1,
			Enabled: enabled[name] && cfg.Providers[name].Enabled,
		})
	}
	return cfg
}

func TestEvalIsolationTable(t *testing.T) {
	cases := []struct {
		name         string
		build        []string
		eval         []string
		enabled      map[string]bool
		wantExcluded []string
		wantIndep    int
		wantBlocked  int
		wantDisabled int
		wantReasons  map[string]string
	}{
		{
			name: "independent provider available", build: []string{"nvidia"},
			eval: []string{"nvidia", "groq"}, enabled: map[string]bool{"nvidia": true, "groq": true},
			wantExcluded: []string{"nvidia"}, wantIndep: 1, wantBlocked: 1,
			wantReasons: map[string]string{"nvidia": "same_provider_blocked", "groq": "available"},
		},
		{
			name: "only one provider", build: []string{"nvidia"},
			eval: []string{"nvidia"}, enabled: map[string]bool{"nvidia": true},
			wantExcluded: []string{"nvidia"}, wantIndep: 0, wantBlocked: 1,
			wantReasons: map[string]string{"nvidia": "same_provider_blocked"},
		},
		{
			name: "all build providers excluded", build: []string{"nvidia", "groq"},
			eval: []string{"nvidia", "groq", "openrouter"}, enabled: map[string]bool{"nvidia": true, "groq": true, "openrouter": true},
			wantExcluded: []string{"nvidia", "groq"}, wantIndep: 1, wantBlocked: 2,
			wantReasons: map[string]string{"nvidia": "same_provider_blocked", "groq": "same_provider_blocked", "openrouter": "available"},
		},
		{
			name: "eval targets disabled", build: []string{"nvidia"},
			eval: []string{"groq"}, enabled: map[string]bool{"nvidia": true, "groq": false},
			wantExcluded: []string{"nvidia"}, wantIndep: 0, wantBlocked: 0, wantDisabled: 1,
			wantReasons: map[string]string{"groq": "disabled"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := evalConfig(tc.build, tc.eval, tc.enabled)
			excluded := buildProviders(cfg)
			if !reflect.DeepEqual(excluded, tc.wantExcluded) {
				t.Fatalf("excluded=%v want %v", excluded, tc.wantExcluded)
			}
			independent, blocked, disabled := evalAvailability(cfg, excluded)
			if independent != tc.wantIndep || blocked != tc.wantBlocked || disabled != tc.wantDisabled {
				t.Fatalf("counts=(%d,%d,%d) want (%d,%d,%d)", independent, blocked, disabled, tc.wantIndep, tc.wantBlocked, tc.wantDisabled)
			}
			if got := evalErrorReasons(cfg, excluded); !reflect.DeepEqual(got, tc.wantReasons) {
				t.Fatalf("reasons=%v want %v", got, tc.wantReasons)
			}
		})
	}
}

func TestEvalAliasNormalization(t *testing.T) {
	for _, tc := range []struct {
		alias string
		want  bool
	}{
		{"agent-eval", true},
		{"padi/agent-eval", true},
		{" agent-eval ", true},
		{"agent-chat", false},
	} {
		if got := isEvalAlias(tc.alias); got != tc.want {
			t.Fatalf("isEvalAlias(%q)=%t want %t", tc.alias, got, tc.want)
		}
	}
}
