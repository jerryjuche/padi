// Command gateway is the M1 Spine: Telegram -> container -> OpenCode -> Telegram.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/approvals"
	"github.com/jerryjuche/padi/internal/gateway/contract"
	"github.com/jerryjuche/padi/internal/gateway/gitops"
	"github.com/jerryjuche/padi/internal/gateway/opencode"
	"github.com/jerryjuche/padi/internal/gateway/pipeline"
	"github.com/jerryjuche/padi/internal/gateway/risk"
	"github.com/jerryjuche/padi/internal/gateway/runner"
	"github.com/jerryjuche/padi/internal/gateway/store"
	"github.com/jerryjuche/padi/internal/gateway/telegram"
)

const (
	modeBuild    = "build"
	modePlan     = "plan"
	modeResearch = "research"
	chatIdleTTL  = 30 * time.Minute

	// chatStyle is prepended to every agent prompt so replies read like a
	// conversational collaborator, not a tool transcript or a generic menu.
	// Build turns are read-only plan turns until the owner taps Execute.
	chatStyle = "Answer the current request directly in 2-4 short sentences: the result, what you verified, then one concrete next step tied to this request. Do not open with a capability disclaimer or repeat a canned status line. Never re-ask a settled question or restate an earlier answer; use what you already learned in this conversation. Before any file change, investigate and end with exactly one whole line `PROPOSAL: <goal; files; steps; risk>`; do not edit files in that turn. The owner receives an Execute button carrying that exact plan. When the prompt says the owner already approved a PROPOSAL, make only that approved change, run the narrowest relevant checks, and do not ask for approval again. Ask one clarifying question with concrete options only when a file-changing choice is genuinely blocked. Never mention tool parameter names such as oldString, newString or replaceAll. Publishing and pull requests happen only after verification and owner approval; finish the local work first. Keep replies under 600 characters.\n\n"
)

var userFacingSecret = regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)\S+|(?i)(authorization:\s*basic\s+)[A-Za-z0-9+/=]+|\b(?:ghs|gho|ghu|ghr|github_pat)_[A-Za-z0-9_]+\b|\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)

func main() {
	if err := run(); err != nil {
		log.Fatalf("gateway: %v", err)
	}
}

func run() error {
	configPath := os.Getenv("GATEWAY_CONFIG")
	if configPath == "" {
		configPath = "config/gateway.yaml"
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	dsn, err := secretFromEnv(cfg.Store.DSNEnv, "database DSN")
	if err != nil {
		return err
	}
	db, err := store.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return err
	}
	policy, err := approvals.Load(cfg.PoliciesFile)
	if err != nil {
		return err
	}
	approvalTTL, err := cfg.ApprovalTTL()
	if err != nil {
		return err
	}
	approvalManager := &approvals.Manager{
		Policy: policy, Store: db, TTL: approvalTTL, OwnerID: 0,
	}

	botToken, err := secretFromEnv("TELEGRAM_BOT_TOKEN", "Telegram bot token")
	if err != nil {
		return err
	}
	ownerRaw, err := secretFromEnvAny("Telegram owner ID", "TELEGRAM_ALLOWED_USER_ID", "TELEGRAM_OWNER_ID")
	if err != nil {
		return err
	}
	var ownerID int64
	if _, err := fmt.Sscan(ownerRaw, &ownerID); err != nil || ownerID <= 0 {
		return fmt.Errorf("gateway: Telegram owner ID must be a positive integer")
	}
	approvalManager.OwnerID = ownerID
	// M2: the only credential that reaches a session container is the router
	// token. Provider keys stay with the router on the host.
	routerToken, err := secretFromEnv(cfg.OpenCode.RouterTokenEnv, "router token")
	if err != nil {
		return err
	}
	routerBaseURL := os.Getenv("PADI_ROUTER_BASE_URL")
	if routerBaseURL == "" {
		routerBaseURL = cfg.OpenCode.RouterBaseURL
	}

	docker, err := runner.NewDocker(os.Getenv("DOCKER_HOST"))
	if err != nil {
		return err
	}
	r := runner.New(docker, runner.Config{
		Image:           cfg.Container.Image,
		Network:         cfg.Container.Network,
		Runtime:         cfg.Container.Runtime,
		InternalNetwork: cfg.Container.InternalNetwork,
		MemoryMB:        cfg.Container.MemoryMB,
		CPUs:            cfg.Container.CPUs,
		Pids:            cfg.Container.Pids,
		Model:           cfg.OpenCode.Model,
		RouterToken:     routerToken,
		RouterBaseURL:   routerBaseURL,
		ProxyURL:        cfg.Container.Proxy,
		BundleMaxMB:     cfg.Container.BundleMaxMB,
	})
	git := &gitops.Git{
		CacheDir: cfg.Git.CacheDir, WorkspacesDir: cfg.Git.WorkspacesDir,
		QuarantineDir: cfg.Git.QuarantineDir, VerifyDir: cfg.Git.VerifyDir,
		BundleMaxMB:  cfg.Container.BundleMaxMB,
		APIURL:       cfg.GitHub.APIURL,
		DefaultDraft: cfg.GitHub.DefaultDraft,
	}
	analyzer := risk.New(risk.Thresholds{
		StaleDays: cfg.Risk.StaleDays, HighFixDensity: cfg.Risk.HighFixDensity,
		HighCommits90d: cfg.Risk.HighCommits90d, HighChangeSize: cfg.Risk.HighChangeSize,
		MediumChangeSize: cfg.Risk.MediumChangeSize, IncidentMedium: cfg.Risk.IncidentMedium,
		IncidentHigh: cfg.Risk.IncidentHigh,
	})
	trust := newTrustPipeline(cfg, approvalManager, db, r, git, analyzer)
	tg := &telegram.Client{
		BaseURL:     os.Getenv("TELEGRAM_API_BASE"),
		Token:       botToken,
		PollSeconds: cfg.Telegram.PollTimeoutS,
	}
	renderer := &telegram.Renderer{
		Client:       tg,
		Recorder:     db,
		EditInterval: time.Duration(cfg.Telegram.EditIntervalMS) * time.Millisecond,
		MaxChars:     cfg.Telegram.MaxMessageChars,
	}
	if _, err := r.Reconcile(ctx, map[string]bool{}); err != nil {
		log.Printf("gateway: reconcile: %v", err)
	}

	app := &application{
		cfg: cfg, db: db, runner: r, git: git, tg: tg, renderer: renderer,
		policy: policy, approvals: approvalManager, pipeline: trust,
		risk:             analyzer,
		protection:       protectionFromPolicy(policy),
		contractDefaults: contractDefaultsFromConfig(cfg),
		checkpointEvery:  time.Duration(cfg.ContractDefaults.CheckpointEveryMin) * time.Minute,
		pending:          make(map[int64]string),
		modes:            make(map[int64]string),
		chats:            make(map[int64]*chatSession),
		actions:          make(map[string]time.Time),
		resumes:          make(map[uuid.UUID]resumeTask),
		proposals:        make(map[uuid.UUID]pipeline.Proposal),
		budgets:          make(map[uuid.UUID]contract.Budget),
		budgetStop:       make(map[uuid.UUID]bool),
		checkpoints:      make(map[uuid.UUID]int),
		publishing:       make(map[uuid.UUID]bool),
		waits:            make(map[uuid.UUID]chan bool),
		tasks:            make(chan taskJob, 32),
		direct:           make(map[int64]*directChatState),
		chatClient: &routerChatClient{
			BaseURL: routerBaseURL, Token: routerToken, Model: cfg.DirectModel(),
		},
	}
	go approvalManager.ExpireLoop(ctx)
	go app.taskWorker(ctx)
	poller := &telegram.Poller{
		Client:  tg,
		Store:   db,
		OwnerID: ownerID,
		Handle:  app.handleUpdate,
	}
	go app.reapIdle(ctx)
	go func() {
		if err := poller.Run(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("gateway: poller stopped: %v", err)
			cancel()
		}
	}()
	log.Printf("gateway: ready (chat=%s, build=%s, plan=%s, network=%s)",
		cfg.DirectModel(), cfg.OpenCodeAlias("build"), cfg.OpenCodeAlias("plan"), cfg.Container.Network)
	<-ctx.Done()
	app.Close()
	return nil
}

