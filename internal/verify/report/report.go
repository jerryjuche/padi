// Package report renders verifier verdicts and tampering findings as
// deterministic Markdown or plain text.
package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jerryjuche/padi/internal/verify/detect"
	"github.com/jerryjuche/padi/internal/verify/tampering"
)

// Aliases let callers use this package as the single reporting surface.
type (
	Verdict = detect.Verdict
	Step    = detect.Step
	Finding = tampering.Finding
)

// RenderMarkdown returns a stable Markdown report.
func RenderMarkdown(v Verdict, findings []Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Verification: %s\n\n", v.Verdict)

	steps := sortedSteps(v.Steps)
	if len(steps) == 0 {
		b.WriteString("_No verification steps recorded._\n\n")
	}
	for _, step := range steps {
		status := "FAIL"
		if step.OK {
			status = "OK"
		}
		fmt.Fprintf(&b, "### %s — %s (%d ms)\n\n", step.Name, status, step.DurationMS)
		if step.OutputTail == "" {
			b.WriteString("_No output._\n\n")
			continue
		}
		b.WriteString("```text\n")
		b.WriteString(strings.TrimRight(step.OutputTail, "\n"))
		b.WriteString("\n```\n\n")
	}

	if len(findings) == 0 {
		b.WriteString("## Findings\n\n_None._\n")
		return b.String()
	}
	b.WriteString("## Findings\n\n")
	for _, f := range sortedFindings(findings) {
		path := f.Path
		if path == "" {
			path = "-"
		}
		fmt.Fprintf(&b, "- **%s** (%s) `%s`: %s\n", f.Kind, f.Severity, path, f.Detail)
	}
	return b.String()
}

// RenderText returns a stable plain-text report.
func RenderText(v Verdict, findings []Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "verdict: %s\n", v.Verdict)
	for _, step := range sortedSteps(v.Steps) {
		status := "fail"
		if step.OK {
			status = "ok"
		}
		fmt.Fprintf(&b, "step: %s status=%s duration_ms=%d\n", step.Name, status, step.DurationMS)
		if step.OutputTail != "" {
			b.WriteString("  output_tail: ")
			b.WriteString(strings.ReplaceAll(strings.TrimRight(step.OutputTail, "\n"), "\n", "\n  "))
			b.WriteString("\n")
		}
	}
	if len(findings) == 0 {
		b.WriteString("findings: none\n")
		return b.String()
	}
	for _, f := range sortedFindings(findings) {
		fmt.Fprintf(&b, "finding: kind=%s severity=%s path=%q detail=%q\n", f.Kind, f.Severity, f.Path, f.Detail)
	}
	return b.String()
}

func sortedSteps(steps []Step) []Step {
	out := append([]Step(nil), steps...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.DurationMS != b.DurationMS {
			return a.DurationMS < b.DurationMS
		}
		return a.OutputTail < b.OutputTail
	})
	return out
}

func sortedFindings(findings []Finding) []Finding {
	out := append([]Finding(nil), findings...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
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
	return out
}
