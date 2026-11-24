// Package risk computes transparent, git-history-based risk signals for the
// approval card. It never reads secrets and never executes git directly: every
// command goes through gitops.Run against the gateway-owned quarantine repo.
package risk

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/gitops"
)

// Level is a transparent risk band. "unknown" means history was unavailable;
// it is never silently upgraded to a guessed level.
type Level string

// Risk levels.
const (
	LevelLow     Level = "low"
	LevelMedium  Level = "medium"
	LevelHigh    Level = "high"
	LevelUnknown Level = "unknown"
)

// gitTimeout is the hard per-command limit required by the spec.
const gitTimeout = 5 * time.Second

// IncidentRef is a confirmed incident (M10). Only confirmed entries are ever
// passed in; this package never queries a database.
type IncidentRef struct {
	ID         uuid.UUID
	Title      string
	OccurredAt time.Time
}

// Signals are the signals shown on the approval card.
type Signals struct {
	LastChangedDays int
	Commits90d      int
	Authors90d      int
	FixDensity      float64
	CoChange        []string
	ChangeSize      float64
	Tags            []string
	Incidents       []IncidentRef
}

// File is one changed path from the quarantine diff. Added/Deleted of zero
// means the analyzer asks git for numstat instead.
type File struct {
	Path    string
	Added   int
	Deleted int
	Status  string
}

// FileRisk is the analysis of a single changed path.
type FileRisk struct {
	Path    string
	Level   Level
	Signals Signals
	Reasons []string
}

// Thresholds tunes the transparent rules. Zero-valued fields get defaults.
type Thresholds struct {
	StaleDays        int
	HighFixDensity   float64
	HighCommits90d   int
	HighChangeSize   float64
	MediumChangeSize float64
	IncidentMedium   int
	IncidentHigh     int
}

// Analyzer holds tunable thresholds and already-confirmed incidents.
type Analyzer struct {
	Thresholds Thresholds
	Incidents  []IncidentRef
}

// New returns an analyzer with defaults for every unset threshold.
func New(t Thresholds) *Analyzer { return &Analyzer{Thresholds: normalize(t)} }

// WithIncidents returns a copy of the analyzer carrying confirmed incidents.
// It does not mutate the receiver.
func (a *Analyzer) WithIncidents(in []IncidentRef) *Analyzer {
	cp := *a
	cp.Incidents = append([]IncidentRef(nil), in...)
	return &cp
}

func normalize(t Thresholds) Thresholds {
	if t.StaleDays <= 0 {
		t.StaleDays = 180
	}
	if t.HighFixDensity <= 0 {
		t.HighFixDensity = 0.4
	}
	if t.HighCommits90d <= 0 {
		t.HighCommits90d = 3
	}
	if t.HighChangeSize <= 0 {
		t.HighChangeSize = 0.5
	}
	if t.MediumChangeSize <= 0 {
		t.MediumChangeSize = 0.2
	}
	if t.IncidentMedium <= 0 {
		t.IncidentMedium = 1
	}
	if t.IncidentHigh <= 0 {
		t.IncidentHigh = 2
	}
	return t
}

// ForDiff analyzes files against the quarantine repository. A per-file git
// failure yields LevelUnknown with a reason; only an invalid quarantine
// directory fails the whole call.
func (a *Analyzer) ForDiff(ctx context.Context, quarantineDir, baseSHA string, files []File) ([]FileRisk, Level, error) {
	if quarantineDir == "" {
		return nil, LevelUnknown, errors.New("risk: empty quarantine directory")
	}
	info, err := os.Stat(quarantineDir)
	if err != nil || !info.IsDir() {
		return nil, LevelUnknown, fmt.Errorf("risk: invalid quarantine directory %q", quarantineDir)
	}
	threshold := normalize(a.Thresholds)
	if err := validateRepo(ctx, quarantineDir); err != nil {
		return nil, LevelUnknown, fmt.Errorf("risk: invalid quarantine repository: %w", err)
	}

	diffPaths := make(map[string]bool, len(files))
	for _, f := range files {
		if clean := cleanPath(f.Path); clean != "" {
			diffPaths[clean] = true
		}
	}

	now := time.Now().UTC()
	risks := make([]FileRisk, 0, len(files))
	aggregate := LevelLow
	hasUnknown, hasHigh := false, false
	for _, f := range files {
		r := analyzeFile(ctx, quarantineDir, baseSHA, f, threshold, a.Incidents, diffPaths, now)
		risks = append(risks, r)
		switch r.Level {
		case LevelHigh:
			hasHigh = true
		case LevelUnknown:
			hasUnknown = true
		case LevelMedium:
			if aggregate == LevelLow {
				aggregate = LevelMedium
			}
		}
	}
	if hasHigh {
		aggregate = LevelHigh
	} else if hasUnknown {
		aggregate = LevelUnknown
	}
	return risks, aggregate, nil
}

