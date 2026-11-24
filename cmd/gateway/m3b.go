package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/approvals"
	"github.com/jerryjuche/padi/internal/gateway/contract"
	"github.com/jerryjuche/padi/internal/gateway/gitops"
	"github.com/jerryjuche/padi/internal/gateway/pipeline"
	"github.com/jerryjuche/padi/internal/gateway/risk"
	"github.com/jerryjuche/padi/internal/gateway/runner"
	"github.com/jerryjuche/padi/internal/gateway/store"
	"github.com/jerryjuche/padi/internal/gateway/telegram"
	"github.com/jerryjuche/padi/internal/verify/detect"
)

var (
	errBudgetExceeded  = errors.New("budget_exceeded")
	errOffLimits       = errors.New("off_limits")
	errApprovalPending = errors.New("approval_pending")
)

// resumeTask is the durable-enough context needed to finish a publish after an
// owner taps an inline button. It lives in memory only for the lifetime of the
// gateway; the approval decisions and approval card are persisted separately.
type resumeTask struct {
	SessionID uuid.UUID
	Repo      string
	Branch    string
	Base      string
	Input     pipeline.Input
	Proposal  pipeline.Proposal
}

// sourceAdapter keeps every host git call in gitops, against gateway-owned
// bundle, quarantine and verify paths. No agent workspace is a git working dir.
type sourceAdapter struct {
	git    *gitops.Git
	runner *runner.Runner
}

func (s sourceAdapter) Export(ctx context.Context, containerID, base, branch, dest string) error {
	if s.git == nil || s.runner == nil {
		return errors.New("gateway: source is not configured")
	}
	return s.git.ExportBundle(ctx, s.runner, containerID, base, branch, dest)
}

func (s sourceAdapter) Quarantine(ctx context.Context, repo, bundlePath, branch string) (string, error) {
	if s.git == nil {
		return "", errors.New("gateway: source is not configured")
	}
	return s.git.Quarantine(ctx, repo, bundlePath, branch)
}

func (s sourceAdapter) Checkout(ctx context.Context, repo, branch, dest string) error {
	if s.git == nil {
		return errors.New("gateway: source is not configured")
	}
	return s.git.CheckoutForVerify(ctx, repo, branch, dest)
}

// verifyAdapter runs agent-verify in a fresh short-lived container and reads
// the JSON verdict from a gateway-owned bind mount. A failing verdict is a
// valid result: the pipeline decides whether it blocks publication.
type verifyAdapter struct {
	runner    *runner.Runner
	outputDir string
}

func (v verifyAdapter) Verify(ctx context.Context, taskID, checkoutDir string) (detect.Verdict, error) {
	unverifiable := detect.Verdict{Verdict: detect.VerdictUnverifiable}
	if v.runner == nil {
		return unverifiable, errors.New("gateway: verifier is not configured")
	}
	if v.outputDir == "" || !filepath.IsAbs(v.outputDir) {
		return unverifiable, errors.New("gateway: verify output directory must be absolute")
	}
	outDir := filepath.Join(v.outputDir, taskID)
	// A reused task ID must never inherit a verdict written by an earlier run.
	if err := os.RemoveAll(outDir); err != nil {
		return unverifiable, fmt.Errorf("clear verify output: %w", err)
	}
	handle, err := v.runner.StartVerifyWithOutput(ctx, taskID, checkoutDir, outDir)
	if err != nil {
		return unverifiable, err
	}
	defer func() {
		_ = v.runner.Stop(context.Background(), handle.ID)
	}()
	_, waitErr := v.runner.Wait(ctx, handle.ID)
	return readVerifyVerdict(outDir, waitErr)
}

