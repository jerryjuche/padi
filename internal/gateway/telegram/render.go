package telegram

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/opencode"
)

// MessageRecorder persists message IDs for later edits/deletes.
type MessageRecorder interface {
	RecordTelegramMessage(ctx context.Context, sessionID, taskID *uuid.UUID, chatID, messageID int64) error
}

// Renderer converts normalised OpenCode events into Telegram HTML messages.
type Renderer struct {
	Client       *Client
	Recorder     MessageRecorder
	EditInterval time.Duration
	MaxChars     int
}

// Render is pure so event-to-line mapping is table-testable.
func Render(ev opencode.Event) string {
	switch ev.Type {
	case "tool_call":
		if ev.Tool != "" {
			return "Running " + ev.Tool + "..."
		}
		return "Running a tool..."
	case "tool_result":
		if ev.Tool != "" {
			return "Finished " + ev.Tool
		}
		return "Tool finished."
	case "file_change":
		if ev.Text != "" {
			return "Changed " + ev.Text
		}
		return "Files changed."
	case "status":
		if ev.Message != "" {
			return ev.Message
		}
		return "Working..."
	case "error":
		if ev.Message != "" {
			return "Error: " + ev.Message
		}
		return "The agent stopped with an error."
	default:
		return ""
	}
}

// Split chunks text on rune boundaries, preferring the last newline.
func Split(text string, max int) []string {
	if max <= 0 {
		max = 3800
	}
	runes := []rune(text)
	if len(runes) <= max {
		return []string{text}
	}
	var out []string
	for len(runes) > max {
		cut := max
		for i := max; i > max/2; i-- {
			if runes[i] == '\n' {
				cut = i + 1
				break
			}
		}
		out = append(out, strings.TrimRight(string(runes[:cut]), "\n"))
		runes = []rune(strings.TrimLeft(string(runes[cut:]), "\n"))
	}
	if len(runes) > 0 {
		out = append(out, string(runes))
	}
	return out
}

func (r *Renderer) interval() time.Duration {
	if r.EditInterval <= 0 {
		return 1200 * time.Millisecond
	}
	return r.EditInterval
}

func (r *Renderer) maxChars() int {
	if r.MaxChars <= 0 {
		return 3800
	}
	return r.MaxChars
}

func (r *Renderer) record(ctx context.Context, sessionID uuid.UUID, taskID *uuid.UUID, chatID, messageID int64) {
	if r.Recorder == nil || messageID == 0 {
		return
	}
	if err := r.Recorder.RecordTelegramMessage(ctx, &sessionID, taskID, chatID, messageID); err != nil {
		fmt.Printf("telegram: record message: %v\n", err)
	}
}

// Relay streams events into one edited Telegram message and returns the final
// assistant summary. A done event closes the relay; a terminal error fails it
// after showing the failure in the same message; transport errors abort.
func (r *Renderer) Relay(ctx context.Context, chatID int64, sessionID uuid.UUID, taskID *uuid.UUID, events <-chan opencode.Event) (string, error) {
	return r.RelayWithKeyboard(ctx, chatID, sessionID, taskID, events, nil)
}

// RelayWithKeyboard is Relay with an inline keyboard attached to the final
// message, so follow-up actions are one tap away.
func (r *Renderer) RelayWithKeyboard(ctx context.Context, chatID int64, sessionID uuid.UUID, taskID *uuid.UUID, events <-chan opencode.Event, rows [][]InlineButton) (string, error) {
	statusID, err := r.Client.SendMessage(ctx, chatID, html.EscapeString("Working..."))
	if err != nil {
		return "", err
	}
	r.record(ctx, sessionID, taskID, chatID, statusID)

	lastText := html.EscapeString("Working...")
	lastMarkup := ""
	edit := func(text string, rows [][]InlineButton) error {
		markup, err := keyboardJSON(rows)
		if err != nil {
			return err
		}
		if text == lastText && markup == lastMarkup {
			return nil
		}
		if err := r.Client.EditMessageTextKeyboard(ctx, chatID, statusID, text, rows); err != nil {
			return err
		}
		lastText, lastMarkup = text, markup
		return nil
	}

	var answer strings.Builder
	var status string
	var lastEdit time.Time
	for ev := range events {
		switch ev.Type {
		case "token":
			answer.WriteString(ev.Text)
		case "done":
			goto finish
		case "error":
			if ev.Terminal {
				message := Render(ev)
				if editErr := edit(html.EscapeString(message), nil); editErr != nil {
					return "", editErr
				}
				return "", fmt.Errorf("%s", message)
			}
			status = Render(ev)
		default:
			if line := Render(ev); line != "" {
				status = line
			}
		}
		if time.Since(lastEdit) < r.interval() {
			continue
		}
		text := "Working..."
		if answer.Len() > 0 {
			text = answer.String()
		} else if status != "" {
			text = status
		}
		if err := edit(html.EscapeString(truncate(text, r.maxChars())), nil); err != nil {
			return "", err
		}
		lastEdit = time.Now()
	}

finish:
	summary := cleanReply(answer.String())
	if summary == "" {
		summary = "Done."
	}
	parts := Split(summary, r.maxChars())
	if err := edit(html.EscapeString(parts[0]), rows); err != nil {
		return "", err
	}
	for _, part := range parts[1:] {
		id, err := r.Client.SendMessage(ctx, chatID, html.EscapeString(part))
		if err != nil {
			return "", err
		}
		r.record(ctx, sessionID, taskID, chatID, id)
	}
	return summary, nil
}

// toolJargon matches internal tool-parameter vocabulary that should never
// appear in a chat reply. Whole lines are dropped so surrounding prose stays.
var toolJargon = regexp.MustCompile(`(?i)oldString|newString|replaceAll|replace_all|"tool_calls"|"finish_reason"`)

func cleanReply(s string) string {
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if toolJargon.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func truncate(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
