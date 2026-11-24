// Package route filters configured targets by capability and context size.
package route

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jerryjuche/padi/internal/router/config"
)

// Request is the normalised view of a chat-completions request.
type Request struct {
	Alias     string
	Stream    bool
	HasTools  bool
	HasImages bool
	EstTokens int
	SessionID string
	Headers   map[string]string
	Body      []byte
}

// Router resolves aliases to ordered candidates.
type Router struct {
	Cfg *config.Config
}

func New(cfg *config.Config) *Router { return &Router{Cfg: cfg} }

// Candidates returns enabled targets that can serve req, in priority order.
func (r *Router) Candidates(req Request) ([]config.Target, error) {
	alias := strings.TrimSpace(req.Alias)
	if i := strings.LastIndex(alias, "/"); i >= 0 {
		alias = alias[i+1:]
	}
	targets, ok := r.Cfg.Routes[alias]
	if !ok {
		return nil, fmt.Errorf("route: unknown alias %q", alias)
	}
	var out []config.Target
	for _, t := range targets {
		if !t.Enabled {
			continue
		}
		p, ok := r.Cfg.Providers[t.Provider]
		if !ok || !p.Enabled {
			continue
		}
		if req.Stream && !p.SupportsStream {
			continue
		}
		if req.HasTools && !p.SupportsTools {
			continue
		}
		if req.HasImages && !p.SupportsImages {
			continue
		}
		if p.MaxContext > 0 && req.EstTokens > p.MaxContext {
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("route: no enabled target for alias %q", alias)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}
