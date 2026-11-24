// Package tampering reports test tampering in a quarantine diff. Check is a
// pure function: it never changes files and never executes commands. Callers
// downgrade a verdict to "unverifiable" when an empty_pass finding is present.
package tampering

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jerryjuche/padi/internal/verify/detect"
)

// Finding kinds.
const (
	KindDeletedTest        = "deleted_test"
	KindSkippedTest        = "skipped_test"
	KindWeakenedAssertions = "weakened_assertions"
	KindTestConfigChanged  = "test_config_changed"
	KindEmptyPass          = "empty_pass"
)

// Finding severities. deleted_test and skipped_test are always high.
const (
	SeverityHigh   = "high"
	SeverityMedium = "medium"
)

// Shared verifier types, aliased for convenience.
type (
	Step    = detect.Step
	Verdict = detect.Verdict
)

// Finding is one tampering signal for the approval card.
type Finding struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Detail   string `json:"detail"`
}

// Hunk is an optional structured way to supply changed lines.
type Hunk struct {
	Path    string
	Added   []string
	Removed []string
}

// Diff is the quarantine diff against the base commit. The primary fields are
// AddedFiles, DeletedFiles, ModifiedFiles, AddedLines and RemovedLines. The
// alias fields exist so callers with a different vocabulary can still feed the
// checker; all of them are merged. Raw accepts a unified diff and is parsed by
// ParseDiff. TestFiles/HasTestFiles describe files present at head, which are
// needed for the empty_pass rule.
type Diff struct {
	AddedFiles    []string
	DeletedFiles  []string
	ModifiedFiles []string

	// Aliases merged by Check.
	RemovedFiles  []string
	FilesAdded    []string
	FilesDeleted  []string
	FilesModified []string

	AddedLines   map[string][]string
	RemovedLines map[string][]string
	Added        map[string][]string
	Removed      map[string][]string
	DeletedLines map[string][]string
	Hunks        []Hunk
	Raw          string

	TestFiles    []string
	HasTestFiles bool
}

// Check reports tampering findings in deterministic order. It is pure: no I/O
// and no mutation of diff or verdict.
func Check(diff Diff, verdict Verdict) []Finding {
	n := normalize(diff)

	var findings []Finding

	// Deleted test files.
	for _, p := range sortedBoolKeys(n.deletedFiles) {
		if IsTestFile(p) {
			findings = append(findings, Finding{
				Kind: KindDeletedTest, Severity: SeverityHigh, Path: p,
				Detail: "test file deleted",
			})
		}
	}

	// Newly skipped tests.
	for _, p := range sortedLineKeys(n.addedLines) {
		for _, line := range n.addedLines[p] {
			if label, ok := skipPattern(line); ok {
				findings = append(findings, Finding{
					Kind: KindSkippedTest, Severity: SeverityHigh, Path: p,
					Detail: "added skip directive: " + label,
				})
			}
		}
	}

	findings = append(findings, weakenedFindings(n.addedLines, n.removedLines)...)
	findings = append(findings, configFindings(n.addedFiles, n.deletedFiles, n.modifiedFiles, n.addedLines, n.removedLines)...)

	// Empty pass: a pass verdict that executed no tests while test files exist.
	if verdict.Verdict == detect.VerdictPass && ExecutedTests(verdict) == 0 && hasTestFiles(diff, n.addedFiles, n.modifiedFiles) {
		findings = append(findings, Finding{
			Kind: KindEmptyPass, Severity: SeverityMedium, Path: "",
			Detail: "verdict is pass with zero executed tests while test files exist",
		})
	}

	findings = dedupe(findings)
	sortFindings(findings)
	return findings
}

// Apply downgrades a pass verdict to unverifiable when an empty_pass finding
// is present. It returns a new Verdict value and does not mutate its input.
func Apply(v Verdict, findings []Finding) Verdict {
	for _, f := range findings {
		if f.Kind == KindEmptyPass {
			v.Verdict = detect.VerdictUnverifiable
			return v
		}
	}
	return v
}

// Downgrade is an alias for Apply, named for the caller's intent.
func Downgrade(v Verdict, findings []Finding) Verdict { return Apply(v, findings) }

// ExecutedTests counts recognized test-result lines across all steps. It is a
// zero/non-zero signal for empty_pass, not an exact test count.
func ExecutedTests(v Verdict) int {
	total := 0
	for _, step := range v.Steps {
		total += countExecuted(step.OutputTail)
	}
	return total
}