// taskRecord is the minimum context a follow-up needs so the agent never has
// to guess what "continue" or "explain" refers to.
type taskRecord struct {
	ID       string
	Prompt   string
	Outcome  string
	Evidence string
	Files    []string
}

// proposalRecord is a structured ask-before-acting handoff. The agent writes a
// PROPOSAL line; the gateway turns it into one Execute button carrying the
// exact plan, so a click cannot drift into a different task.
type proposalRecord struct {
	TaskID string
	Prompt string
}

// chatSession is a long-lived workspace for one Telegram chat. Keeping the
// container and OpenCode session alive is what makes follow-ups conversational:
// the agent remembers what it read, changed and asked in earlier turns.
type chatSession struct {
	mu        sync.Mutex
	repo      string
	mode      string
	session   store.Session
	handle    runner.Handle
	oc        *opencode.Client
	ocSession string
	lastUsed  time.Time
	busy      bool
	state     string
	lastTask  taskRecord
	proposal  *proposalRecord
}

func (c *chatSession) begin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy {
		return false
	}
	c.busy = true
	c.lastUsed = time.Now()
	return true
}

func (c *chatSession) end() {
	c.mu.Lock()
	c.busy = false
	c.lastUsed = time.Now()
	c.mu.Unlock()
}

type chatSnapshot struct {
	Repo      string
	Mode      string
	SessionID uuid.UUID
	LastUsed  time.Time
	Busy      bool
	State     string
	Container string
	Task      taskRecord
}

func (c *chatSession) detail() (repo, mode string, sessionID uuid.UUID, lastUsed time.Time, busy bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.repo, c.mode, c.session.ID, c.lastUsed, c.busy
}

func (c *chatSession) snapshot() chatSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return chatSnapshot{
		Repo: c.repo, Mode: c.mode, SessionID: c.session.ID, LastUsed: c.lastUsed,
		Busy: c.busy, State: c.state, Container: c.handle.ID, Task: c.lastTask,
	}
}

func (c *chatSession) setState(state string) {
	c.mu.Lock()
	c.state = state
	c.mu.Unlock()
}

func (c *chatSession) stateSnapshot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *chatSession) setLastTask(t taskRecord) {
	c.mu.Lock()
	c.lastTask = t
	c.mu.Unlock()
}

func (c *chatSession) setOutcome(outcome string) {
	c.mu.Lock()
	c.lastTask.Outcome = outcome
	c.mu.Unlock()
}

func (c *chatSession) setProposal(p *proposalRecord) {
	c.mu.Lock()
	c.proposal = p
	if p != nil {
		c.state = "decision"
	} else if c.state == "decision" {
		c.state = "ready"
	}
	c.mu.Unlock()
}

func (c *chatSession) lastTaskID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastTask.ID
}

func (c *chatSession) taskFor(taskID string) (taskRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastTask.ID == "" || c.lastTask.ID != taskID {
		return taskRecord{}, false
	}
	return c.lastTask, true
}

func (c *chatSession) hasProposal() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.proposal != nil
}

func (c *chatSession) proposalFor(taskID string) (*proposalRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.proposal == nil || c.proposal.TaskID != taskID {
		return nil, false
	}
	p := *c.proposal
	return &p, true
}

// taskContext is injected ahead of every follow-up prompt. It is what stops
// the agent from re-asking a question that was already settled in the thread.
func (c *chatSession) taskContext() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastTask.ID == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("Conversation context: previous task " + c.lastTask.ID + " (" + c.lastTask.Outcome + ") was: " + c.lastTask.Prompt)
	if len(c.lastTask.Files) > 0 {
		b.WriteString(". Files touched: " + strings.Join(c.lastTask.Files, ", "))
	}
	if c.lastTask.Evidence != "" {
		evidence := c.lastTask.Evidence
		if len(evidence) > 240 {
			evidence = evidence[:240]
		}
		b.WriteString(". Verified: " + evidence)
	}
	b.WriteString(". Continue from this state; do not re-ask settled questions.")
	return b.String()
}

type application struct {
	cfg      Config
	db       *store.PG
	runner   *runner.Runner
	git      *gitops.Git
	tg       *telegram.Client
	renderer *telegram.Renderer

	// M3b trust path. policy is the strict policy file; approvals is the gate;
	// pipeline composes export, quarantine, verify, guard, risk and publish.
	policy           *approvals.Policy
	approvals        *approvals.Manager
	pipeline         *pipeline.Service
	risk             *risk.Analyzer
	protection       contract.Protection
	contractDefaults contract.Budget
	checkpointEvery  time.Duration

	// pending remembers a repo chosen by "clone <repo>" for the next message.
	// modes is a sticky build/plan/research choice per chat.
	mu      sync.Mutex
	pending map[int64]string
	modes   map[int64]string
	chats   map[int64]*chatSession

	// direct holds router-only conversation memory for chats without a
	// workspace task. Workspace tasks keep their own OpenCode session.
	directMu sync.Mutex
	direct   map[int64]*directChatState

	// chatClient calls the same router the session containers use, so normal
	// conversation needs no clone, container or provider keys in the gateway.
	chatClient *routerChatClient

	// actions de-duplicates legacy button tokens and approval callbacks. A
	// token is consumed once; replayed updates cannot start a second run.
	actions map[string]time.Time

	// taps backs one-time inline buttons. Every actionable keyboard issues a
	// fresh short token that is deleted on the first callback, so an old card
	// can never trigger the same action twice.
	taps map[string]buttonTap

	// M3b per-task state used to resume after owner decisions and to avoid
	// publishing from a duplicate callback.
	resumes     map[uuid.UUID]resumeTask
	proposals   map[uuid.UUID]pipeline.Proposal
	budgets     map[uuid.UUID]contract.Budget
	budgetStop  map[uuid.UUID]bool
	checkpoints map[uuid.UUID]int
	publishing  map[uuid.UUID]bool
	waits       map[uuid.UUID]chan bool

	// tasks serialises workspace work off the Telegram poller. contractGate
	// waits for an owner tap, so running it inline would block the same poller
	// that has to deliver that tap.
	tasks chan taskJob
}

type taskJob struct {
	ctx    context.Context
	chatID int64
	cmd    command
	key    string
	fresh  bool
}

// taskWorker owns workspace tasks one at a time. The Telegram poller stays
// free to deliver the approval callback that contractGate is waiting for.
func (a *application) taskWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-a.tasks:
			if err := a.startTask(job.ctx, job.chatID, job.cmd, job.key, job.fresh); err != nil {
				log.Printf("gateway: task %s failed: %s", job.key, scrubForUser(err))
			}
		}
	}
}

// enqueueTask hands workspace work to the single task worker. A nil channel is
// only used by tests that construct an application directly; startup always
// installs the queue and worker.
func (a *application) enqueueTask(ctx context.Context, chatID int64, c command, key string, fresh bool) error {
	job := taskJob{ctx: ctx, chatID: chatID, cmd: c, key: key, fresh: fresh}
	if a.tasks == nil {
		return a.startTask(ctx, chatID, c, key, fresh)
	}
	select {
	case a.tasks <- job:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return a.reportError(ctx, chatID, errors.New("gateway: task queue is full"))
	}
}

// markAction consumes a legacy action or approval token. Tokens are never
// reusable: a replayed callback must not start a second run or decision.
func (a *application) markAction(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.actions == nil {
		a.actions = make(map[string]time.Time)
	}
	if len(a.actions) > 512 {
		for k, t := range a.actions {
			if time.Since(t) > 24*time.Hour {
				delete(a.actions, k)
			}
		}
	}
	if _, ok := a.actions[key]; ok {
		return false
	}
	a.actions[key] = time.Now()
	return true
}

type buttonTap struct {
	ChatID  int64
	TaskID  string
	Action  string
	Label   string
	Kind    string
	Created time.Time
}

var fallbackTapSeq uint64

func newTapToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "x:" + hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("x:%016x", atomic.AddUint64(&fallbackTapSeq, 1))
}

// issueTap creates a fresh one-time token for an actionable button.
func (a *application) issueTap(tap buttonTap) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.taps == nil {
		a.taps = make(map[string]buttonTap)
	}
	if len(a.taps) > 512 {
		for token, old := range a.taps {
			if time.Since(old.Created) > chatIdleTTL {
				delete(a.taps, token)
			}
		}
	}
	token := newTapToken()
	tap.Created = time.Now()
	a.taps[token] = tap
	return token
}

