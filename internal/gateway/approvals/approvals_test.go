package approvals

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// approvalTestClock is a mutex-protected fake clock so the loop and the tests
// can run concurrently under -race.
type approvalTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func newApprovalTestClock(t time.Time) *approvalTestClock {
	return &approvalTestClock{t: t}
}

func (c *approvalTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *approvalTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type approvalAudit struct {
	taskID *uuid.UUID
	action string
	detail string
}

// approvalTestStore implements Store and the optional ExpirationStore with a
// single mutex. DecideApproval is a compare-and-set: the first decision wins.
type approvalTestStore struct {
	mu        sync.Mutex
	approvals map[uuid.UUID]Approval
	audits    []approvalAudit
	approved  map[string]bool
	now       func() time.Time
}

func newApprovalTestStore(now func() time.Time) *approvalTestStore {
	if now == nil {
		now = time.Now
	}
	return &approvalTestStore{
		approvals: make(map[uuid.UUID]Approval),
		approved:  make(map[string]bool),
		now:       now,
	}
}

func (f *approvalTestStore) CreateApproval(_ context.Context, in Approval) (Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvals[in.ID] = in
	return in, nil
}

func (f *approvalTestStore) GetApproval(_ context.Context, id uuid.UUID) (Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.approvals[id]
	if !ok {
		return Approval{}, ErrNotFound
	}
	return a, nil
}

func (f *approvalTestStore) DecideApproval(_ context.Context, id uuid.UUID, approve bool, via string) (Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.approvals[id]
	if !ok {
		return Approval{}, ErrNotFound
	}
	if a.Status != StatusPending {
		return a, ErrAlreadyDecided
	}
	if !f.now().Before(a.ExpiresAt) {
		a.Status = StatusExpired
		f.approvals[id] = a
		return a, ErrExpired
	}
	switch {
	case via == "expired":
		a.Status = StatusExpired
	case approve:
		a.Status = StatusApproved
	default:
		a.Status = StatusRejected
	}
	ts := f.now()
	a.DecidedAt = &ts
	a.DecidedVia = via
	f.approvals[id] = a
	if via != "expired" && approve {
		f.approved[approvalKey(a.TaskID, a.Action)] = true
	}
	return a, nil
}

func (f *approvalTestStore) ExpireApproval(_ context.Context, id uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.approvals[id]
	if !ok {
		return ErrNotFound
	}
	a.Status = StatusExpired
	a.DecidedAt = &at
	a.DecidedVia = "system:expired"
	f.approvals[id] = a
	return nil
}

func (f *approvalTestStore) Audit(_ context.Context, taskID *uuid.UUID, action, detail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var copied *uuid.UUID
	if taskID != nil {
		v := *taskID
		copied = &v
	}
	f.audits = append(f.audits, approvalAudit{taskID: copied, action: action, detail: detail})
	return nil
}

func (f *approvalTestStore) ListExpired(_ context.Context, now time.Time) ([]Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Approval, 0)
	for _, a := range f.approvals {
		if a.Status == StatusPending && !now.Before(a.ExpiresAt) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *approvalTestStore) HasApproved(_ context.Context, taskID uuid.UUID, a Action) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.approved[approvalKey(taskID, a)], nil
}

func (f *approvalTestStore) put(a Approval) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvals[a.ID] = a
}

func (f *approvalTestStore) get(id uuid.UUID) Approval {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.approvals[id]
}

func (f *approvalTestStore) markApproved(taskID uuid.UUID, a Action) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved[approvalKey(taskID, a)] = true
}

func (f *approvalTestStore) auditLines() []approvalAudit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]approvalAudit(nil), f.audits...)
}

