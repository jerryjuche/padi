// Command gateway is the M1 Spine: Telegram -> container -> OpenCode -> Telegram.
package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/gitops"
	"github.com/jerryjuche/padi/internal/gateway/opencode"
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
	chatStyle = "Reply in 2-4 short sentences: the answer, what you verified or the M1 limit, then one next step tied to this request. Ask at most one natural question, and only when the choice changes files or behaviour. Never mention tool parameter names such as oldString, newString or replaceAll. M1 cannot push or open a PR: say so once, then finish the local work. Keep replies under 600 characters.\n\n"
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
		Image:         cfg.Container.Image,
		Network:       cfg.Container.Network,
		MemoryMB:      cfg.Container.MemoryMB,
		CPUs:          cfg.Container.CPUs,
		Pids:          cfg.Container.Pids,
		Model:         cfg.OpenCode.Model,
		RouterToken:   routerToken,
		RouterBaseURL: routerBaseURL,
	})
	git := &gitops.Git{CacheDir: cfg.Git.CacheDir, WorkspacesDir: cfg.Git.WorkspacesDir}
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
		pending: make(map[int64]string),
		modes:   make(map[int64]string),
		chats:   make(map[int64]*chatSession),
	}
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

type application struct {
	cfg      Config
	db       *store.PG
	runner   *runner.Runner
	git      *gitops.Git
	tg       *telegram.Client
	renderer *telegram.Renderer

	// pending remembers a repo chosen by "clone <repo>" for the next message.
	// modes is a sticky build/plan/research choice per chat.
	mu      sync.Mutex
	pending map[int64]string
	modes   map[int64]string
	chats   map[int64]*chatSession
}

func (a *application) event(ctx context.Context, sessionID uuid.UUID, taskID *uuid.UUID, typ string, data any) {
	if _, err := a.db.AppendEvent(ctx, sessionID, taskID, typ, data); err != nil {
		log.Printf("gateway: append event %s: %v", typ, scrubForUser(err))
	}
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
	case strings.HasPrefix(text, "/new"):
		a.closeChat(chatID)
		a.forgetRepo(chatID)
		a.rememberMode(chatID, modeBuild)
		return a.replyKeyboard(ctx, chatID, "New topic. Tell me what you'd like to work on, or send clone <repo> to open a repository.", contextButtons("empty"))
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
			return a.replyKeyboard(ctx, chatID, mode+" mode on. Tell me what you want me to "+modeVerb(mode)+".", contextButtons(a.contextKind(chatID)))
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
	if err := a.tg.AnswerCallbackQuery(ctx, q.ID, ""); err != nil {
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
		return a.replyKeyboard(ctx, chatID, mode+" mode on. Tell me what you want me to "+modeVerb(mode)+".", contextButtons(a.contextKind(chatID)))
	case data == "new":
		a.closeChat(chatID)
		a.forgetRepo(chatID)
		a.rememberMode(chatID, modeBuild)
		return a.replyKeyboard(ctx, chatID, "New topic. Tell me what you'd like to work on, or send clone <repo> to open a repository.", contextButtons("empty"))
	case data == "status":
		return a.status(ctx, chatID)
	case data == "help":
		return a.help(ctx, chatID)
	case strings.HasPrefix(data, "act:"):
		chat := a.getChat(chatID)
		if chat == nil {
			return a.reply(ctx, chatID, "No active workspace yet. Send a request or clone <repo> first.")
		}
		repo, _, _, _, _ := chat.detail()
		prompts := map[string]string{
			"act:summary":  "In two or three sentences, summarize the current state and suggest one next step.",
			"act:checks":   "Run the narrowest relevant checks in this workspace and report the result in two sentences. Do not push.",
			"act:explain":  "Explain the most recent change in two or three sentences, with one concrete next step. Do not push.",
			"act:continue": "Continue the local work that was in progress. Finish what M1 allows and report the result in two sentences. Do not push.",
		}
		prompt, ok := prompts[data]
		if !ok {
			return &telegram.ReportedError{Err: fmt.Errorf("unknown action %q", data)}
		}
		return a.startTask(ctx, chatID, command{repo: repo, prompt: prompt}, fmt.Sprintf("tg-%d-cb-%s", chatID, q.ID), false)
	default:
		return &telegram.ReportedError{Err: fmt.Errorf("unknown callback %q", data)}
	}
}

