package contract

import (
	"reflect"
	"testing"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{name: "star within segment", pattern: "*.go", path: "main.go", want: true},
		{name: "star does not cross slash", pattern: "*.go", path: "pkg/main.go", want: false},
		{name: "segment star", pattern: "pkg/*.go", path: "pkg/main.go", want: true},
		{name: "segment star nested", pattern: "pkg/*.go", path: "pkg/sub/main.go", want: false},
		{name: "double star nested", pattern: "pkg/**", path: "pkg/a/b.go", want: true},
		{name: "double star zero segments", pattern: "pkg/**", path: "pkg", want: true},
		{name: "double star middle zero", pattern: "pkg/**/main.go", path: "pkg/main.go", want: true},
		{name: "double star middle many", pattern: "pkg/**/main.go", path: "pkg/a/b/main.go", want: true},
		{name: "question mark", pattern: "a/?", path: "a/x", want: true},
		{name: "question mark too wide", pattern: "a/?", path: "a/xy", want: false},
		{name: "prefix glob", pattern: "Dockerfile*", path: "Dockerfile.dev", want: true},
		{name: "leading double star", pattern: "**/*.go", path: "a.go", want: true},
		{name: "literal brackets are not a pattern error", pattern: "[bad", path: "[bad", want: true},
		{name: "empty pattern", pattern: "", path: "a", want: false},
		{name: "empty path", pattern: "a", path: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.pattern, tc.path); got != tc.want {
				t.Fatalf("Match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

func TestDraft(t *testing.T) {
	defaults := Budget{
		WallMinutes:     45,
		MaxTokens:       400000,
		MaxToolCalls:    200,
		MaxFilesChanged: 40,
		MaxFixLoops:     3,
	}
	repo := RepoSettings{
		OffLimits:      []string{"**/.env", "**/.env"},
		RequiredChecks: []string{"go test"},
	}
	overrides := Overrides{
		Scope:           []string{"internal/**"},
		OffLimits:       []string{"secrets/**", "**/.env"},
		RequiredChecks:  []string{"go vet"},
		MaxTokens:       1000,
		MaxFilesChanged: -5,
	}

	got := Draft("  fix the bug  ", repo, defaults, overrides)
	want := Contract{
		Goal:           "fix the bug",
		Scope:          []string{"internal/**"},
		OffLimits:      []string{"**/.env", "secrets/**"},
		RequiredChecks: []string{"go test", "go vet"},
		Budget: Budget{
			WallMinutes:     45,
			MaxTokens:       1000,
			MaxToolCalls:    200,
			MaxFilesChanged: 40,
			MaxFixLoops:     3,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Draft mismatch\n got: %+v\nwant: %+v", got, want)
	}

	if plain := Draft("goal", RepoSettings{}, defaults, Overrides{}); plain.Budget != defaults {
		t.Fatalf("zero overrides must keep defaults, got %+v", plain.Budget)
	}
}

func TestCheckScope(t *testing.T) {
	c := Contract{
		Scope:     []string{"internal/**"},
		OffLimits: []string{"**/secret.go"},
	}
	tests := []struct {
		name  string
		files []ChangedFile
		want  []Violation
	}{
		{
			name:  "off limits hard",
			files: []ChangedFile{{Path: "internal/secret.go"}},
			want: []Violation{
				{Kind: KindOffLimits, Path: "internal/secret.go", Detail: "**/secret.go", Hard: true},
			},
		},
		{
			name:  "outside scope soft",
			files: []ChangedFile{{Path: "README.md"}},
			want: []Violation{
				{Kind: KindOutsideScope, Path: "README.md", Detail: "outside contract scope"},
			},
		},
		{
			name:  "off limits hard wins over soft scope",
			files: []ChangedFile{{Path: "other/secret.go"}},
			want: []Violation{
				{Kind: KindOffLimits, Path: "other/secret.go", Detail: "**/secret.go", Hard: true},
				{Kind: KindOutsideScope, Path: "other/secret.go", Detail: "outside contract scope"},
			},
		},
		{
			name:  "empty scope means whole repo",
			files: []ChangedFile{{Path: "README.md"}},
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			subject := c
			if tc.name == "empty scope means whole repo" {
				subject.Scope = nil
				subject.OffLimits = nil
			}
			got := subject.CheckScope(tc.files)
			assertViolations(t, got, tc.want)
		})
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name  string
		c     Contract
		files []ChangedFile
		p     Protection
		want  []Violation
	}{
		{
			name:  "protected path is an approval not a stop",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml"}},
			p:     Protection{ProtectedPaths: []string{".github/workflows/**"}},
			want: []Violation{
				{Kind: KindProtectedPath, Path: ".github/workflows/ci.yml", Detail: ".github/workflows/**"},
			},
		},
		{
			name:  "owner off limits is hard",
			c:     Contract{OffLimits: []string{"secrets/**"}},
			files: []ChangedFile{{Path: "secrets/key.txt"}},
			want: []Violation{
				{Kind: KindOffLimits, Path: "secrets/key.txt", Detail: "secrets/**", Hard: true},
			},
		},
		{
			name:  "scope soft flag",
			c:     Contract{Scope: []string{"internal/**"}},
			files: []ChangedFile{{Path: "docs/guide.md"}},
			want: []Violation{
				{Kind: KindOutsideScope, Path: "docs/guide.md", Detail: "outside contract scope"},
			},
		},
		{
			name:  "same path and kind is not duplicated",
			c:     Contract{OffLimits: []string{"secrets/**", "**/*.txt"}},
			files: []ChangedFile{{Path: "secrets/key.txt"}},
			want: []Violation{
				{Kind: KindOffLimits, Path: "secrets/key.txt", Detail: "secrets/**", Hard: true},
			},
		},
		{
			name:  "custom lockfile pair missing manifest",
			files: []ChangedFile{{Path: "custom.lock"}},
			p:     Protection{LockfilePairs: map[string]string{"custom.lock": "custom.toml"}},
			want: []Violation{
				{Kind: KindLockfileWithoutManifest, Path: "custom.lock", Detail: "custom.toml"},
			},
		},
		{
			name:  "custom lockfile pair manifest present",
			files: []ChangedFile{{Path: "custom.lock"}, {Path: "custom.toml"}},
			p:     Protection{LockfilePairs: map[string]string{"custom.lock": "custom.toml"}},
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertViolations(t, tc.c.Check(tc.files, tc.p), tc.want)
		})
	}
}

func TestCheckBuiltinLockfilePairs(t *testing.T) {
	tests := []struct {
		lock     string
		manifest string
	}{
		{lock: "go.sum", manifest: "go.mod"},
		{lock: "package-lock.json", manifest: "package.json"},
		{lock: "pnpm-lock.yaml", manifest: "package.json"},
		{lock: "yarn.lock", manifest: "package.json"},
		{lock: "Cargo.lock", manifest: "Cargo.toml"},
		{lock: "poetry.lock", manifest: "pyproject.toml"},
	}

	for _, tc := range tests {
		t.Run(tc.lock+"_missing_manifest", func(t *testing.T) {
			got := (Contract{}).Check([]ChangedFile{{Path: tc.lock}}, Protection{})
			want := []Violation{{
				Kind:   KindLockfileWithoutManifest,
				Path:   tc.lock,
				Detail: tc.manifest,
			}}
			assertViolations(t, got, want)
		})
		t.Run(tc.lock+"_manifest_changed", func(t *testing.T) {
			got := (Contract{}).Check([]ChangedFile{
				{Path: "svc/" + tc.lock},
				{Path: "svc/" + tc.manifest},
			}, Protection{})
			if len(got) != 0 {
				t.Fatalf("changed manifest must clear the finding, got %+v", got)
			}
		})
	}
}

func TestLockfilePair(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		pairs  map[string]string
		want   string
		wantOK bool
	}{
		{name: "builtin root", path: "go.sum", want: "go.mod", wantOK: true},
		{name: "builtin nested", path: "pkg/yarn.lock", want: "package.json", wantOK: true},
		{name: "custom overrides builtin", path: "go.sum", pairs: map[string]string{"go.sum": "go.work"}, want: "go.work", wantOK: true},
		{name: "custom full path", path: "pkg/custom.lock", pairs: map[string]string{"pkg/custom.lock": "custom.toml"}, want: "custom.toml", wantOK: true},
		{name: "custom pattern", path: "a/b/special.lock", pairs: map[string]string{"**/special.lock": "special.toml"}, want: "special.toml", wantOK: true},
		{name: "empty custom disables pair", path: "go.sum", pairs: map[string]string{"go.sum": ""}, wantOK: false},
		{name: "unknown lockfile", path: "random.txt", wantOK: false},
		{name: "empty path", path: "", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manifest, ok := LockfilePair(tc.path, tc.pairs)
			if ok != tc.wantOK || manifest != tc.want {
				t.Fatalf("LockfilePair(%q) = (%q, %v), want (%q, %v)", tc.path, manifest, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func assertViolations(t *testing.T, got, want []Violation) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("violations = %+v, want %+v", got, want)
	}
	seen := make(map[string]struct{}, len(got))
	for _, v := range got {
		key := v.Path + "\x00" + v.Kind
		if _, dup := seen[key]; dup {
			t.Fatalf("duplicate path+kind violation: %+v", v)
		}
		seen[key] = struct{}{}
		if v.Kind != KindOffLimits && v.Kind != KindOutsideScope && v.Kind != KindProtectedPath && v.Kind != KindLockfileWithoutManifest {
			t.Fatalf("unexpected kind %q", v.Kind)
		}
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g.Kind == w.Kind && g.Path == w.Path && g.Detail == w.Detail && g.Hard == w.Hard {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing violation %+v in %+v", w, got)
		}
	}
}
