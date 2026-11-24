// Package guard reports test tampering in a quarantine diff. Check is pure:
// it never touches files, never runs commands and never changes a verdict.
// Callers downgrade a pass verdict to unverifiable when empty_pass appears.
package guard

import (
	"path"
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

// Finding severities. deleted_test, skipped_test and empty_pass are high.
const (
	SeverityHigh   = "high"
	SeverityMedium = "medium"
)

// Step and Verdict are aliases of the verifier's types so that a verdict can
// be passed straight from the runner without an import cycle.
type (
	Step    = detect.Step
	Verdict = detect.Verdict
)

// File is one changed path. AddedLines/RemovedLines/Patch are optional richer
// inputs for the line-based rules; OldPath carries a rename source.
type File struct {
	Path         string
	Status       string
	Added        int
	Deleted      int
	OldPath      string
	AddedLines   []string
	RemovedLines []string
	Patch        string
}

// Diff is the quarantine diff against the base commit. The map fields are
// optional aliases for callers that already have line-level data.
type Diff struct {
	Files        []File
	AddedLines   map[string][]string
	RemovedLines map[string][]string
	Raw          string
	TestFiles    []string
	HasTestFiles bool
}

// Finding is one tampering signal for the approval card.
type Finding struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Detail   string `json:"detail"`
}

// Check reports findings in deterministic order.
func Check(diff Diff, verdict Verdict) []Finding {
	var findings []Finding
	for _, f := range diff.Files {
		if finding, ok := deletedFinding(f); ok {
			findings = append(findings, finding)
		}
		if finding, ok := skippedFinding(f, diff); ok {
			findings = append(findings, finding)
		}
		if finding, ok := weakenedFinding(f, diff); ok {
			findings = append(findings, finding)
		}
		if isTestConfig(f.Path) || isTestConfig(f.OldPath) {
			findings = append(findings, Finding{
				Kind: KindTestConfigChanged, Severity: SeverityMedium, Path: f.Path,
				Detail: "test configuration changed",
			})
		}
	}
	for p, lines := range diff.AddedLines {
		if !isTestFile(p) {
			continue
		}
		if label, ok := skippedLines(lines); ok {
			findings = append(findings, Finding{
				Kind: KindSkippedTest, Severity: SeverityHigh, Path: p,
				Detail: "added skip directive: " + label,
			})
		}
	}
	rawAdds, rawRemoves, rawDeleted := parseRawPatch(diff.Raw)
	for _, p := range rawDeleted {
		if isTestFile(p) {
			findings = append(findings, Finding{
				Kind: KindDeletedTest, Severity: SeverityHigh, Path: p,
				Detail: "test file deleted or renamed away",
			})
		}
	}
	for p, lines := range rawAdds {
		if !isTestFile(p) {
			continue
		}
		synthetic := File{Path: p, Status: "M", AddedLines: lines, RemovedLines: rawRemoves[p]}
		if finding, ok := skippedFinding(synthetic, diff); ok {
			findings = append(findings, finding)
		}
		if finding, ok := weakenedFinding(synthetic, diff); ok {
			findings = append(findings, finding)
		}
	}
	if verdict.Verdict == detect.VerdictPass && hasTestFiles(diff) &&
		(!anyStepOK(verdict.Steps) || executedTests(verdict) == 0) {
		findings = append(findings, Finding{
			Kind: KindEmptyPass, Severity: SeverityHigh,
			Detail: "verdict is pass with zero executed tests while test files exist",
		})
	}
	return dedupeSort(findings)
}

func deletedFinding(f File) (Finding, bool) {
	status := strings.ToUpper(strings.TrimSpace(f.Status))
	old := f.OldPath
	if old == "" {
		old = f.Path
	}
	deleted := strings.HasPrefix(status, "D") || status == "DELETED" || status == "RENAMED" || strings.HasPrefix(status, "R")
	if !deleted {
		return Finding{}, false
	}
	oldTest, newTest := isTestFile(old), isTestFile(f.Path)
	if !oldTest {
		return Finding{}, false
	}
	if strings.HasPrefix(status, "R") && f.OldPath == "" && newTest {
		// Without a source path we cannot prove a test was renamed away.
		return Finding{}, false
	}
	if newTest && old != f.Path && !strings.HasPrefix(status, "D") {
		return Finding{}, false
	}
	return Finding{
		Kind: KindDeletedTest, Severity: SeverityHigh, Path: old,
		Detail: "test file deleted or renamed away",
	}, true
}

