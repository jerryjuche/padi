package risk

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLevelFromString(t *testing.T) {
	cases := map[string]Level{
		"low": LevelLow, "HIGH": LevelHigh, " Medium ": LevelMedium,
		"unknown": LevelUnknown, "garbage": LevelUnknown, "": LevelUnknown,
	}
	for in, want := range cases {
		if got := LevelFromString(in); got != want {
			t.Errorf("LevelFromString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderEscapesHTMLAndCapsLength(t *testing.T) {
	risks := []FileRisk{
		{
			Path:  `<script>alert(1)</script>/x.go`,
			Level: LevelHigh,
			Signals: Signals{
				Commits90d: 7, LastChangedDays: 2, FixDensity: 1, ChangeSize: 0.6,
				CoChange:  []string{"a<b>.go"},
				Incidents: []IncidentRef{{ID: uuid.New(), Title: "incident", OccurredAt: time.Now()}},
			},
			Reasons: []string{"fix density 1.00 <b>across</b> 7 commits"},
		},
		{
			Path: "internal/broken.go", Level: LevelUnknown,
			Reasons: []string{"history unavailable: git log failed"},
		},
	}
	card := Render(risks, LevelHigh)
	if strings.Contains(card, "<script>") || strings.Contains(card, "<b>") {
		t.Fatalf("card contains unescaped HTML: %s", card)
	}
	if !strings.Contains(card, "&lt;script&gt;") {
		t.Fatalf("card does not escape path: %s", card)
	}
	if !strings.HasPrefix(card, "Risk: high") {
		t.Fatalf("missing risk header: %s", card)
	}
	if !strings.Contains(card, "Unknown: internal/broken.go") {
		t.Fatalf("missing unknown line: %s", card)
	}
	if !strings.Contains(card, "co-change:") || !strings.Contains(card, "incidents: 1 in 365d") {
		t.Fatalf("missing signal detail: %s", card)
	}
	if len([]rune(card)) > maxCardRunes {
		t.Fatalf("card has %d runes", len([]rune(card)))
	}
}

func TestRenderTopFiveAndUnknownAggregate(t *testing.T) {
	risks := make([]FileRisk, 9)
	for i := range risks {
		risks[i] = FileRisk{Path: "file" + string(rune('a'+i)) + ".go", Level: LevelLow}
	}
	risks[8].Level = LevelUnknown
	card := Render(risks, Aggregate(risks))
	if !strings.HasPrefix(card, "Risk: unknown") {
		t.Fatalf("aggregate not unknown: %s", card)
	}
	if !strings.Contains(card, "... 4 more files") {
		t.Fatalf("expected truncation of file list: %s", card)
	}
	if !strings.Contains(card, "Unknown: filei.go") {
		t.Fatalf("expected unknown path: %s", card)
	}
}
