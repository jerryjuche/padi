package research

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jerryjuche/padi/internal/egress/search"
)

func TestSearxNGFiltersAndCapsResults(t *testing.T) {
	payload := map[string]any{
		"results": []map[string]string{
			{"title": "Docs", "url": "https://example.com/docs/a", "content": "snippet a", "ignored": "drop me"},
			{"title": "Dup", "url": "https://example.com/docs/a?x=1", "content": "duplicate"},
			{"title": "Private", "url": "http://127.0.0.1/admin", "content": "private"},
			{"title": "BadPort", "url": "https://example.com:22/x", "content": "bad port"},
			{"title": "Second", "url": "https://other.example/b", "content": "snippet b"},
		},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("format") != "json" || r.URL.Query().Get("q") == "" {
			t.Errorf("unexpected searx request: %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer upstream.Close()
	s := &SearxNG{
		BaseURL: upstream.URL, Engines: []string{"github"}, MaxResults: 2,
		Client: upstream.Client(), Limits: testConfig().URLLimits(),
	}
	results, err := s.Search(context.Background(), "go egress", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2", results)
	}
	if results[0].Title != "Docs" || results[1].URL != "https://other.example/b" {
		t.Fatalf("unexpected results: %+v", results)
	}
	for _, r := range results {
		if r.Snippet == "" || r.URL == "" {
			t.Fatalf("incomplete result: %+v", r)
		}
	}
}

func TestSearxNGUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	s := &SearxNG{BaseURL: upstream.URL, MaxResults: 5, Client: upstream.Client(), Limits: testConfig().URLLimits()}
	_, err := s.Search(context.Background(), "nothing", 5)
	if !errors.Is(err, search.ErrUnavailable) {
		t.Fatalf("empty result error = %v, want ErrUnavailable", err)
	}
	upstream.Close()
	_, err = s.Search(context.Background(), "nothing", 5)
	if !errors.Is(err, search.ErrUnavailable) {
		t.Fatalf("closed upstream error = %v, want ErrUnavailable", err)
	}
}