func validateRepo(ctx context.Context, dir string) error {
	_, err := git(ctx, dir, "-C", dir, "rev-parse", "--git-dir")
	return err
}

// git runs one git command with its own five-second deadline. All arguments
// are passed as separate values; there is never a shell.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	return gitops.Run(runCtx, dir, nil, args...)
}

func analyzeFile(ctx context.Context, dir, baseSHA string, f File, t Thresholds, incidents []IncidentRef, diffPaths map[string]bool, now time.Time) FileRisk {
	r := FileRisk{Path: f.Path, Level: LevelLow}
	path := cleanPath(f.Path)
	if path == "" {
		return unknownRisk(r, "empty path")
	}

	added, deleted := f.Added, f.Deleted
	if added == 0 && deleted == 0 {
		if baseSHA == "" {
			return unknownRisk(r, "base SHA unavailable for numstat")
		}
		out, err := git(ctx, dir, "-C", dir, "diff", "--numstat", baseSHA, "--", path)
		if err != nil {
			return unknownRisk(r, shortError("numstat", err))
		}
		added, deleted = parseNumstat(out)
	}

	recent, err := git(ctx, dir, "-C", dir, "log", "--since=90.days", "--format=%H%x09%an%x09%at", "--", path)
	if err != nil {
		return unknownRisk(r, shortError("90d log", err))
	}
	commits, authors, latest := parseRecentLog(recent)
	r.Signals.Commits90d = commits
	r.Signals.Authors90d = len(authors)
	switch {
	case !latest.IsZero():
		days := int(now.Sub(latest).Hours() / 24)
		if days < 0 {
			days = 0
		}
		r.Signals.LastChangedDays = days
	default:
		// A 90-day log is empty for both stale and brand-new files. One
		// extra bounded query distinguishes them without guessing.
		old, err := git(ctx, dir, "-C", dir, "log", "-n", "1", "--format=%at", "--", path)
		if err != nil {
			return unknownRisk(r, shortError("last change", err))
		}
		if ts := firstTimestamp(old); !ts.IsZero() {
			days := int(now.Sub(ts).Hours() / 24)
			if days < 0 {
				days = 0
			}
			r.Signals.LastChangedDays = days
		}
	}

	subjects, err := git(ctx, dir, "-C", dir, "log", "--since=180.days", "--format=%s", "--", path)
	if err != nil {
		return unknownRisk(r, shortError("fix density log", err))
	}
	fixCount, total := countFixes(subjects)
	if total > 0 {
		r.Signals.FixDensity = float64(fixCount) / float64(total)
	}

	names, err := git(ctx, dir, "-C", dir, "log", "-n", "200", "--name-only", "--format=%H", "--", path)
	if err != nil {
		return unknownRisk(r, shortError("co-change log", err))
	}
	relevant := shaSet(names)
	if len(relevant) > 0 {
		// A pathspec restricts --name-only to that one path, so the full
		// log is needed to see which other files those commits touched.
		full, err := git(ctx, dir, "-C", dir, "log", "-n", "200", "--name-only", "--format=%H")
		if err != nil {
			return unknownRisk(r, shortError("co-change detail", err))
		}
		r.Signals.CoChange = topCoChanges(full, path, 3, relevant)
	}

	lineCount := 0
	if baseSHA != "" {
		out, err := git(ctx, dir, "-C", dir, "show", baseSHA+":"+path)
		if err == nil {
			lineCount = countLines(out)
		}
		// A missing base blob is a new file, not a history failure.
	}
	denominator := lineCount
	if denominator == 0 {
		denominator = added + deleted
	}
	if denominator > 0 {
		r.Signals.ChangeSize = float64(added+deleted) / float64(denominator)
	}
	if r.Signals.ChangeSize > 1 {
		r.Signals.ChangeSize = 1
	}

	r.Signals.Tags = sensitiveTags(path)
	touching := touchingIncidents(incidents, path, now)
	r.Signals.Incidents = touching

	level := LevelLow
	if len(r.Signals.Tags) > 0 {
		level = LevelHigh
		r.Reasons = append(r.Reasons, "sensitive tag: "+strings.Join(r.Signals.Tags, ", "))
	}
	if r.Signals.FixDensity >= t.HighFixDensity && r.Signals.Commits90d >= t.HighCommits90d {
		level = LevelHigh
		r.Reasons = append(r.Reasons, fmt.Sprintf("fix density %.2f across %d commits in 180d", r.Signals.FixDensity, r.Signals.Commits90d))
	} else if r.Signals.Commits90d >= t.HighCommits90d {
		r.Reasons = append(r.Reasons, fmt.Sprintf("churn: %d commits in 90d", r.Signals.Commits90d))
	}
	if r.Signals.ChangeSize >= t.HighChangeSize {
		level = LevelHigh
		r.Reasons = append(r.Reasons, fmt.Sprintf("change size %.0f%% of file", r.Signals.ChangeSize*100))
	}
	if r.Signals.Commits90d == 0 && r.Signals.LastChangedDays > t.StaleDays && r.Signals.ChangeSize >= t.MediumChangeSize {
		if level != LevelHigh {
			level = LevelMedium
		}
		r.Reasons = append(r.Reasons, fmt.Sprintf("stale: last changed %dd ago with %.0f%% churn", r.Signals.LastChangedDays, r.Signals.ChangeSize*100))
	}
	if level != LevelHigh {
		for _, co := range r.Signals.CoChange {
			if !diffPaths[co] {
				level = LevelMedium
				r.Reasons = append(r.Reasons, "co-change: "+co+" not in this diff")
				break
			}
		}
	}
	if len(touching) >= t.IncidentHigh {
		level = LevelHigh
		r.Reasons = append(r.Reasons, fmt.Sprintf("confirmed incidents: %d in 365d", len(touching)))
	} else if len(touching) >= t.IncidentMedium && level != LevelHigh {
		level = LevelMedium
		r.Reasons = append(r.Reasons, fmt.Sprintf("confirmed incident: %d in 365d", len(touching)))
	}
	if len(r.Reasons) == 0 {
		r.Reasons = append(r.Reasons, "low: no elevated history signals")
	}
	r.Level = level
	return r
}

