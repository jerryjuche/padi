package approvals

import (
	"encoding/hex"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Button is a deliberately small local copy of the Telegram inline-button
// shape. It avoids importing internal/gateway/telegram, which would introduce
// an import cycle when the bot wires approvals into its callback handler.
type Button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// ShortID is the first eight hex characters of a UUID without dashes. It is
// short enough for Telegram's 64-byte callback_data limit.
func ShortID(id uuid.UUID) string {
	return hex.EncodeToString(id[:])[:8]
}

// Callback builds the callback_data for an approve/reject button.
func Callback(id uuid.UUID, approve bool) string {
	vote := "n"
	if approve {
		vote = "y"
	}
	return "a:" + ShortID(id) + ":" + vote
}

// ParseCallback strictly parses callback_data produced by Callback.
func ParseCallback(data string) (shortID string, approve bool, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "a" || len(parts[1]) != 8 {
		return "", false, false
	}
	raw, err := hex.DecodeString(parts[1])
	if err != nil || len(raw) != 4 {
		return "", false, false
	}
	switch parts[2] {
	case "y":
		return strings.ToLower(parts[1]), true, true
	case "n":
		return strings.ToLower(parts[1]), false, true
	default:
		return "", false, false
	}
}

// ApprovalCallback is the M3b callback namespace. The legacy `a:` prefix is
// used by M1 quick actions, so approval buttons must not collide with it.
func ApprovalCallback(id uuid.UUID, approve bool) string {
	vote := "n"
	if approve {
		vote = "y"
	}
	return "ap:" + ShortID(id) + ":" + vote
}

// ParseApprovalCallback strictly parses data produced by ApprovalCallback.
func ParseApprovalCallback(data string) (shortID string, approve bool, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "ap" || len(parts[1]) != 8 {
		return "", false, false
	}
	raw, err := hex.DecodeString(parts[1])
	if err != nil || len(raw) != 4 {
		return "", false, false
	}
	switch parts[2] {
	case "y":
		return strings.ToLower(parts[1]), true, true
	case "n":
		return strings.ToLower(parts[1]), false, true
	default:
		return "", false, false
	}
}

// Keyboard returns the two approval buttons in one row.
// Deprecated: use ApprovalKeyboard in M3b wiring to avoid the M1 `a:` namespace.
func Keyboard(id uuid.UUID) [][]Button {
	return [][]Button{{
		{Text: "Approve", CallbackData: Callback(id, true)},
		{Text: "Reject", CallbackData: Callback(id, false)},
	}}
}

// ApprovalKeyboard returns owner buttons scoped to the approval namespace.
func ApprovalKeyboard(id uuid.UUID) [][]Button {
	return [][]Button{{
		{Text: "Approve", CallbackData: ApprovalCallback(id, true)},
		{Text: "Reject", CallbackData: ApprovalCallback(id, false)},
	}}
}

// Render formats an approval card as Telegram HTML. Approval.Detail is never
// rendered: it can contain credentials and other secrets. cardKind, when set,
// inserts a placeholder marker where the risk card will be attached.
func Render(a Approval, cardKind string) string {
	status := a.Status
	if status == "" {
		status = StatusPending
	}
	var b strings.Builder
	b.WriteString("Approval required: <b>" + html.EscapeString(string(a.Action)) + "</b>\n")
	b.WriteString("Status: " + html.EscapeString(status) + "\n")
	b.WriteString("Task: <code>" + html.EscapeString(a.TaskID.String()) + "</code>\n")
	b.WriteString("Approval: <code>" + html.EscapeString(ShortID(a.ID)) + "</code>\n")
	b.WriteString("Expires: <code>" + html.EscapeString(a.ExpiresAt.UTC().Format(time.RFC3339)) + "</code>\n")
	if cardKind != "" {
		b.WriteString(fmt.Sprintf("[risk-card:%s]\n", html.EscapeString(cardKind)))
	}
	return strings.TrimSpace(b.String())
}
