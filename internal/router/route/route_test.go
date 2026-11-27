package route

import (
	"testing"

	"github.com/jerryjuche/padi/internal/router/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Providers: map[string]config.Provider{
			"tools":  {Name: "tools", RPM: 10, Concurrency: 1, Enabled: true, SupportsTools: true, SupportsStream: true, MaxContext: 1000},
			"vision": {Name: "vision", RPM: 10, Concurrency: 1, Enabled: true, SupportsImages: true, MaxContext: 100},
			"plain":  {Name: "plain", RPM: 10, Concurrency: 1, Enabled: true, MaxContext: 50},
			"embed":  {Name: "embed", RPM: 10, Concurrency: 1, Enabled: true, MaxContext: 100},
		},
		Routes: map[string][]config.Target{
			"chat": {
				{Provider: "plain", Model: "plain/model", Enabled: true, Priority: 1},
				{Provider: "tools", Model: "tools/model", Enabled: true, Priority: 2},
				{Provider: "vision", Model: "vision/model", Enabled: true, Priority: 3},
				{Provider: "embed", Model: "embed/model", Enabled: true, Priority: 4, Embeddings: true, Dims: 384},
			},
		},
	}
}

func candidates(t *testing.T, req Request) []string {
	t.Helper()
	got, err := New(testConfig()).Candidates(req)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, target := range got {
		out = append(out, target.Provider)
	}
	return out
}

func TestCandidatesFiltersCapabilitiesAndContext(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want []string
	}{
		{"plain", Request{Alias: "chat", EstTokens: 10}, []string{"plain", "tools", "vision"}},
		{"tools", Request{Alias: "chat", HasTools: true, EstTokens: 10}, []string{"tools"}},
		{"vision", Request{Alias: "chat", HasImages: true, EstTokens: 10}, []string{"vision"}},
		{"context", Request{Alias: "chat", EstTokens: 200}, []string{"tools"}},
		{"excluded provider", Request{Alias: "chat", EstTokens: 10, ExcludeProviders: []string{"plain"}}, []string{"tools", "vision"}},
		{"embeddings", Request{Alias: "chat", Embeddings: true, EstTokens: 10}, []string{"embed"}},
		{"provider-prefixed alias", Request{Alias: "padi/chat", EstTokens: 10}, []string{"plain", "tools", "vision"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := candidates(t, tc.req)
			if len(got) != len(tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got=%v want=%v", got, tc.want)
				}
			}
		})
	}
}

func TestCandidatesNoTarget(t *testing.T) {
	_, err := New(testConfig()).Candidates(Request{Alias: "chat", HasTools: true, HasImages: true, EstTokens: 10})
	if err == nil {
		t.Fatal("want no-candidate error")
	}
	if _, err := New(testConfig()).Candidates(Request{Alias: "missing"}); err == nil {
		t.Fatal("want unknown-alias error")
	}
}