func countExecuted(out string) int {
	passed := regexp.MustCompile(`(?i)(?:\bTests:\s*)?\b(\d+)\s+passed\b`)
	if matches := passed.FindAllStringSubmatch(out, -1); len(matches) > 0 {
		total := 0
		for _, m := range matches {
			n, err := strconv.Atoi(m[1])
			if err == nil {
				total += n
			}
		}
		return total
	}
	running := regexp.MustCompile(`(?m)^\s*running\s+(\d+)\s+tests?\b`)
	if matches := running.FindAllStringSubmatch(out, -1); len(matches) > 0 {
		total := 0
		for _, m := range matches {
			n, err := strconv.Atoi(m[1])
			if err == nil {
				total += n
			}
		}
		return total
	}
	verbose := regexp.MustCompile(`(?m)^\s*---\s+PASS:`)
	if n := len(verbose.FindAllString(out, -1)); n > 0 {
		return n
	}
	goPackage := regexp.MustCompile(`(?m)^ok\s+\S+`)
	return len(goPackage.FindAllString(out, -1))
}

// IsTestFile reports whether path matches the repository's test-file patterns.
func IsTestFile(path string) bool {
	p := cleanPath(path)
	p = strings.ReplaceAll(p, "\\", "/")
	base := filepath.Base(p)
	if strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, ".test.js") || strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".test.jsx") || strings.HasSuffix(base, ".test.tsx") ||
		strings.HasSuffix(base, ".spec.js") || strings.HasSuffix(base, ".spec.ts") ||
		strings.HasSuffix(base, ".spec.py") || base == "spec.py" ||
		(strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py")) {
		return true
	}
	segments := strings.Split(p, "/")
	for _, seg := range segments {
		if seg == "tests" || seg == "__tests__" || seg == "test" {
			return true
		}
	}
	return false
}

// ScanTestFiles returns sorted, repository-relative test files under dir.
// Vendor and generated directories are skipped.
func ScanTestFiles(dir string) []string {
	if dir == "" {
		dir = "."
	}
	var out []string
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor", "target", "dist", "build", ".venv", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if IsTestFile(rel) {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// ParseDiff converts a unified diff into a Diff. It handles new/deleted file
// modes and the standard ---/+++ headers.
func ParseDiff(raw string) Diff {
	d := Diff{Raw: raw}
	var current, old string
	deletedMode, addedMode := false, false

	flush := func() {
		newPath := cleanPath(current)
		oldPath := cleanPath(old)
		switch {
		case deletedMode && oldPath != "" && oldPath != "/dev/null":
			d.DeletedFiles = appendUnique(d.DeletedFiles, oldPath)
		case addedMode && newPath != "" && newPath != "/dev/null":
			d.AddedFiles = appendUnique(d.AddedFiles, newPath)
		case newPath != "" && newPath != "/dev/null":
			d.ModifiedFiles = appendUnique(d.ModifiedFiles, newPath)
		}
		current, old = "", ""
		deletedMode, addedMode = false, false
	}

	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				old, current = fields[2], fields[3]
			}
		case strings.HasPrefix(line, "deleted file mode"):
			deletedMode = true
		case strings.HasPrefix(line, "new file mode"):
			addedMode = true
		case strings.HasPrefix(line, "+++ "):
			p := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			if p != "/dev/null" {
				current = p
			}
		case strings.HasPrefix(line, "--- "):
			p := strings.TrimSpace(strings.TrimPrefix(line, "--- "))
			if p != "/dev/null" {
				old = p
			}
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			if current == "" || current == "/dev/null" {
				continue
			}
			appendLine(&d.AddedLines, cleanPath(current), strings.TrimPrefix(line, "+"))
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			key := current
			if key == "" || key == "/dev/null" {
				key = old
			}
			if key == "" || key == "/dev/null" {
				continue
			}
			appendLine(&d.RemovedLines, cleanPath(key), strings.TrimPrefix(line, "-"))
		}
	}
	flush()
	return d
}

type normalizedDiff struct {
	addedFiles    map[string]bool
	deletedFiles  map[string]bool
	modifiedFiles map[string]bool
	addedLines    map[string][]string
	removedLines  map[string][]string
}

