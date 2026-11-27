package telegram

import (
	"fmt"
	"html"
	"strings"
	"sync"
	timepkg "time"
)

// LiveEvent is one short entry in the underground activity trace.
type LiveEvent struct {
	At     timepkg.Time
	Kind   string // phase, tool, file, error, status
	Label  string
	Detail string
}

// LiveTrace accumulates what the gateway and the session agent are actually
// doing while a task runs. It is bounded so a long task cannot grow memory
// without limit; durable history lives in agent.events.
type LiveTrace struct {
	mu         sync.Mutex
	TaskID     string
	Repo       string
	Mode       string
	Started    timepkg.Time
	Phase      string
	PhaseLabel string
	PhaseIndex int
	Events     []LiveEvent
	Tokens     int
	PulseAt    timepkg.Time
	Retry      int
	PulseError string
}

// livePhases is the owner-facing phase track. The index is authoritative for
// done/current/pending rendering.
var livePhases = []struct{ Key, Name string }{
	{"queued", "QUEUED"},
	{"contract", "CONTRACT"},
	{"clone", "CLONE"},
	{"sandbox", "SANDBOX"},
	{"execute", "EXECUTE"},
	{"verify", "VERIFY"},
	{"publish", "PUBLISH"},
	{"done", "DONE"},
}

const liveMaxEvents = 200

func NewLiveTrace(taskID, repo, mode string) *LiveTrace {
	return &LiveTrace{
		TaskID: taskID, Repo: repo, Mode: mode,
		Started: timepkg.Now(), Phase: "queued", PhaseLabel: "Queued",
	}
}

var phaseIndex = func() map[string]int {
	m := make(map[string]int, len(livePhases))
	for i, p := range livePhases {
		m[p.Key] = i
	}
	return m
}()

// SetPhase advances the visible phase track. Repeated calls with the same key
// are harmless; unknown keys are ignored so a typo cannot blank the track.
func (l *LiveTrace) SetPhase(key, label string) {
	if l == nil {
		return
	}
	idx, ok := phaseIndex[key]
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if idx < l.PhaseIndex {
		return
	}
	l.Phase = key
	l.PhaseLabel = label
	l.PhaseIndex = idx
	l.Events = append(l.Events, LiveEvent{At: timepkg.Now(), Kind: "phase", Label: label})
	l.trim()
}

// Add records one underground event. Detail is escaped by the renderer, never
// trusted as markup.
func (l *LiveTrace) Add(kind, label, detail string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Events = append(l.Events, LiveEvent{
		At: timepkg.Now(), Kind: kind, Label: label, Detail: detail,
	})
	l.trim()
}

// AddTokens increases the running token counter without creating an event.
func (l *LiveTrace) AddTokens(n int) {
	if l == nil || n <= 0 {
		return
	}
	l.mu.Lock()
	l.Tokens += n
	l.mu.Unlock()
}

// SetPulse records a heartbeat from the session probe. It does not add an
// event, so a long silent turn cannot flood the trace with duplicate lines.
func (l *LiveTrace) SetPulse(retry int, errText string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.PulseAt = timepkg.Now()
	l.Retry = retry
	l.PulseError = liveClip(errText, 120)
	l.mu.Unlock()
}

func (l *LiveTrace) trim() {
	if len(l.Events) > liveMaxEvents {
		l.Events = append([]LiveEvent(nil), l.Events[len(l.Events)-liveMaxEvents:]...)
	}
}

// LiveSnapshot is a stable copy safe to render without holding the lock.
type LiveSnapshot struct {
	TaskID     string
	Repo       string
	Mode       string
	Started    timepkg.Time
	Phase      string
	PhaseLabel string
	PhaseIndex int
	Events     []LiveEvent
	Tokens     int
	PulseAt    timepkg.Time
	Retry      int
	PulseError string
	Total      int
}