func approvalKey(taskID uuid.UUID, a Action) string {
	return taskID.String() + "|" + string(a)
}

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "policies.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadStrictPolicy(t *testing.T) {
	valid := `
protected_paths:
  - ".github/workflows/**"
  - "Dockerfile*"
  - ".env*"
  - "**/secrets/**"
  - "AGENTS.md"
test_patterns: ["*_test.go", "tests/**"]
sensitive_tags:
  auth: ["**/auth/**"]
actions:
  push_branch: ask
  web_research: auto_readonly
  touch_default_branch: ask
lockfile_pairs:
  foo.lock: foo.toml
`
	p, err := Load(writePolicy(t, valid))
	if err != nil {
		t.Fatalf("Load(valid): %v", err)
	}
	if got := p.Evaluate(ActionPushBranch); got != Ask {
		t.Errorf("push_branch = %q, want ask", got)
	}
	if got := p.Evaluate(ActionWebResearch); got != AutoReadonly {
		t.Errorf("web_research = %q, want auto_readonly", got)
	}
	if got := p.Evaluate(ActionTouchDefaultBranch); got != Deny {
		t.Errorf("touch_default_branch = %q, want deny", got)
	}
	if got := p.Evaluate(ActionNewDomain); got != Ask {
		t.Errorf("missing action = %q, want ask", got)
	}

	protected := []struct {
		path string
		want bool
	}{
		{".github/workflows/ci.yml", true},
		{"Dockerfile.dev", true},
		{".env.local", true},
		{"secrets/api.key", true},
		{"svc/secrets/nested/key", true},
		{"AGENTS.md", true},
		{"src/main.go", false},
		{"docs/AGENTS.md", false},
		{"cmd/Dockerfile.dev", false},
	}
	for _, tc := range protected {
		if got := p.IsProtected(tc.path); got != tc.want {
			t.Errorf("IsProtected(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}

	invalid := []struct {
		name string
		yaml string
	}{
		{"unknown action key", "actions:\n  push_branch: ask\n  rogue_action: ask\n"},
		{"unknown top-level key", "actions:\n  push_branch: ask\nunknown_field: true\n"},
		{"invalid decision", "actions:\n  push_branch: maybe\n"},
		{"second document", "actions:\n  push_branch: ask\n---\nactions: {}\n"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Load(writePolicy(t, tc.yaml))
			if err == nil {
				t.Fatalf("Load() = %+v, want error", got)
			}
			if got != nil {
				t.Errorf("Load() policy = %+v, want nil", got)
			}
		})
	}
}

func TestRequiresLockfileApproval(t *testing.T) {
	p := &Policy{LockfilePairs: map[string]string{"custom.lock": "custom.toml"}}
	cases := []struct {
		name    string
		changed []string
		want    bool
	}{
		{"go lock only", []string{"go.sum"}, true},
		{"go lock with manifest", []string{"go.mod", "go.sum"}, false},
		{"npm lock only", []string{"package-lock.json"}, true},
		{"npm lock with manifest", []string{"package-lock.json", "package.json"}, false},
		{"pnpm lock only", []string{"pnpm-lock.yaml"}, true},
		{"pnpm lock with manifest", []string{"pnpm-lock.yaml", "package.json"}, false},
		{"cargo lock with manifest", []string{"Cargo.lock", "Cargo.toml"}, false},
		{"poetry lock only", []string{"poetry.lock"}, true},
		{"custom pair only", []string{"custom.lock"}, true},
		{"custom pair with manifest", []string{"custom.lock", "custom.toml"}, false},
		{"no lockfile", []string{"main.go", "README.md"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.RequiresLockfileApproval(tc.changed); got != tc.want {
				t.Errorf("RequiresLockfileApproval(%v) = %v, want %v", tc.changed, got, tc.want)
			}
		})
	}
}

func TestRequestCreatesPendingApprovalWithoutAuditSecrets(t *testing.T) {
	base := time.Date(2026, time.November, 24, 12, 0, 0, 0, time.UTC)
	clock := newApprovalTestClock(base)
	store := newApprovalTestStore(clock.Now)
	m := &Manager{Policy: &Policy{Actions: map[Action]Decision{
		ActionPushBranch: Ask,
	}}, Store: store, OwnerID: 7, Now: clock.Now}
	taskID := uuid.New()
	sessionID := uuid.New()
	secret := "ghp_do_not_log_me"

	id, err := m.Request(context.Background(), taskID, Request{
		Action:    ActionPushBranch,
		Detail:    map[string]any{"token": secret, "branch": "agent/test"},
		ChatID:    42,
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("Request returned nil id")
	}
	a := store.get(id)
	if a.Status != StatusPending || a.ChatID != 42 || a.SessionID != sessionID {
		t.Errorf("stored approval = %+v", a)
	}
	if want := base.Add(30 * time.Minute); !a.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", a.ExpiresAt, want)
	}
	lines := store.auditLines()
	if len(lines) != 1 || lines[0].action != "approval_requested" {
		t.Fatalf("audit lines = %+v", lines)
	}
	if strings.Contains(lines[0].detail, secret) {
		t.Errorf("audit leaked secret: %q", lines[0].detail)
	}
	if strings.Contains(Render(a, "diff"), secret) {
		t.Error("render leaked secret")
	}

	if _, err := m.Request(context.Background(), taskID, Request{Action: ActionTouchDefaultBranch, ChatID: 42}); !errors.Is(err, ErrDenied) {
		t.Fatalf("Request(touch_default_branch) = %v, want ErrDenied", err)
	}
}

func TestDecide(t *testing.T) {
	base := time.Date(2026, time.November, 24, 12, 0, 0, 0, time.UTC)
	id := uuid.MustParse("00112233-4455-6677-8899-aabbccddeeff")
	taskID := uuid.New()

	cases := []struct {
		name       string
		status     string
		expiresIn  time.Duration
		via        string
		ownerID    int64
		approve    bool
		wantErr    error
		wantStatus string
	}{
		{"approve", StatusPending, 10 * time.Minute, "telegram:42:7", 7, true, nil, StatusApproved},
		{"reject", StatusPending, 10 * time.Minute, "telegram:42:7", 7, false, nil, StatusRejected},
		{"expired time", StatusPending, -time.Minute, "telegram:42:7", 7, true, ErrExpired, StatusPending},
		{"expired status", StatusExpired, 10 * time.Minute, "telegram:42:7", 7, true, ErrExpired, StatusExpired},
		{"cross chat", StatusPending, 10 * time.Minute, "telegram:99:7", 7, true, ErrCrossChat, StatusPending},
		{"unauthorized user", StatusPending, 10 * time.Minute, "telegram:42:8", 7, true, ErrUnauthorized, StatusPending},
		{"malformed via", StatusPending, 10 * time.Minute, "telegram:42", 7, true, ErrInvalidVia, StatusPending},
		{"already decided", StatusApproved, 10 * time.Minute, "telegram:42:7", 7, false, ErrAlreadyDecided, StatusApproved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newApprovalTestClock(base)
			store := newApprovalTestStore(clock.Now)
			store.put(Approval{
				ID:        id,
				TaskID:    taskID,
				Action:    ActionPushBranch,
				Status:    tc.status,
				ChatID:    42,
				ExpiresAt: base.Add(tc.expiresIn),
				CreatedAt: base,
			})
			m := &Manager{Store: store, OwnerID: tc.ownerID, Now: clock.Now}
			err := m.Decide(context.Background(), id, tc.approve, tc.via)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Decide() = %v, want %v", err, tc.wantErr)
			}
			if got := store.get(id).Status; got != tc.wantStatus {
				t.Errorf("status = %q, want %q", got, tc.wantStatus)
			}
			if tc.wantErr == nil {
				lines := store.auditLines()
				if len(lines) != 1 || lines[0].action != "approval_decided" {
					t.Errorf("audit lines = %+v", lines)
				}
			}
		})
	}
}