func normalize(d Diff) normalizedDiff {
	if strings.TrimSpace(d.Raw) != "" {
		parsed := ParseDiff(d.Raw)
		d.AddedFiles = append(append([]string(nil), d.AddedFiles...), parsed.AddedFiles...)
		d.DeletedFiles = append(append([]string(nil), d.DeletedFiles...), parsed.DeletedFiles...)
		d.ModifiedFiles = append(append([]string(nil), d.ModifiedFiles...), parsed.ModifiedFiles...)
		d.AddedLines = mergeLineMaps(d.AddedLines, parsed.AddedLines)
		d.RemovedLines = mergeLineMaps(d.RemovedLines, parsed.RemovedLines)
	}

	n := normalizedDiff{
		addedFiles:    map[string]bool{},
		deletedFiles:  map[string]bool{},
		modifiedFiles: map[string]bool{},
		addedLines:    map[string][]string{},
		removedLines:  map[string][]string{},
	}
	for _, p := range d.AddedFiles {
		n.addedFiles[cleanPath(p)] = true
	}
	for _, p := range d.FilesAdded {
		n.addedFiles[cleanPath(p)] = true
	}
	for _, p := range d.DeletedFiles {
		n.deletedFiles[cleanPath(p)] = true
	}
	for _, p := range d.RemovedFiles {
		n.deletedFiles[cleanPath(p)] = true
	}
	for _, p := range d.FilesDeleted {
		n.deletedFiles[cleanPath(p)] = true
	}
	for _, p := range d.ModifiedFiles {
		n.modifiedFiles[cleanPath(p)] = true
	}
	for _, p := range d.FilesModified {
		n.modifiedFiles[cleanPath(p)] = true
	}
	for _, source := range []map[string][]string{d.AddedLines, d.Added} {
		for path, lines := range source {
			n.addedLines[cleanPath(path)] = append(n.addedLines[cleanPath(path)], lines...)
		}
	}
	for _, source := range []map[string][]string{d.RemovedLines, d.DeletedLines, d.Removed} {
		for path, lines := range source {
			n.removedLines[cleanPath(path)] = append(n.removedLines[cleanPath(path)], lines...)
		}
	}
	for _, h := range d.Hunks {
		key := cleanPath(h.Path)
		n.addedLines[key] = append(n.addedLines[key], h.Added...)
		n.removedLines[key] = append(n.removedLines[key], h.Removed...)
	}
	for _, p := range d.TestFiles {
		p = cleanPath(p)
		if IsTestFile(p) {
			n.modifiedFiles[p] = true
		}
	}
	return n
}

func skipPattern(line string) (string, bool) {
	patterns := []struct {
		label string
		re    *regexp.Regexp
	}{
		{"t.Skip", regexp.MustCompile(`\bt\.Skipf?\s*\(`)},
		{"pytest.mark.skip", regexp.MustCompile(`@pytest\.mark\.skip\b`)},
		{"pytest.skip", regexp.MustCompile(`\bpytest\.skip\s*\(`)},
		{".skip", regexp.MustCompile(`\.skip\s*\(`)},
		{"xit", regexp.MustCompile(`\bxit\s*\(`)},
		{"xdescribe", regexp.MustCompile(`\bxdescribe\s*\(`)},
		{"it.todo", regexp.MustCompile(`\bit\.todo\s*\(`)},
		{"@Ignore", regexp.MustCompile(`@Ignore\b`)},
		{"#[ignore]", regexp.MustCompile(`#\[ignore\b`)},
	}
	for _, p := range patterns {
		if p.re.MatchString(line) {
			return p.label, true
		}
	}
	return "", false
}

func weakenedFindings(added, removed map[string][]string) []Finding {
	var out []Finding
	for _, path := range sortedBoolKeys(unionKeys(added, removed)) {
		a, r := added[path], removed[path]
		removedCount, addedCount := assertionCount(r), assertionCount(a)
		if removedCount > 0 && float64(addedCount) <= float64(removedCount)*0.8 {
			out = append(out, Finding{
				Kind: KindWeakenedAssertions, Severity: SeverityMedium, Path: path,
				Detail: fmt.Sprintf("assertion calls dropped from %d to %d (20%% or more)", removedCount, addedCount),
			})
		}
		limit := len(r)
		if len(a) < limit {
			limit = len(a)
		}
		for i := 0; i < limit; i++ {
			if detail := weakenedPair(r[i], a[i]); detail != "" {
				out = append(out, Finding{
					Kind: KindWeakenedAssertions, Severity: SeverityMedium, Path: path, Detail: detail,
				})
			}
		}
	}
	return out
}

