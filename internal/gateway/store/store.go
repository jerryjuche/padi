// Package store holds the minimal M1 persistence layer: sessions, tasks and
// events. It deliberately implements only the subset of the full Store
// interface that the M1 spine needs.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// State is a task state. Transition is the only legal way to change it.
type State string

const (
	StateQueued               State = "QUEUED"
	StatePreparing            State = "PREPARING"
	StatePlanning             State = "PLANNING"
	StateAwaitingPlanApproval State = "AWAITING_PLAN_APPROVAL"
	StateExecuting            State = "EXECUTING"
	StateVerifying            State = "VERIFYING"
	StateFixing               State = "FIXING"
	StateAwaitingPushApproval State = "AWAITING_PUSH_APPROVAL"
	StatePushing              State = "PUSHING"
	StatePROpened             State = "PR_OPENED"
	StatePreviewing           State = "PREVIEWING"
	StateDone                 State = "DONE"
	StateFailed               State = "FAILED"
	StateCancelled            State = "CANCELLED"
	StateTimedOut             State = "TIMED_OUT"
)

// terminalStates are fixed by BUILD_SPEC.md §5.1. No transition may leave one.
var terminalStates = map[State]bool{
	StateDone: true, StateFailed: true, StateCancelled: true, StateTimedOut: true,
}

var allowedTransitions = map[State]map[State]bool{
	StateQueued:               {StatePreparing: true, StateCancelled: true},
	StatePreparing:            {StatePlanning: true, StateAwaitingPlanApproval: true, StateExecuting: true, StateFailed: true, StateCancelled: true},
	StatePlanning:             {StateAwaitingPlanApproval: true, StateExecuting: true, StateFailed: true, StateCancelled: true, StateTimedOut: true},
	StateAwaitingPlanApproval: {StateExecuting: true, StateCancelled: true, StateTimedOut: true},
	// Direct EXECUTING -> DONE is retained for read-only M1 plan/research and
	// quick-action tasks that never enter the publish pipeline.
	StateExecuting:            {StateVerifying: true, StateDone: true, StateFailed: true, StateCancelled: true, StateTimedOut: true},
	StateVerifying:            {StateAwaitingPushApproval: true, StateFixing: true, StateFailed: true, StateCancelled: true, StateTimedOut: true},
	StateFixing:               {StateExecuting: true, StateFailed: true},
	StateAwaitingPushApproval: {StatePushing: true, StateExecuting: true, StateCancelled: true, StateTimedOut: true},
	StatePushing:              {StatePROpened: true, StateFailed: true},
	StatePROpened:             {StatePreviewing: true, StateDone: true},
	StatePreviewing:           {StateDone: true, StateFailed: true},
}

var (
	ErrStaleState  = errors.New("stale task state")
	ErrBadState    = errors.New("invalid state transition")
	ErrNotFound    = errors.New("not found")
	ErrIDakeyEmpty = errors.New("idempotency key is required")
)

// Session mirrors the M0 agent.sessions table.
type Session struct {
	ID        uuid.UUID
	CreatedAt time.Time
	Channel   string
	Title     string
	Mode      string
	LastSeq   int64
}

