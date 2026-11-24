package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jerryjuche/padi/internal/router/config"
	"github.com/jerryjuche/padi/internal/router/limiter"
)

func testCfg(baseURL string) *config.Config {
	return &config.Config{
		Listen:      ":8090",
		RouterToken: "router-token",
		Providers: map[string]config.Provider{
			"fake": {
				Name: "fake", BaseURL: baseURL + "/v1", APIKey: "provider-secret-value", RPM: 100, Concurrency: 1,
				SupportsStream: true, SupportsTools: true, MaxContext: 10000, Enabled: true,
			},
		},
		Routes: map[string][]config.Target{
			"agent-chat": {{Provider: "fake", Model: "upstream/model", Enabled: true, Priority: 1}},
		},
		Retry:            config.RetryCfg{Attempts: 2, Backoff: 10 * time.Millisecond},
		Breaker:          config.BreakerCfg{ConsecutiveFailures: 3, InitialCooldown: 10 * time.Second, MaxCooldown: time.Minute},
		RLCooldown:       []time.Duration{time.Second, 2 * time.Second, 5 * time.Second},
		QueueMaxWait:     time.Second,
		FirstByteTimeout: time.Second,
		TotalTimeout:     5 * time.Second,
	}
}

func TestHandlerForced429FailsOverToNextTarget(t *testing.T) {
	var primaryHits, secondaryHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"from-secondary"}}]}`))
	}))
	defer secondary.Close()

	cfg := testCfg(primary.URL)
	cfg.Retry.Attempts = 1
	cfg.Providers["secondary"] = config.Provider{
		Name: "secondary", BaseURL: secondary.URL + "/v1", APIKey: "secondary-key", RPM: 100, Concurrency: 1,
		SupportsStream: true, SupportsTools: true, MaxContext: 10000, Enabled: true,
	}
	cfg.Routes["agent-chat"] = []config.Target{
		{Provider: "fake", Model: "primary/model", Enabled: true, Priority: 1},
		{Provider: "secondary", Model: "secondary/model", Enabled: true, Priority: 2},
	}
	h := NewHandler(cfg, limiter.NewFakeClock(time.Unix(100, 0)))

	w := serve(h, chatRequest(`{"model":"agent-chat","stream":false,"messages":[]}`))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "from-secondary") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if primaryHits.Load() != 1 || secondaryHits.Load() != 1 {
		t.Fatalf("primary=%d secondary=%d", primaryHits.Load(), secondaryHits.Load())
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("successful failover must not set Retry-After, got %q", got)
	}
}

func TestHandlerQueuesParallelRequestsWithConcurrencyOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	cfg := testCfg(srv.URL)
	provider := cfg.Providers["fake"]
	provider.Concurrency = 1
	cfg.Providers["fake"] = provider
	cfg.Retry.Attempts = 1
	h := NewHandler(cfg, limiter.NewFakeClock(time.Unix(100, 0)))

	const requests = 6
	codes := make(chan int, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- serve(h, chatRequest(`{"model":"agent-chat","stream":false,"messages":[]}`)).Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("parallel request returned %d, want 200", code)
		}
	}
}

func chatRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer router-token")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Task-Id", "task-9")
	r.Header.Set("X-Session-Id", "session-9")
	return r
}

func serve(h *Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHandlerAllExhausted503WithRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer srv.Close()
	clock := limiter.NewFakeClock(time.Unix(100, 0))
	h := NewHandler(testCfg(srv.URL), clock)

	w := serve(h, chatRequest(`{"model":"agent-chat","stream":false,"messages":[]}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("retry-after=%q", got)
	}
	if !strings.Contains(w.Body.String(), "provider_exhausted") || !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestHandler429EscalationWithoutRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	clock := limiter.NewFakeClock(time.Unix(100, 0))
	h := NewHandler(testCfg(srv.URL), clock)

	first := serve(h, chatRequest(`{"model":"agent-chat","messages":[]}`))
	if first.Header().Get("Retry-After") != "1" {
		t.Fatalf("first retry-after=%q", first.Header().Get("Retry-After"))
	}
	clock.Advance(2 * time.Second)
	// The first request consumed cooldown steps 1s and 2s (one per attempt),
	// so the next 429 escalates to the 5s step.
	second := serve(h, chatRequest(`{"model":"agent-chat","messages":[]}`))
	if second.Header().Get("Retry-After") != "5" {
		t.Fatalf("second retry-after=%q", second.Header().Get("Retry-After"))
	}
}

func TestHandler500RetrySucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"recovered"}}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`))
	}))
	defer srv.Close()
	clock := limiter.NewFakeClock(time.Unix(100, 0))
	h := NewHandler(testCfg(srv.URL), clock)

	w := serve(h, chatRequest(`{"model":"agent-chat","stream":false,"messages":[]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "recovered") {
		t.Fatalf("body=%s", w.Body.String())
	}
	if hits.Load() != 2 {
		t.Fatalf("hits=%d want 2", hits.Load())
	}
}

func TestHandlerStatusSecretFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	h := NewHandler(testCfg(srv.URL), limiter.NewFakeClock(time.Unix(100, 0)))

	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.Header.Set("Authorization", "Bearer router-token")
	w := serve(h, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "provider-secret-value") || strings.Contains(body, srv.URL) {
		t.Fatalf("status leaked secret or base URL: %s", body)
	}
	var decoded map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	providers, ok := decoded["providers"].(map[string]any)
	if !ok {
		t.Fatalf("providers=%v", decoded["providers"])
	}
	fake, ok := providers["fake"].(map[string]any)
	if !ok || fake["rpm"] != float64(100) || fake["breaker_state"] != "closed" {
		t.Fatalf("fake=%v", fake)
	}

	unauth := serve(h, httptest.NewRequest(http.MethodGet, "/status", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d", unauth.Code)
	}
}

func TestHandlerHealthAndModels(t *testing.T) {
	h := NewHandler(testCfg("https://upstream.invalid"), limiter.NewFakeClock(time.Unix(100, 0)))
	w := serve(h, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("health status=%d body=%s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer router-token")
	w = serve(h, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "agent-chat") {
		t.Fatalf("models status=%d body=%s", w.Code, w.Body.String())
	}
}
