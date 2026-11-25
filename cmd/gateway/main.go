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
	chatStyle = "Answer the current request directly in 2-4 short sentences: the result, what you verified, then one concrete next step tied to this request. Do not open with a capability disclaimer or repeat a canned status line. Never re-ask a settled question or restate an earlier answer; use what you already learned in this conversation. Ask a question only when a file-changing choice is genuinely blocked, and offer concrete options when you do. If a next step needs approval, end with exactly one line `PROPOSAL: <goal; files; steps; risk>`; the owner will get an Execute button carrying that exact plan. Never mention tool parameter names such as oldString, newString or replaceAll. Publishing and pull requests happen only after verification and owner approval; finish the local work first. Keep replies under 600 characters.\n\n"
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
	}
	go approvalManager.ExpireLoop(ctx)
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
	log.Printf("gateway: ready (model=%s, network=%s)", cfg.OpenCode.Model, cfg.Container.Network)
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

func (c *chatSession) detail() (repo, mode string, sessionID uuid.UUID, lastUsed time.Time, busy bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.repo, c.mode, c.session.ID, c.lastUsed, c.busy
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

// command is the parsed form of one owner message.
type command struct {
	repo           string
	prompt         string
	askRepo        bool
	awaitingPrompt bool
}

// parseCommand supports a short conversation:
//
//	clone owner/repo and <prompt>  -> start immediately
//	clone owner/repo               -> remember repo, ask what to do next
//	<prompt>                       -> use the remembered or default repo
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
	if a.cfg.Git.DefaultRepo != "" {
		return command{repo: a.cfg.Git.DefaultRepo, prompt: text}
	}
	return command{askRepo: true}
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
		return a.continueChat(ctx, chatID, rest, fmt.Sprintf("tg-%d-%d", chatID, u.Message.MessageID), mode)
	}

	// A plain message in an active workspace is a follow-up: same repo, same
	// OpenCode session, same container. No clone, no repeated setup.
	active := a.getChat(chatID)
	if active != nil && !strings.HasPrefix(strings.ToLower(text), "clone ") {
		return a.continueChat(ctx, chatID, text, fmt.Sprintf("tg-%d-%d", chatID, u.Message.MessageID), a.currentMode(chatID))
	}

	c := a.parseCommand(chatID, text)
	if c.askRepo {
		return a.reply(ctx, chatID, "Which repo? Send 'clone owner/repo' (a GitHub URL works too) and I'll ask what to do next.")
	}
	if c.awaitingPrompt {
		a.rememberRepo(chatID, c.repo)
		return a.reply(ctx, chatID, "Got "+c.repo+". What should I do in it?")
	}
	fresh := active == nil
	return a.startTask(ctx, chatID, c, fmt.Sprintf("tg-%d-%d", chatID, u.Message.MessageID), fresh)
}