// consumeTap removes a token before the action runs. Even a failed action is
// spent; an owner must ask for a fresh keyboard to retry it.
func (a *application) consumeTap(chatID int64, token string) (buttonTap, bool) {
	a.mu.Lock()
	tap, ok := a.taps[token]
	if ok {
		delete(a.taps, token)
	}
	a.mu.Unlock()
	if !ok || tap.ChatID != chatID {
		return buttonTap{}, false
	}
	return tap, true
}

func (a *application) event(ctx context.Context, sessionID uuid.UUID, taskID *uuid.UUID, typ string, data any) bool {
	exceeded := a.trackUsage(taskID, typ, data)
	if _, err := a.db.AppendEvent(ctx, sessionID, taskID, typ, data); err != nil {
		log.Printf("gateway: append event %s: %v", typ, scrubForUser(err))
	}
	return exceeded
}

// command is the parsed form of one owner message. readOnly tasks use the
// plan agent (no edits); approved tasks carry an accepted PROPOSAL and may
// switch to the build agent immediately.
type command struct {
	repo           string
	prompt         string
	askRepo        bool
	awaitingPrompt bool
	readOnly       bool
	approved       bool
}

// parseCommand handles repository selection only. Normal conversation is
// answered by the router without a container; a default repo is never implied.
func (a *application) parseCommand(chatID int64, text string) command {
	fields := strings.Fields(text)
	if len(fields) >= 2 && strings.EqualFold(fields[0], "clone") {
		repo := normalizeRepo(fields[1])
		if !validRepo(repo) {
			return command{askRepo: true}
		}
		rest := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
		rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[1]))
		if len(rest) >= 3 && strings.EqualFold(rest[:3], "and") &&
			(len(rest) == 3 || rest[3] == ' ' || rest[3] == '\n' || rest[3] == '\t') {
			rest = strings.TrimSpace(rest[3:])
		}
		if rest == "" {
			return command{repo: repo, awaitingPrompt: true}
		}
		return command{repo: repo, prompt: rest}
	}
	if repo := a.forgetRepo(chatID); repo != "" {
		return command{repo: repo, prompt: text}
	}
	return command{prompt: text}
}

// normalizeRepo turns the forms people actually type into an owner/repo key.
// Absolute paths are preserved for the local M1 spike.
func normalizeRepo(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "/") {
		return strings.TrimRight(s, "/")
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if colon, at := strings.IndexByte(s, ':'), strings.LastIndexByte(s, '@'); at >= 0 && colon > at {
		s = s[colon+1:] // scp style: git@github.com:owner/repo.git
	}
	if at := strings.LastIndexByte(s, '@'); at >= 0 {
		if slash := strings.IndexByte(s, '/'); slash < 0 || at < slash {
			s = s[at+1:] // credentials in an https URL
		}
	}
	if i := strings.IndexByte(s, '/'); i > 0 && strings.Contains(s[:i], ".") {
		s = s[i+1:] // github.com/owner/repo or GitHub.com/owner/repo
	}
	s = strings.Trim(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return s
}

func validRepo(repo string) bool {
	if repo == "" || strings.ContainsAny(repo, " \t\n") {
		return false
	}
	if strings.HasPrefix(repo, "/") {
		return true
	}
	return strings.Count(repo, "/") >= 1
}

// peekRepo shows a repo waiting for its first prompt without consuming it.
func (a *application) peekRepo(chatID int64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pending[chatID]
}

func (a *application) rememberRepo(chatID int64, repo string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending == nil {
		a.pending = make(map[int64]string)
	}
	a.pending[chatID] = repo
}

func (a *application) forgetRepo(chatID int64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	repo := a.pending[chatID]
	delete(a.pending, chatID)
	return repo
}

func (a *application) rememberMode(chatID int64, mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.modes == nil {
		a.modes = make(map[int64]string)
	}
	a.modes[chatID] = mode
}

func (a *application) currentMode(chatID int64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if mode := a.modes[chatID]; mode != "" {
		return mode
	}
	return modeBuild
}

func (a *application) putChat(chatID int64, chat *chatSession) {
	a.mu.Lock()
	a.chats[chatID] = chat
	a.mu.Unlock()
}

func (a *application) getChat(chatID int64) *chatSession {
	a.mu.Lock()
	chat := a.chats[chatID]
	if chat != nil {
		_, _, _, last, busy := chat.detail()
		if !busy && time.Since(last) > chatIdleTTL {
			delete(a.chats, chatID)
			go a.stopChat(chat)
			chat = nil
		}
	}
	a.mu.Unlock()
	return chat
}

// repoSwitched reports whether an active chat must be retired before a task
// for repo starts. A normal follow-up has an empty repo and keeps its session.
func repoSwitched(chat *chatSession, repo string) bool {
	if chat == nil || repo == "" {
		return false
	}
	current, _, _, _, _ := chat.detail()
	return current != "" && !strings.EqualFold(current, repo)
}

// prepareWorkspace applies the max_sessions=1 cross-repo boundary. It returns
// the chat to reuse, or nil after closing the old chat and stopping its
// container; the caller must then open a fresh session and container.
func (a *application) prepareWorkspace(chatID int64, chat *chatSession, repo string, fresh bool) *chatSession {
	if chat == nil {
		return nil
	}
	if fresh || repoSwitched(chat, repo) {
		a.closeChat(chatID)
		return nil
	}
	return chat
}

func (a *application) closeChat(chatID int64) {
	a.mu.Lock()
	chat := a.chats[chatID]
	delete(a.chats, chatID)
	a.mu.Unlock()
	if chat != nil {
		a.stopChat(chat)
	}
}

func (a *application) stopChat(chat *chatSession) {
	chat.mu.Lock()
	handle := chat.handle
	chat.mu.Unlock()
	if handle.ID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.runner.Stop(ctx, handle.ID); err != nil {
		log.Printf("gateway: stop container %s: %v", handle.ID, scrubForUser(err))
	}
}

func (a *application) Close() {
	a.mu.Lock()
	ids := make([]int64, 0, len(a.chats))
	for id := range a.chats {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.closeChat(id)
	}
}

func (a *application) reapIdle(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.mu.Lock()
			var stale []*chatSession
			for id, chat := range a.chats {
				_, _, _, last, busy := chat.detail()
				if !busy && time.Since(last) > chatIdleTTL {
					delete(a.chats, id)
					stale = append(stale, chat)
				}
			}
			a.mu.Unlock()
			for _, chat := range stale {
				a.stopChat(chat)
			}
		}
	}
}

func (a *application) handleUpdate(ctx context.Context, u telegram.Update) error {
	if q := u.CallbackQuery; q != nil {
		return a.handleCallback(ctx, q)
	}
	if u.Message == nil {
		return nil
	}
	chatID := u.Message.Chat.ID
	text := strings.TrimSpace(u.Message.Text)
	if text == "" {
		return nil
	}

	key := fmt.Sprintf("tg-%d-%d", chatID, u.Message.MessageID)
	switch {
	case strings.HasPrefix(text, "/start"), strings.HasPrefix(text, "/help"):
		return a.help(ctx, chatID)
	case strings.HasPrefix(text, "/status"):
		return a.status(ctx, chatID)
	case strings.HasPrefix(text, "/undo"):
		return a.undo(ctx, chatID, strings.TrimSpace(strings.TrimPrefix(text, "/undo")))
	case strings.HasPrefix(text, "/new"):
		a.closeChat(chatID)
		a.forgetRepo(chatID)
		a.clearDirect(chatID)
		a.rememberMode(chatID, modeBuild)
		return a.replyKeyboard(ctx, chatID, "New topic. Tell me what you'd like to work on, or send clone <repo> to open a repository.", a.buttons(chatID, "empty", ""))
	case modeFromCommand(text) != "":
		mode := modeFromCommand(text)
		a.rememberMode(chatID, mode)
		if chat := a.getChat(chatID); chat != nil {
			chat.mu.Lock()
			chat.mode = mode
			chat.mu.Unlock()
		}
		rest := modePromptRest(text)
		if rest == "" {
			return a.replyKeyboard(ctx, chatID, mode+" mode on. Tell me what you want me to "+modeVerb(mode)+".", a.buttonsForChat(chatID))
		}
		return a.handleConversation(ctx, chatID, rest, key, mode)
	}

	// Explicit clone opens or replaces a workspace. Everything else is a
	// conversation turn first; the model decides if it needs that workspace.
	if strings.HasPrefix(strings.ToLower(text), "clone ") {
		c := a.parseCommand(chatID, text)
		if c.askRepo {
			return a.reply(ctx, chatID, "That doesn't look like a repository. Send a GitHub URL (owner/repo works too), or just tell me what you'd like to chat about.")
		}
		if c.awaitingPrompt {
			a.rememberRepo(chatID, c.repo)
			return a.reply(ctx, chatID, "Got "+c.repo+". What should I do in it?")
		}
		fresh := a.getChat(chatID) == nil
		return a.enqueueTask(ctx, chatID, c, key, fresh)
	}
	return a.handleConversation(ctx, chatID, text, key, "")
}

