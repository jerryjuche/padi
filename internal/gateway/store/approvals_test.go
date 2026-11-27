package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/approvals"
)

var (
	_ approvals.Store           = (*PG)(nil)
	_ approvals.ExpirationStore = (*PG)(nil)
)

func TestApprovalStoreRejectsNilPoolWithoutPanicking(t *testing.T) {
	t.Parallel()
	p := &PG{}
	ctx := context.Background()
	id := uuid.New()
	task := uuid.New()
	cases := []struct {
		name string
		call func() error
	}{
		{"create", func() error {
			_, err := p.CreateApproval(ctx, approvals.Approval{ID: id, TaskID: task, Action: approvals.ActionPushBranch, ExpiresAt: time.Now()})
			return err
		}},
		{"get", func() error { _, err := p.GetApproval(ctx, id); return err }},
		{"decide", func() error { _, err := p.DecideApproval(ctx, id, true, "telegram:1:1"); return err }},
		{"expire", func() error { return p.ExpireApproval(ctx, id, time.Now()) }},
		{"list", func() error { _, err := p.ListExpired(ctx, time.Now()); return err }},
		{"gate", func() error { _, err := p.HasApproved(ctx, task, approvals.ActionOpenPR); return err }},
		{"audit", func() error { return p.Audit(ctx, &task, "test", "detail") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Fatal("expected nil-pool error")
			}
		})
	}
}

func TestDecideViaExpiredUsesExpirePath(t *testing.T) {
	t.Parallel()
	p := &PG{}
	_, err := p.DecideApproval(context.Background(), uuid.New(), false, "expired")
	if err == nil || !strings.Contains(err.Error(), "nil pool") {
		t.Fatalf("want nil-pool error from expire path, got %v", err)
	}
}

// Callback lookups pass exactly one argument. The earlier approvalSelect+...$2
// form had two parameters and made every Telegram approval tap fail with
// "expected 2 arguments, got 1".
func TestApprovalSelectParameterCounts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{"by id", approvalSelect, 1},
		{"by short id", shortIDSelect, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Count(tc.sql, "$"); got != tc.want {
				t.Fatalf("%s has %d query parameters, want %d", tc.name, got, tc.want)
			}
		})
	}
}
