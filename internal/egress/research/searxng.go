package research

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jerryjuche/padi/internal/egress/config"
	"github.com/jerryjuche/padi/internal/egress/search"
)

// SearxNG is the M12 provider. It talks to an unmodified SearxNG container
// over HTTP and drops every field except title, URL and snippet.
type SearxNG struct {
	BaseURL    string
	Engines    []string
	MaxResults int
	Client     *http.Client
	Limits     config.URLLimits
	Timeout    time.Duration
}

type searxResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// Search returns safe, deduplicated results or search.ErrUnavailable.
func (s *SearxNG) Search(ctx context.Context, q string, n int) ([]search.Result, error) {
	if s == nil || s.BaseURL == "" {
		return nil, search.ErrUnavailable
	}
	if n <= 0 || n > s.MaxResults {
		n = s.MaxResults
	}
	base, err := url.Parse(s.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, search.ErrUnavailable
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/search"
	query := base.Query()
	query.Set("q", q)
	query.Set("format", "json")
	if len(s.Engines) > 0 {
		query.Set("engines", strings.Join(s.Engines, ","))
	}
	base.RawQuery = query.Encode()

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, search.ErrUnavailable
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", search.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, search.ErrUnavailable
	}
	var decoded searxResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&decoded); err != nil {
		return nil, search.ErrUnavailable
	}
	out := make([]search.Result, 0, n)
	seen := make(map[string]struct{}, n)
	for _, item := range decoded.Results {
		if len(out) >= n {
			break
		}
		v := config.CheckURL(item.URL, s.Limits)
		if !v.Allow {
			continue
		}
		u, err := url.Parse(item.URL)
		if err != nil {
			continue
		}
		key := strings.ToLower(u.Hostname()) + u.EscapedPath()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, search.Result{
			Title:   clip(item.Title, 200),
			URL:     item.URL,
			Snippet: clip(item.Content, 300),
		})
	}
	if len(out) == 0 {
		return nil, search.ErrUnavailable
	}
	return out, nil
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max && max > 0 {
		return s[:max]
	}
	return s
}