// handleConversation answers a normal message through the router. The router
// may hand the request back with a workspace marker; a repository is requested
// only for work that genuinely needs one.
func (a *application) handleConversation(ctx context.Context, chatID int64, text, key, modeHint string) error {
	reply, err := a.directCompletion(ctx, chatID, text, modeHint)
	if err != nil {
		log.Printf("gateway: direct chat: %v", scrubForUser(err))
		return a.replyKeyboard(ctx, chatID, "I couldn't reach the chat model. Try again, or send /help.", a.buttons(chatID, "empty", ""))
	}
	kind, message := classifyDirectReply(reply, text)
	state := a.getDirect(chatID)

	switch kind {
	case directWorkspace:
		chat := a.getChat(chatID)
		state.add("user", text)
		if chat == nil {
			question := defaultRepoQuestion(message)
			if modeHint != "" && modeHint != modeBuild {
				question += " I'll use " + modeHint + " mode."
			}
			state.add("assistant", question)
			return a.replyKeyboard(ctx, chatID, question, a.buttons(chatID, "empty", ""))
		}
		repo, _, _, _, _ := chat.detail()
		return a.enqueueTask(ctx, chatID, command{repo: repo, prompt: message}, key, false)
	case directNeedsRepo:
		question := ensureRepoQuestion(message, text)
		if modeHint != "" && modeHint != modeBuild {
			question += " I'll use " + modeHint + " mode."
		}
		state.add("user", text)
		state.add("assistant", question)
		return a.replyKeyboard(ctx, chatID, question, a.buttons(chatID, "empty", ""))
	default:
		state.add("user", text)
		state.add("assistant", message)
		return a.replyKeyboard(ctx, chatID, message, a.buttons(chatID, "empty", ""))
	}
}

func (a *application) directCompletion(ctx context.Context, chatID int64, text, modeHint string) (string, error) {
	if a.chatClient == nil {
		return "I'm here. Tell me what you'd like to work on.", nil
	}
	history := a.getDirect(chatID).snapshot()
	return a.chatClient.Complete(ctx, a.buildDirectSystem(chatID, modeHint), history, text)
}

// buildDirectSystem injects only workspace metadata. The model never receives
// repo credentials and cannot claim it read files in this direct-chat path.
func (a *application) buildDirectSystem(chatID int64, modeHint string) string {
	workspace := "No repository is open."
	if chat := a.getChat(chatID); chat != nil {
		repo, mode, _, _, _ := chat.detail()
		workspace = "Active workspace: " + repo + " in " + mode + " mode. You may refer to it, but you have not read its files in this chat."
	}
	if modeHint != "" {
		workspace += " The owner selected " + modeHint + " mode for the next workspace task."
	}
	return directSystemPrompt + "\n" + workspace
}

func (a *application) handleCallback(ctx context.Context, q *telegram.CallbackQuery) error {
	ack := ""
	if strings.HasPrefix(q.Data, "x:") {
		ack = "Started"
	}
	if err := a.tg.AnswerCallbackQuery(ctx, q.ID, ack); err != nil {
		log.Printf("gateway: answer callback: %v", scrubForUser(err))
	}
	if q.Message == nil {
		return &telegram.ReportedError{Err: errors.New("callback has no message")}
	}
	chatID := q.Message.Chat.ID
	switch data := q.Data; {
	case strings.HasPrefix(data, "mode:"):
		mode := strings.TrimPrefix(data, "mode:")
		if !validMode(mode) {
			return &telegram.ReportedError{Err: fmt.Errorf("unknown mode %q", mode)}
		}
		a.rememberMode(chatID, mode)
		if chat := a.getChat(chatID); chat != nil {
			chat.mu.Lock()
			chat.mode = mode
			chat.mu.Unlock()
		}
		return a.replyToCallback(ctx, q, mode+" mode on. Tell me what you want me to "+modeVerb(mode)+".", a.buttonsForChat(chatID))
	case data == "new":
		a.closeChat(chatID)
		a.forgetRepo(chatID)
		a.clearDirect(chatID)
		a.rememberMode(chatID, modeBuild)
		return a.replyToCallback(ctx, q, "New topic. Tell me what you'd like to work on, or send clone <repo> to open a repository.", a.buttons(chatID, "empty", ""))
	case data == "status":
		return a.replyHTMLToCallback(ctx, q, a.statusCard(ctx, chatID), statusButtons())
	case data == "help":
		return a.replyToCallback(ctx, q, a.helpText(), a.buttonsForChat(chatID))
	case strings.HasPrefix(data, "ap:"):
		return a.handleApprovalCallback(ctx, chatID, q, data)
	case strings.HasPrefix(data, "exec:"):
		taskID := strings.TrimPrefix(data, "exec:")
		chat := a.getChat(chatID)
		if chat == nil {
			return a.reply(ctx, chatID, "No active workspace yet. Send a request or clone <repo> first.")
		}
		p, ok := chat.proposalFor(taskID)
		if !ok {
			return a.reply(ctx, chatID, "That plan is no longer active. Ask for a fresh plan and I'll prepare it.")
		}
		if !a.markAction(data) {
			return a.replyToCallback(ctx, q, "That plan was already executed.", a.buttonsForChat(chatID))
		}
		if err := a.replyToCallback(ctx, q, "Executing the approved plan...", nil); err != nil {
			log.Printf("gateway: spend plan card: %v", scrubForUser(err))
		}
		chat.setProposal(nil)
		repo, _, _, _, _ := chat.detail()
		return a.enqueueTask(ctx, chatID, command{repo: repo, prompt: p.Prompt, approved: true}, "exec:"+taskID, false)
	case strings.HasPrefix(data, "undo:"):
		return a.undoCallback(ctx, chatID, data)
	case strings.HasPrefix(data, "x:"):
		return a.handleTap(ctx, chatID, q, data)
	case strings.HasPrefix(data, "a:"):
		parts := strings.Split(data, ":")
		if len(parts) != 3 {
			return &telegram.ReportedError{Err: fmt.Errorf("malformed action %q", data)}
		}
		return a.runLegacyAction(ctx, chatID, parts[1], parts[2], data)
	case strings.HasPrefix(data, "act:"):
		// Keyboards sent before the task-scoped protocol stay harmless: map
		// them to the current task instead of starting a generic new task.
		chat := a.getChat(chatID)
		if chat == nil || chat.lastTaskID() == "" {
			return a.reply(ctx, chatID, "That button is from an older topic. Send a fresh request and I'll use the new actions.")
		}
		action := strings.TrimPrefix(data, "act:")
		switch action {
		case "explain", "checks", "continue", "summary":
			return a.runLegacyAction(ctx, chatID, chat.lastTaskID(), action, data)
		default:
			return &telegram.ReportedError{Err: fmt.Errorf("unknown action %q", data)}
		}
	default:
		return &telegram.ReportedError{Err: fmt.Errorf("unknown callback %q", data)}
	}
}

