package report

import (
	"strings"
	"testing"

	"github.com/jerryjuche/padi/internal/verify/detect"
	"github.com/jerryjuche/padi/internal/verify/tampering"
)

func TestRenderIsDeterministic(t *testing.T) {
	v := detect.Verdict{
		Verdict: detect.VerdictFail,
		Steps: []detect.Step{
			{Name: "z step", OK: false, DurationMS: 20, OutputTail: "last z"},
			{Name: "a step", OK: true, DurationMS: 10, OutputTail: "first a"},
		},
	}
	findings := []tampering.Finding{
		{Kind: tampering.KindSkippedTest, Severity: tampering.SeverityHigh, Path: "z_test.go", Detail: "added t.Skip"},
		{Kind: tampering.KindDeletedTest, Severity: tampering.SeverityHigh, Path: "a_test.go", Detail: "deleted"},
	}

	firstMarkdown := RenderMarkdown(v, findings)
	firstText := RenderText(v, findings)

	reversedV := detect.Verdict{
		Verdict: v.Verdict,
		Steps:   []detect.Step{v.Steps[1], v.Steps[0]},
	}
	reversedFindings := []tampering.Finding{findings[1], findings[0]}
	if got := RenderMarkdown(reversedV, reversedFindings); got != firstMarkdown {
		t.Fatal("RenderMarkdown depends on input order")
	}
	if got := RenderText(reversedV, reversedFindings); got != firstText {
		t.Fatal("RenderText depends on input order")
	}

	for _, rendered := range []string{firstMarkdown, firstText} {
		for _, want := range []string{"fail", "a step", "z step", "deleted_test", "skipped_test", "a_test.go", "z_test.go"} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("report does not contain %q:\n%s", want, rendered)
			}
		}
	}
	if strings.Index(firstText, "a step") > strings.Index(firstText, "z step") {
		t.Fatal("text report steps are not sorted by name")
	}
	if strings.Index(firstText, "deleted_test") > strings.Index(firstText, "skipped_test") {
		t.Fatal("text report findings are not sorted by kind")
	}
}

func TestRenderEmptyFindings(t *testing.T) {
	v := detect.Verdict{Verdict: detect.VerdictPass, Steps: []detect.Step{}}
	markdown := RenderMarkdown(v, nil)
	if !strings.Contains(markdown, "## Verification: pass") || !strings.Contains(markdown, "_None._") {
		t.Fatalf("unexpected markdown report:\n%s", markdown)
	}
	text := RenderText(v, nil)
	if !strings.Contains(text, "verdict: pass") || !strings.Contains(text, "findings: none") {
		t.Fatalf("unexpected text report:\n%s", text)
	}
}
