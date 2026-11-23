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
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/gitops"
	"github.com/jerryjuche/padi/internal/gateway/opencode"
	"github.com/jerryjuche/padi/internal/gateway/runner"
	"github.com/jerryjuche/padi/internal/gateway/store"
	"github.com/jerryjuche/padi/internal/gateway/telegram"
)

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
	// M1 temporary deviation: a throwaway free-tier key arrives via env only.
	modelKey, err := secretFromEnv("PADI_MODEL_API_KEY", "throwaway model key")
	if err != nil {
		return err
	}
	modelBaseURL := os.Getenv("PADI_MODEL_BASE_URL")
	if modelBaseURL == "" {
		modelBaseURL = cfg.OpenCode.ModelBaseURL
	}

	docker, err := runner.NewDocker(os.Getenv("DOCKER_HOST"))
	if err != nil {
		return err
	}
	r := runner.New(docker, runner.Config{
		Image:        cfg.Container.Image,
		Network:      cfg.Container.Network,
		MemoryMB:     cfg.Container.MemoryMB,
		CPUs:         cfg.Container.CPUs,
		Pids:         cfg.Container.Pids,
		Model:        cfg.OpenCode.Model,
		ModelKeyEnv:  cfg.OpenCode.ModelKeyEnv,
		ModelKey:     modelKey,
		ModelBaseURL: modelBaseURL,
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
	}
	poller := &telegram.Poller{
		Client:  tg,
		Store:   db,
		OwnerID: ownerID,
		Handle:  app.handleUpdate,
	}
	go func() {
		if err := poller.Run(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("gateway: poller stopped: %v", err)
			cancel()
		}
	}()
	log.Printf("gateway: M1 spine ready (model=%s, network=%s)", cfg.OpenCode.Model, cfg.Container.Network)
	<-ctx.Done()
	return nil
}

type application struct {
	cfg      Config
	db       *store.PG
	runner   *runner.Runner
	git      *gitops.Git
	tg       *telegram.Client
	renderer *telegram.Renderer
}

func (a *application) event(ctx context.Context, sessionID uuid.UUID, taskID *uuid.UUID, typ string, data any) {
	if _, err := a.db.AppendEvent(ctx, sessionID, taskID, typ, data); err != nil {
		log.Printf("gateway: append event %s: %v", typ, err)
	}
}

// parseRepo accepts "clone <repo> and <prompt>" or the configured default.
func (a *application) parseRepo(text string) (repo, prompt string) {
	fields := strings.Fields(text)
	if len(fields) >= 2 && strings.EqualFold(fields[0], "clone") {
		return fields[1], text
	}
	return a.cfg.Git.DefaultRepo, text
}

func (a *application) handleUpdate(ctx context.Context, u telegram.Update) error {
	if u.Message == nil {
		return nil
	}
	chatID := u.Message.Chat.ID
	text := strings.TrimSpace(u.Message.Text)
	if text == "" {
		return nil
	}
	if strings.HasPrefix(text, "/status") || strings.HasPrefix(text, "/help") {
		_, err := a.tg.SendMessage(ctx, chatID, html.EscapeString("M1 spine: send 'clone <repo> and <prompt>' to start one task."))
		return err
	}

	repo, prompt := a.parseRepo(text)
	if repo == "" {
		_, err := a.tg.SendMessage(ctx, chatID, html.EscapeString("No repository. Use: clone <repo> and <prompt>"))
		return err
	}

	session, err := a.db.CreateSession(ctx, "telegram", fmt.Sprintf("tg-%d", chatID))
	if err != nil {
		return err
	}
	task, err := a.db.CreateTask(ctx, store.NewTask{
		SessionID:      session.ID,
		Repo:           repo,
		Prompt:         prompt,
		IdempotencyKey: fmt.Sprintf("tg-%d-%d", chatID, u.Message.MessageID),
	})
	if err != nil {
		return err
	}
	a.event(ctx, session.ID, &task.ID, "status", map[string]string{"state": "QUEUED"})
	if err := a.db.Transition(ctx, task.ID, store.StateQueued, store.StatePreparing, ""); err != nil {
		return err
	}

	taskCtx, taskCancel := context.WithTimeout(ctx, 20*time.Minute)
	defer taskCancel()

	workDir, branch, err := a.git.CloneForTask(taskCtx, repo, task.ID.String())
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
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		if err := a.runner.Stop(stopCtx, handle.ID); err != nil {
			log.Printf("gateway: stop container %s: %v", handle.ID, err)
		}
	}()

	if err := a.db.Transition(taskCtx, task.ID, store.StatePreparing, store.StateExecuting, ""); err != nil {
		return err
	}
	summary, err := a.runTask(taskCtx, session, task, chatID, handle, prompt)
	if err != nil {
		a.fail(taskCtx, session, task, chatID, err)
		return nil
	}
	if err := a.db.Transition(taskCtx, task.ID, store.StateExecuting, store.StateDone, ""); err != nil {
		return err
	}
	a.event(taskCtx, session.ID, &task.ID, "done", map[string]string{"summary": summary})
	return nil
}

func (a *application) runTask(ctx context.Context, session store.Session, task store.Task, chatID int64, handle runner.Handle, prompt string) (string, error) {
	oc := &opencode.Client{BaseURL: handle.Address, Password: handle.Password}
	if err := runner.WaitHealthy(ctx, oc.Healthy, 60*time.Second); err != nil {
		return "", err
	}
	rawEvents, err := oc.Events(ctx)
	if err != nil {
		return "", err
	}
	ocSession, err := oc.CreateSession(ctx)
	if err != nil {
		return "", err
	}
	normalized := make(chan opencode.Event, 128)
	feedCtx, feedCancel := context.WithCancel(ctx)
	go func() {
		defer close(normalized)
		defer feedCancel()
		for raw := range rawEvents {
			ev, ok := opencode.Translate(raw)
			if !ok {
				continue
			}
			if ev.SessionID != "" && ev.SessionID != ocSession {
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
	defer feedCancel()

	if err := oc.Prompt(ctx, ocSession, prompt, a.cfg.OpenCode.Model); err != nil {
		return "", err
	}
	summary, err := a.renderer.Relay(ctx, chatID, session.ID, &task.ID, normalized)
	if err != nil {
		return "", err
	}
	return summary, nil
}

func (a *application) fail(ctx context.Context, session store.Session, task store.Task, chatID int64, cause error) {
	log.Printf("gateway: task %s failed: %v", task.ID, cause)
	a.event(ctx, session.ID, &task.ID, "error", map[string]string{"message": cause.Error()})
	current, err := a.db.GetTask(ctx, task.ID)
	if err != nil {
		log.Printf("gateway: load task %s: %v", task.ID, err)
	} else if current.State == store.StateQueued || current.State == store.StatePreparing || current.State == store.StateExecuting {
		if err := a.db.Transition(ctx, task.ID, current.State, store.StateFailed, cause.Error()); err != nil {
			log.Printf("gateway: fail task %s: %v", task.ID, err)
		}
	}
	msg := "Task failed: " + cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if _, err := a.tg.SendMessage(ctx, chatID, msg); err != nil {
		log.Printf("gateway: send failure: %v", err)
	}
}
