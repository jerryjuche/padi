// Package contract implements the M3b task contract. A contract records the
// agreed goal, scope, off-limits paths, required checks and budgets; Check and
// CheckScope compare a changed-file list against it without trusting the agent.
package contract

import (
	"path"
	"sort"
	"strings"
)

// Violation kinds. Kind is always one of these four values.
const (
	KindOffLimits               = "off_limits"
	KindOutsideScope            = "outside_scope"
	KindProtectedPath           = "protected_path"
	KindLockfileWithoutManifest = "lockfile_without_manifest"
)

// ChangedFile is one entry in the final diff. Path is repo-relative; Added and
// Deleted are line counts; Status is whatever the diff producer reports.
type ChangedFile struct {
	Path    string
	Added   int
	Deleted int
	Status  string
}

// Violation is one contract finding. Hard reports an owner-set stop condition;
// soft reports something the approval card must show but does not block by
// itself.
type Violation struct {
	Kind   string
	Path   string
	Detail string
	Hard   bool
}

// Budget holds the task limits. A zero (or negative) value means unlimited for
// that field; wall_minutes is enforced by the watchdog, not by Meter.
type Budget struct {
	WallMinutes     int
	MaxTokens       int
	MaxToolCalls    int
	MaxFilesChanged int
	MaxFixLoops     int
}

// Contract is the approved task definition.
type Contract struct {
	Goal           string
	Scope          []string
	OffLimits      []string
	RequiredChecks []string
	Budget         Budget
	Research       bool
}

// RepoSettings is the repo's contract_defaults section, as supplied by
// onboarding. It never overrides an explicit owner instruction.
type RepoSettings struct {
	OffLimits      []string
	RequiredChecks []string
}

// Overrides is what the owner added in the task message. Zero budget fields
// mean "no override"; negative values are ignored as invalid.
type Overrides struct {
	Scope           []string
	OffLimits       []string
	RequiredChecks  []string
	WallMinutes     int
	MaxTokens       int
	MaxToolCalls    int
	MaxFilesChanged int
	MaxFixLoops     int
}

// Protection holds gateway-side policy paths and lockfile/manifest pairs.
// SensitiveTags and TestPatterns are reserved for the verifier's test-tampering
// guard and are deliberately not used by the contract diff check.
type Protection struct {
	ProtectedPaths []string
	SensitiveTags  []string
	TestPatterns   []string
	LockfilePairs  map[string]string
}

// Draft builds a contract from the gateway defaults, repo defaults and owner
// overrides. Patterns are trimmed and de-duplicated; ordering is stable.
func Draft(goal string, repo RepoSettings, defaults Budget, o Overrides) Contract {
	return Contract{
		Goal:           strings.TrimSpace(goal),
		Scope:          cleanPatterns(o.Scope),
		OffLimits:      mergePatterns(repo.OffLimits, o.OffLimits),
		RequiredChecks: mergePatterns(repo.RequiredChecks, o.RequiredChecks),
		Budget:         draftBudget(defaults, o),
	}
}

func draftBudget(defaults Budget, o Overrides) Budget {
	return Budget{
		WallMinutes:     pickBudget(defaults.WallMinutes, o.WallMinutes),
		MaxTokens:       pickBudget(defaults.MaxTokens, o.MaxTokens),
		MaxToolCalls:    pickBudget(defaults.MaxToolCalls, o.MaxToolCalls),
		MaxFilesChanged: pickBudget(defaults.MaxFilesChanged, o.MaxFilesChanged),
		MaxFixLoops:     pickBudget(defaults.MaxFixLoops, o.MaxFixLoops),
	}
}

func pickBudget(def, override int) int {
	if override > 0 {
		return override
	}
	if def < 0 {
		return 0
	}
	return def
}

