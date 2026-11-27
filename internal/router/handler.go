// Package router implements the M2/M4 chat-completions handler: capability
// filtering, RPM wait, concurrency, retries, cooldowns and fallback.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jerryjuche/padi/internal/router/breaker"
	"github.com/jerryjuche/padi/internal/router/config"
	"github.com/jerryjuche/padi/internal/router/limiter"
	"github.com/jerryjuche/padi/internal/router/route"
	"github.com/jerryjuche/padi/internal/router/upstream"
)

// Handler serves /healthz, /status, /v1/models and /v1/chat/completions.
type Handler struct {
	cfg      *config.Config
	routes   *route.Router
	upstream *upstream.Client
	limiters map[string]*limiter.Sliding
	sems     map[string]*limiter.Sem
	clock    limiter.Clock
	usage    Recorder

	mu           sync.Mutex
	breakers     map[string]*breaker.Breaker
	cooldown     map[string]time.Time
	cooldownStep map[string]int
	lastError    map[string]upstream.Outcome
	dailyTokens  map[string]int
}

// NewHandler builds all per-provider state from config. A recorder may be
// supplied for usage rows; tests and deployments without a DSN get a no-op.
func NewHandler(cfg *config.Config, clock limiter.Clock, recorder ...Recorder) *Handler {
	if clock == nil {
		clock = limiter.RealClock{}
	}
	var rec Recorder = noopRecorder{}
	if len(recorder) > 0 && recorder[0] != nil {
		rec = recorder[0]
	}
	h := &Handler{
		cfg:          cfg,
		routes:       route.New(cfg),
		upstream:     upstream.New(cfg.FirstByteTimeout, cfg.TotalTimeout),
		limiters:     make(map[string]*limiter.Sliding, len(cfg.Providers)),
		sems:         make(map[string]*limiter.Sem, len(cfg.Providers)),
		clock:        clock,
		usage:        rec,
		breakers:     make(map[string]*breaker.Breaker, len(cfg.Providers)),
		cooldown:     make(map[string]time.Time),
		cooldownStep: make(map[string]int),
		lastError:    make(map[string]upstream.Outcome),
		dailyTokens:  make(map[string]int),
	}
	for name, p := range cfg.Providers {
		h.limiters[name] = limiter.NewSliding(p.RPM, clock)
		h.sems[name] = limiter.NewSem(p.Concurrency)
		h.breakers[name] = breaker.New()
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		h.health(w)
		return
	case "/status":
		if !h.authorized(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized", nil)
			return
		}
		h.status(w)
		return
	case "/v1/models":
		if !h.authorized(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized", nil)
			return
		}
		h.models(w)
		return
	case "/v1/chat/completions":
		if !h.authorized(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized", nil)
			return
		}
		h.chat(w, r)
		return
	default:
		writeError(w, http.StatusNotFound, "not_found", nil)
	}
}

func (h *Handler) authorized(r *http.Request) bool {
	if h.cfg.RouterToken == "" {
		return false
	}
	header := r.Header.Get("Authorization")
	return strings.TrimSpace(header) == "Bearer "+h.cfg.RouterToken
}

