package pipeline

import (
	"fmt"
	"html"
	"strings"

	"github.com/jerryjuche/padi/internal/gateway/risk"
)

// RenderCard builds the Telegram approval card. All caller-controlled strings
// are escaped; risk.Render is already escaped and bounded to 3500 runes.
func RenderCard(p Proposal) string {
	var b strings.Builder
	b.WriteString("<b>Approval required: publish</b>\n")
	fmt.Fprintf(&b, "Task: <code>%s</code>\n", html.EscapeString(p.TaskID.String()))
	fmt.Fprintf(&b, "Repo: %s\n", html.EscapeString(p.Repo))
	fmt.Fprintf(&b, "Branch: <code>%s</code> -&gt; <code>%s</code>\n",
		html.EscapeString(p.Branch), html.EscapeString(p.Base))
	fmt.Fprintf(&b, "Verdict: %s\n", html.EscapeString(p.Verdict.Verdict))
	fmt.Fprintf(&b, "Budget: %s\n", html.EscapeString(string(p.BudgetState)))
	if p.BudgetState == "warn80" {
		b.WriteString("Budget warning: 80%% of a configured limit reached.\n")
	}

	if card := risk.Render(p.Risks, p.RiskLevel); card != "" {
		b.WriteString("\n" + card + "\n")
	}

	if len(p.Findings) > 0 {
		b.WriteString("\nTest guard:\n")
		for _, f := range p.Findings {
			fmt.Fprintf(&b, "- %s (%s): %s — %s\n",
				html.EscapeString(f.Kind), html.EscapeString(f.Severity),
				html.EscapeString(f.Path), html.EscapeString(f.Detail))
		}
	}
	if len(p.Violations) > 0 {
		b.WriteString("\nContract notes:\n")
		for _, v := range p.Violations {
			fmt.Fprintf(&b, "- %s: %s — %s\n",
				html.EscapeString(v.Kind), html.EscapeString(v.Path), html.EscapeString(v.Detail))
		}
	}
	if len(p.Files) > 0 {
		b.WriteString("\nFiles:\n")
		for i, f := range p.Files {
			if i >= 5 {
				fmt.Fprintf(&b, "- and %d more\n", len(p.Files)-5)
				break
			}
			fmt.Fprintf(&b, "- %s (+%d -%d)\n", html.EscapeString(f.Path), f.Added, f.Deleted)
		}
	}
	return strings.TrimSpace(b.String())
}
