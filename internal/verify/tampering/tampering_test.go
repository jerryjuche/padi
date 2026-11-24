package tampering

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jerryjuche/padi/internal/verify/detect"
)

func TestCheckAllKinds(t *testing.T) {
	passWithoutTests := detect.Verdict{
		Verdict: detect.VerdictPass,
		Steps:   []detect.Step{{Name: "go test", OK: true, OutputTail: "? example.com/pkg [no test files]\n"}},
	}

	cases := []struct {
		name      string
		diff      Diff
		verdict   detect.Verdict
		wantKinds []string
		highKinds []string
	}{
		{
			name:      "deleted test file",
			diff:      Diff{DeletedFiles: []string{"pkg/calculator_test.go"}},
			verdict:   passWithoutTests,
			wantKinds: []string{KindDeletedTest},
			highKinds: []string{KindDeletedTest},
		},
		{
			name:      "deleted spec.py",
			diff:      Diff{DeletedFiles: []string{"spec.py"}},
			verdict:   passWithoutTests,
			wantKinds: []string{KindDeletedTest},
			highKinds: []string{KindDeletedTest},
		},
		{
			name: "added skip directives",
			diff: Diff{AddedLines: map[string][]string{"a_test.go": {
				`t.Skip("flaky")`,
				"it.skip('broken', () => {});",
				"pytest.skip('no env')",
				"#[ignore]",
			}}},
			verdict:   passWithoutTests,
			wantKinds: []string{KindSkippedTest},
			highKinds: []string{KindSkippedTest},
		},
		{
			name: "weakened assertion by reduced count",
			diff: Diff{
				RemovedLines: map[string][]string{"a_test.go": {"assert.Equal(t, 3, len(got))"}},
				AddedLines:   map[string][]string{"a_test.go": {"assert.Equal(t, 2, len(got))"}},
			},
			verdict:   passWithoutTests,
			wantKinds: []string{KindWeakenedAssertions},
		},
		{
			name: "weakened assertion by broadened tolerance",
			diff: Diff{
				RemovedLines: map[string][]string{"a_test.go": {"assert math.Abs(got-want) < 0.01"}},
				AddedLines:   map[string][]string{"a_test.go": {"assert math.Abs(got-want) < 0.1"}},
			},
			verdict:   passWithoutTests,
			wantKinds: []string{KindWeakenedAssertions},
		},
		{
			name: "weakened assertion count dropped by 20 percent",
			diff: Diff{
				RemovedLines: map[string][]string{"a_test.go": {
					"assert.Equal(t, 1, a)", "assert.Equal(t, 2, b)",
					"assert.Equal(t, 3, c)", "assert.Equal(t, 4, d)", "assert.Equal(t, 5, e)",
				}},
				AddedLines: map[string][]string{"a_test.go": {
					"assert.Equal(t, 1, a)", "assert.Equal(t, 2, b)", "assert.Equal(t, 3, c)",
				}},
			},
			verdict:   passWithoutTests,
			wantKinds: []string{KindWeakenedAssertions},
		},
		{
			name:      "changed jest config",
			diff:      Diff{ModifiedFiles: []string{"jest.config.js"}},
			verdict:   passWithoutTests,
			wantKinds: []string{KindTestConfigChanged},
		},
		{
			name: "changed pyproject pytest section",
			diff: Diff{
				ModifiedFiles: []string{"pyproject.toml"},
				AddedLines:    map[string][]string{"pyproject.toml": {"[tool.pytest.ini_options]", "addopts = \"-q\""}},
			},
			verdict:   passWithoutTests,
			wantKinds: []string{KindTestConfigChanged},
		},
		{
			name: "changed go test flags",
			diff: Diff{
				ModifiedFiles: []string{"Makefile"},
				RemovedLines:  map[string][]string{"Makefile": {"go test -race ./..."}},
				AddedLines:    map[string][]string{"Makefile": {"go test ./..."}},
			},
			verdict:   passWithoutTests,
			wantKinds: []string{KindTestConfigChanged},
		},
		{
			name: "empty pass with test files",
			diff: Diff{HasTestFiles: true},
			verdict: detect.Verdict{
				Verdict: detect.VerdictPass,
				Steps:   []detect.Step{{Name: "go test", OK: true, OutputTail: "? pkg [no test files]\n"}},
			},
			wantKinds: []string{KindEmptyPass},
		},
		{
			name: "genuine pass with executed tests is clean",
			diff: Diff{HasTestFiles: true},
			verdict: detect.Verdict{
				Verdict: detect.VerdictPass,
				Steps:   []detect.Step{{Name: "go test", OK: true, OutputTail: "ok  example.com/pkg 0.01s\n"}},
			},
			wantKinds: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := Check(tc.diff, tc.verdict)
			gotKinds := make([]string, 0, len(findings))
			for _, f := range findings {
				gotKinds = append(gotKinds, f.Kind)
			}
			if len(tc.wantKinds) == 0 && len(gotKinds) != 0 {
				t.Fatalf("findings = %#v, want none", findings)
			}
			for _, want := range tc.wantKinds {
				found := false
				for _, f := range findings {
					if f.Kind == want {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("findings = %#v, want kind %q", findings, want)
				}
			}
			for _, wantHigh := range tc.highKinds {
				for _, f := range findings {
					if f.Kind == wantHigh && f.Severity != SeverityHigh {
						t.Fatalf("%s severity = %q, want high", f.Kind, f.Severity)
					}
				}
			}
			if !reflect.ValueOf(tc.diff).IsZero() {
				// Check must not mutate the caller's maps.
				if len(tc.diff.AddedLines) > 0 {
					for path, lines := range tc.diff.AddedLines {
						if len(lines) == 0 {
							t.Fatalf("AddedLines[%q] was emptied by Check", path)
						}
					}
				}
			}
		})
	}
}