// handleTap resolves a one-time keyboard token, spends it, and starts the
// action bound to it. The tapped card is edited to a spent state first, so the
// same buttons cannot be clicked twice and stale cards stop offering actions.
func (a *application) handleTap(ctx context.Context, chatID int64, q *telegram.CallbackQuery, token string) error {
	tap, ok := a.consumeTap(chatID, token)
	if !ok {
		return a.replyToCallback(ctx, q, "That button was already used. Current actions are below.", a.buttonsForChat(chatID))
	}
	chat := a.getChat(chatID)
	if chat == nil {
		return a.replyToCallback(ctx, q, "No active workspace yet. Send a request or clone <repo> first.", a.buttons(chatID, "empty", ""))
	}
	if chat.lastTaskID() != tap.TaskID {
		return a.replyToCallback(ctx, q, "That button belongs to an older task. Current actions are below.", a.buttonsForChat(chatID))
	}
	verb := tap.Label
	if verb == "" {
		verb = "For this task..."
	}
	if err := a.replyToCallback(ctx, q, verb+"...", nil); err != nil {
		log.Printf("gateway: spend button card: %v", scrubForUser(err))
	}
	switch tap.Action {
	case "execute":
		if _, ok := chat.proposalFor(tap.TaskID); !ok {
			return a.reply(ctx, chatID, "That plan is no longer active. Ask for a fresh plan and I'll prepare it.")
		}
		p, _ := chat.proposalFor(tap.TaskID)
		chat.setProposal(nil)
		repo, _, _, _, _ := chat.detail()
		return a.enqueueTask(ctx, chatID, command{repo: repo, prompt: p.Prompt, approved: true}, "tap:"+token, false)
	case "undo":
		return a.undo(ctx, chatID, "")
	default:
		return a.runAction(ctx, chatID, chat, tap.TaskID, tap.Action, "tap:"+token)
	}
}

// runAction starts a quick action with the exact prior task as context.
func (a *application) runAction(ctx context.Context, chatID int64, chat *chatSession, taskID, action, key string) error {
	t, ok := chat.taskFor(taskID)
	if !ok {
		return a.reply(ctx, chatID, "That button belongs to an older task. The current actions are below.")
	}
	_, _, _, _, busy := chat.detail()
	if busy {
		return a.reply(ctx, chatID, "I'm still working on the previous message. I'll pick this up next.")
	}
	if chat.hasProposal() && action != "revise" {
		return a.replyKeyboard(ctx, chatID, "A plan is waiting. Use Execute to approve it or Revise to change it.", a.buttonsForChat(chatID))
	}
	if !chat.hasProposal() && action == "revise" {
		return a.reply(ctx, chatID, "There's no active plan to revise. Ask for a new plan and I'll prepare one.")
	}
	prompt, ok := actionPrompt(action, t)
	if !ok {
		return &telegram.ReportedError{Err: fmt.Errorf("unknown action %q", action)}
	}
	repo, _, _, _, _ := chat.detail()
	readOnly := action == "checks" || action == "explain" || action == "diff" || action == "summary"
	return a.enqueueTask(ctx, chatID, command{repo: repo, prompt: prompt, readOnly: readOnly}, key, false)
}

// runLegacyAction keeps keyboards issued by an older binary harmless. Those
// tokens are single-use too, but they cannot be edited to a spent card here.
func (a *application) runLegacyAction(ctx context.Context, chatID int64, taskID, action, token string) error {
	chat := a.getChat(chatID)
	if chat == nil {
		return a.reply(ctx, chatID, "No active workspace yet. Send a request or clone <repo> first.")
	}
	if !a.markAction(token) {
		return a.reply(ctx, chatID, "That action was already used. Ask for fresh buttons.")
	}
	return a.runAction(ctx, chatID, chat, taskID, action, "cb:"+taskID+":"+action)
}

func (a *application) helpText() string {
	return "Padi chat\n\n" +
		"Just write naturally. General chat needs no repository. When a request really needs a workspace, I'll ask you to open one; before any file change I'll propose a plan and wait for your Execute tap.\n\n" +
		"Commands:\n" +
		"• clone <repo> — open a GitHub repo (URLs work)\n" +
		"• /build — implement an approved plan (default mode)\n" +
		"• /plan — investigate and propose a plan before editing\n" +
		"• /research — read, search and report; no edits\n" +
		"• /status — show the active workspace\n" +
		"• /undo [n] — restore a saved checkpoint\n" +
		"• /new — close this workspace and start fresh\n\n" +
		"Buttons offer next steps for this conversation. Use /build, /plan or /research when you want to switch mode."
}

func (a *application) help(ctx context.Context, chatID int64) error {
	return a.replyKeyboard(ctx, chatID, a.helpText(), a.buttonsForChat(chatID))
}

func shortID(value string, n int) string {
	if value == "" {
		return "—"
	}
	if len(value) <= n {
		return value
	}
	return value[:n] + "…"
}

func clipText(value string, n int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= n {
		return value
	}
	return value[:n-1] + "…"
}

// statusVisual gives a compact, scannable state indicator. The bar is visual
// only; the authoritative state is the text label beside it.
func statusVisual(state string) (icon, bar string) {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "QUEUED":
		return "◌", "░░░░░░░░"
	case "PREPARING", "PLANNING", "RUNNING":
		return "◍", "██░░░░░░"
	case "AWAITING_PLAN_APPROVAL":
		return "⏸", "████░░░░"
	case "EXECUTING", "FIXING":
		return "▶", "██████░░"
	case "VERIFYING":
		return "◓", "███████░"
	case "AWAITING_PUSH_APPROVAL", "PR_OPENED", "PREVIEWING":
		return "⏸", "████████"
	case "PUSHING":
		return "⇧", "████████"
	case "DONE":
		return "✅", "████████"
	case "FAILED":
		return "⛔", "████████"
	case "CANCELLED":
		return "⊘", "░░░░░░░░"
	case "TIMED_OUT":
		return "⌛", "░░░░░░░░"
	default:
		return "○", "░░░░░░░░"
	}
}

func statusButtons() [][]telegram.InlineButton {
	return [][]telegram.InlineButton{
		{
			{Text: "↻ Refresh", CallbackData: "status"},
			{Text: "Plan", CallbackData: "mode:plan"},
			{Text: "Build", CallbackData: "mode:build"},
		},
		{
			{Text: "Research", CallbackData: "mode:research"},
			{Text: "Help", CallbackData: "help"},
			{Text: "New", CallbackData: "new"},
		},
	}
}

// statusCard builds a trusted HTML card. Every dynamic value is escaped; only
// the fixed markup and status glyphs are written directly.
func (a *application) statusCard(ctx context.Context, chatID int64) string {
	esc := html.EscapeString
	var snap chatSnapshot
	if chat := a.getChat(chatID); chat != nil {
		snap = chat.snapshot()
	}
	repo := snap.Repo
	if repo == "" {
		repo = a.peekRepo(chatID)
	}
	mode := snap.Mode
	if mode == "" {
		mode = a.currentMode(chatID)
	}
	if mode == "" {
		mode = "chat"
	}
	state := snap.State
	if snap.Busy {
		state = "RUNNING"
	}
	if state == "" {
		state = "IDLE"
	}
	task := snap.Task
	if a.db != nil && task.ID != "" {
		if id, err := uuid.Parse(task.ID); err == nil {
			if current, err := a.db.GetTask(ctx, id); err == nil {
				state = string(current.State)
				if current.Prompt != "" {
					task.Prompt = current.Prompt
				}
			}
		}
	}
	icon, bar := statusVisual(state)
	sessionText := "—"
	if snap.SessionID != uuid.Nil {
		sessionText = shortID(snap.SessionID.String(), 8)
	}
	repoText := "— none —"
	if repo != "" {
		repoText = repo
	}
	taskText := "— none —"
	if task.ID != "" {
		taskText = shortID(task.ID, 8) + "  " + clipText(task.Prompt, 60)
	}
	containerText := "stopped"
	if snap.Container != "" {
		containerText = shortID(snap.Container, 12)
	}
	queued := 0
	if a.tasks != nil {
		queued = len(a.tasks)
	}
	next := "Send a repo and task, or chat normally."
	switch strings.ToUpper(state) {
	case "AWAITING_PLAN_APPROVAL":
		next = "Tap Approve contract to start the read-only plan."
	case "AWAITING_PUSH_APPROVAL":
		next = "Tap Approve push, then Approve PR, to publish the draft."
	case "RUNNING", "EXECUTING", "VERIFYING", "PUSHING", "PREPARING", "PLANNING":
		next = "Work is active. Tap Refresh for a new snapshot."
	case "FAILED", "TIMED_OUT":
		next = "Send /new or ask me to continue locally."
	}

	var b strings.Builder
	b.WriteString("<b>PADI / CONTROL CENTER</b>\n")
	b.WriteString("<i>" + esc(time.Now().UTC().Format("02 Jan 15:04 UTC")) + " · live snapshot</i>\n\n")
	b.WriteString("▸ <b>Repo</b>      <code>" + esc(repoText) + "</code>\n")
	b.WriteString("▸ <b>Mode</b>      " + esc(strings.ToUpper(mode)) +
		"   <b>Session</b> <code>" + esc(sessionText) + "</code>\n")
	b.WriteString("▸ <b>Task</b>      <code>" + esc(taskText) + "</code>\n")
	b.WriteString("▸ <b>State</b>      " + icon + " <b>" + esc(strings.ToUpper(state)) + "</b>\n")
	b.WriteString("                <code>" + bar + "</code>\n")
	b.WriteString("▸ <b>Sandbox</b>   " + esc(containerText) +
		"   <b>Queue</b> " + fmt.Sprintf("%d", queued) + "\n")
	if task.Outcome != "" {
		b.WriteString("▸ <b>Last</b>      <code>" + esc(clipText(task.Outcome, 72)) + "</code>\n")
	}
	b.WriteString("\n<b>Explore next</b>\n• " + esc(next) + "\n")
	b.WriteString("• /status refresh · /plan · /build · /research · /new\n")
	return b.String()
}