func mergePatterns(groups ...[]string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, group := range groups {
		for _, raw := range group {
			p := cleanPath(raw)
			if p == "" {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

func cleanPatterns(group []string) []string {
	return mergePatterns(group)
}

// CheckScope reports owner off-limits hits as hard and files outside a
// non-empty scope as soft. It does not look at gateway protection policy.
func (c Contract) CheckScope(files []ChangedFile) []Violation {
	var out []Violation
	seen := make(map[string]struct{})
	for _, f := range files {
		p := cleanPath(f.Path)
		if p == "" {
			continue
		}
		if pattern, ok := matchAny(c.OffLimits, p); ok {
			addViolation(&out, seen, Violation{
				Kind:   KindOffLimits,
				Path:   f.Path,
				Detail: pattern,
				Hard:   true,
			})
		}
		if len(c.Scope) > 0 && !matchesAny(c.Scope, p) {
			addViolation(&out, seen, Violation{
				Kind:   KindOutsideScope,
				Path:   f.Path,
				Detail: "outside contract scope",
			})
		}
	}
	return out
}

// Check runs CheckScope and adds the gateway-side findings: protected paths
// are approval triggers, and a changed lockfile without its paired manifest is
// reported as a soft finding. The same path and kind is never reported twice.
func (c Contract) Check(files []ChangedFile, p Protection) []Violation {
	out := c.CheckScope(files)
	seen := make(map[string]struct{}, len(out))
	for _, v := range out {
		seen[v.Path+"\x00"+v.Kind] = struct{}{}
	}

	changed := make(map[string]struct{}, len(files))
	for _, f := range files {
		if cp := cleanPath(f.Path); cp != "" {
			changed[cp] = struct{}{}
		}
	}
	for _, f := range files {
		cp := cleanPath(f.Path)
		if cp == "" {
			continue
		}
		if pattern, ok := matchAny(p.ProtectedPaths, cp); ok {
			addViolation(&out, seen, Violation{
				Kind:   KindProtectedPath,
				Path:   f.Path,
				Detail: pattern,
			})
		}
		if manifest, ok := LockfilePair(f.Path, p.LockfilePairs); ok && !manifestChanged(changed, cp, manifest) {
			addViolation(&out, seen, Violation{
				Kind:   KindLockfileWithoutManifest,
				Path:   f.Path,
				Detail: manifest,
			})
		}
	}
	return out
}

func addViolation(out *[]Violation, seen map[string]struct{}, v Violation) {
	key := v.Path + "\x00" + v.Kind
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	*out = append(*out, v)
}

func matchAny(patterns []string, p string) (string, bool) {
	for _, raw := range patterns {
		pattern := cleanPath(raw)
		if pattern != "" && Match(pattern, p) {
			return pattern, true
		}
	}
	return "", false
}

func matchesAny(patterns []string, p string) bool {
	_, ok := matchAny(patterns, p)
	return ok
}

func manifestChanged(changed map[string]struct{}, lockPath, manifest string) bool {
	m := cleanPath(manifest)
	if m == "" {
		return false
	}
	if _, ok := changed[m]; ok {
		return true
	}
	if !path.IsAbs(m) && path.Dir(m) == "." && path.Dir(lockPath) != "." {
		m = path.Join(path.Dir(lockPath), m)
		if _, ok := changed[m]; ok {
			return true
		}
	}
	for cp := range changed {
		if Match(m, cp) {
			return true
		}
	}
	return false
}

// builtinLockfiles are the default manifest pairs. Custom Protection pairs
// override these by file name, full path or pattern.
var builtinLockfiles = map[string]string{
	"go.sum":            "go.mod",
	"package-lock.json": "package.json",
	"pnpm-lock.yaml":    "package.json",
	"yarn.lock":         "package.json",
	"Cargo.lock":        "Cargo.toml",
	"poetry.lock":       "pyproject.toml",
}

// LockfilePair returns the manifest paired with path, if any. Custom pairs
// take precedence over the built-in map. An empty custom manifest value
// disables the pair.
func LockfilePair(p string, pairs map[string]string) (manifest string, ok bool) {
	cp := cleanPath(p)
	if cp == "" {
		return "", false
	}
	base := path.Base(cp)
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	lookup := func(k string) (string, bool) {
		m := cleanPath(pairs[k])
		if m == "" {
			return "", false
		}
		return m, true
	}
	for _, k := range keys {
		if cleanPath(k) == cp {
			return lookup(k)
		}
	}
	for _, k := range keys {
		if path.Base(cleanPath(k)) == base {
			return lookup(k)
		}
	}
	for _, k := range keys {
		if Match(cleanPath(k), cp) {
			return lookup(k)
		}
	}
	if m, found := builtinLockfiles[base]; found {
		return m, true
	}
	return "", false
}

// Match reports whether path matches pattern. '*' and '?' stay inside one
// path segment; a '**' segment matches zero or more whole segments. Invalid
// or empty input simply does not match: Match never errors or panics.
func Match(pattern, p string) bool {
	pat := cleanPath(pattern)
	name := cleanPath(p)
	if pat == "" || name == "" {
		return false
	}
	return matchSegments(strings.Split(pat, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, segments []string) bool {
	rows := len(pattern) + 1
	cols := len(segments) + 1
	dp := make([][]bool, rows)
	for i := range dp {
		dp[i] = make([]bool, cols)
	}
	dp[0][0] = true
	for i := 1; i < rows; i++ {
		seg := pattern[i-1]
		if seg == "**" {
			dp[i][0] = dp[i-1][0]
			for j := 1; j < cols; j++ {
				dp[i][j] = dp[i-1][j] || dp[i][j-1]
			}
			continue
		}
		for j := 1; j < cols; j++ {
			dp[i][j] = dp[i-1][j-1] && matchSegment(seg, segments[j-1])
		}
	}
	return dp[rows-1][cols-1]
}

func matchSegment(pattern, segment string) bool {
	pat := []rune(pattern)
	seg := []rune(segment)
	rows := len(pat) + 1
	cols := len(seg) + 1
	dp := make([][]bool, rows)
	for i := range dp {
		dp[i] = make([]bool, cols)
	}
	dp[0][0] = true
	for i := 1; i < rows; i++ {
		ch := pat[i-1]
		if ch == '*' {
			dp[i][0] = dp[i-1][0]
			for j := 1; j < cols; j++ {
				dp[i][j] = dp[i-1][j] || dp[i][j-1]
			}
			continue
		}
		for j := 1; j < cols; j++ {
			if ch == '?' || ch == seg[j-1] {
				dp[i][j] = dp[i-1][j-1]
			}
		}
	}
	return dp[rows-1][cols-1]
}

func cleanPath(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\\", "/")
	for strings.HasPrefix(s, "./") {
		s = strings.TrimPrefix(s, "./")
	}
	s = path.Clean(s)
	if s == "." {
		return ""
	}
	return s
}
