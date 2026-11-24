package research

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jerryjuche/padi/internal/egress/config"
	"github.com/jerryjuche/padi/internal/egress/proxy"
)

// Page is the read-only result returned to the agent.
type Page struct {
	URL       string    `json:"url"`
	FinalURL  string    `json:"final_url"`
	Status    int       `json:"status"`
	Title     string    `json:"title"`
	Text      string    `json:"text"`
	Truncated bool      `json:"truncated"`
	FetchedAt time.Time `json:"fetched_at"`
}

// RefusedError is a stable denial from the URL or SSRF guard.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "research_refused: " + e.Reason }

// Fetcher fetches static pages under strict size, type and address limits.
type Fetcher struct {
	cfg     *config.Config
	dialer  *proxy.GuardedDialer
	client  *http.Client
	now     func() time.Time
	limits  config.URLLimits
	maxBody int64
}

// FetcherOption customises dependencies for tests.
type FetcherOption func(*Fetcher)

// WithFetchClock injects a clock for deterministic FetchedAt values.
func WithFetchClock(now func() time.Time) FetcherOption {
	return func(f *Fetcher) { f.now = now }
}

// WithFetchDialer injects the SSRF-safe dialer.
func WithFetchDialer(d *proxy.GuardedDialer) FetcherOption {
	return func(f *Fetcher) { f.dialer = d }
}

// NewFetcher builds a fetcher. The dialer resolves DNS in-proxy and checks
// every address before connecting.
func NewFetcher(cfg *config.Config, d *proxy.GuardedDialer, opts ...FetcherOption) *Fetcher {
	f := &Fetcher{cfg: cfg, now: time.Now, limits: cfg.URLLimits(), maxBody: int64(cfg.Research.MaxBodyMB) * 1024 * 1024}
	if d == nil {
		d = &proxy.GuardedDialer{Ports: cfg.Ports, DenyNets: cfg.DenyNets}
	}
	f.dialer = d
	for _, opt := range opts {
		opt(f)
	}
	transport := &http.Transport{
		Proxy:             nil,
		DialContext:       f.dialer.DialContext,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: false,
	}
	f.client = &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 30 * time.Second,
	}
	return f
}

// Fetch retrieves rawURL, following at most max_redirects hops. Every hop is
// re-validated at the URL and IP level. No cookies, custom headers or body are
// ever sent to an upstream page.
func (f *Fetcher) Fetch(ctx context.Context, sessionID, rawURL string) (Page, error) {
	_ = sessionID // retained in the contract for request-scoped audit callers
	current := stripFragment(rawURL)
	maxHops := f.cfg.Research.MaxRedirects
	if maxHops > 5 {
		maxHops = 5
	}
	for hop := 0; hop <= maxHops; hop++ {
		v := config.CheckURL(current, f.limits)
		if !v.Allow {
			return Page{}, &RefusedError{Reason: v.Reason}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return Page{}, &RefusedError{Reason: "bad_url"}
		}
		req.Header = make(http.Header)
		resp, err := f.client.Do(req)
		if err != nil {
			var de *config.DenyError
			if errors.As(err, &de) {
				return Page{}, &RefusedError{Reason: de.Reason}
			}
			return Page{}, fmt.Errorf("research fetch: %w", err)
		}
		if isRedirect(resp.StatusCode) && resp.Header.Get("Location") != "" {
			location := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if hop == maxHops {
				return Page{}, errors.New("research_refused: too_many_redirects")
			}
			base, err := url.Parse(current)
			if err != nil {
				return Page{}, &RefusedError{Reason: "bad_url"}
			}
			next, err := base.Parse(location)
			if err != nil {
				return Page{}, &RefusedError{Reason: "bad_redirect"}
			}
			current = stripFragment(next.String())
			continue
		}
		defer resp.Body.Close()
		if !allowedContentType(resp.Header.Get("Content-Type")) {
			return Page{}, &RefusedError{Reason: "content_type_not_allowed"}
		}
		buf, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBody+1))
		if err != nil {
			return Page{}, fmt.Errorf("research read: %w", err)
		}
		bodyTruncated := int64(len(buf)) > f.maxBody
		if bodyTruncated {
			buf = buf[:f.maxBody]
		}
		title, text, textTruncated, err := extractText(bytes.NewReader(buf), f.cfg.Research.MaxChars)
		if err != nil {
			return Page{}, fmt.Errorf("research extract: %w", err)
		}
		final := resp.Request.URL
		if final != nil {
			final.Fragment = ""
			final.RawFragment = ""
		}
		finalURL := current
		if final != nil {
			finalURL = final.String()
		}
		return Page{
			URL:       stripFragment(rawURL),
			FinalURL:  finalURL,
			Status:    resp.StatusCode,
			Title:     title,
			Text:      text,
			Truncated: bodyTruncated || textTruncated,
			FetchedAt: f.now().UTC(),
		}, nil
	}
	return Page{}, errors.New("research_refused: too_many_redirects")
}

func stripFragment(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

func isRedirect(status int) bool {
	return status >= 300 && status < 400
}

func allowedContentType(header string) bool {
	media, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	media = strings.ToLower(media)
	switch media {
	case "text/html", "application/xhtml+xml", "text/plain",
		"application/json", "text/json", "text/markdown",
		"application/markdown", "text/x-markdown", "text/xml", "application/xml":
		return true
	}
	return strings.HasSuffix(media, "+json") || strings.HasSuffix(media, "+xml")
}