func TestCheckParseDiff(t *testing.T) {
	raw := strings.Join([]string{
		"diff --git a/old_test.go b/old_test.go",
		"deleted file mode 100644",
		"--- a/old_test.go",
		"+++ /dev/null",
		"@@ -1,2 +0,0 @@",
		"-package old",
		"diff --git a/new_test.go b/new_test.go",
		"--- a/new_test.go",
		"+++ b/new_test.go",
		"@@ -1 +1,2 @@",
		"+package new",
		`+t.Skip("temporarily disabled")`,
		"",
	}, "\n")

	findings := Check(Diff{Raw: raw}, detect.Verdict{Verdict: detect.VerdictFail})
	kinds := map[string]bool{}
	for _, f := range findings {
		kinds[f.Kind] = true
	}
	if !kinds[KindDeletedTest] || !kinds[KindSkippedTest] {
		t.Fatalf("findings = %#v, want deleted_test and skipped_test", findings)
	}
}

func TestApplyDowngradesEmptyPass(t *testing.T) {
	v := detect.Verdict{Verdict: detect.VerdictPass, Steps: []detect.Step{{Name: "pytest"}}}
	findings := []Finding{{Kind: KindEmptyPass, Severity: SeverityMedium}}
	got := Apply(v, findings)
	if got.Verdict != detect.VerdictUnverifiable {
		t.Fatalf("Apply verdict = %q, want unverifiable", got.Verdict)
	}
	if v.Verdict != detect.VerdictPass {
		t.Fatal("Apply mutated its input")
	}
	if Downgrade(v, []Finding{{Kind: KindDeletedTest}}).Verdict != detect.VerdictPass {
		t.Fatal("Downgrade changed a verdict without an empty_pass finding")
	}
}

func TestExecutedTests(t *testing.T) {
	cases := []struct {
		name string
		tail string
		want int
	}{
		{"go package ok", "ok  example.com/pkg 0.01s\n", 1},
		{"go verbose pass", "--- PASS: TestFoo (0.00s)\nPASS\n", 1},
		{"pytest passed", "================= 2 passed in 0.1s =================\n", 2},
		{"jest passed", "Tests: 3 passed, 3 total\n", 3},
		{"cargo zero", "running 0 tests\ntest result: ok. 0 passed; 0 failed\n", 0},
		{"no tests", "? example.com/pkg [no test files]\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := detect.Verdict{Steps: []detect.Step{{OutputTail: tc.tail}}}
			if got := ExecutedTests(v); got != tc.want {
				t.Fatalf("ExecutedTests(%q) = %d, want %d", tc.tail, got, tc.want)
			}
		})
	}
}

func TestIsTestFile(t *testing.T) {
	cases := map[string]bool{
		"pkg/a_test.go":         true,
		"spec.py":               true,
		"tests/test_api.py":     true,
		"__tests__/app.test.js": true,
		"app.test.ts":           true,
		"main.go":               false,
		"README.md":             false,
	}
	for path, want := range cases {
		if got := IsTestFile(path); got != want {
			t.Fatalf("IsTestFile(%q) = %v, want %v", path, got, want)
		}
	}
}