func (a *application) status(ctx context.Context, chatID int64) error {
	_, err := a.tg.SendMessageKeyboard(ctx, chatID, a.statusCard(ctx, chatID), statusButtons())
	return err
}

// replyToCallback edits the tapped message when Telegram supplied a message ID,
// so a button press does not stack another card in the chat. Stubbed callbacks
// in tests have no message ID and fall back to a normal reply.
func (a *application) replyToCallback(ctx context.Context, q *telegram.CallbackQuery, text string, rows [][]telegram.InlineButton) error {
	if q != nil && q.Message != nil && q.Message.MessageID != 0 {
		return a.tg.EditMessageTextKeyboard(ctx, q.Message.Chat.ID, q.Message.MessageID, html.EscapeString(text), rows)
	}
	chatID := int64(0)
	if q != nil && q.Message != nil {
		chatID = q.Message.Chat.ID
	}
	return a.replyKeyboard(ctx, chatID, text, rows)
}

// replyHTMLToCallback edits or sends a pre-built trusted HTML card. Callers
// must escape every dynamic value before passing text here.
func (a *application) replyHTMLToCallback(ctx context.Context, q *telegram.CallbackQuery, text string, rows [][]telegram.InlineButton) error {
	if q != nil && q.Message != nil && q.Message.MessageID != 0 {
		return a.tg.EditMessageTextKeyboard(ctx, q.Message.Chat.ID, q.Message.MessageID, text, rows)
	}
	chatID := int64(0)
	if q != nil && q.Message != nil {
		chatID = q.Message.Chat.ID
	}
	_, err := a.tg.SendMessageKeyboard(ctx, chatID, text, rows)
	return err
}

// startTask creates the audit task and either opens a fresh workspace or reuses
// an existing one. The OpenCode session is preserved across follow-ups.
func (a *application) startTask(ctx context.Context, chatID int64, c command, key string, fresh bool) error {
	if c.repo == "" || c.prompt == "" {
		return a.reply(ctx, chatID, "I need a repo and a request. Try 'clone owner/repo and add a README'.")
	}
	mode := a.currentMode(chatID)
	agentName := taskAgent(mode, c)
	agentPrompt := taskPrompt(mode, c)
	eventMode := mode
	if mode == modeBuild && !c.approved {
		eventMode = modePlan // the first build turn is a read-only plan
	}
	chat := a.getChat(chatID)
	// A cloned repo is a new workspace. Retire the previous chat and its
	// container before any repo/session state is created, so a follow-up can
	// never run inside a different repository's idle container and at most one
	// session container stays active.
	chat = a.prepareWorkspace(chatID, chat, c.repo, fresh)
	if err := a.db.EnsureRepo(ctx, c.repo, "main"); err != nil {
		return a.reportError(ctx, chatID, err)
	}

	var session store.Session
	if chat != nil {
		_, _, sessionID, _, _ := chat.detail()
		found, err := a.db.GetSession(ctx, sessionID)
		if err != nil {
			return a.reportError(ctx, chatID, err)
		}
		session = found
	} else {
		created, err := a.db.CreateSession(ctx, "telegram", fmt.Sprintf("tg-%d", chatID))
		if err != nil {
			return a.reportError(ctx, chatID, err)
		}
		session = created
	}

	// A replayed callback or polled update must never start a second agent run.
	// CreateTask is idempotent, but the existing row is only visible after the
	// insert; check first so a live task is left untouched.
	if existing, err := a.db.GetTaskByKey(ctx, key); err == nil {
		if existing.State == store.StateDone || existing.State == store.StateFailed ||
			existing.State == store.StateCancelled || existing.State == store.StateTimedOut {
			return a.reply(ctx, chatID, "I already handled that message.")
		}
		return a.reply(ctx, chatID, "That message is already being handled.")
	} else if !errors.Is(err, store.ErrNotFound) {
		return a.reportError(ctx, chatID, err)
	}
	task, err := a.db.CreateTask(ctx, store.NewTask{
		SessionID: session.ID, Repo: c.repo, Prompt: c.prompt, IdempotencyKey: key,
	})
	if err != nil {
		return a.reportError(ctx, chatID, err)
	}
	a.mu.Lock()
	if a.budgets == nil {
		a.budgets = make(map[uuid.UUID]contract.Budget)
	}
	a.budgets[task.ID] = a.contractDefaults
	a.mu.Unlock()
	// Capture the previous task before this one becomes current; it is injected
	// into the agent prompt so follow-ups do not re-open settled questions.
	var priorContext string
	if chat != nil {
		priorContext = chat.taskContext()
	}
	a.event(ctx, session.ID, &task.ID, "status", map[string]string{"state": "QUEUED", "mode": eventMode})
	if err := a.db.Transition(ctx, task.ID, store.StateQueued, store.StatePreparing, ""); err != nil {
		return a.reportError(ctx, chatID, err)
	}

	agreed := contract.Draft(c.prompt, contract.RepoSettings{}, a.budgetFor(task.ID), contractOverrides(c.prompt))
	if err := a.db.SaveTaskContract(ctx, task.ID, agreed); err != nil {
		return a.reportError(ctx, chatID, err)
	}
	accepted, err := a.contractGate(ctx, session, task, chatID, agreed)
	if err != nil {
		return a.reportError(ctx, chatID, err)
	}
	if !accepted {
		return nil
	}
	if err := a.beginExecution(ctx, task.ID); err != nil {
		return a.reportError(ctx, chatID, err)
	}

	taskCtx, taskCancel := context.WithTimeout(ctx, 20*time.Minute)
	defer taskCancel()

	if chat == nil {
		workDir, branch, err := a.git.CloneForTask(taskCtx, c.repo, task.ID.String())
		if err != nil {
			a.fail(taskCtx, session, task, chatID, fmt.Errorf("clone: %w", err))
			return nil
		}
		a.event(taskCtx, session.ID, &task.ID, "status", map[string]string{"state": "PREPARING", "branch": branch})
		handle, err := a.runner.Start(taskCtx, task.ID.String(), workDir)
		if err != nil {
			a.fail(taskCtx, session, task, chatID, fmt.Errorf("start container: %w", err))
			return nil
		}
		oc := &opencode.Client{BaseURL: handle.Address, Password: handle.Password}
		if err := runner.WaitHealthy(taskCtx, oc.Healthy, 60*time.Second); err != nil {
			a.runner.Stop(context.Background(), handle.ID)
			a.fail(taskCtx, session, task, chatID, fmt.Errorf("open code health: %w", err))
			return nil
		}
		ocSession, err := oc.CreateSession(taskCtx)
		if err != nil {
			a.runner.Stop(context.Background(), handle.ID)
			a.fail(taskCtx, session, task, chatID, fmt.Errorf("create agent session: %w", err))
			return nil
		}
		chat = &chatSession{
			repo: c.repo, mode: mode, session: session, handle: handle,
			oc: oc, ocSession: ocSession, lastUsed: time.Now(), busy: true,
			state: "running", lastTask: taskRecord{ID: task.ID.String(), Prompt: c.prompt, Outcome: "running"},
		}
		task.Branch = branch
		if err := a.db.SetTaskBranch(taskCtx, task.ID, branch); err != nil {
			log.Printf("gateway: record branch task %s: %s", task.ID, scrubForUser(err))
		}
		a.putChat(chatID, chat)
		if _, err := a.checkpoint(taskCtx, task.ID, session.ID, handle.ID); err != nil {
			log.Printf("gateway: baseline checkpoint task %s: %s", task.ID, scrubForUser(err))
		}
	} else if !chat.begin() {
		return a.reply(ctx, chatID, "I'm still working on the previous message. I'll pick this up next.")
	} else {
		chat.setProposal(nil)
		chat.setState("running")
		chat.setLastTask(taskRecord{ID: task.ID.String(), Prompt: c.prompt, Outcome: "running"})
	}
	defer chat.end()

	if err := a.beginExecution(taskCtx, task.ID); err != nil {
		return a.reportError(ctx, chatID, err)
	}
	stopCheckpoints := a.checkpointLoop(taskCtx, task.ID, session.ID, chat.handle.ID)
	summary, err := a.runTask(taskCtx, chat, session, task, chatID, agentName, agentPrompt, priorContext)
	stopCheckpoints()
	if err != nil {
		if errors.Is(err, errBudgetExceeded) || errors.Is(err, errOffLimits) {
			if _, cpErr := a.checkpoint(taskCtx, task.ID, session.ID, chat.handle.ID); cpErr != nil {
				log.Printf("gateway: abort checkpoint task %s: %s", task.ID, scrubForUser(cpErr))
			}
			a.closeChat(chatID)
		}
		a.clearBudgetStop(task.ID)
		a.fail(taskCtx, session, task, chatID, err)
		return nil
	}
	a.clearBudgetStop(task.ID)
	if _, err := a.checkpoint(taskCtx, task.ID, session.ID, chat.handle.ID); err != nil {
		log.Printf("gateway: final checkpoint task %s: %s", task.ID, scrubForUser(err))
	}
	// Keep the router-only conversation aware of workspace results so a
	// follow-up does not sound like the earlier request was never answered.
	a.getDirect(chatID).add("assistant", summary)

	// Publish only build-mode work that actually changed files. Chat replies,
	// research and plans finish locally; they never create approval cards.
	if mode == modeBuild && a.hasWorkspaceChanges(taskCtx, chat.handle.ID) {
		if err := a.publishTask(taskCtx, chat, session, task, chatID, c.prompt); err != nil {
			a.fail(taskCtx, session, task, chatID, err)
			return nil
		}
		chat.setState("awaiting_approval")
		chat.setLastTask(taskRecord{ID: task.ID.String(), Prompt: c.prompt, Outcome: "awaiting approval", Evidence: summary})
		chat.setProposal(nil)
		a.event(taskCtx, session.ID, &task.ID, "status", map[string]string{"state": "AWAITING_PUSH_APPROVAL"})
		return nil
	}

	if err := a.db.Transition(taskCtx, task.ID, store.StateExecuting, store.StateDone, ""); err != nil {
		return a.reportError(ctx, chatID, err)
	}
	chat.setState("done")
	chat.setLastTask(taskRecord{ID: task.ID.String(), Prompt: c.prompt, Outcome: "completed", Evidence: summary})
	// The final task card already carries the correct Execute/Revise buttons
	// when the reply contains a PROPOSAL; do not stack a second keyboard.
	if proposal := parseProposal(summary, task.ID.String()); proposal != nil {
		chat.setProposal(proposal)
	} else {
		chat.setProposal(nil)
	}
	a.event(taskCtx, session.ID, &task.ID, "done", map[string]string{"summary": summary})
	return nil
}