func assertionCount(lines []string) int {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\bassert(?:ion)?\b`),
		regexp.MustCompile(`\bexpect\s*\(`),
		regexp.MustCompile(`\brequire\.`),
		regexp.MustCompile(`\bt\.(?:Error|Fatal|Errorf|Fatalf)\s*\(`),
	}
	total := 0
	for _, line := range lines {
		for _, re := range patterns {
			total += len(re.FindAllString(line, -1))
		}
	}
	return total
}

func weakenedPair(removed, added string) string {
	r, a := strings.TrimSpace(removed), strings.TrimSpace(added)
	if r == "" || a == "" || r == a {
		return ""
	}
	switch {
	case strings.Contains(r, ".toStrictEqual(") && strings.Contains(a, ".toEqual("):
		return "strict equality assertion relaxed to .toEqual"
	case strings.Contains(r, ".toBe(") &&
		(strings.Contains(a, ".toBeTruthy(") || strings.Contains(a, ".toBeDefined(") || strings.Contains(a, ".toBeInstanceOf(")):
		return "exact value assertion relaxed to a truthiness/type check"
	case strings.Contains(r, "assert.Equal(") && !strings.Contains(a, "assert.Equal(") && assertionCount([]string{a}) > 0:
		return "assert.Equal removed or replaced by a weaker assertion"
	}
	if detail := reducedCount(r, a); detail != "" {
		return detail
	}
	if detail := broadenedTolerance(r, a); detail != "" {
		return detail
	}
	return ""
}

func reducedCount(removed, added string) string {
	countHint := regexp.MustCompile(`(?i)(toHaveBeenCalledTimes|assert\.Len|assert\.Equal|\blen\s*\()`)
	if !countHint.MatchString(removed) || !countHint.MatchString(added) {
		return ""
	}
	numbers := regexp.MustCompile(`\b\d+\b`)
	r, a := numbers.FindAllString(removed, -1), numbers.FindAllString(added, -1)
	if len(r) == 0 || len(a) == 0 {
		return ""
	}
	rv, err1 := strconv.Atoi(r[len(r)-1])
	av, err2 := strconv.Atoi(a[len(a)-1])
	if err1 != nil || err2 != nil || av >= rv {
		return ""
	}
	return fmt.Sprintf("expected count reduced from %d to %d", rv, av)
}

func broadenedTolerance(removed, added string) string {
	rv, rok := toleranceNumber(removed)
	av, aok := toleranceNumber(added)
	if !rok || !aok || av <= rv {
		return ""
	}
	return fmt.Sprintf("tolerance broadened from %g to %g", rv, av)
}

func toleranceNumber(line string) (float64, bool) {
	closeTo := regexp.MustCompile(`(?i)toBeCloseTo\s*\([^,]+,\s*(\d+)`)
	if m := closeTo.FindStringSubmatch(line); len(m) == 2 {
		digits, err := strconv.Atoi(m[1])
		if err == nil {
			// Fewer precision digits means a broader tolerance.
			return -float64(digits), true
		}
	}
	lower := strings.ToLower(line)
	if !strings.Contains(lower, "approx") && !strings.Contains(lower, "tolerance") &&
		!strings.Contains(lower, "delta") && !strings.Contains(lower, "precision") &&
		!strings.Contains(lower, "tol") && !strings.Contains(lower, "abs") {
		return 0, false
	}
	named := regexp.MustCompile(`(?i)(?:rel|abs|delta|tolerance|precision|tol)\s*[:=]\s*([0-9]+(?:\.[0-9]+)?(?:[eE][+-]?\d+)?)`)
	if m := named.FindStringSubmatch(line); len(m) == 2 {
		v, err := strconv.ParseFloat(m[1], 64)
		return v, err == nil
	}
	compare := regexp.MustCompile(`[<<=]\s*([0-9]+(?:\.[0-9]+)?(?:[eE][+-]?\d+)?)`)
	if m := compare.FindStringSubmatch(line); len(m) == 2 {
		v, err := strconv.ParseFloat(m[1], 64)
		return v, err == nil
	}
	return 0, false
}

func configFindings(addedFiles, deletedFiles, modifiedFiles map[string]bool, added, removed map[string][]string) []Finding {
	var out []Finding
	paths := unionKeys(unionKeys(addedFiles, deletedFiles), modifiedFiles)
	paths = unionKeys(paths, unionKeys(added, removed))
	for _, path := range sortedStringKeys(paths) {
		base := filepath.Base(path)
		if strings.HasPrefix(base, "jest.config.") || base == "jest.config" ||
			base == "pytest.ini" || base == "tox.ini" || base == "setup.cfg" {
			out = append(out, Finding{
				Kind: KindTestConfigChanged, Severity: SeverityMedium, Path: path,
				Detail: "test configuration file changed",
			})
		}
		a, r := added[path], removed[path]
		if base == "pyproject.toml" && linesMention(append(append([]string(nil), a...), r...), "[tool.pytest", "pytest") {
			out = append(out, Finding{
				Kind: KindTestConfigChanged, Severity: SeverityMedium, Path: path,
				Detail: "pytest configuration section changed",
			})
		}
		if (strings.EqualFold(base, "Makefile") || strings.Contains(path, ".github/workflows/")) &&
			linesMentionTestCommand(append(append([]string(nil), a...), r...)) {
			out = append(out, Finding{
				Kind: KindTestConfigChanged, Severity: SeverityMedium, Path: path,
				Detail: "test command in build or CI configuration changed",
			})
		}
		if detail := goTestFlagChange(a, r); detail != "" {
			out = append(out, Finding{
				Kind: KindTestConfigChanged, Severity: SeverityMedium, Path: path, Detail: detail,
			})
		}
	}
	return out
}

func linesMention(lines []string, needles ...string) bool {
	for _, line := range lines {
		for _, needle := range needles {
			if strings.Contains(strings.ToLower(line), strings.ToLower(needle)) {
				return true
			}
		}
	}
	return false
}

func linesMentionTestCommand(lines []string) bool {
	return linesMention(lines, "go test", "pytest", "npm test", "cargo test")
}

func goTestFlagChange(added, removed []string) string {
	flagRe := regexp.MustCompile(`-{1,2}[A-Za-z0-9][A-Za-z0-9_-]*(?:=[^\s]+)?`)
	collect := func(lines []string) map[string]bool {
		out := map[string]bool{}
		for _, line := range lines {
			if !strings.Contains(line, "go test") {
				continue
			}
			for _, flag := range flagRe.FindAllString(line, -1) {
				out[flag] = true
			}
		}
		return out
	}
	a, r := collect(added), collect(removed)
	if len(a) == 0 && len(r) == 0 {
		return ""
	}
	if sameFlags(a, r) {
		return ""
	}
	return fmt.Sprintf("go test flags changed from %s to %s", formatFlags(r), formatFlags(a))
}

func sameFlags(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func formatFlags(flags map[string]bool) string {
	if len(flags) == 0 {
		return "(none)"
	}
	keys := sortedBoolKeys(flags)
	return strings.Join(keys, " ")
}

func hasTestFiles(d Diff, addedFiles, modifiedFiles map[string]bool) bool {
	if d.HasTestFiles || len(d.TestFiles) > 0 {
		return true
	}
	for p := range addedFiles {
		if IsTestFile(p) {
			return true
		}
	}
	for p := range modifiedFiles {
		if IsTestFile(p) {
			return true
		}
	}
	return false
}

func dedupe(findings []Finding) []Finding {
	index := map[string]int{}
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		key := f.Kind + "\x00" + f.Path
		if i, ok := index[key]; ok {
			if severityRank(f.Severity) > severityRank(out[i].Severity) {
				out[i].Severity = f.Severity
			}
			if f.Detail != "" && f.Detail != out[i].Detail {
				out[i].Detail += "; " + f.Detail
			}
			continue
		}
		index[key] = len(out)
		out = append(out, f)
	}
	return out
}

func severityRank(s string) int {
	switch s {
	case SeverityHigh:
		return 2
	case SeverityMedium:
		return 1
	default:
		return 0
	}
}

func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Severity != b.Severity {
			return a.Severity < b.Severity
		}
		return a.Detail < b.Detail
	})
}

func sortedBoolKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func sortedLineKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func sortedStringKeys(m map[string]bool) []string { return sortedBoolKeys(m) }

func unionKeys[K comparable, V any](a, b map[K]V) map[K]bool {
	out := make(map[K]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func appendLine(m *map[string][]string, path, line string) {
	if path == "" {
		return
	}
	if *m == nil {
		*m = map[string][]string{}
	}
	(*m)[path] = append((*m)[path], line)
}

func appendUnique(list []string, value string) []string {
	if value == "" {
		return list
	}
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

func mergeLineMaps(a, b map[string][]string) map[string][]string {
	out := make(map[string][]string, len(a)+len(b))
	for path, lines := range a {
		out[path] = append(out[path], lines...)
	}
	for path, lines := range b {
		out[path] = append(out[path], lines...)
	}
	return out
}

func cleanPath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	path = strings.TrimPrefix(path, "./")
	path = strings.TrimPrefix(path, "a/")
	path = strings.TrimPrefix(path, "b/")
	return path
}