func skippedFinding(f File, diff Diff) (Finding, bool) {
	added := addedLines(f, diff)
	if label, ok := skippedLines(added); ok {
		return Finding{
			Kind: KindSkippedTest, Severity: SeverityHigh, Path: f.Path,
			Detail: "added skip directive: " + label,
		}, true
	}
	return Finding{}, false
}

func weakenedFinding(f File, diff Diff) (Finding, bool) {
	if !isTestFile(f.Path) && !isTestFile(f.OldPath) {
		return Finding{}, false
	}
	added, removed := addedLines(f, diff), removedLines(f, diff)
	if explicitWeakening(added, removed) || assertionDrop(added, removed) {
		return Finding{
			Kind: KindWeakenedAssertions, Severity: SeverityMedium, Path: f.Path,
			Detail: "assertions weakened in test file",
		}, true
	}
	return Finding{}, false
}

func addedLines(f File, diff Diff) []string {
	out := append([]string(nil), f.AddedLines...)
	if extra := diff.AddedLines[f.Path]; len(extra) > 0 {
		out = append(out, extra...)
	}
	if extra := diff.AddedLines[f.OldPath]; f.OldPath != "" && len(extra) > 0 {
		out = append(out, extra...)
	}
	out = append(out, patchLines(f.Patch, true)...)
	return out
}

func removedLines(f File, diff Diff) []string {
	out := append([]string(nil), f.RemovedLines...)
	if extra := diff.RemovedLines[f.Path]; len(extra) > 0 {
		out = append(out, extra...)
	}
	if extra := diff.RemovedLines[f.OldPath]; f.OldPath != "" && len(extra) > 0 {
		out = append(out, extra...)
	}
	out = append(out, patchLines(f.Patch, false)...)
	return out
}

var skipLabels = []string{
	"t.skip(", "it.skip(", "describe.skip(", "test.skip(", ".skip(",
	"xit(", "xdescribe(", "it.todo(", "@pytest.mark.skip", "pytest.skip(",
	"@ignore", "skipif",
}

func skippedLines(lines []string) (string, bool) {
	for _, line := range lines {
		lower := strings.ToLower(strings.TrimSpace(line))
		if lower == "skip" || strings.HasPrefix(lower, "skip(") ||
			strings.HasPrefix(lower, "@skip") || strings.HasPrefix(lower, "# skip") {
			return "skip", true
		}
		for _, label := range skipLabels {
			if strings.Contains(lower, label) {
				return label, true
			}
		}
	}
	return "", false
}

var weakenPairs = [][2]string{
	{"assert.equal", "assert.ok"},
	{"assert.equal", "ok("},
	{"assert.equal", "tobetruthy"},
	{"assert.equal", "tobetruthy("},
	{"assert.true", "assert("},
	{"asserttrue", "assert("},
	{".to.equal", "tobetruthy"},
	{".to.equal", "tobetruthy("},
	{".to.equal", "to.be.truthy"},
	{".to.equal", "to.be.ok"},
	{"expect(", "tobetruthy"},
}

func explicitWeakening(added, removed []string) bool {
	lowerAdded := lowerAll(added)
	lowerRemoved := lowerAll(removed)
	hasOld := func(needle string) bool {
		for _, line := range lowerRemoved {
			if strings.Contains(line, needle) {
				return true
			}
		}
		return false
	}
	hasNew := func(needle string) bool {
		for _, line := range lowerAdded {
			if strings.Contains(line, needle) {
				return true
			}
		}
		return false
	}
	for _, pair := range weakenPairs {
		if hasOld(pair[0]) && hasNew(pair[1]) {
			return true
		}
	}
	for _, line := range lowerAdded {
		if strings.Contains(line, "assert") && (strings.Contains(line, "remove") || strings.Contains(line, "delete")) {
			return true
		}
	}
	return false
}

func assertionDrop(added, removed []string) bool {
	before, after := 0, 0
	for _, line := range removed {
		before += assertionCount(line)
	}
	for _, line := range added {
		after += assertionCount(line)
	}
	return before > 0 && float64(after) <= float64(before)*0.8
}

func assertionCount(line string) int {
	lower := strings.ToLower(line)
	total := 0
	for _, token := range []string{"assert", "expect(", "require.", "t.error"} {
		for start := 0; start < len(lower); {
			i := strings.Index(lower[start:], token)
			if i < 0 {
				break
			}
			total++
			start += i + len(token)
		}
	}
	return total
}

func lowerAll(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, strings.ToLower(line))
	}
	return out
}

func anyStepOK(steps []Step) bool {
	for _, s := range steps {
		if s.OK {
			return true
		}
	}
	return false
}

