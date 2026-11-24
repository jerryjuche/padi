// Package search defines the research search-provider contract and the safe
// "none" provider used before M12.
package search

import (
	"context"
	"errors"
)

// Result is the only search data returned to an agent: no raw provider JSON.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Provider is implemented by M12's SearxNG adapter.
type Provider interface {
	Search(ctx context.Context, query string, n int) ([]Result, error)
}

// ErrUnavailable means no usable provider answered. The research API maps this
// to 501 search_unavailable.
var ErrUnavailable = errors.New("search_unavailable: no search provider available")

// None is the default provider. It never makes a network call.
type None struct{}

// Search always reports that search is unavailable.
func (None) Search(context.Context, string, int) ([]Result, error) {
	return nil, ErrUnavailable
}