// Task mirrors the subset of agent.tasks used in M1.
type Task struct {
	ID         uuid.UUID
	SessionID  uuid.UUID
	Repo       string
	Branch     string
	State      State
	FailReason string
	Prompt     string
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// NewTask is the input to CreateTask.
type NewTask struct {
	SessionID      uuid.UUID
	Repo           string
	Prompt         string
	IdempotencyKey string
}

// Event is a persisted session event.
type Event struct {
	SessionID uuid.UUID
	Seq       int64
	TaskID    *uuid.UUID
	Type      string
	Data      json.RawMessage
	TS        time.Time
}

// Store is the M1 subset of the full gateway Store interface.
type Store interface {
	Migrate(ctx context.Context) error
	CreateSession(ctx context.Context, channel, title string) (Session, error)
	GetSession(ctx context.Context, sessionID uuid.UUID) (Session, error)
	AppendEvent(ctx context.Context, sessionID uuid.UUID, taskID *uuid.UUID, typ string, data any) (int64, error)
	EventsAfter(ctx context.Context, sessionID uuid.UUID, after int64, limit int) ([]Event, error)
	EnsureRepo(ctx context.Context, fullName, defaultBranch string) error
	CreateTask(ctx context.Context, in NewTask) (Task, error)
	Transition(ctx context.Context, taskID uuid.UUID, from, to State, reason string) error
	GetTask(ctx context.Context, taskID uuid.UUID) (Task, error)
	LoadLastUpdateID(ctx context.Context) (int64, error)
	SaveLastUpdateID(ctx context.Context, id int64) error
	RecordTelegramMessage(ctx context.Context, sessionID, taskID *uuid.UUID, chatID, messageID int64) error
}

// PG is the Postgres implementation. DSNs come from the environment at wiring
// time; no credential is ever stored in config or an image.
type PG struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, dsn string) (*PG, error) {
	if dsn == "" {
		return nil, errors.New("store: empty DSN")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &PG{pool: pool}, nil
}

func (p *PG) Close() { p.pool.Close() }

// Migrate verifies that the M0 migrations have been applied. It does not
// create schema: migrations stay in sql/001_init.sql and sql/002_auth_and_telegram.sql.
func (p *PG) Migrate(ctx context.Context) error {
	for _, table := range []string{"sessions", "tasks", "events", "telegram_state"} {
		var n int
		err := p.pool.QueryRow(ctx, `
			select count(*) from pg_class c
			join pg_namespace n on n.oid=c.relnamespace
			where n.nspname='agent' and c.relname=$1`, table).Scan(&n)
		if err != nil {
			return fmt.Errorf("store: check %s: %w", table, err)
		}
		if n == 0 {
			return fmt.Errorf("store: missing agent.%s; apply M0 migrations first", table)
		}
	}
	return nil
}

func (p *PG) CreateSession(ctx context.Context, channel, title string) (Session, error) {
	var s Session
	var mode string
	err := p.pool.QueryRow(ctx, `
		insert into agent.sessions (channel, title) values ($1, $2)
		returning id, created_at, channel, coalesce(title, ''), mode, last_seq`,
		channel, title).Scan(&s.ID, &s.CreatedAt, &s.Channel, &s.Title, &mode, &s.LastSeq)
	if err != nil {
		return Session{}, fmt.Errorf("store: create session: %w", err)
	}
	s.Mode = mode
	return s, nil
}

func (p *PG) GetSession(ctx context.Context, sessionID uuid.UUID) (Session, error) {
	var s Session
	var mode string
	err := p.pool.QueryRow(ctx, `
		select id, created_at, channel, coalesce(title, ''), mode, last_seq
		from agent.sessions where id=$1`, sessionID).
		Scan(&s.ID, &s.CreatedAt, &s.Channel, &s.Title, &mode, &s.LastSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: get session: %w", err)
	}
	s.Mode = mode
	return s, nil
}

func (p *PG) AppendEvent(ctx context.Context, sessionID uuid.UUID, taskID *uuid.UUID, typ string, data any) (int64, error) {
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("store: marshal event: %w", err)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var last int64
	if err := tx.QueryRow(ctx, `select last_seq from agent.sessions where id=$1 for update`, sessionID).Scan(&last); err != nil {
		return 0, fmt.Errorf("store: lock session: %w", err)
	}
	seq := last + 1
	var tid any
	if taskID != nil {
		tid = *taskID
	}
	if _, err := tx.Exec(ctx, `
		insert into agent.events (session_id, seq, task_id, type, data)
		values ($1, $2, $3, $4, $5::jsonb)`, sessionID, seq, tid, typ, string(raw)); err != nil {
		return 0, fmt.Errorf("store: insert event: %w", err)
	}
	if _, err := tx.Exec(ctx, `update agent.sessions set last_seq=$2 where id=$1`, sessionID, seq); err != nil {
		return 0, fmt.Errorf("store: bump seq: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit: %w", err)
	}
	return seq, nil
}

func (p *PG) EventsAfter(ctx context.Context, sessionID uuid.UUID, after int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.pool.Query(ctx, `
		select session_id, seq, task_id, type, data, ts
		from agent.events where session_id=$1 and seq>$2 order by seq limit $3`,
		sessionID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var taskID *uuid.UUID
		var raw []byte
		if err := rows.Scan(&e.SessionID, &e.Seq, &taskID, &e.Type, &raw, &e.TS); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		e.TaskID = taskID
		e.Data = json.RawMessage(raw)
		out = append(out, e)
	}
	return out, rows.Err()
}

// EnsureRepo registers a repo in agent.repos before a task references it.
// tasks.repo has a foreign key to repos.full_name, so an unknown repo would
// otherwise fail with SQLSTATE 23503 and leave the user with no reply.
func (p *PG) EnsureRepo(ctx context.Context, fullName, defaultBranch string) error {
	if strings.TrimSpace(fullName) == "" {
		return errors.New("store: empty repo name")
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	_, err := p.pool.Exec(ctx, `
		insert into agent.repos (full_name, default_branch) values ($1, $2)
		on conflict (full_name) do nothing`, fullName, defaultBranch)
	if err != nil {
		return fmt.Errorf("store: ensure repo: %w", err)
	}
	return nil
}

func (p *PG) CreateTask(ctx context.Context, in NewTask) (Task, error) {
	if in.IdempotencyKey == "" {
		return Task{}, ErrIDakeyEmpty
	}
	id := uuid.New()
	_, err := p.pool.Exec(ctx, `
		insert into agent.tasks (id, session_id, repo, prompt, state, idempotency_key)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (idempotency_key) do nothing`,
		id, in.SessionID, nullable(in.Repo), in.Prompt, string(StateQueued), in.IdempotencyKey)
	if err != nil {
		return Task{}, fmt.Errorf("store: create task: %w", err)
	}
	return p.taskBy(ctx, "idempotency_key", in.IdempotencyKey)
}

func (p *PG) GetTask(ctx context.Context, taskID uuid.UUID) (Task, error) {
	return p.taskBy(ctx, "id", taskID)
}

// GetTaskByKey returns the task previously created with an idempotency key.
// It lets the gateway recognise a replayed Telegram update before touching the
// agent again. ErrNotFound means the key has not been used.
func (p *PG) GetTaskByKey(ctx context.Context, key string) (Task, error) {
	if key == "" {
		return Task{}, ErrIDakeyEmpty
	}
	return p.taskBy(ctx, "idempotency_key", key)
}

func (p *PG) taskBy(ctx context.Context, column string, value any) (Task, error) {
	query := `select id, session_id, coalesce(repo,''), coalesce(branch,''), state,
		coalesce(fail_reason,''), prompt, created_at, finished_at
		from agent.tasks where ` + column + `=$1`
	var t Task
	var state string
	err := p.pool.QueryRow(ctx, query, value).Scan(&t.ID, &t.SessionID, &t.Repo, &t.Branch,
		&state, &t.FailReason, &t.Prompt, &t.CreatedAt, &t.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("store: get task: %w", err)
	}
	t.State = State(state)
	return t, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ValidTransition is pure so it can be table-tested without a database.
func ValidTransition(from, to State) bool {
	if terminalStates[from] {
		return false
	}
	return allowedTransitions[from][to]
}

func (p *PG) Transition(ctx context.Context, taskID uuid.UUID, from, to State, reason string) error {
	if !ValidTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrBadState, from, to)
	}
	tag, err := p.pool.Exec(ctx, `
		update agent.tasks
		set state=$3,
		    fail_reason=case when $3 in ('FAILED','CANCELLED') then $4 else fail_reason end,
		    finished_at=case when $3 in ('DONE','FAILED','CANCELLED') then now() else finished_at end
		where id=$1 and state=$2`, taskID, string(from), string(to), nullable(reason))
	if err != nil {
		return fmt.Errorf("store: transition: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleState
	}
	return nil
}

func (p *PG) LoadLastUpdateID(ctx context.Context) (int64, error) {
	var id int64
	err := p.pool.QueryRow(ctx, `select last_update_id from agent.telegram_state where id=1`).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: load update id: %w", err)
	}
	return id, nil
}

func (p *PG) SaveLastUpdateID(ctx context.Context, id int64) error {
	_, err := p.pool.Exec(ctx, `update agent.telegram_state set last_update_id=$1 where id=1`, id)
	if err != nil {
		return fmt.Errorf("store: save update id: %w", err)
	}
	return nil
}

func (p *PG) RecordTelegramMessage(ctx context.Context, sessionID, taskID *uuid.UUID, chatID, messageID int64) error {
	_, err := p.pool.Exec(ctx, `
		insert into agent.telegram_messages (session_id, task_id, chat_id, message_id)
		values ($1, $2, $3, $4) on conflict (chat_id, message_id) do nothing`,
		sessionID, taskID, chatID, messageID)
	if err != nil {
		return fmt.Errorf("store: record telegram message: %w", err)
	}
	return nil
}