// readVerifyVerdict treats any non-zero verifier exit as unverifiable, even if
// a verdict file exists from a prior step or a partially completed run.
func readVerifyVerdict(outDir string, waitErr error) (detect.Verdict, error) {
	unverifiable := detect.Verdict{Verdict: detect.VerdictUnverifiable}
	if waitErr != nil {
		return unverifiable, fmt.Errorf("verify wait: %w", waitErr)
	}
	raw, err := os.ReadFile(filepath.Join(outDir, "verdict.json"))
	if err != nil {
		return unverifiable, fmt.Errorf("verify verdict: %w", err)
	}
	var verdict detect.Verdict
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return unverifiable, fmt.Errorf("decode verify verdict: %w", err)
	}
	if verdict.Verdict == "" {
		verdict.Verdict = detect.VerdictUnverifiable
	}
	return verdict, nil
}

// approvalAdapter maps the pipeline's small Approver seam onto the strict gate.
type approvalAdapter struct{ manager *approvals.Manager }

func (a approvalAdapter) Request(ctx context.Context, taskID uuid.UUID, action approvals.Action, detail map[string]any, chatID int64, sessionID uuid.UUID) (uuid.UUID, error) {
	if a.manager == nil {
		return uuid.Nil, errors.New("gateway: approval gate is not configured")
	}
	return a.manager.Request(ctx, taskID, approvals.Request{
		Action: action, Detail: detail, ChatID: chatID, SessionID: sessionID,
	})
}

func (a approvalAdapter) Gate(ctx context.Context, taskID uuid.UUID, action approvals.Action) error {
	if a.manager == nil {
		return errors.New("gateway: approval gate is not configured")
	}
	return a.manager.Gate(ctx, taskID, action)
}

// publisherAdapter keeps the branch and PR allowlists inside gitops.
type publisherAdapter struct{ git *gitops.Git }

func (p publisherAdapter) Push(ctx context.Context, repo, branch, quarantineDir string) error {
	if p.git == nil {
		return errors.New("gateway: publisher is not configured")
	}
	return p.git.PushAgentBranch(ctx, repo, branch, quarantineDir)
}

func (p publisherAdapter) OpenPR(ctx context.Context, repo, head, base, title, body string, draft bool) (string, error) {
	if p.git == nil {
		return "", errors.New("gateway: publisher is not configured")
	}
	return p.git.OpenPRDraft(ctx, repo, head, base, title, body, draft)
}

func newTrustPipeline(cfg Config, manager *approvals.Manager, _ *store.PG, r *runner.Runner, git *gitops.Git, analyzer *risk.Analyzer) *pipeline.Service {
	outputDir := filepath.Join(cfg.Git.VerifyDir, "output")
	if !filepath.IsAbs(outputDir) {
		outputDir = filepath.Join("/srv/agent/verify", "output")
	}
	return &pipeline.Service{
		Source:    sourceAdapter{git: git, runner: r},
		Verifier:  verifyAdapter{runner: r, outputDir: outputDir},
		Approver:  approvalAdapter{manager: manager},
		Publisher: publisherAdapter{git: git},
		Analyzer:  analyzer,
	}
}

func protectionFromPolicy(p *approvals.Policy) contract.Protection {
	if p == nil {
		return contract.Protection{}
	}
	tags := make([]string, 0, len(p.SensitiveTags))
	for tag, patterns := range p.SensitiveTags {
		tags = append(tags, tag)
		tags = append(tags, patterns...)
	}
	return contract.Protection{
		ProtectedPaths: append([]string(nil), p.ProtectedPaths...),
		SensitiveTags:  tags,
		TestPatterns:   append([]string(nil), p.TestPatterns...),
		LockfilePairs:  p.LockfilePairs,
	}
}

func contractDefaultsFromConfig(cfg Config) contract.Budget {
	return contract.Budget{
		WallMinutes:     cfg.ContractDefaults.WallMinutes,
		MaxTokens:       cfg.ContractDefaults.MaxTokens,
		MaxToolCalls:    cfg.ContractDefaults.MaxToolCalls,
		MaxFilesChanged: cfg.ContractDefaults.MaxFilesChanged,
		MaxFixLoops:     cfg.ContractDefaults.MaxFixLoops,
	}
}