func (l *LiveTrace) Snapshot() LiveSnapshot {
	if l == nil {
		return LiveSnapshot{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	events := append([]LiveEvent(nil), l.Events...)
	return LiveSnapshot{
		TaskID: l.TaskID, Repo: l.Repo, Mode: l.Mode, Started: l.Started,
		Phase: l.Phase, PhaseLabel: l.PhaseLabel, PhaseIndex: l.PhaseIndex,
		Events: events, Tokens: l.Tokens, PulseAt: l.PulseAt,
		Retry: l.Retry, PulseError: l.PulseError, Total: len(events),
	}
}

func liveElapsed(d timepkg.Duration) string {
	if d < 0 {
		d = 0
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}

func liveShortID(value string, n int) string {
	if value == "" {
		return "—"
	}
	if len(value) <= n {
		return value
	}
	return value[:n] + "…"
}

func liveClip(value string, n int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= n {
		return value
	}
	return value[:n-1] + "…"
}

func liveEventIcon(kind string) string {
	switch kind {
	case "phase":
		return "◆"
	case "tool":
		return "⚙"
	case "file":
		return "✎"
	case "error":
		return "⛔"
	case "status":
		return "•"
	default:
		return "·"
	}
}

// RenderLiveCard renders a compact, escaped HTML card. Only fixed markup and
// glyphs are trusted; every dynamic value goes through html.EscapeString.
func RenderLiveCard(s LiveSnapshot, draft string, tail int) string {
	esc := html.EscapeString
	if tail <= 0 {
		tail = 8
	}
	var b strings.Builder
	b.WriteString("<b>PADI / LIVE TRACE</b>")
	b.WriteString(" <i>" + esc(liveShortID(s.TaskID, 8)) + " · " + esc(s.Repo) +
		" · " + esc(strings.ToUpper(s.Mode)) + " · " + esc(liveElapsed(timepkg.Since(s.Started))) + "</i>\n\n")

	b.WriteString("▶ <b>" + esc(strings.ToUpper(s.PhaseLabel)) + "</b>\n")
	var track strings.Builder
	for i, p := range livePhases {
		if i > 0 {
			track.WriteString(" ")
		}
		mark := "○"
		if i < s.PhaseIndex {
			mark = "✓"
		} else if i == s.PhaseIndex {
			mark = "▶"
		}
		track.WriteString(mark + " " + p.Name)
	}
	b.WriteString("<code>" + esc(track.String()) + "</code>\n")
	if !s.PulseAt.IsZero() {
		pulse := "♥ alive"
		if s.Retry > 0 {
			pulse += fmt.Sprintf(" · retry %d", s.Retry)
		}
		if s.PulseError != "" {
			pulse += " · " + s.PulseError
		}
		b.WriteString("<code>" + esc(liveClip(pulse, 132)) + "</code>\n")
	}
	b.WriteString("\n")

	b.WriteString("<b>Events</b> <i>" + fmt.Sprintf("%d total", s.Total) + "</i>\n")
	start := len(s.Events) - tail
	if start < 0 {
		start = 0
	}
	if start >= len(s.Events) {
		b.WriteString("<code>waiting for activity…</code>\n")
	} else {
		b.WriteString("<code>")
		for _, ev := range s.Events[start:] {
			line := ev.At.UTC().Format("15:04:05") + " " + liveEventIcon(ev.Kind) + " " + ev.Label
			if ev.Detail != "" {
				line += ": " + ev.Detail
			}
			b.WriteString(esc(liveClip(line, 78)) + "\n")
		}
		b.WriteString("</code>")
	}

	if cleaned := liveClip(draft, 140); cleaned != "" {
		b.WriteString("\n<b>Draft</b>\n<code>" + esc(cleaned) + "</code>\n")
	}
	if s.Tokens > 0 {
		b.WriteString("<i>" + fmt.Sprintf("%d tokens", s.Tokens) + "</i>\n")
	}
	b.WriteString("<i>/options for full trace</i>")
	return b.String()
}