func TestDecideDuplicateFirstWins(t *testing.T) {
	base := time.Now().UTC()
	id := uuid.New()
	store := newApprovalTestStore(func() time.Time { return base })
	store.put(Approval{ID: id, TaskID: uuid.New(), Action: ActionOpenPR, Status: StatusPending, ChatID: 42, ExpiresAt: base.Add(time.Hour)})
	m := &Manager{Store: store, OwnerID: 7, Now: func() time.Time { return base }}
	via := "telegram:42:7"

	if err := m.Decide(context.Background(), id, true, via); err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	if err := m.Decide(context.Background(), id, false, via); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("second Decide = %v, want ErrAlreadyDecided", err)
	}
	if got := store.get(id).Status; got != StatusApproved {
		t.Errorf("status after duplicate = %q, want approved", got)
	}
}

func TestGateRequiresApproval(t *testing.T) {
	taskID := uuid.New()
	otherTask := uuid.New()
	p := &Policy{Actions: map[Action]Decision{
		ActionPushBranch:  Ask,
		ActionOpenPR:      Ask,
		ActionWebResearch: Auto,
	}}
	cases := []struct {
		name         string
		action       Action
		approvedTask uuid.UUID
		approvedAct  Action
		wantErr      error
	}{
		{"push without approval", ActionPushBranch, uuid.Nil, "", ErrNotApproved},
		{"push approved", ActionPushBranch, taskID, ActionPushBranch, nil},
		{"open_pr not bundled", ActionOpenPR, taskID, ActionPushBranch, ErrNotApproved},
		{"approval for other task", ActionPushBranch, otherTask, ActionPushBranch, ErrNotApproved},
		{"auto action", ActionWebResearch, uuid.Nil, "", nil},
		{"default branch", ActionTouchDefaultBranch, taskID, ActionTouchDefaultBranch, ErrDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newApprovalTestStore(time.Now)
			if tc.approvedTask != uuid.Nil {
				store.markApproved(tc.approvedTask, tc.approvedAct)
			}
			m := &Manager{Policy: p, Store: store}
			err := m.Gate(context.Background(), taskID, tc.action)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Gate(%s) = %v, want %v", tc.action, err, tc.wantErr)
			}
		})
	}
}

