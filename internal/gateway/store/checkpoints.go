package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Checkpoint mirrors agent.session_checkpoints. Ref is the in-container
// refs/agent/checkpoints/<n> commit captured without moving the task branch.
type Checkpoint struct {
	ID         uuid.UUID
	TaskID     uuid.UUID
	SessionID  uuid.UUID
	N          int
	Ref        string
	Tree       string
	CreatedAt  time.Time
	RestoredAt *time.Time
}

var ErrInvalidCheckpoint = errors.New("store: invalid checkpoint")

func validateCheckpoint(c Checkpoint) error {
	if c.N < 0 || c.Ref == "" || c.TaskID == uuid.Nil || c.SessionID == uuid.Nil {
		return fmt.Errorf("%w: task=%s session=%s n=%d ref=%q",
			ErrInvalidCheckpoint, c.TaskID, c.SessionID, c.N, c.Ref)
	}
	return nil
}

// SaveCheckpoint records or replaces one numbered checkpoint for a task.
func (p *PG) SaveCheckpoint(ctx context.Context, in Checkpoint) (Checkpoint, error) {
	if err := validateCheckpoint(in); err != nil {
		return Checkpoint{}, err
	}
	if p == nil || p.pool == nil {
		return Checkpoint{}, errors.New("store: nil pool")
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	row := p.pool.QueryRow(ctx, `
		insert into agent.session_checkpoints (id, task_id, session_id, n, ref, tree)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (task_id, n) do update
		  set ref=excluded.ref, tree=excluded.tree, created_at=now(), restored_at=null
		returning id, task_id, session_id, n, ref, tree, created_at, coalesce(restored_at::text, '')`,
		in.ID, in.TaskID, in.SessionID, in.N, in.Ref, in.Tree)
	return p.scanCheckpoint(row)
}

// LoadCheckpoint returns one checkpoint by number. It is the read side of
// resume: the caller passes the ref to contract.Resume inside the container.
func (p *PG) LoadCheckpoint(ctx context.Context, taskID uuid.UUID, n int) (Checkpoint, error) {
	if n < 0 || taskID == uuid.Nil {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	if p == nil || p.pool == nil {
		return Checkpoint{}, errors.New("store: nil pool")
	}
	row := p.pool.QueryRow(ctx, `
		select id, task_id, session_id, n, ref, tree, created_at, coalesce(restored_at::text, '')
		from agent.session_checkpoints where task_id=$1 and n=$2`, taskID, n)
	return p.scanCheckpoint(row)
}

// LatestCheckpoint returns the highest numbered checkpoint, or ErrNotFound.
func (p *PG) LatestCheckpoint(ctx context.Context, taskID uuid.UUID) (Checkpoint, error) {
	if taskID == uuid.Nil {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	if p == nil || p.pool == nil {
		return Checkpoint{}, errors.New("store: nil pool")
	}
	row := p.pool.QueryRow(ctx, `
		select id, task_id, session_id, n, ref, tree, created_at, coalesce(restored_at::text, '')
		from agent.session_checkpoints where task_id=$1 order by n desc limit 1`, taskID)
	return p.scanCheckpoint(row)
}

// RewindCheckpoint marks checkpoint n as restored at the current time. The
// actual tree restore happens in the container via contract.Rewind; this call
// records the durable audit fact.
func (p *PG) RewindCheckpoint(ctx context.Context, taskID uuid.UUID, n int) (Checkpoint, error) {
	if n < 0 || taskID == uuid.Nil {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	if p == nil || p.pool == nil {
		return Checkpoint{}, errors.New("store: nil pool")
	}
	row := p.pool.QueryRow(ctx, `
		update agent.session_checkpoints set restored_at=now()
		where task_id=$1 and n=$2
		returning id, task_id, session_id, n, ref, tree, created_at, coalesce(restored_at::text, '')`,
		taskID, n)
	return p.scanCheckpoint(row)
}

func (p *PG) ListCheckpoints(ctx context.Context, taskID uuid.UUID) ([]Checkpoint, error) {
	if taskID == uuid.Nil {
		return nil, ErrInvalidCheckpoint
	}
	if p == nil || p.pool == nil {
		return nil, errors.New("store: nil pool")
	}
	rows, err := p.pool.Query(ctx, `
		select id, task_id, session_id, n, ref, tree, created_at, coalesce(restored_at::text, '')
		from agent.session_checkpoints where task_id=$1 order by n desc`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: list checkpoints: %w", err)
	}
	defer rows.Close()
	out := []Checkpoint{}
	for rows.Next() {
		c, err := p.scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p *PG) scanCheckpoint(row rowScanner) (Checkpoint, error) {
	var c Checkpoint
	var restored string
	err := row.Scan(&c.ID, &c.TaskID, &c.SessionID, &c.N, &c.Ref, &c.Tree, &c.CreatedAt, &restored)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, ErrNotFound
	}
	if err != nil {
		return Checkpoint{}, fmt.Errorf("store: scan checkpoint: %w", err)
	}
	if restored != "" {
		at, err := time.Parse(time.RFC3339Nano, restored)
		if err != nil {
			return Checkpoint{}, fmt.Errorf("store: checkpoint restored_at: %w", err)
		}
		c.RestoredAt = &at
	}
	return c, nil
}
