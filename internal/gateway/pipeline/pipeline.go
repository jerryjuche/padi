// Package pipeline composes the M3b trust path: export, quarantine, verify,
// guard, contract, risk, owner approval, push and PR. Every destructive step is
// behind an interface so the whole path is testable without Docker or GitHub.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/approvals"
	"github.com/jerryjuche/padi/internal/gateway/contract"
	"github.com/jerryjuche/padi/internal/gateway/gitops"
	"github.com/jerryjuche/padi/internal/gateway/risk"
	"github.com/jerryjuche/padi/internal/verify/detect"
	"github.com/jerryjuche/padi/internal/verify/guard"
)

var (
	// ErrBlocked means the change is not eligible for an approval card.
	ErrBlocked = errors.New("pipeline: blocked")
	// ErrNotReady means an required approval or precondition is missing.
	ErrNotReady = errors.New("pipeline: not ready")
)

var branchPattern = regexp.MustCompile(`^agent/[a-z0-9._-]{1,60}$`)

// Source is the host-side bundle/quarantine/checkout path. All git work happens
// in gateway-owned directories; no agent workspace is ever a git working dir.
type Source interface {
	Export(ctx context.Context, containerID, base, branch, dest string) error
	Quarantine(ctx context.Context, repo, bundlePath, branch string) (string, error)
	Checkout(ctx context.Context, repo, branch, dest string) error
}

// Verifier runs in a fresh container against the quarantine checkout.
type Verifier interface {
	Verify(ctx context.Context, taskID, checkoutDir string) (detect.Verdict, error)
}

// Approver is the approval gate. Requests are dry; Gate enforces decisions.
type Approver interface {
	Request(ctx context.Context, taskID uuid.UUID, action approvals.Action, detail map[string]any, chatID int64, sessionID uuid.UUID) (uuid.UUID, error)
	Gate(ctx context.Context, taskID uuid.UUID, action approvals.Action) error
}

// Publisher pushes from quarantine and opens the PR after both gates pass.
type Publisher interface {
	Push(ctx context.Context, repo, branch, quarantineDir string) error
	OpenPR(ctx context.Context, repo, head, base, title, body string, draft bool) (string, error)
}

// Input is one verified publish attempt.
type Input struct {
	TaskID        uuid.UUID
	SessionID     uuid.UUID
	ChatID        int64
	Repo          string
	Branch        string
	Base          string
	BaseSHA       string
	HeadSHA       string
	BundlePath    string
	QuarantineDir string
	VerifyDir     string
	ContainerID   string
	Contract      contract.Contract
	Protection    contract.Protection
	Budget        contract.Budget
	DraftPR       bool
}

// Proposal is the approval card input. No gate has been passed at this point.
type Proposal struct {
	TaskID        uuid.UUID
	Repo          string
	Branch        string
	Base          string
	BaseSHA       string
	HeadSHA       string
	QuarantineDir string
	VerifyDir     string
	Files         []ChangedFile
	Verdict       detect.Verdict
	Findings      []guard.Finding
	Risks         []risk.FileRisk
	RiskLevel     risk.Level
	Violations    []contract.Violation
	BudgetState   contract.BudgetState
	BudgetUsed    map[string]int
	PushApproval  uuid.UUID
	PRApproval    uuid.UUID
	Draft         bool
	Card          string
	State         string
	Reason        string
	Title         string
	Body          string
}

// Result is the outcome of a gated publish.
type Result struct {
	State string
	PRURL string
}

// Service composes the trust path.
type Service struct {
	Source    Source
	Verifier  Verifier
	Approver  Approver
	Publisher Publisher
	Analyzer  *risk.Analyzer

	mu     sync.Mutex
	meters map[uuid.UUID]*contract.Meter
}