// trackUsage feeds the contract meter from gateway events. It never interprets
// raw provider usage fields and never records text in the database.
func (a *application) trackUsage(taskID *uuid.UUID, typ string, data any) bool {
	if a == nil || taskID == nil || *taskID == uuid.Nil || a.pipeline == nil {
		return false
	}
	kind, n := "", 0
	switch typ {
	case "token", "text":
		kind = contract.UsageTokens
		n = (len(eventText(data)) + 3) / 4
	case "tool_call":
		kind, n = contract.UsageToolCalls, 1
	default:
		return false
	}
	if n <= 0 {
		return false
	}
	a.mu.Lock()
	if a.budgets == nil {
		a.budgets = make(map[uuid.UUID]contract.Budget)
	}
	budget := a.budgets[*taskID]
	if budget == (contract.Budget{}) {
		budget = a.contractDefaults
		a.budgets[*taskID] = budget
	}
	a.mu.Unlock()

	m := a.pipeline.Meter(*taskID, budget)
	state := m.Add(kind, n)
	if state != contract.BudgetWarn80 && state != contract.BudgetExceeded {
		return false
	}
	exceeded := false
	if state == contract.BudgetExceeded {
		a.mu.Lock()
		if a.budgetStop == nil {
			a.budgetStop = make(map[uuid.UUID]bool)
		}
		if !a.budgetStop[*taskID] {
			a.budgetStop[*taskID] = true
			exceeded = true
		}
		a.mu.Unlock()
	}
	log.Printf("gateway: task %s budget %s: %s +%d", *taskID, string(state), kind, n)
	if a.db != nil {
		_ = a.db.SaveBudgetUsed(context.Background(), *taskID, m.Used())
	}
	return exceeded
}

func (a *application) clearBudgetStop(taskID uuid.UUID) {
	if a == nil {
		return
	}
	a.mu.Lock()
	delete(a.budgetStop, taskID)
	a.mu.Unlock()
}

// contractOverrides reads the owner's explicit off-limits instruction from the
// message. It never infers limits from prose outside these exact prefixes.
func contractOverrides(prompt string) contract.Overrides {
	var o contract.Overrides
	for _, raw := range strings.Split(prompt, "\n") {
		line := strings.TrimSpace(raw)
		lower := strings.ToLower(line)
		var rest string
		for _, prefix := range []string{"off-limits:", "off_limits:", "do not edit:", "don't edit:"} {
			if strings.HasPrefix(lower, prefix) {
				rest = strings.TrimSpace(line[len(prefix):])
				break
			}
		}
		if rest == "" {
			continue
		}
		fields := strings.FieldsFunc(rest, func(r rune) bool { return r == ',' || r == ';' })
		if strings.HasPrefix(lower, "do not edit:") || strings.HasPrefix(lower, "don't edit:") {
			fields = strings.Fields(rest)
		}
		for _, part := range fields {
			p := strings.Trim(strings.TrimSpace(part), "`\"',;")
			if p != "" {
				o.OffLimits = append(o.OffLimits, p)
			}
		}
	}
	return o
}

// contractViolation checks one changed path against the approved contract.
// Owner off-limits hits are hard stops; protected paths stay on the approval
// card instead of aborting the agent.
func (a *application) contractViolation(ctx context.Context, taskID uuid.UUID, changedPath string) error {
	if a == nil || a.db == nil || strings.TrimSpace(changedPath) == "" {
		return nil
	}
	c, err := a.db.LoadTaskContract(ctx, taskID)
	if err != nil {
		return nil
	}
	return contractViolationIn(c, changedPath)
}

func contractViolationIn(c contract.Contract, changedPath string) error {
	for _, v := range c.CheckScope([]contract.ChangedFile{{Path: changedPath}}) {
		if v.Hard {
			return fmt.Errorf("%w: %s matches %s", errOffLimits, v.Path, v.Detail)
		}
	}
	return nil
}

func eventText(data any) string {
	switch v := data.(type) {
	case map[string]string:
		return v["text"]
	case map[string]any:
		if s, ok := v["text"].(string); ok {
			return s
		}
	}
	return ""
}