func unknownRisk(r FileRisk, reason string) FileRisk {
	r.Level = LevelUnknown
	r.Reasons = []string{"history unavailable: " + reason}
	return r
}

func shortError(operation string, err error) string {
	detail := ""
	if err != nil {
		detail = strings.ReplaceAll(err.Error(), "\n", " ")
	}
	if len(detail) > 160 {
		detail = detail[:160] + "..."
	}
	return operation + " failed" + maybeDetail(detail)
}

func maybeDetail(s string) string {
	if s == "" {
		return ""
	}
	return ": " + html.EscapeString(s)
}

func cleanPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	p = strings.TrimPrefix(p, "./")
	return strings.Trim(p, "/")
}

func parseNumstat(out []byte) (added, deleted int) {
	line := strings.TrimSpace(string(out))
	if line == "" {
		return 0, 0
	}
	fields := strings.SplitN(line, "\t", 3)
	if len(fields) < 2 {
		return 0, 0
	}
	if n, err := strconv.Atoi(fields[0]); err == nil {
		added = n
	}
	if n, err := strconv.Atoi(fields[1]); err == nil {
		deleted = n
	}
	// Binary numstat uses "-" for both counts; churn is still a full rewrite.
	if strings.TrimSpace(fields[0]) == "-" && strings.TrimSpace(fields[1]) == "-" {
		return 1, 1
	}
	return added, deleted
}

func parseRecentLog(out []byte) (int, map[string]bool, time.Time) {
	authors := make(map[string]bool)
	commits := 0
	var latest time.Time
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		commits++
		if name := strings.TrimSpace(fields[1]); name != "" {
			authors[name] = true
		}
		if sec, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64); err == nil {
			ts := time.Unix(sec, 0).UTC()
			if ts.After(latest) {
				latest = ts
			}
		}
	}
	return commits, authors, latest
}