// beginExecution starts the agent from whichever state the contract gate
// left behind. Auto-approved tasks stay PREPARING; owner-approved contracts
// pass through AWAITING_PLAN_APPROVAL.
func (a *application) beginExecution(ctx context.Context, taskID uuid.UUID) error {
	if a == nil || a.db == nil {
		return errors.New("gateway: store is not configured")
	}
	current, err := a.db.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	var from store.State
	switch current.State {
	case store.StateAwaitingPlanApproval, store.StatePreparing:
		from = current.State
	case store.StateExecuting:
		return nil // idempotent: the pre-container call already advanced it
	default:
		return fmt.Errorf("%w: %s -> %s", store.ErrBadState, current.State, store.StateExecuting)
	}
	if err := a.db.Transition(ctx, taskID, from, store.StateExecuting, "agent_start"); err != nil &&
		!errors.Is(err, store.ErrStaleState) {
		return err
	}
	return nil
}

func (a *application) runTask(ctx context.Context, chat *chatSession, session store.Session, task store.Task, chatID int64, agentName, agentPrompt, priorContext string) (string, error) {
	feedCtx, feedCancel := context.WithCancel(ctx)
	defer feedCancel()
	rawEvents, err := chat.oc.Events(feedCtx)
	if err != nil {
		return "", err
	}
	normalized := make(chan opencode.Event, 128)
	abortHit := make(chan error, 1)
	go func() {
		defer close(normalized)
		signalAbort := func(cause error) {
			select {
			case abortHit <- cause:
			default:
			}
			feedCancel()
		}
		for raw := range rawEvents {
			ev, ok := opencode.Translate(raw)
			if !ok {
				continue
			}
			if ev.SessionID != "" && ev.SessionID != chat.ocSession {
				continue
			}
			if a.event(feedCtx, session.ID, &task.ID, ev.Type, map[string]string{
				"text": ev.Text, "tool": ev.Tool, "message": ev.Message,
			}) {
				signalAbort(errBudgetExceeded)
				return
			}
			if ev.Type == "file_change" {
				if violation := a.contractViolation(feedCtx, task.ID, ev.Text); violation != nil {
					signalAbort(violation)
					return
				}
			}
			select {
			case normalized <- ev:
			case <-feedCtx.Done():
				return
			}
			if ev.Terminal {
				return
			}
		}
	}()

	if priorContext != "" {
		agentPrompt = priorContext + "\n\n" + agentPrompt
	}
	if err := chat.oc.PromptWithAgent(ctx, chat.ocSession, agentPrompt, a.cfg.TaskModel(agentName), agentName); err != nil {
		return "", err
	}
	summary, err := a.renderer.RelayWithKeyboardFunc(ctx, chatID, session.ID, &task.ID, normalized, func(final string) [][]telegram.InlineButton {
		kind := "done"
		if parseProposal(final, task.ID.String()) != nil {
			kind = "decision"
		} else if agentName == "plan" {
			kind = "workspace"
		}
		return a.buttons(chatID, kind, task.ID.String())
	})
	select {
	case cause := <-abortHit:
		return "", cause
	default:
	}
	if err != nil {
		return "", err
	}
	return summary, nil
}

// reply sends escaped plain text. telegram.Client uses parse_mode=HTML, so
// raw angle brackets from user-supplied repo names would be rejected.
func (a *application) reply(ctx context.Context, chatID int64, text string) error {
	_, err := a.tg.SendMessage(ctx, chatID, html.EscapeString(text))
	return err
}

func (a *application) replyKeyboard(ctx context.Context, chatID int64, text string, rows [][]telegram.InlineButton) error {
	_, err := a.tg.SendMessageKeyboard(ctx, chatID, html.EscapeString(text), rows)
	return err
}

// ownerErrorText turns a small set of infra failures into an actionable card.
// Raw Docker, SQL and token details stay in the server log only.
func ownerErrorText(err error) string {
	if errors.Is(err, runner.ErrNetworkNotInternal) {
		return "Sandbox check failed: the private session network is not isolated. " +
			"Nothing was changed or started. The operator has been alerted; retry after the network is recreated."
	}
	return "I couldn't start that task. Check the repo name and try again, or send /help."
}

// reportError tells the owner what happened and marks the update as handled so
// the poller does not replay it. Details stay in the server log.
func (a *application) reportError(ctx context.Context, chatID int64, err error) error {
	log.Printf("gateway: update failed: %v", scrubForUser(err))
	if _, sendErr := a.tg.SendMessage(ctx, chatID, html.EscapeString(ownerErrorText(err))); sendErr != nil {
		return fmt.Errorf("telegram: send error reply: %w", sendErr)
	}
	return &telegram.ReportedError{Err: err}
}

