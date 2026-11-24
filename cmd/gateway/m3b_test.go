package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/approvals"
	"github.com/jerryjuche/padi/internal/gateway/contract"
	"github.com/jerryjuche/padi/internal/gateway/pipeline"
	"github.com/jerryjuche/padi/internal/gateway/telegram"
	"github.com/jerryjuche/padi/internal/verify/detect"
)

type m3bFakeApprovalStore struct {
	approved map[approvals.Action]bool
}

func (s *m3bFakeApprovalStore) CreateApproval(_ context.Context, in approvals.Approval) (approvals.Approval, error) {
	return in, nil
}

func (s *m3bFakeApprovalStore) GetApproval(_ context.Context, _ uuid.UUID) (approvals.Approval, error) {
	return approvals.Approval{}, approvals.ErrNotFound
}

func (s *m3bFakeApprovalStore) DecideApproval(_ context.Context, _ uuid.UUID, _ bool, _ string) (approvals.Approval, error) {
	return approvals.Approval{}, approvals.ErrNotFound
}

func (s *m3bFakeApprovalStore) Audit(_ context.Context, _ *uuid.UUID, _, _ string) error { return nil }

func (s *m3bFakeApprovalStore) ListExpired(_ context.Context, _ time.Time) ([]approvals.Approval, error) {
	return nil, nil
}

func (s *m3bFakeApprovalStore) HasApproved(_ context.Context, _ uuid.UUID, a approvals.Action) (bool, error) {
	return s.approved[a], nil
}

func m3bTelegramStub(t *testing.T) (*telegram.Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	return &telegram.Client{BaseURL: srv.URL, Token: "test", HTTP: srv.Client()}, srv.Close
}

func TestContractOverridesAndHardViolation(t *testing.T) {
	o := contractOverrides("Please refactor the parser.\nDo not edit: config/secrets.env, deploy/key.pem\nOff-limits: migrations/*.sql\n")
	joined := strings.Join(o.OffLimits, ",")
	for _, want := range []string{"config/secrets.env", "deploy/key.pem", "migrations/*.sql"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("overrides=%q, want %q", joined, want)
		}
	}
	c := contract.Draft("refactor", contract.RepoSettings{}, contract.Budget{}, o)
	if err := contractViolationIn(c, "config/secrets.env"); !errors.Is(err, errOffLimits) {
		t.Fatalf("off-limits violation=%v, want errOffLimits", err)
	}
	if err := contractViolationIn(c, "README.md"); err != nil {
		t.Fatalf("normal file violation=%v, want nil", err)
	}
}

func TestTrackUsageSignalsBudgetAbort(t *testing.T) {
	taskID := uuid.New()
	a := &application{
		pipeline:         &pipeline.Service{},
		contractDefaults: contract.Budget{MaxToolCalls: 2},
	}
	if a.trackUsage(&taskID, "tool_call", nil) {
		t.Fatal("first tool call signalled abort")
	}
	if !a.trackUsage(&taskID, "tool_call", nil) {
		t.Fatal("second tool call did not signal abort")
	}
	if a.trackUsage(&taskID, "tool_call", nil) {
		t.Fatal("budget abort signalled twice for one task")
	}
	a.clearBudgetStop(taskID)
	if a.budgetStop[taskID] {
		t.Fatal("clearBudgetStop did not release the task")
	}
	if !a.trackUsage(&taskID, "tool_call", nil) {
		t.Fatal("cleared stop did not re-arm the abort signal")
	}
}

func TestReadVerifyVerdictRejectsNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "verdict.json")
	if err := os.WriteFile(stale, []byte(`{"verdict":"pass"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readVerifyVerdict(dir, errors.New("verify container exit 1"))
	if err == nil || got.Verdict != detect.VerdictUnverifiable {
		t.Fatalf("wait error: verdict=%q err=%v", got.Verdict, err)
	}
	got, err = readVerifyVerdict(dir, nil)
	if err != nil || got.Verdict != detect.VerdictPass {
		t.Fatalf("clean run: verdict=%q err=%v", got.Verdict, err)
	}
}

func TestMaybePublishKeepsResumeUntilBothGates(t *testing.T) {
	taskID := uuid.New()
	tg, closeServer := m3bTelegramStub(t)
	defer closeServer()
	manager := &approvals.Manager{
		Policy: &approvals.Policy{Actions: map[approvals.Action]approvals.Decision{
			approvals.ActionPushBranch: approvals.Ask,
			approvals.ActionOpenPR:     approvals.Ask,
		}},
		Store: &m3bFakeApprovalStore{approved: map[approvals.Action]bool{
			approvals.ActionPushBranch: true,
		}},
		OwnerID: 42,
	}
	a := &application{
		pipeline:  &pipeline.Service{},
		approvals: manager,
		tg:        tg,
		resumes: map[uuid.UUID]resumeTask{taskID: {
			Input: pipeline.Input{ChatID: 42},
		}},
	}
	if err := a.maybePublish(context.Background(), taskID); !errors.Is(err, errApprovalPending) {
		t.Fatalf("maybePublish error=%v, want errApprovalPending", err)
	}
	if _, ok := a.resumes[taskID]; !ok {
		t.Fatal("resume was deleted before the second approval")
	}
	if a.publishing[taskID] {
		t.Fatal("publish lock was not released")
	}
}