// checkpoint records one fixed-script snapshot and its durable ref. Numbers are
// allocated under the application mutex so concurrent ticks cannot collide.
func (a *application) checkpoint(ctx context.Context, taskID, sessionID uuid.UUID, containerID string) (int, error) {
	if a == nil || a.runner == nil || containerID == "" {
		return 0, errors.New("gateway: no live container for checkpoint")
	}
	a.mu.Lock()
	if a.checkpoints == nil {
		a.checkpoints = make(map[uuid.UUID]int)
	}
	n := a.checkpoints[taskID]
	a.checkpoints[taskID] = n + 1
	a.mu.Unlock()

	ref, err := contract.Checkpoint(ctx, a.runner, containerID, n)
	if err != nil {
		return n, err
	}
	if a.db != nil {
		if _, err := a.db.SaveCheckpoint(ctx, store.Checkpoint{
			TaskID: taskID, SessionID: sessionID, N: n, Ref: ref, Tree: ref,
		}); err != nil {
			return n, err
		}
	}
	return n, nil
}

// checkpointLoop takes scheduled snapshots until its cancel function is called.
func (a *application) checkpointLoop(ctx context.Context, taskID, sessionID uuid.UUID, containerID string) func() {
	if a == nil || a.checkpointEvery <= 0 || containerID == "" {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(a.checkpointEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := a.checkpoint(ctx, taskID, sessionID, containerID); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("gateway: checkpoint task %s: %s", taskID, scrubForUser(err))
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// hasWorkspaceChanges asks the container's own git for a porcelain status. It
// runs only inside the session container; the host never runs git in a workspace.
func (a *application) hasWorkspaceChanges(ctx context.Context, containerID string) bool {
	if a == nil || a.runner == nil || containerID == "" {
		return false
	}
	out, err := a.runner.Exec(ctx, containerID,
		"git", "-c", "safe.directory=/workspace", "-C", "/workspace", "status", "--porcelain")
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// publishTask runs the M3b trust path. A blocked or unverifiable result never
// reaches a button: the task goes to FAILED and nothing is pushed.
func (a *application) publishTask(ctx context.Context, chat *chatSession, session store.Session, task store.Task, chatID int64, prompt string) error {
	if a == nil || a.pipeline == nil || a.db == nil || chat == nil {
		return errors.New("gateway: publish path is not configured")
	}
	repo, _, _, _, _ := chat.detail()
	branch, err := a.dbTaskBranch(ctx, task, repo)
	if err != nil {
		return err
	}
	base, err := a.git.BaseSHA(ctx, repo)
	if err != nil {
		return fmt.Errorf("base sha: %w", err)
	}
	// Use the contract the owner already approved. Re-drafting here could
	// silently widen scope after the approval card was accepted.
	c, err := a.db.LoadTaskContract(ctx, task.ID)
	if errors.Is(err, store.ErrNotFound) {
		c = contract.Draft(prompt, contract.RepoSettings{}, a.budgetFor(task.ID), contract.Overrides{})
		if err := a.db.SaveTaskContract(ctx, task.ID, c); err != nil {
			return fmt.Errorf("save contract: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("load contract: %w", err)
	}
	budget := c.Budget
	if err := a.db.SetTaskBranch(ctx, task.ID, branch); err != nil {
		return fmt.Errorf("record branch: %w", err)
	}

	if err := a.db.Transition(ctx, task.ID, store.StateExecuting, store.StateVerifying, ""); err != nil {
		return err
	}
	bundlePath := filepath.Join(a.cfg.Git.QuarantineDir, task.ID.String()+".bundle")
	if err := os.MkdirAll(a.cfg.Git.QuarantineDir, 0o700); err != nil {
		return err
	}
	input := pipeline.Input{
		TaskID: task.ID, SessionID: session.ID, ChatID: chatID,
		Repo: repo, Branch: branch, Base: "main", BaseSHA: base,
		BundlePath:  bundlePath,
		VerifyDir:   filepath.Join(a.cfg.Git.VerifyDir, task.ID.String()),
		ContainerID: chat.handle.ID,
		Contract:    c, Protection: a.protection, Budget: budget,
		DraftPR: a.cfg.GitHub.DefaultDraft,
	}
	proposal, err := a.pipeline.Prepare(ctx, input)
	if err != nil {
		a.blockPublish(ctx, session, task, chatID, proposal, err)
		return nil
	}
	if err := a.db.SaveBudgetUsed(ctx, task.ID, proposal.BudgetUsed); err != nil {
		log.Printf("gateway: save budget task %s: %s", task.ID, scrubForUser(err))
	}
	if err := a.db.Transition(ctx, task.ID, store.StateVerifying, store.StateAwaitingPushApproval, ""); err != nil {
		return err
	}
	a.mu.Lock()
	if a.resumes == nil {
		a.resumes = make(map[uuid.UUID]resumeTask)
	}
	a.resumes[task.ID] = resumeTask{SessionID: session.ID, Repo: repo, Branch: branch, Base: input.Base, Input: input, Proposal: proposal}
	a.mu.Unlock()

	rows := [][]telegram.InlineButton{}
	rows = telegram.AppendApprovalButtons(rows, "push", proposal.PushApproval)
	rows = telegram.AppendApprovalButtons(rows, "PR", proposal.PRApproval)
	if _, err := a.tg.SendMessageKeyboard(ctx, chatID, proposal.Card, rows); err != nil {
		return fmt.Errorf("send approval card: %w", err)
	}
	a.event(ctx, session.ID, &task.ID, "approval_card", map[string]string{
		"verdict": proposal.Verdict.Verdict, "risk": string(proposal.RiskLevel),
	})
	return nil
}

func (a *application) dbTaskBranch(ctx context.Context, task store.Task, repo string) (string, error) {
	if task.Branch != "" {
		return task.Branch, nil
	}
	branch := gitops.BranchName(repo, task.ID.String())
	if err := a.gitRefuseBranch(branch); err != nil {
		return "", err
	}
	return branch, nil
}

func (a *application) gitRefuseBranch(branch string) error {
	if branch == "main" || branch == "master" || !strings.HasPrefix(branch, "agent/") {
		return fmt.Errorf("%w: %q", gitops.ErrForbiddenBranch, branch)
	}
	return nil
}

func (a *application) budgetFor(taskID uuid.UUID) contract.Budget {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b := a.budgets[taskID]; b != (contract.Budget{}) {
		return b
	}
	return a.contractDefaults
}

func (a *application) blockPublish(ctx context.Context, session store.Session, task store.Task, chatID int64, proposal pipeline.Proposal, cause error) {
	reason := strings.TrimSpace(scrubForUser(cause))
	if i := strings.Index(reason, ": "); i >= 0 {
		reason = strings.TrimSpace(reason[i+2:])
	}
	if reason == "" {
		reason = "verification blocked publication"
	}
	if current, err := a.db.GetTask(ctx, task.ID); err == nil {
		if current.State == store.StateVerifying {
			_ = a.db.Transition(ctx, task.ID, store.StateVerifying, store.StateFailed, reason)
		}
	}
	a.event(ctx, session.ID, &task.ID, "publish_blocked", map[string]string{"reason": reason, "card": proposal.Card})
	message := "Verification blocked publication: " + reason + ". Nothing was pushed."
	if len(message) > 600 {
		message = message[:600]
	}
	if err := a.replyKeyboard(ctx, chatID, message, a.buttonsForChat(chatID)); err != nil {
		log.Printf("gateway: send blocked card: %s", scrubForUser(err))
	}
}

// contractGate asks the owner to accept the task contract before the agent
// starts. A rejection or timeout never starts the prompt.
func (a *application) contractGate(ctx context.Context, session store.Session, task store.Task, chatID int64, c contract.Contract) (bool, error) {
	if a == nil || a.approvals == nil || a.policy == nil || a.policy.Evaluate(approvals.ActionTaskContract) != approvals.Ask {
		return true, nil
	}
	if a.db == nil {
		return false, errors.New("gateway: store is not configured for approvals")
	}
	id, err := a.approvals.Request(ctx, task.ID, approvals.Request{
		Action: approvals.ActionTaskContract, ChatID: chatID, SessionID: session.ID,
		Detail: map[string]any{"goal": c.Goal, "budget": c.Budget.WallMinutes},
	})
	if err != nil {
		return false, err
	}
	// The owner sees AWAITING_PLAN_APPROVAL from the moment the card is sent.
	if err := a.db.Transition(ctx, task.ID, store.StatePreparing, store.StateAwaitingPlanApproval, ""); err != nil {
		return false, err
	}
	advance := func(to store.State, reason string) {
		if err := a.db.Transition(context.Background(), task.ID, store.StateAwaitingPlanApproval, to, reason); err != nil {
			log.Printf("gateway: contract state task %s: %s", task.ID, scrubForUser(err))
		}
	}
	ch := make(chan bool, 1)
	a.mu.Lock()
	if a.waits == nil {
		a.waits = make(map[uuid.UUID]chan bool)
	}
	a.waits[task.ID] = ch
	a.mu.Unlock()
	defer a.clearWait(task.ID)

	text := "Task contract: " + c.Goal
	if len(text) > 500 {
		text = text[:500]
	}
	rows := telegram.ApprovalButtons("contract", id)
	if err := a.replyKeyboard(ctx, chatID, text, rows); err != nil {
		advance(store.StateCancelled, "contract_card_send")
		return false, err
	}
	ttl, err := a.cfg.ApprovalTTL()
	if err != nil {
		ttl = 30 * time.Minute
	}
	select {
	case ok := <-ch:
		if !ok {
			advance(store.StateCancelled, "contract_rejected")
		}
		return ok, nil
	case <-time.After(ttl):
		advance(store.StateTimedOut, "approval_expired")
		return false, a.reply(ctx, chatID, "The contract approval expired. Nothing was started.")
	case <-ctx.Done():
		advance(store.StateCancelled, "contract_wait_cancelled")
		return false, ctx.Err()
	}
}

func (a *application) clearWait(taskID uuid.UUID) {
	a.mu.Lock()
	delete(a.waits, taskID)
	a.mu.Unlock()
}

func (a *application) signalWait(taskID uuid.UUID, ok bool) {
	a.mu.Lock()
	ch := a.waits[taskID]
	delete(a.waits, taskID)
	a.mu.Unlock()
	if ch != nil {
		select {
		case ch <- ok:
		default:
		}
	}
}

// handleApprovalCallback resolves an ap:<short>:<y|n> tap, records exactly one
// decision through the gate, then resumes either the contract wait or publish.
func (a *application) handleApprovalCallback(ctx context.Context, chatID int64, q *telegram.CallbackQuery, data string) error {
	shortID, approve, ok := approvals.ParseApprovalCallback(data)
	if !ok {
		return &telegram.ReportedError{Err: fmt.Errorf("malformed approval callback %q", data)}
	}
	if a.approvals == nil {
		return &telegram.ReportedError{Err: errors.New("approval gate is not configured")}
	}
	if q.From.ID != a.approvals.OwnerID {
		return a.editApprovalCard(ctx, chatID, q, "Only the owner can decide approvals.")
	}
	if !a.markAction(data) {
		return a.editApprovalCard(ctx, chatID, q, "That decision was already recorded.")
	}
	approval, err := a.approvals.ResolveShortID(ctx, shortID)
	if err != nil {
		return a.reportError(ctx, chatID, fmt.Errorf("resolve approval %s: %w", shortID, err))
	}
	via := fmt.Sprintf("telegram:%d:%d", chatID, q.From.ID)
	err = a.approvals.Decide(ctx, approval.ID, approve, via)
	decided := approve
	switch {
	case errors.Is(err, approvals.ErrAlreadyDecided):
		current, lookupErr := a.approvals.ResolveShortID(ctx, shortID)
		if lookupErr == nil {
			decided = current.Status == approvals.StatusApproved
		}
	case errors.Is(err, approvals.ErrExpired):
		if approval.Action != approvals.ActionTaskContract {
			a.dropProposal(ctx, approval.TaskID, "approval_expired")
		}
		return a.editApprovalCard(ctx, chatID, q, "Approval expired. Nothing was changed or pushed.")
	case err != nil:
		return a.reportError(ctx, chatID, err)
	}
	if a.db != nil {
		current, err := a.db.GetTask(ctx, approval.TaskID)
		want := store.StateAwaitingPushApproval
		if approval.Action == approvals.ActionTaskContract {
			want = store.StateAwaitingPlanApproval
		}
		if err != nil || current.State != want {
			return a.editApprovalCard(ctx, chatID, q, "That decision is no longer active.")
		}
	}
	word := "Rejected"
	if decided {
		word = "Approved"
	}
	if approval.Action == approvals.ActionTaskContract {
		a.signalWait(approval.TaskID, decided)
		return a.editApprovalCard(ctx, chatID, q, word+" task contract.")
	}
	if !decided {
		a.cancelProposal(ctx, approval.TaskID)
		return a.editApprovalCard(ctx, chatID, q, word+". Nothing was pushed.")
	}
	if err := a.editApprovalCard(ctx, chatID, q, word+". Checking the other gate..."); err != nil {
		log.Printf("gateway: edit approval card: %s", scrubForUser(err))
	}
	err = a.maybePublish(ctx, approval.TaskID)
	if errors.Is(err, errApprovalPending) {
		return a.editApprovalCard(ctx, chatID, q, "Approved. Waiting for the other decision; nothing was pushed yet.")
	}
	if err != nil {
		return a.reportError(ctx, chatID, err)
	}
	return nil
}

func (a *application) editApprovalCard(ctx context.Context, chatID int64, q *telegram.CallbackQuery, text string) error {
	if q.Message == nil {
		return a.reply(ctx, chatID, html.EscapeString(text))
	}
	err := a.tg.EditMessageTextKeyboard(ctx, chatID, q.Message.MessageID, html.EscapeString(text), nil)
	if err != nil {
		log.Printf("gateway: edit approval card: %s", scrubForUser(err))
	}
	return nil
}

// maybePublish releases a proposal only after both push and PR approvals are
// recorded. Duplicate taps and replayed callbacks cannot start a second push.
func (a *application) maybePublish(ctx context.Context, taskID uuid.UUID) error {
	if a == nil || a.pipeline == nil || a.approvals == nil {
		return errors.New("gateway: publish path is not configured")
	}
	a.mu.Lock()
	if a.publishing == nil {
		a.publishing = make(map[uuid.UUID]bool)
	}
	if a.publishing[taskID] {
		a.mu.Unlock()
		return nil
	}
	resume, ok := a.resumes[taskID]
	a.publishing[taskID] = true
	a.mu.Unlock()
	if !ok {
		return nil
	}
	// Clear only the in-flight lock here. The resume stays until publish,
	// rejection or a terminal failure so the second owner tap can release it.
	defer func() {
		a.mu.Lock()
		delete(a.publishing, taskID)
		a.mu.Unlock()
	}()
	finish := func() {
		a.mu.Lock()
		delete(a.resumes, taskID)
		a.mu.Unlock()
	}
	for _, action := range []approvals.Action{approvals.ActionPushBranch, approvals.ActionOpenPR} {
		if err := a.approvals.Gate(ctx, taskID, action); err != nil {
			return errApprovalPending
		}
	}
	if a.db != nil {
		if err := a.db.Transition(ctx, taskID, store.StateAwaitingPushApproval, store.StatePushing, ""); err != nil {
			finish()
			return err
		}
	}
	result, err := a.pipeline.Publish(ctx, resume.Proposal)
	if err != nil {
		finish()
		if a.db != nil {
			if current, loadErr := a.db.GetTask(ctx, taskID); loadErr == nil && current.State == store.StatePushing {
				_ = a.db.Transition(ctx, taskID, store.StatePushing, store.StateFailed, scrubForUser(err))
			}
		}
		return a.reply(ctx, resume.Input.ChatID, "Publish failed: "+scrubForUser(err)+". The branch was not opened as a PR.")
	}
	if a.db != nil {
		_ = a.db.SetPRURL(ctx, taskID, result.PRURL)
		if err := a.db.Transition(ctx, taskID, store.StatePushing, store.StatePROpened, ""); err != nil {
			finish()
			return err
		}
		if err := a.db.Transition(ctx, taskID, store.StatePROpened, store.StateDone, ""); err != nil {
			finish()
			return err
		}
		_ = a.db.Audit(ctx, &taskID, "pr_opened", fmt.Sprintf("task=%s url=%s", taskID, result.PRURL))
	}
	finish()
	return a.replyKeyboard(ctx, resume.Input.ChatID, "PR opened: "+result.PRURL, a.buttonsForChat(resume.Input.ChatID))
}

func (a *application) dropProposal(ctx context.Context, taskID uuid.UUID, reason string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	delete(a.resumes, taskID)
	a.mu.Unlock()
	if a.db != nil {
		if current, err := a.db.GetTask(ctx, taskID); err == nil && current.State == store.StateAwaitingPushApproval {
			_ = a.db.Transition(ctx, taskID, store.StateAwaitingPushApproval, store.StateCancelled, reason)
		}
		_ = a.db.Audit(ctx, &taskID, "approval_rejected", fmt.Sprintf("task=%s reason=%s", taskID, reason))
	}
}

func (a *application) cancelProposal(ctx context.Context, taskID uuid.UUID) {
	a.dropProposal(ctx, taskID, "approval_rejected")
}

// undo restores the newest container checkpoint (or an explicit numbered one)
// without asking the agent to repeat a prompt.
func (a *application) undo(ctx context.Context, chatID int64, arg string) error {
	chat := a.getChat(chatID)
	if chat == nil || chat.handle.ID == "" {
		return a.reply(ctx, chatID, "No live workspace to rewind. Work in a topic first.")
	}
	taskID, err := uuid.Parse(chat.lastTaskID())
	if err != nil {
		return a.reply(ctx, chatID, "That workspace has no checkpointed task yet.")
	}
	if a.runner == nil || a.db == nil {
		return a.reply(ctx, chatID, "Checkpoints are unavailable in this gateway.")
	}
	raw := strings.TrimSpace(arg)
	n := -1
	if raw != "" {
		n, err = strconv.Atoi(raw)
		if err != nil || n < 0 {
			return a.reply(ctx, chatID, "Use /undo or /undo <checkpoint number>.")
		}
	}
	var cp store.Checkpoint
	if n < 0 {
		cp, err = a.db.LatestCheckpoint(ctx, taskID)
	} else {
		cp, err = a.db.LoadCheckpoint(ctx, taskID, n)
	}
	if errors.Is(err, store.ErrNotFound) {
		return a.reply(ctx, chatID, "No checkpoint has been saved for this task yet.")
	}
	if err != nil {
		return a.reportError(ctx, chatID, err)
	}
	if err := contract.Rewind(ctx, a.runner, chat.handle.ID, cp.N); err != nil {
		return a.reportError(ctx, chatID, err)
	}
	if _, err := a.db.RewindCheckpoint(ctx, taskID, cp.N); err != nil {
		return a.reportError(ctx, chatID, err)
	}
	a.event(ctx, cp.SessionID, &taskID, "checkpoint_rewind", map[string]string{"n": strconv.Itoa(cp.N)})
	return a.replyKeyboard(ctx, chatID,
		fmt.Sprintf("Restored checkpoint %d. The workspace is back before the later changes.", cp.N),
		a.buttonsForChat(chatID))
}

func (a *application) undoCallback(ctx context.Context, chatID int64, data string) error {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "undo" {
		return &telegram.ReportedError{Err: fmt.Errorf("malformed undo callback %q", data)}
	}
	chat := a.getChat(chatID)
	if chat == nil || chat.lastTaskID() != parts[1] {
		return a.reply(ctx, chatID, "That undo button belongs to an older task.")
	}
	if !a.markAction(data) {
		return a.reply(ctx, chatID, "That rewind is already running.")
	}
	arg := parts[2]
	if arg == "latest" {
		arg = ""
	}
	return a.undo(ctx, chatID, arg)
}