func (a *application) fail(ctx context.Context, session store.Session, task store.Task, chatID int64, cause error) {
	reason := scrubForUser(cause)
	log.Printf("gateway: task %s failed: %s", task.ID, reason)
	if chat := a.getChat(chatID); chat != nil {
		chat.setProposal(nil)
		chat.setOutcome("failed: " + reason)
		chat.setState("failed")
	}
	a.event(ctx, session.ID, &task.ID, "error", map[string]string{"message": reason})
	current, err := a.db.GetTask(ctx, task.ID)
	if err != nil {
		log.Printf("gateway: load task %s: %v", task.ID, err)
	} else if current.State == store.StateQueued || current.State == store.StatePreparing || current.State == store.StateExecuting {
		if err := a.db.Transition(ctx, task.ID, current.State, store.StateFailed, reason); err != nil {
			log.Printf("gateway: fail task %s: %v", task.ID, err)
		}
	}
	msg := "Task failed: " + reason
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if err := a.replyKeyboard(ctx, chatID, msg, a.buttonsForChat(chatID)); err != nil {
		log.Printf("gateway: send failure: %v", err)
	}
}

func scrubForUser(err error) string {
	if err == nil {
		return ""
	}
	return userFacingSecret.ReplaceAllString(err.Error(), "[REDACTED]")
}

func validMode(mode string) bool {
	switch mode {
	case modeBuild, modePlan, modeResearch:
		return true
	default:
		return false
	}
}

func modeAgent(mode string) string {
	if mode == modePlan || mode == modeResearch {
		return "plan" // built-in primary agent; edits are denied in config
	}
	return "build"
}

// modePrompt prefixes the owner's text so the shared plan agent can behave as
// a researcher without relying on an unsupported custom agent definition.
func modePrompt(mode, prompt string) string {
	switch mode {
	case modePlan:
		return "PLAN MODE: Investigate first. Do not edit files. End with exactly one whole line beginning `PROPOSAL: <goal; files; steps; risk>`. If a file-changing choice is blocked, ask one concrete question instead.\n\n" + chatStyle + prompt
	case modeResearch:
		return "RESEARCH MODE: Read and search only. Cite sources. Do not edit files, push, or open pull requests. Do not emit a PROPOSAL unless the owner asked for one.\n\n" + chatStyle + prompt
	default:
		return chatStyle + prompt
	}
}

// taskAgent keeps every file-changing turn read-only until the owner approves
// a PROPOSAL. Quick read-only actions use the plan agent for the same reason.
func taskAgent(mode string, c command) string {
	if mode != modeBuild {
		return modeAgent(mode)
	}
	if c.approved {
		return modeAgent(modeBuild)
	}
	return modeAgent(modePlan) // plan agent has edit/write tools denied
}

// taskPrompt chooses the contract for this turn: plan first, read-only
// answer, or an already-approved implementation.
func taskPrompt(mode string, c command) string {
	if mode != modeBuild {
		return modePrompt(mode, c.prompt)
	}
	if c.approved {
		return chatStyle + c.prompt
	}
	if c.readOnly {
		return "READ-ONLY MODE: Use the existing workspace context. Do not edit files. Do not emit a PROPOSAL unless the owner asked for a plan.\n\n" + chatStyle + c.prompt
	}
	return modePrompt(modePlan, c.prompt)
}

func modeVerb(mode string) string {
	switch mode {
	case modePlan:
		return "plan before editing"
	case modeResearch:
		return "research and report"
	default:
		return "build"
	}
}

func modeFromCommand(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return ""
	}
	switch strings.ToLower(strings.TrimPrefix(fields[0], "/")) {
	case "build":
		return modeBuild
	case "plan":
		return modePlan
	case "research":
		return modeResearch
	default:
		return ""
	}
}

func modePromptRest(text string) string {
	if i := strings.IndexByte(text, ' '); i >= 0 {
		return strings.TrimSpace(text[i+1:])
	}
	return ""
}

// parseProposal reads the agent's structured ask-before-acting line. Only a
// whole line beginning with PROPOSAL: counts, so prose is never misinterpreted.
func parseProposal(summary, taskID string) *proposalRecord {
	for _, line := range strings.Split(summary, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 9 || !strings.EqualFold(line[:9], "PROPOSAL:") {
			continue
		}
		body := strings.TrimSpace(line[9:])
		if body == "" {
			return nil
		}
		return &proposalRecord{
			TaskID: taskID,
			Prompt: "The owner approved this exact plan. Do not ask for approval again. Execute this approved plan in this workspace: " + body +
				". Make only these changes, run the narrowest relevant checks, and report in two sentences. Do not push.",
		}
	}
	return nil
}

// actionPrompt turns a quick action into a fully specified task. The previous
// task's own prompt is injected, so the agent never has to guess the referent.
func actionPrompt(action string, t taskRecord) (string, bool) {
	switch action {
	case "checks":
		return "Run the narrowest relevant checks for this request: " + t.Prompt + ". Report the result in two sentences. Do not push.", true
	case "explain":
		return "Explain the change made for this request: " + t.Prompt + ". Two or three sentences with one concrete next step. Do not push.", true
	case "continue":
		return "Continue this request: " + t.Prompt + ". It ended as " + t.Outcome + ". Finish the local work only; do not push.", true
	case "diff":
		return "List the exact files changed for this request: " + t.Prompt + ". Summarise them in two sentences. Do not push.", true
	case "summary":
		return "In two or three sentences, summarise the state of this request: " + t.Prompt + ". Suggest one next step.", true
	case "revise":
		return "Revise the plan for this request: " + t.Prompt + ". Return a new PROPOSAL line only. Do not edit files.", true
	default:
		return "", false
	}
}

// buttonSpec is one button before its token is bound. Static buttons are
// informational; action buttons receive a fresh, single-use token.
type buttonSpec struct {
	Text   string
	Static string
	Action string
}

// buttonLayout returns only actions legal in the current task state. A stale
// keyboard therefore cannot offer a step that belongs to another stage.
func buttonLayout(kind, taskID string) [][]buttonSpec {
	if taskID == "" {
		return [][]buttonSpec{{
			{Text: "How to start", Static: "help"},
			{Text: "Status", Static: "status"},
		}}
	}
	act := func(label, action string) buttonSpec {
		return buttonSpec{Text: label, Action: action}
	}
	static := func(label, data string) buttonSpec {
		return buttonSpec{Text: label, Static: data}
	}
	switch kind {
	case "running", "awaiting_approval", "awaiting_plan_approval":
		return [][]buttonSpec{{static("Status", "status"), static("New topic", "new")}}
	case "decision":
		return [][]buttonSpec{{act("Execute", "execute"), act("Revise", "revise"), static("New topic", "new")}}
	case "done":
		return [][]buttonSpec{
			{act("Run checks", "checks"), act("Explain this", "explain"), act("Show diff", "diff")},
			{act("Undo last", "undo"), static("New topic", "new")},
		}
	case "failed":
		return [][]buttonSpec{
			{act("Continue locally", "continue"), act("Run checks", "checks"), act("Undo last", "undo")},
			{static("New topic", "new")},
		}
	default:
		return [][]buttonSpec{
			{act("Summarize", "summary"), act("Run checks", "checks")},
			{static("New topic", "new")},
		}
	}
}

// buttons binds a state-specific layout to short-lived callback tokens.
func (a *application) buttons(chatID int64, kind, taskID string) [][]telegram.InlineButton {
	layout := buttonLayout(kind, taskID)
	rows := make([][]telegram.InlineButton, 0, len(layout))
	for _, row := range layout {
		buttons := make([]telegram.InlineButton, 0, len(row))
		for _, spec := range row {
			button := telegram.InlineButton{Text: spec.Text, CallbackData: spec.Static}
			if spec.Action != "" {
				label := "Running " + spec.Text
				switch spec.Action {
				case "execute":
					label = "Executing the approved plan"
				case "undo":
					label = "Rewinding the workspace"
				}
				button.CallbackData = a.issueTap(buttonTap{
					ChatID: chatID, TaskID: taskID, Action: spec.Action, Label: label, Kind: kind,
				})
			}
			buttons = append(buttons, button)
		}
		rows = append(rows, buttons)
	}
	return rows
}

func (a *application) buttonsForChat(chatID int64) [][]telegram.InlineButton {
	chat := a.getChat(chatID)
	if chat == nil {
		return a.buttons(chatID, "empty", "")
	}
	_, _, _, _, busy := chat.detail()
	state := chat.stateSnapshot()
	if busy {
		state = "running"
	} else if chat.hasProposal() {
		state = "decision"
	} else if state != "failed" && state != "awaiting_approval" && state != "awaiting_plan_approval" {
		state = "workspace"
	}
	return a.buttons(chatID, state, chat.lastTaskID())
}