func (a *application) help(ctx context.Context, chatID int64) error {
	text := "Padi chat\n\n" +
		"Just write naturally. I keep the repository and conversation open between messages, so you can ask follow-ups or ask me to clarify anything.\n\n" +
		"Commands:\n" +
		"• clone <repo> — open a GitHub repo (URLs work)\n" +
		"• /build — implement changes (default)\n" +
		"• /plan — investigate and propose a plan before editing\n" +
		"• /research — read, search and report; no edits\n" +
		"• /status — show the active workspace\n" +
		"• /new — close this workspace and start fresh\n\n" +
		"Buttons offer next steps for this conversation. Use /build, /plan or /research when you want to switch mode."
	return a.replyKeyboard(ctx, chatID, text, contextButtons(a.contextKind(chatID)))
}

func (a *application) status(ctx context.Context, chatID int64) error {
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
	return a.replyKeyboard(ctx, chatID, text, contextButtons(a.contextKind(chatID)))
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

	task, err := a.db.CreateTask(ctx, store.NewTask{
		SessionID: session.ID, Repo: c.repo, Prompt: c.prompt, IdempotencyKey: key,
	})
	if err != nil {
		return a.reportError(ctx, chatID, err)
	}
	if task.State == store.StateDone || task.State == store.StateFailed {
		return a.reply(ctx, chatID, "I already handled that message.")
	}
	a.event(ctx, session.ID, &task.ID, "status", map[string]string{"state": "QUEUED", "mode": mode})
	if err := a.db.Transition(ctx, task.ID, store.StateQueued, store.StatePreparing, ""); err != nil {
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
		}
		a.putChat(chatID, chat)
		a.reply(taskCtx, chatID, "Working in "+c.repo+"...")
	} else if !chat.begin() {
		return a.reply(ctx, chatID, "I'm still working on the previous message. I'll pick this up next.")
	}
	defer chat.end()

	if err := a.db.Transition(taskCtx, task.ID, store.StatePreparing, store.StateExecuting, ""); err != nil {
		return a.reportError(ctx, chatID, err)
	}
	summary, err := a.runTask(taskCtx, chat, session, task, chatID, c.prompt, mode)
	if err != nil {
		a.fail(taskCtx, session, task, chatID, err)
		return nil
	}
	if err := a.db.Transition(taskCtx, task.ID, store.StateExecuting, store.StateDone, ""); err != nil {
		return a.reportError(ctx, chatID, err)
	}
	a.event(taskCtx, session.ID, &task.ID, "done", map[string]string{"summary": summary})
	return nil
}

func (a *application) runTask(ctx context.Context, chat *chatSession, session store.Session, task store.Task, chatID int64, prompt, mode string) (string, error) {
	feedCtx, feedCancel := context.WithCancel(ctx)
	defer feedCancel()
	rawEvents, err := chat.oc.Events(feedCtx)
	if err != nil {
		return "", err
	}
	normalized := make(chan opencode.Event, 128)
	go func() {
		defer close(normalized)
		for raw := range rawEvents {
			ev, ok := opencode.Translate(raw)
			if !ok {
				continue
			}
			if ev.SessionID != "" && ev.SessionID != chat.ocSession {
				continue
			}
			a.event(feedCtx, session.ID, &task.ID, ev.Type, map[string]string{
				"text": ev.Text, "tool": ev.Tool, "message": ev.Message,
			})
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

	if err := chat.oc.PromptWithAgent(ctx, chat.ocSession, modePrompt(mode, prompt), a.cfg.OpenCode.Model, modeAgent(mode)); err != nil {
		return "", err
	}
	summary, err := a.renderer.RelayWithKeyboard(ctx, chatID, session.ID, &task.ID, normalized, contextButtons("done"))
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
	if err := a.replyKeyboard(ctx, chatID, msg, contextButtons("failed")); err != nil {
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

// contextButtons offers next steps that fit the current conversation instead
// of a permanent mode-only menu. Mode commands remain available as text.
func contextButtons(kind string) [][]telegram.InlineButton {
	switch kind {
	case "empty":
		return [][]telegram.InlineButton{{
			{Text: "How to start", CallbackData: "help"},
			{Text: "Status", CallbackData: "status"},
		}}
	case "done":
		return [][]telegram.InlineButton{{
			{Text: "Explain this", CallbackData: "act:explain"},
			{Text: "Run checks", CallbackData: "act:checks"},
			{Text: "New topic", CallbackData: "new"},
		}}
	case "failed":
		return [][]telegram.InlineButton{{
			{Text: "Continue locally", CallbackData: "act:continue"},
			{Text: "Status", CallbackData: "status"},
			{Text: "New topic", CallbackData: "new"},
		}}
	default:
		return [][]telegram.InlineButton{{
			{Text: "Summarize", CallbackData: "act:summary"},
			{Text: "Run checks", CallbackData: "act:checks"},
			{Text: "New topic", CallbackData: "new"},
		}}
	}
}

func (a *application) contextKind(chatID int64) string {
	if a.getChat(chatID) == nil {
		return "empty"
	}
	return "workspace"
}
