package guard

import "testing"

func hasFinding(findings []Finding, kind string) bool {
	for _, f := range findings {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

func TestCheck(t *testing.T) {
	passWithTests := Verdict{
		Verdict: "pass",
		Steps:   []Step{{Name: "go test", OK: true, OutputTail: "ok  example.com/pkg 0.02s\n3 passed"}},
	}

	cases := []struct {
		name     string
		diff     Diff
		verdict  Verdict
		wantKind string
		wantSev  string
	}{
		{
			name:     "deleted test",
			diff:     Diff{Files: []File{{Path: "pkg/foo_test.go", Status: "D", Deleted: 40}}},
			verdict:  passWithTests,
			wantKind: KindDeletedTest, wantSev: SeverityHigh,
		},
		{
			name:     "renamed away test",
			diff:     Diff{Files: []File{{Path: "pkg/helper.go", OldPath: "pkg/helper_test.go", Status: "R100"}}},
			verdict:  passWithTests,
			wantKind: KindDeletedTest, wantSev: SeverityHigh,
		},
		{
			name: "added skip",
			diff: Diff{Files: []File{{Path: "pkg/foo_test.go", Status: "M", AddedLines: []string{
				`func TestFoo(t *testing.T) { t.Skip("not ready") }`,
			}}}},
			verdict:  passWithTests,
			wantKind: KindSkippedTest, wantSev: SeverityHigh,
		},
		{
			name: "pytest skip",
			diff: Diff{Files: []File{{Path: "tests/test_api.py", Status: "M", AddedLines: []string{
				"@pytest.mark.skip(reason='flaky')",
			}}}},
			verdict:  passWithTests,
			wantKind: KindSkippedTest, wantSev: SeverityHigh,
		},
		{
			name: "weakened assertion",
			diff: Diff{Files: []File{{Path: "pkg/foo_test.go", Status: "M",
				RemovedLines: []string{`assert.equal(got, want)`},
				AddedLines:   []string{`assert.ok(got)`},
			}}},
			verdict:  passWithTests,
			wantKind: KindWeakenedAssertions, wantSev: SeverityMedium,
		},
		{
			name: "assertion count drop",
			diff: Diff{Files: []File{{Path: "pkg/foo_test.go", Status: "M",
				RemovedLines: []string{`assert.Equal(a, b)`, `assert.Equal(c, d)`, `assert.Equal(e, f)`},
				AddedLines:   []string{`assert.True(a)`},
			}}},
			verdict:  passWithTests,
			wantKind: KindWeakenedAssertions, wantSev: SeverityMedium,
		},
		{
			name:     "test config changed",
			diff:     Diff{Files: []File{{Path: ".github/workflows/ci.yml", Status: "M"}}},
			verdict:  passWithTests,
			wantKind: KindTestConfigChanged, wantSev: SeverityMedium,
		},
		{
			name: "empty pass",
			diff: Diff{Files: []File{{Path: "pkg/foo_test.go", Status: "M"}}},
			verdict: Verdict{Verdict: "pass", Steps: []Step{{
				Name: "go test", OK: true, OutputTail: "no tests to run",
			}}},
			wantKind: KindEmptyPass, wantSev: SeverityHigh,
		},
		{
			name:     "empty pass no steps",
			diff:     Diff{Files: []File{{Path: "pkg/foo_test.go", Status: "M"}}},
			verdict:  Verdict{Verdict: "pass"},
			wantKind: KindEmptyPass, wantSev: SeverityHigh,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := Check(tc.diff, tc.verdict)
			var found *Finding
			for i := range findings {
				if findings[i].Kind == tc.wantKind {
					found = &findings[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("findings = %+v, want %s", findings, tc.wantKind)
			}
			if found.Severity != tc.wantSev {
				t.Fatalf("severity = %s, want %s", found.Severity, tc.wantSev)
			}
			if tc.wantKind == KindEmptyPass && findings[0].Severity != SeverityHigh {
				t.Fatalf("empty_pass must be high severity: %+v", findings)
			}
		})
	}
}

func TestCheckCleanDiff(t *testing.T) {
	diff := Diff{Files: []File{{Path: "pkg/foo_test.go", Status: "M",
		AddedLines: []string{`assert.Equal(got, want)`},
	}}}
	verdict := Verdict{Verdict: "pass", Steps: []Step{{Name: "go test", OK: true, OutputTail: "Tests: 3 passed"}}}
	if findings := Check(diff, verdict); len(findings) != 0 {
		t.Fatalf("unexpected findings: %+v", findings)
	}
	// A non-pass verdict is never an empty pass.
	fail := Verdict{Verdict: "fail"}
	if findings := Check(Diff{Files: []File{{Path: "pkg/foo_test.go"}}}, fail); len(findings) != 0 {
		t.Fatalf("unexpected findings on fail: %+v", findings)
	}
}

func TestCheckIsPureAndDeterministic(t *testing.T) {
	f := File{Path: "pkg/foo_test.go", Status: "M", AddedLines: []string{`t.Skip("x")`}}
	diff := Diff{Files: []File{f}}
	v := Verdict{Verdict: "pass", Steps: []Step{{Name: "go test", OK: true, OutputTail: "1 passed"}}}
	first := Check(diff, v)
	second := Check(diff, v)
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Fatalf("non-deterministic findings: %+v / %+v", first, second)
	}
	if v.Verdict != "pass" || diff.Files[0].Path != f.Path {
		t.Fatal("Check mutated its inputs")
	}
}
