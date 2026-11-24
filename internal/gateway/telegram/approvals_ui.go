package telegram

import (
	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/approvals"
)

// ApprovalButtons renders one owner decision as an inline-keyboard row. The
// approval namespace is "ap:", deliberately distinct from the M1 quick-action
// namespace "a:", so a stale quick-action button can never decide an approval.
func ApprovalButtons(label string, id uuid.UUID) [][]InlineButton {
	return [][]InlineButton{{
		{Text: "Approve " + label, CallbackData: approvals.ApprovalCallback(id, true)},
		{Text: "Reject " + label, CallbackData: approvals.ApprovalCallback(id, false)},
	}}
}

// AppendApprovalButtons adds one decision row to an existing keyboard.
func AppendApprovalButtons(rows [][]InlineButton, label string, id uuid.UUID) [][]InlineButton {
	return append(rows, ApprovalButtons(label, id)...)
}