// executedTests is a zero/non-zero signal, not an exact tally. It recognizes
// the standard Go, Jest and pytest summary lines without running anything.
func executedTests(v Verdict) int {
	total := 0
	for _, step := range v.Steps {
		total += countExecuted(step.OutputTail)
	}
	return total
}

func countExecuted(out string) int {
	total := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.ToLower(line))
		for i, field := range fields {
			field = strings.TrimRight(field, ",.;:%")
			if field != "passed" && field != "pass" {
				continue
			}
			if i > 0 {
				if n, err := strconv.Atoi(strings.TrimRight(fields[i-1], ",%")); err == nil && n > 0 {
					total += n
					break
				}
			}
			if field == "pass" {
				total++
			}
		}
		if len(fields) > 0 && fields[0] == "ok" && len(fields) > 1 {
			total++
		}
		if len(fields) > 2 && fields[0] == "running" {
			if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 {
				total += n
			}
		}
	}
	return total
}

func hasTestFiles(diff Diff) bool {
	if diff.HasTestFiles {
		return true
	}
	for _, p := range diff.TestFiles {
		if p != "" {
			return true
		}
	}
	for _, f := range diff.Files {
		if isTestFile(f.Path) || isTestFile(f.OldPath) {
			return true
		}
	}
	if p := normalizedPatchPath(diff.Raw); p != "" && isTestFile(p) {
		return true
	}
	_, _, deleted := parseRawPatch(diff.Raw)
	for _, p := range deleted {
		if isTestFile(p) {
			return true
		}
	}
	return false
}

// IsTestFile reports whether a path matches the configured test patterns.
func IsTestFile(p string) bool { return isTestFile(p) }

func isTestFile(p string) bool {
	p = cleanSlash(strings.TrimSpace(p))
	if p == "" {
		return false
	}
	base := path.Base(p)
	if strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, ".spec.go") {
		return true
	}
	for _, suffix := range []string{".test.js", ".test.jsx", ".test.ts", ".test.tsx", ".test.mjs", ".spec.js", ".spec.jsx", ".spec.ts", ".spec.tsx", ".spec.mjs"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	if strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") {
		return true
	}
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		if i == 0 && seg == "" {
			continue
		}
		if seg == "tests" || seg == "__tests__" {
			return true
		}
	}
	return false
}

func isTestConfig(p string) bool {
	p = cleanSlash(strings.TrimSpace(p))
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, ".github/workflows/") {
		return true
	}
	base := strings.ToLower(path.Base(p))
	switch {
	case base == "package.json", base == "pyproject.toml", base == "pytest.ini",
		base == "tox.ini", base == "makefile", base == "jest.config.js",
		base == "jest.config.ts", base == "jest.config.mjs", base == "jest.config.cjs",
		base == "vitest.config.ts", base == "vitest.config.js", base == "vitest.config.mts":
		return true
	}
	return strings.HasPrefix(base, "jest.config.") || strings.HasPrefix(base, "vitest.config.")
}

func cleanSlash(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	return strings.Trim(p, "/")
}

func patchLines(patch string, added bool) []string {
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		if added && strings.HasPrefix(line, "+") {
			out = append(out, strings.TrimPrefix(line, "+"))
		}
		if !added && strings.HasPrefix(line, "-") {
			out = append(out, strings.TrimPrefix(line, "-"))
		}
	}
	return out
}

func parseRawPatch(raw string) (map[string][]string, map[string][]string, []string) {
	adds, removes := make(map[string][]string), make(map[string][]string)
	var deleted []string
	current, oldPath := "", ""
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "--- a/") {
			oldPath = cleanSlash(strings.TrimPrefix(line, "--- a/"))
			continue
		}
		if strings.HasPrefix(line, "+++ b/") {
			current = cleanSlash(strings.TrimPrefix(line, "+++ b/"))
			oldPath = ""
			continue
		}
		if strings.HasPrefix(line, "+++ /dev/null") {
			if oldPath != "" {
				deleted = append(deleted, oldPath)
			}
			current, oldPath = "", ""
			continue
		}
		if current == "" {
			continue
		}
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			adds[current] = append(adds[current], strings.TrimPrefix(line, "+"))
		}
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			removes[current] = append(removes[current], strings.TrimPrefix(line, "-"))
		}
	}
	return adds, removes, deleted
}

func normalizedPatchPath(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "+++ b/") {
			return cleanSlash(strings.TrimPrefix(line, "+++ b/"))
		}
	}
	return ""
}

func dedupeSort(findings []Finding) []Finding {
	seen := make(map[Finding]bool, len(findings))
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}