// Meter returns the per-task budget meter.
func (s *Service) Meter(taskID uuid.UUID, b contract.Budget) *contract.Meter {
	if s == nil {
		return contract.NewMeter(b)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meters == nil {
		s.meters = make(map[uuid.UUID]*contract.Meter)
	}
	m := s.meters[taskID]
	if m == nil {
		m = contract.NewMeter(b)
		s.meters[taskID] = m
	}
	return m
}

// AddUsage records token, tool-call, file or fix-loop usage for a task.
func (s *Service) AddUsage(taskID uuid.UUID, b contract.Budget, kind string, n int) contract.BudgetState {
	return s.Meter(taskID, b).Add(kind, n)
}

// Prepare runs export, quarantine, verification, guard, contract and risk,
// then requests the publish approvals. A blocked result never reaches Telegram
// as an actionable card.
func (s *Service) Prepare(ctx context.Context, in Input) (Proposal, error) {
	p := Proposal{
		TaskID: in.TaskID, Repo: in.Repo, Branch: in.Branch, Base: in.Base,
		BaseSHA: in.BaseSHA, QuarantineDir: in.QuarantineDir, VerifyDir: in.VerifyDir,
		Draft: in.DraftPR, State: "blocked",
	}
	if in.TaskID == uuid.Nil || in.SessionID == uuid.Nil || !branchPattern.MatchString(in.Branch) ||
		in.Repo == "" || in.Base == "" {
		p.Reason = "invalid task input"
		return p, fmt.Errorf("%w: %s", ErrBlocked, p.Reason)
	}
	if s == nil || s.Source == nil || s.Verifier == nil || s.Analyzer == nil {
		p.Reason = "pipeline is not configured"
		return p, fmt.Errorf("%w: %s", ErrBlocked, p.Reason)
	}
	if in.BundlePath != "" {
		if err := s.Source.Export(ctx, in.ContainerID, in.BaseSHA, in.Branch, in.BundlePath); err != nil {
			p.Reason = "bundle export failed"
			return p, fmt.Errorf("%w: %s: %w", ErrBlocked, p.Reason, err)
		}
		head, err := s.Source.Quarantine(ctx, in.Repo, in.BundlePath, in.Branch)
		if err != nil {
			p.Reason = "quarantine rejected the bundle"
			return p, fmt.Errorf("%w: %s: %w", ErrBlocked, p.Reason, err)
		}
		p.HeadSHA = head
	} else {
		p.HeadSHA = in.HeadSHA
	}
	if p.HeadSHA == "" {
		p.Reason = "missing quarantine head"
		return p, fmt.Errorf("%w: %s", ErrBlocked, p.Reason)
	}
	if in.VerifyDir != "" {
		if err := s.Source.Checkout(ctx, in.Repo, in.Branch, in.VerifyDir); err != nil {
			p.Reason = "verify checkout rejected"
			return p, fmt.Errorf("%w: %s: %w", ErrBlocked, p.Reason, err)
		}
	}

	verdict, err := s.Verifier.Verify(ctx, in.TaskID.String(), in.VerifyDir)
	if err != nil {
		p.Verdict = detect.Verdict{Verdict: detect.VerdictUnverifiable}
		p.Reason = "verification failed"
		return p, fmt.Errorf("%w: %s: %w", ErrBlocked, p.Reason, err)
	}
	p.Verdict = verdict
	if verdict.Verdict != detect.VerdictPass {
		p.Reason = "verification verdict: " + verdict.Verdict
		return p, fmt.Errorf("%w: %s", ErrBlocked, p.Reason)
	}

	raw, files, err := Diff(ctx, in.QuarantineDir, in.BaseSHA, p.HeadSHA)
	if err != nil {
		p.Reason = "quarantine diff failed"
		return p, fmt.Errorf("%w: %s: %w", ErrBlocked, p.Reason, err)
	}
	p.Files = files

	baseFiles := make([]contract.ChangedFile, 0, len(files))
	gdFiles := make([]guard.File, 0, len(files))
	riskFiles := make([]risk.File, 0, len(files))
	for _, f := range files {
		baseFiles = append(baseFiles, f.ChangedFile)
		gdFiles = append(gdFiles, guard.File{
			Path: f.Path, Status: f.Status, Added: f.Added, Deleted: f.Deleted,
			OldPath: f.OldPath, Patch: f.Patch,
		})
		riskFiles = append(riskFiles, risk.File{
			Path: f.Path, Added: f.Added, Deleted: f.Deleted, Status: f.Status,
		})
	}
	p.Findings = guard.Check(guard.Diff{Raw: raw, Files: gdFiles}, guard.Verdict(verdict))
	// An empty pass with no other high-severity signal downgrades to
	// unverifiable and is never actionable. A deleted or skipped test is
	// already high risk and stays on the card for the owner to see.
	otherHigh := false
	for _, f := range p.Findings {
		if f.Severity == guard.SeverityHigh && f.Kind != guard.KindEmptyPass {
			otherHigh = true
		}
	}
	for _, f := range p.Findings {
		if f.Kind == guard.KindEmptyPass && !otherHigh && p.Verdict.Verdict == detect.VerdictPass {
			p.Verdict.Verdict = detect.VerdictUnverifiable
			p.Reason = "verification verdict: " + p.Verdict.Verdict
			return p, fmt.Errorf("%w: %s", ErrBlocked, p.Reason)
		}
	}

	violations := in.Contract.Check(baseFiles, in.Protection)
	for _, v := range violations {
		if v.Hard {
			p.Violations = violations
			p.Reason = "contract violation: " + v.Kind + " " + v.Path
			return p, fmt.Errorf("%w: %s", ErrBlocked, p.Reason)
		}
	}
	p.Violations = violations

	risks, level, err := s.Analyzer.ForDiff(ctx, in.QuarantineDir, in.BaseSHA, riskFiles)
	if err != nil {
		p.Reason = "risk analysis failed"
		return p, fmt.Errorf("%w: %s: %w", ErrBlocked, p.Reason, err)
	}
	p.Risks = risks
	p.RiskLevel = level
	for _, f := range p.Findings {
		if f.Severity == guard.SeverityHigh && (level == risk.LevelLow || level == risk.LevelMedium || level == "") {
			p.RiskLevel = risk.LevelHigh
		}
	}

	m := s.Meter(in.TaskID, in.Budget)
	p.BudgetState = m.Add(contract.UsageFiles, len(files))
	p.BudgetUsed = m.Used()
	if p.BudgetState == contract.BudgetExceeded {
		p.Reason = "budget exceeded"
		return p, fmt.Errorf("%w: %s", ErrBlocked, p.Reason)
	}

	p.Title = "Padi: " + firstLine(in.Contract.Goal)
	if len(p.Title) > 96 {
		p.Title = p.Title[:96]
	}
	p.Body = renderBody(in, p)
	p.Card = RenderCard(p)

	if s.Approver == nil {
		p.Reason = "no approval gate"
		return p, fmt.Errorf("%w: %s", ErrBlocked, p.Reason)
	}
	pushID, err := s.Approver.Request(ctx, in.TaskID, approvals.ActionPushBranch, map[string]any{
		"repo": in.Repo, "branch": in.Branch, "head": p.HeadSHA,
	}, in.ChatID, in.SessionID)
	if err != nil {
		p.Reason = "push approval request failed"
		return p, fmt.Errorf("%w: %s: %w", ErrBlocked, p.Reason, err)
	}
	prID, err := s.Approver.Request(ctx, in.TaskID, approvals.ActionOpenPR, map[string]any{
		"repo": in.Repo, "head": in.Branch, "base": in.Base, "draft": in.DraftPR,
	}, in.ChatID, in.SessionID)
	if err != nil {
		p.Reason = "PR approval request failed"
		return p, fmt.Errorf("%w: %s: %w", ErrBlocked, p.Reason, err)
	}
	p.PushApproval = pushID
	p.PRApproval = prID
	p.State = "awaiting_approval"
	return p, nil
}

// Publish enforces both gates immediately before the first network publish.
func (s *Service) Publish(ctx context.Context, p Proposal) (Result, error) {
	if s == nil || s.Approver == nil || s.Publisher == nil {
		return Result{State: "blocked"}, fmt.Errorf("%w: publisher is not configured", ErrNotReady)
	}
	if p.State != "awaiting_approval" || p.TaskID == uuid.Nil {
		return Result{State: "blocked"}, fmt.Errorf("%w: proposal is not awaiting approval", ErrNotReady)
	}
	for _, a := range []approvals.Action{approvals.ActionPushBranch, approvals.ActionOpenPR} {
		if err := s.Approver.Gate(ctx, p.TaskID, a); err != nil {
			return Result{State: "blocked"}, fmt.Errorf("%w: %s: %w", ErrNotReady, a, err)
		}
	}
	if err := s.Publisher.Push(ctx, p.Repo, p.Branch, p.QuarantineDir); err != nil {
		return Result{State: "failed"}, err
	}
	url, err := s.Publisher.OpenPR(ctx, p.Repo, p.Branch, p.Base, p.Title, p.Body, p.Draft)
	if err != nil {
		return Result{State: "failed"}, err
	}
	return Result{State: "PR_OPENED", PRURL: url}, nil
}

// ChangedFile is extended with rename metadata for the guard.
type ChangedFile struct {
	contract.ChangedFile
	OldPath string
	Patch   string
}

// Diff reads the quarantine diff through gitops.Run. It is the only place the
// gateway derives changed files for contract, guard and risk checks.
func Diff(ctx context.Context, dir, base, head string) (string, []ChangedFile, error) {
	if dir == "" || !gitops.IsSafeDir(dir) {
		return "", nil, fmt.Errorf("pipeline: unsafe quarantine dir %q", dir)
	}
	rangeSpec := base + ".." + head
	nameOut, err := gitops.Run(ctx, dir, nil, "-C", dir, "diff", "--name-status", rangeSpec)
	if err != nil {
		return "", nil, err
	}
	numOut, err := gitops.Run(ctx, dir, nil, "-C", dir, "diff", "--numstat", rangeSpec)
	if err != nil {
		return "", nil, err
	}
	rawOut, err := gitops.Run(ctx, dir, nil, "-C", dir, "diff", "--unified=0", rangeSpec)
	if err != nil {
		return "", nil, err
	}
	counts := map[string][2]int{}
	for _, line := range strings.Split(string(numOut), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 3 {
			continue
		}
		add, errA := strconv.Atoi(fields[0])
		del, errD := strconv.Atoi(fields[1])
		if errA != nil || errD != nil {
			add, del = 0, 0
		}
		counts[fields[2]] = [2]int{add, del}
	}
	var files []ChangedFile
	for _, line := range strings.Split(string(nameOut), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 2 {
			continue
		}
		status := fields[0]
		path := fields[len(fields)-1]
		n := counts[path]
		f := ChangedFile{
			ChangedFile: contract.ChangedFile{Path: path, Added: n[0], Deleted: n[1], Status: status},
			Patch:       patchFor(string(rawOut), path),
		}
		if strings.HasPrefix(status, "R") && len(fields) >= 3 {
			f.OldPath = fields[1]
		}
		files = append(files, f)
	}
	return string(rawOut), files, nil
}

func patchFor(raw, path string) string {
	marker := "+++ b/" + path
	i := strings.Index(raw, marker)
	if i < 0 {
		return ""
	}
	rest := raw[i:]
	if next := strings.Index(rest[1:], "\ndiff --git "); next >= 0 {
		rest = rest[:next+1]
	}
	return rest
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "trusted change"
	}
	return s
}

func renderBody(in Input, p Proposal) string {
	var b strings.Builder
	b.WriteString("Goal: " + firstLine(in.Contract.Goal) + "\n\n")
	b.WriteString("Verification: " + p.Verdict.Verdict + "\n")
	b.WriteString("Risk: " + string(p.RiskLevel) + "\n")
	b.WriteString("Branch: " + in.Branch + " -> " + in.Base + "\n\n")
	b.WriteString("Files:\n")
	for _, f := range p.Files {
		fmt.Fprintf(&b, "- %s (%s +%d -%d)\n", f.Path, f.Status, f.Added, f.Deleted)
	}
	if len(p.Findings) > 0 {
		b.WriteString("\nGuard findings:\n")
		for _, f := range p.Findings {
			fmt.Fprintf(&b, "- %s: %s (%s)\n", f.Kind, f.Path, f.Severity)
		}
	}
	return b.String()
}
