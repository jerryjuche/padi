package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/approvals"
	"github.com/jerryjuche/padi/internal/gateway/contract"
	"github.com/jerryjuche/padi/internal/gateway/gitops"
	"github.com/jerryjuche/padi/internal/gateway/risk"
	"github.com/jerryjuche/padi/internal/verify/detect"
)

type fakeSource struct{}

func (fakeSource) Export(context.Context, string, string, string, string) error { return nil }
func (fakeSource) Quarantine(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (fakeSource) Checkout(context.Context, string, string, string) error { return nil }

type fakeVerifier struct{ verdict detect.Verdict }

func (v fakeVerifier) Verify(context.Context, string, string) (detect.Verdict, error) {
	return v.verdict, nil
}

type fakeApprover struct {
	mu       sync.Mutex
	ids      map[approvals.Action]uuid.UUID
	approved map[approvals.Action]bool
	requests int
}

func newFakeApprover() *fakeApprover {
	return &fakeApprover{
		ids:      map[approvals.Action]uuid.UUID{},
		approved: map[approvals.Action]bool{},
	}
}

func (f *fakeApprover) Request(_ context.Context, _ uuid.UUID, a approvals.Action, _ map[string]any, _ int64, _ uuid.UUID) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	id := uuid.New()
	f.ids[a] = id
	return id, nil
}

func (f *fakeApprover) Gate(_ context.Context, _ uuid.UUID, a approvals.Action) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.approved[a] {
		return approvals.ErrNotApproved
	}
	return nil
}

func (f *fakeApprover) approve(a approvals.Action) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved[a] = true
}

type fakePublisher struct {
	mu     sync.Mutex
	pushes int
	prs    int
	url    string
}

func (f *fakePublisher) Push(context.Context, string, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes++
	return nil
}

func (f *fakePublisher) OpenPR(context.Context, string, string, string, string, string, bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs++
	return f.url, nil
}

func (f *fakePublisher) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pushes, f.prs
}

func gitFixtureDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "padi-pipeline-")
	if err != nil {
		t.Fatal(err)
	}
	// Git creates read-only object files. Leave the fixture under /tmp rather
	// than letting a cleanup race with a lingering git process fail the test.
	return dir
}

