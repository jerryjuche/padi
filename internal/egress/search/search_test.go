package search

import (
	"context"
	"errors"
	"testing"
)

func TestNoneProvider(t *testing.T) {
	tests := []struct {
		name  string
		query string
		n     int
	}{
		{"empty", "", 0},
		{"normal", "go proxy", 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results, err := None{}.Search(context.Background(), tt.query, tt.n)
			if results != nil {
				t.Fatalf("results = %+v, want nil", results)
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want ErrUnavailable", err)
			}
		})
	}
}