func firstTimestamp(out []byte) time.Time {
	for _, line := range strings.Split(string(out), "\n") {
		if sec, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64); err == nil {
			return time.Unix(sec, 0).UTC()
		}
	}
	return time.Time{}
}

var fixTokens = []string{"fix", "fixes", "fixed", "revert", "hotfix", "rollback"}

func countFixes(out []byte) (fixes, total int) {
	for _, line := range strings.Split(string(out), "\n") {
		subject := strings.TrimSpace(line)
		if subject == "" {
			continue
		}
		total++
		raw := strings.ToLower(subject)
		for _, token := range fixTokens {
			if containsWord(raw, token) {
				fixes++
				break
			}
		}
	}
	return fixes, total
}

func containsWord(s, word string) bool {
	for start := 0; start < len(s); {
		i := strings.Index(s[start:], word)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(word)
		before := i == 0 || !isWordByte(s[i-1])
		after := end == len(s) || !isWordByte(s[end])
		if before && after {
			return true
		}
		start = i + 1
	}
	return false
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func shaSet(out []byte) map[string]bool {
	set := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		if sha := strings.TrimSpace(line); isSHA(sha) {
			set[sha] = true
		}
	}
	return set
}

func topCoChanges(out []byte, self string, limit int, relevant map[string]bool) []string {
	self = cleanPath(self)
	counts := make(map[string]int)
	current := ""
	paths := make(map[string]map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if isSHA(line) {
			current = line
			if paths[current] == nil {
				paths[current] = make(map[string]bool)
			}
			continue
		}
		if current == "" || line == "" {
			continue
		}
		p := cleanPath(line)
		if p != "" && !strings.HasPrefix(p, "commit ") {
			paths[current][p] = true
		}
	}
	for sha, files := range paths {
		if len(relevant) > 0 && !relevant[sha] {
			continue
		}
		if !files[self] {
			continue
		}
		for p := range files {
			if p != self {
				counts[p]++
			}
		}
	}
	ordered := make([]string, 0, len(counts))
	for p := range counts {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if counts[ordered[i]] != counts[ordered[j]] {
			return counts[ordered[i]] > counts[ordered[j]]
		}
		return ordered[i] < ordered[j]
	})
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	return ordered
}

func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')) {
			return false
		}
	}
	return true
}

func countLines(in []byte) int {
	if len(in) == 0 {
		return 0
	}
	n := 1
	for _, b := range in {
		if b == '\n' {
			n++
		}
	}
	return n
}

var sensitiveTagWords = []string{"auth", "payment", "payments", "billing", "migration", "migrations", "config", "secret", "secrets", "admin"}

func sensitiveTags(path string) []string {
	lower := strings.ToLower(cleanPath(path))
	var tags []string
	for _, word := range sensitiveTagWords {
		if containsWord(lower, word) || strings.Contains(lower, word+"_") || strings.Contains(lower, "/"+word) {
			tags = append(tags, word)
		}
	}
	return tags
}

func touchingIncidents(incidents []IncidentRef, path string, now time.Time) []IncidentRef {
	cutoff := now.AddDate(0, 0, -365)
	path = cleanPath(path)
	base := filepath.Base(path)
	var out []IncidentRef
	for _, in := range incidents {
		if in.OccurredAt.Before(cutoff) || in.OccurredAt.After(now) {
			continue
		}
		if incidentTouches(in.Title, path, base) {
			out = append(out, in)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out
}

func incidentTouches(title, path, base string) bool {
	title = strings.ToLower(strings.ReplaceAll(title, "\\", "/"))
	if path != "" && strings.Contains(title, strings.ToLower(path)) {
		return true
	}
	if len(base) >= 4 && strings.Contains(title, strings.ToLower(base)) {
		return true
	}
	// Titles often name the component rather than the full path.
	skipSegments := map[string]bool{
		"internal": true, "pkg": true, "src": true, "app": true,
		"cmd": true, "lib": true, "test": true, "tests": true, "web": true,
	}
	for _, seg := range strings.Split(strings.ToLower(path), "/") {
		if len(seg) >= 5 && !skipSegments[seg] && strings.Contains(title, seg) {
			return true
		}
	}
	return false
}

// clampRune keeps card output bounded without splitting UTF-8 sequences.
func clampRune(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}