func TestExpireOnceAndLoopCancellation(t *testing.T) {
	base := time.Date(2026, time.November, 24, 12, 0, 0, 0, time.UTC)
	clock := newApprovalTestClock(base)
	store := newApprovalTestStore(clock.Now)
	id := uuid.New()
	taskID := uuid.New()
	store.put(Approval{ID: id, TaskID: taskID, Action: ActionPushBranch, Status: StatusPending, ChatID: 42, ExpiresAt: base.Add(-time.Minute)})
	m := &Manager{Policy: &Policy{}, Store: store, OwnerID: 7, Now: clock.Now}

	m.expireOnce(context.Background())
	if got := store.get(id).Status; got != StatusExpired {
		t.Errorf("status = %q, want expired", got)
	}
	lines := store.auditLines()
	if len(lines) != 1 || lines[0].action != "approval_expired" {
		t.Errorf("audit lines = %+v", lines)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.ExpireLoop(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ExpireLoop did not stop on cancellation")
	}
}

func TestTelegramHelpers(t *testing.T) {
	id := uuid.MustParse("00112233-4455-6677-8899-aabbccddeeff")
	taskID := uuid.New()

	if got := ShortID(id); got != "00112233" {
		t.Errorf("ShortID = %q, want 00112233", got)
	}
	if got := Callback(id, true); got != "a:00112233:y" {
		t.Errorf("Callback(approve) = %q", got)
	}
	if got := Callback(id, false); got != "a:00112233:n" {
		t.Errorf("Callback(reject) = %q", got)
	}

	parsed := []struct {
		data    string
		short   string
		approve bool
		ok      bool
	}{
		{"a:00112233:y", "00112233", true, true},
		{"a:00112233:n", "00112233", false, true},
		{"b:00112233:y", "", false, false},
		{"a:short:y", "", false, false},
		{"a:zzzzzzzz:y", "", false, false},
		{"a:00112233:maybe", "", false, false},
		{"a:00112233:y:extra", "", false, false},
	}
	for _, tc := range parsed {
		t.Run("parse "+tc.data, func(t *testing.T) {
			short, approve, ok := ParseCallback(tc.data)
			if short != tc.short || approve != tc.approve || ok != tc.ok {
				t.Errorf("ParseCallback(%q) = (%q,%v,%v), want (%q,%v,%v)", tc.data, short, approve, ok, tc.short, tc.approve, tc.ok)
			}
		})
	}

	rows := Keyboard(id)
	if len(rows) != 1 || len(rows[0]) != 2 {
		t.Fatalf("Keyboard = %+v, want one row of two buttons", rows)
	}
	if rows[0][0].Text != "Approve" || rows[0][0].CallbackData != Callback(id, true) {
		t.Errorf("approve button = %+v", rows[0][0])
	}
	if rows[0][1].Text != "Reject" || rows[0][1].CallbackData != Callback(id, false) {
		t.Errorf("reject button = %+v", rows[0][1])
	}

	card := Render(Approval{
		ID: id, TaskID: taskID, Action: ActionPushBranch, Status: StatusPending,
		ExpiresAt: baseTime().Add(time.Hour),
		Detail:    map[string]any{"token": "sk-should-not-appear"},
	}, `<b>diff</b>`)
	for _, want := range []string{
		string(ActionPushBranch), taskID.String(), ShortID(id),
		baseTime().Add(time.Hour).UTC().Format(time.RFC3339),
		"[risk-card:&lt;b&gt;diff&lt;/b&gt;]",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("Render missing %q: %s", want, card)
		}
	}
	if strings.Contains(card, "sk-should-not-appear") || strings.Contains(card, "<b>diff</b>") {
		t.Errorf("Render leaked unescaped content: %s", card)
	}
}

func baseTime() time.Time {
	return time.Date(2026, time.November, 24, 12, 0, 0, 0, time.UTC)
}