func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", nil)
		return
	}
	var envelope struct {
		Model    string            `json:"model"`
		Stream   bool              `json:"stream"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", nil)
		return
	}
	alias := envelope.Model
	if alias == "" {
		if enabled := h.cfg.EnabledAliases(); len(enabled) > 0 {
			alias = enabled[0]
		}
	}
	normalized := route.NormalizeAlias(alias)
	req := route.Request{
		Alias:     alias,
		Body:      raw,
		Stream:    envelope.Stream,
		HasTools:  len(envelope.Tools) > 0,
		HasImages: hasImages(envelope.Messages),
		EstTokens: len(raw) / 4,
		Headers:   map[string]string{},
	}
	for _, key := range []string{"X-Task-Id", "X-Session-Id"} {
		if v := r.Header.Get(key); v != "" {
			req.Headers[key] = v
		}
	}

	// Evaluation must judge a build from a different upstream. Excluding every
	// enabled agent-build provider here keeps the rule independent of target
	// order, retries and the per-provider breaker state.
	excluded := []string(nil)
	if isEvalAlias(alias) {
		excluded = buildProviders(h.cfg)
		req.ExcludeProviders = excluded
		if independent, _, _ := evalAvailability(h.cfg, excluded); independent == 0 {
			writeError(w, http.StatusServiceUnavailable, "eval_provider_isolation", evalErrorReasons(h.cfg, excluded))
			return
		}
	}

	w.Header().Set("X-Padi-Alias", normalized)
	candidates, err := h.routes.Candidates(req)
	if err != nil {
		if strings.Contains(err.Error(), "unknown alias") {
			writeError(w, http.StatusNotFound, "unknown_model", nil)
		} else {
			writeError(w, http.StatusServiceUnavailable, "no_enabled_target", nil)
		}
		return
	}

	reasons := map[string]string{}
	var retryAfter time.Duration
	for depth, target := range candidates {
		p := h.cfg.Providers[target.Provider]
		// Safe routing metadata. The provider name is operational state, not a
		// secret; agent-eval uses it to prove cross-provider isolation.
		w.Header().Set("X-Padi-Provider", target.Provider)
		now := h.clock.Now()
		record := func(status int, res upstream.Result, elapsed time.Duration) {
			if status <= 0 {
				status = http.StatusBadGateway
			}
			tokensIn, tokensOut := res.TokensIn, res.TokensOut
			h.usage.Record(UsageRow{
				Time:          time.Now().UTC(),
				SessionID:     req.Headers["X-Session-Id"],
				TaskID:        req.Headers["X-Task-Id"],
				Alias:         normalized,
				Provider:      target.Provider,
				Model:         target.Model,
				Status:        status,
				FallbackDepth: depth,
				FirstByte:     res.FirstByte,
				Total:         elapsed,
				TokensIn:      tokensIn,
				TokensOut:     tokensOut,
				CostMicros:    costMicros(tokensIn, tokensOut, p),
				QuotaTokens:   tokensIn + tokensOut,
			})
		}
		if !h.breakerAllow(target.Provider, now) {
			reasons[target.Provider] = "breaker_" + string(h.breakerState(target.Provider, now))
			record(http.StatusServiceUnavailable, upstream.Result{}, 0)
			continue
		}
		if until := h.cooldownUntil(target.Provider); now.Before(until) {
			reasons[target.Provider] = "cooldown"
			if left := until.Sub(now); left < retryAfter || retryAfter == 0 {
				retryAfter = left
			}
			record(http.StatusTooManyRequests, upstream.Result{}, 0)
			continue
		}
		if p.DailyTokenCap > 0 && h.dailyUsed(target.Provider) >= p.DailyTokenCap {
			reasons[target.Provider] = "daily_cap"
			record(http.StatusServiceUnavailable, upstream.Result{}, 0)
			continue
		}

		queueCtx, cancel := context.WithTimeout(r.Context(), h.cfg.QueueMaxWait)
		err := h.limiters[target.Provider].Wait(queueCtx)
		if err == nil {
			err = h.sems[target.Provider].Acquire(queueCtx)
		}
		cancel()
		if err != nil {
			reasons[target.Provider] = "queue_timeout"
			record(http.StatusServiceUnavailable, upstream.Result{}, 0)
			continue
		}
		for attempt := 0; attempt < h.cfg.Retry.Attempts; attempt++ {
			if attempt > 0 {
				acqCtx, acqCancel := context.WithTimeout(r.Context(), h.cfg.QueueMaxWait)
				err := h.sems[target.Provider].Acquire(acqCtx)
				acqCancel()
				if err != nil {
					reasons[target.Provider] = "queue_timeout"
					record(http.StatusServiceUnavailable, upstream.Result{}, 0)
					break
				}
			}
			attemptStart := time.Now()
			res, _ := h.upstream.Do(r.Context(), target, p, req, w)
			h.sems[target.Provider].Release()
			record(res.Status, res, time.Since(attemptStart))

			if res.MidStream {
				h.setLastError(target.Provider, res.Outcome)
				return
			}
			switch res.Outcome {
			case upstream.OK:
				h.breakerSuccess(target.Provider)
				h.addDaily(target.Provider, res.TokensIn+res.TokensOut)
				return
			case upstream.Retryable:
				h.breakerFail(target.Provider, now)
				h.setLastError(target.Provider, res.Outcome)
				if attempt+1 >= h.cfg.Retry.Attempts {
					reasons[target.Provider] = "retryable"
					break
				}
				if err := h.clock.Sleep(r.Context(), h.cfg.Retry.Backoff); err != nil {
					reasons[target.Provider] = "cancelled"
					return
				}
			case upstream.RateLimited:
				d := res.RetryAfter
				if d <= 0 {
					d = h.nextRLCooldown(target.Provider)
				}
				h.setCooldown(target.Provider, h.clock.Now().Add(d))
				h.setLastError(target.Provider, res.Outcome)
				reasons[target.Provider] = "rate_limited"
				if d < retryAfter || retryAfter == 0 {
					retryAfter = d
				}
			default:
				h.setLastError(target.Provider, res.Outcome)
				writeError(w, res.Status, string(res.Outcome), nil)
				return
			}
		}
	}

	if retryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds()+0.999)))
	}
	writeError(w, http.StatusServiceUnavailable, "provider_exhausted", reasons)
}

func (h *Handler) breakerState(name string, now time.Time) breaker.State {
	return h.breakers[name].State(now)
}

func (h *Handler) breakerAllow(name string, now time.Time) bool {
	return h.breakers[name].Allow(now)
}

func (h *Handler) breakerSuccess(name string) {
	h.breakers[name].Success()
}

func (h *Handler) breakerFail(name string, now time.Time) {
	h.breakers[name].Fail(now, h.cfg.Breaker.ConsecutiveFailures, h.cfg.Breaker.InitialCooldown, h.cfg.Breaker.MaxCooldown)
}

func (h *Handler) cooldownUntil(name string) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cooldown[name]
}

func (h *Handler) setCooldown(name string, until time.Time) {
	h.mu.Lock()
	h.cooldown[name] = until
	h.mu.Unlock()
}

func (h *Handler) nextRLCooldown(name string) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	steps := h.cfg.RLCooldown
	i := h.cooldownStep[name]
	if i >= len(steps) {
		i = len(steps) - 1
	}
	h.cooldownStep[name]++
	return steps[i]
}

func (h *Handler) setLastError(name string, outcome upstream.Outcome) {
	h.mu.Lock()
	h.lastError[name] = outcome
	h.mu.Unlock()
}

func (h *Handler) dailyUsed(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dailyTokens[name]
}

func (h *Handler) addDaily(name string, tokens int) {
	if tokens <= 0 {
		return
	}
	h.mu.Lock()
	h.dailyTokens[name] += tokens
	h.mu.Unlock()
}

func formatUntil(until, now time.Time) string {
	if until.IsZero() || !until.After(now) {
		return ""
	}
	return until.UTC().Format(time.RFC3339)
}

func hasImages(messages []struct {
	Content json.RawMessage `json:"content"`
}) bool {
	for _, m := range messages {
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &parts) == nil {
			for _, p := range parts {
				if p.Type == "image_url" {
					return true
				}
			}
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code string, reasons map[string]string) {
	body := map[string]any{"error": map[string]any{"message": code, "type": "router_error", "code": code}}
	if len(reasons) > 0 {
		body["providers"] = reasons
	}
	writeJSON(w, status, body)
}
