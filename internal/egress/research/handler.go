package research

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jerryjuche/padi/internal/egress/config"
	"github.com/jerryjuche/padi/internal/egress/proxy"
	"github.com/jerryjuche/padi/internal/egress/search"
)

// SearchProvider is the interface consumed by the research API.
type SearchProvider = search.Provider

// Result is the only search data returned to an agent.
type Result = search.Result

// Limits caps research calls per session per minute and per task overall.
type Limits struct {
	mu       sync.Mutex
	rate     int
	perTask  int
	now      func() time.Time
	sessions map[string][]time.Time
	tasks    map[string]int
}

// NewLimits creates per-session and per-task research limits.
func NewLimits(rate, perTask int, now func() time.Time) *Limits {
	if now == nil {
		now = time.Now
	}
	return &Limits{
		rate:     rate,
		perTask:  perTask,
		now:      now,
		sessions: make(map[string][]time.Time),
		tasks:    make(map[string]int),
	}
}

// Allow consumes one slot. It returns a stable reason code when refused.
func (l *Limits) Allow(session, task string) (bool, string) {
	if l == nil {
		return true, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	recent := l.sessions[session][:0]
	for _, t := range l.sessions[session] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.rate {
		l.sessions[session] = recent
		return false, "session_rate_limited"
	}
	if task != "" && l.tasks[task] >= l.perTask {
		return false, "task_cap_reached"
	}
	l.sessions[session] = append(recent, now)
	if task != "" {
		l.tasks[task]++
	}
	return true, ""
}

type fetchRequest struct {
	URL       string `json:"url"`
	SessionID string `json:"session_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
}

type searchRequest struct {
	Query     string `json:"query"`
	N         int    `json:"n"`
	SessionID string `json:"session_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
}

// Handler serves POST /v1/research/fetch and /v1/research/search.
type Handler struct {
	fetcher   *Fetcher
	provider  SearchProvider
	audit     *config.Audit
	counters  *config.Counters
	denials   *proxy.Denials
	token     string
	limits    *Limits
	urlLimits config.URLLimits
	truncate  func(string) string
}

// NewHandler builds the research API handler.
func NewHandler(fetcher *Fetcher, provider SearchProvider, token string, audit *config.Audit, counters *config.Counters, denials *proxy.Denials, limits *Limits, urlLimits config.URLLimits) *Handler {
	if provider == nil {
		provider = search.None{}
	}
	return &Handler{
		fetcher:   fetcher,
		provider:  provider,
		audit:     audit,
		counters:  counters,
		denials:   denials,
		token:     token,
		limits:    limits,
		urlLimits: urlLimits,
		truncate: func(s string) string {
			if len(s) > 253 {
				return s[:253]
			}
			return s
		},
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	kind := "research"
	switch r.URL.Path {
	case "/v1/research/fetch":
		kind = "research_fetch"
	case "/v1/research/search":
		kind = "research_search"
	}
	host := ""
	session, task := headerField(r, "X-Session-Id"), headerField(r, "X-Task-Id")
	verdict, reason := "deny", "unauthorized"
	var bytes int64
	defer func() {
		if reason == "session_rate_limited" || reason == "task_cap_reached" {
			h.counters.ResearchDeny.Add(1)
		} else if verdict == "deny" && reason != "unauthorized" && reason != "bad_request" {
			h.counters.ResearchDeny.Add(1)
		}
		h.audit.Line(kind, host, session, task, verdict, reason, bytes)
	}()

	if kind == "research" {
		reason = "not_found"
		writeProblem(w, http.StatusNotFound, "not_found", "Unknown research endpoint.")
		return
	}
	if r.Method != http.MethodPost {
		reason = "method_not_allowed"
		writeProblem(w, http.StatusMethodNotAllowed, reason, "POST is required.")
		return
	}
	if h.token == "" || !constantTimeEqual(bearerToken(r.Header.Get("Authorization")), h.token) {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "Bearer token required.")
		return
	}

	switch kind {
	case "research_fetch":
		bytes, host, verdict, reason = h.fetch(w, r, &session, &task)
	case "research_search":
		bytes, _, verdict, reason = h.search(w, r, &session, &task)
	}
}

func (h *Handler) fetch(w http.ResponseWriter, r *http.Request, session, task *string) (int64, string, string, string) {
	var req fetchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return 0, "", "deny", "bad_request"
	}
	if req.SessionID != "" {
		*session = req.SessionID
	}
	if req.TaskID != "" {
		*task = req.TaskID
	}
	if ok, why := h.limits.Allow(*session, *task); !ok {
		writeProblem(w, http.StatusTooManyRequests, why, "Research rate limit reached.")
		return 0, "", "deny", why
	}
	host := hostOf(req.URL)
	if v := config.CheckURL(req.URL, h.urlLimits); !v.Allow {
		h.denials.Record("research", host, v.Reason, *session, *task)
		if host != "" {
			w.Header().Set("X-Egress-Deny", h.truncate(host))
		}
		writeProblem(w, http.StatusForbidden, "research_refused", "Web lookup blocked by safety limits.")
		return 0, host, "deny", v.Reason
	}
	page, err := h.fetcher.Fetch(r.Context(), *session, req.URL)
	if err != nil {
		var refused *RefusedError
		if errors.As(err, &refused) {
			h.denials.Record("research", host, refused.Reason, *session, *task)
			if host != "" {
				w.Header().Set("X-Egress-Deny", h.truncate(host))
			}
			writeProblem(w, http.StatusForbidden, "research_refused", "Web lookup blocked by safety limits.")
			return 0, host, "deny", refused.Reason
		}
		writeProblem(w, http.StatusBadGateway, "research_failed", "Web lookup failed.")
		return 0, host, "deny", "fetch_failed"
	}
	body, err := json.Marshal(page)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "encode_failed", "Could not encode research result.")
		return 0, host, "deny", "encode_failed"
	}
	h.counters.ResearchFetch.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
	return int64(len(body)), host, "allow", ""
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request, session, task *string) (int64, string, string, string) {
	var req searchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return 0, "", "deny", "bad_request"
	}
	if req.SessionID != "" {
		*session = req.SessionID
	}
	if req.TaskID != "" {
		*task = req.TaskID
	}
	if ok, why := h.limits.Allow(*session, *task); !ok {
		writeProblem(w, http.StatusTooManyRequests, why, "Research rate limit reached.")
		return 0, "", "deny", why
	}
	if v := config.CheckQuery(req.Query, h.urlLimits); !v.Allow {
		h.denials.Record("research", "", v.Reason, *session, *task)
		writeProblem(w, http.StatusForbidden, "research_refused", "Web lookup blocked by safety limits.")
		return 0, "", "deny", v.Reason
	}
	results, err := h.provider.Search(r.Context(), req.Query, req.N)
	if err != nil {
		if errors.Is(err, search.ErrUnavailable) {
			writeProblem(w, http.StatusNotImplemented, "search_unavailable", "Web search is unavailable right now. I'll use known documentation links instead.")
			return 0, "", "deny", "search_unavailable"
		}
		writeProblem(w, http.StatusBadGateway, "search_failed", "Web search failed.")
		return 0, "", "deny", "search_failed"
	}
	body, err := json.Marshal(results)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "encode_failed", "Could not encode search results.")
		return 0, "", "deny", "encode_failed"
	}
	h.counters.ResearchSearch.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
	return int64(len(body)), "", "allow", ""
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad_request", "Invalid JSON request.")
		return err
	}
	return nil
}

func writeProblem(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"code":"` + sanitize(code) + `","message":"` + sanitize(message) + `"}`))
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 {
			return -1
		}
		return r
	}, s)
}

func bearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func constantTimeEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func headerField(r *http.Request, name string) string {
	v := strings.TrimSpace(r.Header.Get(name))
	if len(v) > 128 {
		return v[:128]
	}
	return v
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