// continueChat starts a task on an existing workspace, or falls back to normal
// repo selection when no workspace is active (e.g. /plan followed by a prompt).
func (a *application) continueChat(ctx context.Context, chatID int64, prompt, key, mode string) error {
	if chat := a.getChat(chatID); chat != nil {
		repo, _, _, _, _ := chat.detail()
		return a.startTask(ctx, chatID, command{repo: repo, prompt: prompt}, key, false)
	}
	c := a.parseCommand(chatID, prompt)
	if c.askRepo {
		return a.reply(ctx, chatID, "Which repo should I use? Send 'clone owner/repo' first.")
	}
	return a.startTask(ctx, chatID, c, key, true)
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
		a.rememberMode(chatID, modeBuild)
		return a.replyToCallback(ctx, q, "New topic. Tell me what you'd like to work on, or send clone <repo> to open a repository.", a.buttons(chatID, "empty", ""))
	case data == "status":
		return a.replyToCallback(ctx, q, a.statusText(chatID), a.buttonsForChat(chatID))
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
		return a.startTask(ctx, chatID, command{repo: repo, prompt: p.Prompt}, "exec:"+taskID, false)
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
		return a.startTask(ctx, chatID, command{repo: repo, prompt: p.Prompt}, "tap:"+token, false)
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
	return a.startTask(ctx, chatID, command{repo: repo, prompt: prompt}, key, false)
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
		"Just write naturally. I keep the repository and conversation open between messages, so you can ask follow-ups or ask me to clarify anything.\n\n" +
		"Commands:\n" +
		"• clone <repo> — open a GitHub repo (URLs work)\n" +
		"• /build — implement changes (default)\n" +
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

func (a *application) statusText(chatID int64) string {
	chat := a.getChat(chatID)
	text := "No active workspace. Send a request, or clone <repo> to open a repository."
	if chat != nil {
		repo, mode, sessionID, last, busy := chat.detail()
		state := "idle"
		if busy {
			state = "working"
		}
		text = fmt.Sprintf("Workspace: %s\nMode: %s\nState: %s\nSession: %s\nLast used: %s ago",
			repo, mode, state, sessionID.String(), time.Since(last).Round(time.Second))
	}
	return text
}

func (a *application) status(ctx context.Context, chatID int64) error {
	return a.replyKeyboard(ctx, chatID, a.statusText(chatID), a.buttonsForChat(chatID))
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

// startTask creates the audit task and either opens a fresh workspace or reuses
// an existing one. The OpenCode session is preserved across follow-ups.
func (a *application) startTask(ctx context.Context, chatID int64, c command, key string, fresh bool) error {
	if c.repo == "" || c.prompt == "" {
		return a.reply(ctx, chatID, "I need a repo and a request. Try 'clone owner/repo and add a README'.")
	}
	mode := a.currentMode(chatID)
	chat := a.getChat(chatID)
	if fresh && chat != nil {
		a.closeChat(chatID)
		chat = nil
	}
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
	a.event(ctx, session.ID, &task.ID, "status", map[string]string{"state": "QUEUED", "mode": mode})
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
	summary, err := a.runTask(taskCtx, chat, session, task, chatID, c.prompt, mode, priorContext)
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
	switch current.State {
	case store.StateAwaitingPlanApproval:
		return a.db.Transition(ctx, taskID, store.StateAwaitingPlanApproval, store.StateExecuting, "")
	case store.StatePreparing:
		return a.db.Transition(ctx, taskID, store.StatePreparing, store.StateExecuting, "")
	default:
		return fmt.Errorf("%w: %s -> %s", store.ErrBadState, current.State, store.StateExecuting)
	}
}

func (a *application) runTask(ctx context.Context, chat *chatSession, session store.Session, task store.Task, chatID int64, prompt, mode, priorContext string) (string, error) {
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

	agentPrompt := modePrompt(mode, prompt)
	if priorContext != "" {
		agentPrompt = priorContext + "\n\n" + agentPrompt
	}
	if err := chat.oc.PromptWithAgent(ctx, chat.ocSession, agentPrompt, a.cfg.OpenCode.Model, modeAgent(mode)); err != nil {
		return "", err
	}
	summary, err := a.renderer.RelayWithKeyboardFunc(ctx, chatID, session.ID, &task.ID, normalized, func(final string) [][]telegram.InlineButton {
		kind := "done"
		if parseProposal(final, task.ID.String()) != nil {
			kind = "decision"
		} else if mode == modePlan || mode == modeResearch {
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

// reportError tells the owner what happened and marks the update as handled so
// the poller does not replay it. Details stay in the server log.
func (a *application) reportError(ctx context.Context, chatID int64, err error) error {
	log.Printf("gateway: update failed: %v", scrubForUser(err))
	message := "I couldn't start that task. Check the repo name and try again, or send /help."
	if _, sendErr := a.tg.SendMessage(ctx, chatID, html.EscapeString(message)); sendErr != nil {
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
		return "PLAN MODE: Investigate first. Return a short plan with files, steps, tests and risks. Do not edit files.\n\n" + chatStyle + prompt
	case modeResearch:
		return "RESEARCH MODE: Read and search only. Cite sources. Do not edit files, push, or open pull requests.\n\n" + chatStyle + prompt
	default:
		return chatStyle + prompt
	}
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
			Prompt: "Execute this approved plan in this workspace: " + body +
				". Make the changes, run the narrowest relevant checks, and report in two sentences. Do not push.",
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
