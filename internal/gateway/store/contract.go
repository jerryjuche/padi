package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jerryjuche/padi/internal/gateway/contract"
)

// SaveTaskContract persists the agreed contract on agent.tasks.contract.
func (p *PG) SaveTaskContract(ctx context.Context, taskID uuid.UUID, c contract.Contract) error {
	if p == nil || p.pool == nil {
		return errors.New("store: nil pool")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("store: marshal contract: %w", err)
	}
	tag, err := p.pool.Exec(ctx, `update agent.tasks set contract=$2::jsonb where id=$1`, taskID, string(raw))
	if err != nil {
		return fmt.Errorf("store: save contract: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// LoadTaskContract reads the task contract. A task created before M3b has no
// contract, so callers must treat ErrNotFound as "use gateway defaults".
func (p *PG) LoadTaskContract(ctx context.Context, taskID uuid.UUID) (contract.Contract, error) {
	if p == nil || p.pool == nil {
		return contract.Contract{}, errors.New("store: nil pool")
	}
	var raw []byte
	err := p.pool.QueryRow(ctx, `select contract from agent.tasks where id=$1`, taskID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.Contract{}, ErrNotFound
	}
	if err != nil {
		return contract.Contract{}, fmt.Errorf("store: load contract: %w", err)
	}
	if len(raw) == 0 {
		return contract.Contract{}, ErrNotFound
	}
	var c contract.Contract
	if err := json.Unmarshal(raw, &c); err != nil {
		return contract.Contract{}, fmt.Errorf("store: decode contract: %w", err)
	}
	return c, nil
}

// SetTaskBranch records the gateway-owned branch created for a task.
func (p *PG) SetTaskBranch(ctx context.Context, taskID uuid.UUID, branch string) error {
	if p == nil || p.pool == nil {
		return errors.New("store: nil pool")
	}
	tag, err := p.pool.Exec(ctx, `update agent.tasks set branch=$2 where id=$1`, taskID, branch)
	if err != nil {
		return fmt.Errorf("store: set task branch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPRURL records the pull request opened after both owner gates pass.
func (p *PG) SetPRURL(ctx context.Context, taskID uuid.UUID, url string) error {
	if p == nil || p.pool == nil {
		return errors.New("store: nil pool")
	}
	tag, err := p.pool.Exec(ctx, `update agent.tasks set pr_url=$2 where id=$1`, taskID, url)
	if err != nil {
		return fmt.Errorf("store: set PR URL: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SaveBudgetUsed persists the meter snapshot on agent.tasks.budget_used.
func (p *PG) SaveBudgetUsed(ctx context.Context, taskID uuid.UUID, used map[string]int) error {
	if p == nil || p.pool == nil {
		return errors.New("store: nil pool")
	}
	raw, err := json.Marshal(used)
	if err != nil {
		return fmt.Errorf("store: marshal budget: %w", err)
	}
	tag, err := p.pool.Exec(ctx, `update agent.tasks set budget_used=$2::jsonb where id=$1`, taskID, string(raw))
	if err != nil {
		return fmt.Errorf("store: save budget: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
