package router

import (
	"net/http"
	"sort"
)

// status reports operational state only; no key, token or base URL is exposed.
func (h *Handler) status(w http.ResponseWriter) {
	now := h.clock.Now()
	h.mu.Lock()
	providers := make(map[string]any, len(h.cfg.Providers))
	aliases := make(map[string]any, len(h.cfg.Routes))
	usage := map[string]any{"enabled": false, "dropped": uint64(0)}
	if pgw, ok := h.usage.(*PGUsageWriter); ok {
		usage["enabled"] = true
		usage["dropped"] = pgw.Dropped()
	}
	for name, p := range h.cfg.Providers {
		providers[name] = map[string]any{
			"limiter_used":       h.limiters[name].Used(),
			"rpm":                p.RPM,
			"breaker_state":      string(h.breakers[name].State(now)),
			"cooldown_until":     formatUntil(h.cooldown[name], now),
			"last_error_class":   string(h.lastError[name]),
			"concurrency_in_use": h.sems[name].InUse(),
			"daily_tokens":       h.dailyTokens[name],
		}
	}
	for alias, targets := range h.cfg.Routes {
		enabled := 0
		for _, t := range targets {
			if t.Enabled && h.cfg.Providers[t.Provider].Enabled {
				enabled++
			}
		}
		entry := map[string]any{"enabled_targets": enabled, "total_targets": len(targets)}
		if isEvalAlias(alias) {
			excluded := buildProviders(h.cfg)
			independent, blocked, disabled := evalAvailability(h.cfg, excluded)
			sort.Strings(excluded)
			entry["excluded_build_providers"] = excluded
			entry["independent_targets"] = independent
			entry["blocked_targets"] = blocked
			entry["disabled_targets"] = disabled
		}
		aliases[alias] = entry
	}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers, "aliases": aliases, "usage": usage})
}