func fixtureRepo(t *testing.T, files map[string]string, deletes ...string) (dir, base, head string) {
	t.Helper()
	ctx := context.Background()
	dir = gitFixtureDir(t)
	run := func(args ...string) string {
		t.Helper()
		out, err := gitops.Run(ctx, dir, nil, append([]string{"-C", dir}, args...)...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "--quiet")
	run("config", "user.name", "sandbox-agent")
	run("config", "user.email", "noreply@sandbox.local")
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "base")
	base = run("rev-parse", "HEAD")
	for _, name := range deletes {
		if name == "" {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"src/app.go": "package app\nfunc Fix() {}\nfunc Run() { Fix() }\n"} {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "fix: update app")
	head = run("rev-parse", "HEAD")
	return dir, base, head
}

func commitChange(t *testing.T, dir, name, body string) string {
	t.Helper()
	ctx := context.Background()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := gitops.Run(ctx, dir, nil, append([]string{"-C", dir}, args...)...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "change "+name)
	return run("rev-parse", "HEAD")
}

func testService(source Source, verifier Verifier, appr Approver, pub Publisher) *Service {
	return &Service{
		Source: source, Verifier: verifier, Approver: appr, Publisher: pub,
		Analyzer: risk.New(risk.Thresholds{
			StaleDays: 180, HighFixDensity: 0.4, HighCommits90d: 3,
			HighChangeSize: 0.5, MediumChangeSize: 0.2,
			IncidentMedium: 1, IncidentHigh: 2,
		}),
	}
}

func testInput(dir, base, head string) Input {
	return Input{
		TaskID: uuid.New(), SessionID: uuid.New(), ChatID: 42,
		Repo: "owner/repo", Branch: "agent/test-1234abcd", Base: "main",
		BaseSHA: base, HeadSHA: head, QuarantineDir: dir, VerifyDir: dir,
		Contract: contract.Contract{
			Goal: "fix the app", Scope: []string{"src/**", "app_test.go"},
			Budget: contract.Budget{MaxFilesChanged: 10, MaxFixLoops: 3},
		},
		Protection: contract.Protection{LockfilePairs: map[string]string{"go.sum": "go.mod"}},
		Budget:     contract.Budget{MaxFilesChanged: 10, MaxFixLoops: 3},
	}
}

func TestPrepareAndPublishGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir, base, head := fixtureRepo(t, map[string]string{
		"go.mod":      "module example.com/app\n",
		"src/app.go":  "package app\nfunc Run() {}\n",
		"app_test.go": "package app\nimport \"testing\"\nfunc TestRun(t *testing.T) { Run() }\n",
	})
	in := testInput(dir, base, head)
	appr := newFakeApprover()
	pub := &fakePublisher{url: "https://github.test/owner/repo/pull/1"}
	s := testService(fakeSource{}, fakeVerifier{detect.Verdict{
		Verdict: detect.VerdictPass,
		Steps:   []detect.Step{{Name: "go test", OK: true, DurationMS: 10}},
	}}, appr, pub)

	proposal, err := s.Prepare(ctx, in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if proposal.State != "awaiting_approval" || proposal.PushApproval == uuid.Nil || proposal.PRApproval == uuid.Nil {
		t.Fatalf("proposal=%+v", proposal)
	}
	if appr.requests != 2 {
		t.Fatalf("requests=%d want 2", appr.requests)
	}
	if !strings.Contains(proposal.Card, "Risk:") {
		t.Fatalf("risk block missing: %s", proposal.Card)
	}

	if _, err := s.Publish(ctx, proposal); !errors.Is(err, ErrNotReady) {
		t.Fatalf("publish before approval err=%v want ErrNotReady", err)
	}
	if pushes, prs := pub.counts(); pushes != 0 || prs != 0 {
		t.Fatalf("unapproved publish counts=%d/%d", pushes, prs)
	}

	appr.approve(approvals.ActionPushBranch)
	appr.approve(approvals.ActionOpenPR)
	result, err := s.Publish(ctx, proposal)
	if err != nil {
		t.Fatalf("approved Publish: %v", err)
	}
	if result.State != "PR_OPENED" || result.PRURL != pub.url {
		t.Fatalf("result=%+v", result)
	}
	if pushes, prs := pub.counts(); pushes != 1 || prs != 1 {
		t.Fatalf("publish counts=%d/%d want 1/1", pushes, prs)
	}
}

func TestPrepareBlocksUnsafeChanges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		verdict string
		mutate  func(*Input)
		files   map[string]string
		delete  string
		want    string
	}{
		{
			name: "failing verification", verdict: detect.VerdictFail,
			files: map[string]string{"go.mod": "module x\n", "src/app.go": "package app\n"},
			want:  "verification verdict",
		},
		{
			name: "off-limits edit", verdict: detect.VerdictPass,
			files:  map[string]string{"go.mod": "module x\n", "src/app.go": "package app\n"},
			mutate: func(in *Input) { in.Contract.OffLimits = []string{"src/app.go"} },
			want:   "contract violation",
		},
		{
			name: "deleted test raises high risk", verdict: detect.VerdictPass,
			files:  map[string]string{"go.mod": "module x\n", "app_test.go": "package app\n"},
			delete: "app_test.go",
			mutate: func(in *Input) { in.Contract.Scope = []string{"**"} },
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := fixtureRepo(t, tc.files, tc.delete)
			in := testInput(dir, base, head)
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			appr := newFakeApprover()
			s := testService(fakeSource{}, fakeVerifier{detect.Verdict{
				Verdict: tc.verdict,
				Steps:   []detect.Step{{Name: "go test", OK: tc.verdict == detect.VerdictPass}},
			}}, appr, &fakePublisher{})
			proposal, err := s.Prepare(context.Background(), in)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err=%v want %q", err, tc.want)
				}
				if appr.requests != 0 {
					t.Fatalf("blocked change requested %d approvals", appr.requests)
				}
				return
			}
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if proposal.RiskLevel != risk.LevelHigh {
				t.Fatalf("risk=%s want high; findings=%+v", proposal.RiskLevel, proposal.Findings)
			}
			var high bool
			for _, f := range proposal.Findings {
				if f.Kind == "deleted_test" && f.Severity == "high" {
					high = true
				}
			}
			if !high {
				t.Fatalf("deleted_test finding missing: %+v", proposal.Findings)
			}
		})
	}
}

func TestLockfileWithoutManifestIsApprovalNote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir, base, _ := fixtureRepo(t, map[string]string{
		"go.mod":     "module example.com/app\n",
		"src/app.go": "package app\n",
	})
	head := commitChange(t, dir, "go.sum", "example.com/app v1.0.0 h1:deadbeef\n")
	in := testInput(dir, base, head)
	in.Protection.ProtectedPaths = []string{"go.sum"}
	appr := newFakeApprover()
	s := testService(fakeSource{}, fakeVerifier{detect.Verdict{Verdict: detect.VerdictPass}}, appr, &fakePublisher{})
	proposal, err := s.Prepare(ctx, in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	var lockNote bool
	for _, v := range proposal.Violations {
		if v.Kind == contract.KindLockfileWithoutManifest {
			lockNote = true
		}
	}
	if !lockNote {
		t.Fatalf("lockfile note missing: %+v", proposal.Violations)
	}
	if proposal.State != "awaiting_approval" {
		t.Fatalf("state=%s", proposal.State)
	}
}

func TestAddUsageStickyBudget(t *testing.T) {
	t.Parallel()
	s := &Service{}
	id := uuid.New()
	b := contract.Budget{MaxTokens: 10, MaxToolCalls: 2}
	if got := s.AddUsage(id, b, contract.UsageTokens, 8); got != contract.BudgetWarn80 {
		t.Fatalf("got %s", got)
	}
	if got := s.AddUsage(id, b, contract.UsageToolCalls, 2); got != contract.BudgetExceeded {
		t.Fatalf("got %s", got)
	}
	if got := s.AddUsage(id, b, contract.UsageTokens, -100); got != contract.BudgetExceeded {
		t.Fatalf("exceeded state is not sticky: %s", got)
	}
}
