package risk

import (
	"fmt"
	"html"
	"sort"
	"strings"
)

const maxCardRunes = 3500

// LevelFromString parses a level case-insensitively. Anything unrecognized is
// "unknown", so a bad config never silently becomes "low".
func LevelFromString(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(LevelLow):
		return LevelLow
	case string(LevelMedium):
		return LevelMedium
	case string(LevelHigh):
		return LevelHigh
	default:
		return LevelUnknown
	}
}

// Render builds a bounded, HTML-escaped Telegram approval-card block. It shows
// at most five files and never emits raw git output or file content.
func Render(risks []FileRisk, level Level) string {
	if level == "" {
		level = Aggregate(risks)
	}
	ordered := append([]FileRisk(nil), risks...)
	sort.SliceStable(ordered, func(i, j int) bool {
		li, lj := levelRank(ordered[i].Level), levelRank(ordered[j].Level)
		if li != lj {
			return li > lj
		}
		si, sj := ordered[i].Signals.ChangeSize, ordered[j].Signals.ChangeSize
		if si != sj {
			return si > sj
		}
		return ordered[i].Path < ordered[j].Path
	})

	var b strings.Builder
	fmt.Fprintf(&b, "Risk: %s (%d files)\n", escape(string(level)), len(ordered))
	shown := 0
	for _, r := range ordered {
		if shown >= 5 || runeLen(b.String()) >= maxCardRunes-160 {
			break
		}
		shown++
		fmt.Fprintf(&b, "%d. %s [%s]\n", shown, escape(r.Path), escape(string(r.Level)))
		detail := signalSummary(r)
		if detail != "" {
			b.WriteString("   " + escape(detail) + "\n")
		}
		for i, reason := range r.Reasons {
			if i >= 2 {
				break
			}
			b.WriteString("   - " + escape(clampRune(reason, 140)) + "\n")
		}
	}
	if shown < len(ordered) {
		fmt.Fprintf(&b, "... %d more files\n", len(ordered)-shown)
	}

	var unknown []string
	for _, r := range risks {
		if r.Level == LevelUnknown {
			unknown = append(unknown, escape(r.Path))
		}
	}
	if level == LevelUnknown || len(unknown) > 0 {
		if len(unknown) == 0 {
			b.WriteString("Unknown: history unavailable\n")
		} else {
			b.WriteString("Unknown: " + strings.Join(unknown, ", ") + "\n")
		}
	}
	return clampRune(strings.TrimRight(b.String(), "\n"), maxCardRunes)
}

// Aggregate returns max(low < medium < high), with unknown winning unless a
// high exists. It matches the rule used by Analyzer.ForDiff.
func Aggregate(risks []FileRisk) Level {
	level := LevelLow
	hasUnknown, hasHigh := false, false
	for _, r := range risks {
		switch r.Level {
		case LevelHigh:
			hasHigh = true
		case LevelUnknown:
			hasUnknown = true
		case LevelMedium:
			level = LevelMedium
		}
	}
	if hasHigh {
		return LevelHigh
	}
	if hasUnknown {
		return LevelUnknown
	}
	return level
}

func levelRank(l Level) int {
	switch l {
	case LevelHigh:
		return 3
	case LevelMedium:
		return 2
	case LevelUnknown:
		return 1
	default:
		return 0
	}
}

func signalSummary(r FileRisk) string {
	var parts []string
	if r.Signals.Commits90d > 0 || r.Signals.LastChangedDays > 0 {
		parts = append(parts, fmt.Sprintf("%d commits in 90d, last change %dd ago", r.Signals.Commits90d, r.Signals.LastChangedDays))
	}
	if r.Signals.FixDensity > 0 {
		parts = append(parts, fmt.Sprintf("fix density %.2f", r.Signals.FixDensity))
	}
	if r.Signals.ChangeSize > 0 {
		parts = append(parts, fmt.Sprintf("churn %.0f%%", r.Signals.ChangeSize*100))
	}
	if len(r.Signals.CoChange) > 0 {
		parts = append(parts, "co-change: "+strings.Join(r.Signals.CoChange, ", "))
	}
	if len(r.Signals.Incidents) > 0 {
		parts = append(parts, fmt.Sprintf("incidents: %d in 365d", len(r.Signals.Incidents)))
	}
	return strings.Join(parts, "; ")
}

func escape(s string) string { return html.EscapeString(s) }

func runeLen(s string) int { return len([]rune(s)) }
