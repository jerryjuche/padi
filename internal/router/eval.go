package router

import (
	"github.com/jerryjuche/padi/internal/router/config"
	"github.com/jerryjuche/padi/internal/router/route"
)

// evalAlias is the only alias subject to the cross-provider rule. Evaluation
// must not judge work produced by the same upstream provider that wrote it.
const evalAlias = "agent-eval"

func isEvalAlias(alias string) bool {
	return route.NormalizeAlias(alias) == evalAlias
}

// buildProviders returns every provider currently able to serve agent-build.
// All are excluded from agent-eval candidates; a task must be judged elsewhere.
func buildProviders(cfg *config.Config) []string {
	var out []string
	seen := make(map[string]bool)
	for _, t := range cfg.Routes["agent-build"] {
		if !t.Enabled || seen[t.Provider] || !cfg.Providers[t.Provider].Enabled {
			continue
		}
		seen[t.Provider] = true
		out = append(out, t.Provider)
	}
	return out
}

// evalAvailability counts usable eval targets after the build-provider
// exclusion. blocked counts enabled targets that were rejected by that rule;
// disabled counts placeholder or keyless targets.
func evalAvailability(cfg *config.Config, excluded []string) (independent, blocked, disabled int) {
	for _, t := range cfg.Routes[evalAlias] {
		if !t.Enabled || !cfg.Providers[t.Provider].Enabled {
			disabled++
			continue
		}
		if containsProvider(excluded, t.Provider) {
			blocked++
			continue
		}
		independent++
	}
	return independent, blocked, disabled
}

func containsProvider(list []string, provider string) bool {
	for _, name := range list {
		if name == provider {
			return true
		}
	}
	return false
}

// evalErrorReasons is safe for the owner: provider names and state names only.
// No model ID, key, base URL or header value is included.
func evalErrorReasons(cfg *config.Config, excluded []string) map[string]string {
	reasons := make(map[string]string, len(cfg.Routes[evalAlias]))
	for _, t := range cfg.Routes[evalAlias] {
		switch {
		case !t.Enabled || !cfg.Providers[t.Provider].Enabled:
			reasons[t.Provider] = "disabled"
		case containsProvider(excluded, t.Provider):
			reasons[t.Provider] = "same_provider_blocked"
		default:
			reasons[t.Provider] = "available"
		}
	}
	if len(reasons) == 0 {
		reasons["agent-build"] = "single_provider"
	}
	return reasons
}
