package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jerryjuche/padi/internal/gateway/approvals"
)

// Approval methods implement approvals.Store on PG. Decisions are atomic
// compare-and-set operations: whichever client updates status first wins.
func (p *PG) CreateApproval(ctx context.Context, in approvals.Approval) (approvals.Approval, error) {
	if p == nil || p.pool == nil {
		return approvals.Approval{}, errors.New("store: nil pool")
	}
	detail, err := json.Marshal(in.Detail)
	if err != nil {
		return approvals.Approval{}, fmt.Errorf("store: approval detail: %w", err)
	}
	var chatID any
	if in.ChatID != 0 {
		chatID = in.ChatID
	}
	var sessionID any
	if in.SessionID != uuid.Nil {
		sessionID = in.SessionID
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	if in.Status == "" {
		in.Status = approvals.StatusPending
	}
	if _, err := p.pool.Exec(ctx, `
		insert into agent.approvals
		  (id, task_id, action, detail, status, expires_at, chat_id, session_id)
		values ($1, $2, $3, $4::jsonb, $5, $6, $7, $8)`,
		in.ID, in.TaskID, string(in.Action), string(detail), in.Status, in.ExpiresAt, chatID, sessionID); err != nil {
		return approvals.Approval{}, fmt.Errorf("store: create approval: %w", err)
	}
	in.Detail = cloneDetail(in.Detail)
	return in, nil
}

func (p *PG) GetApproval(ctx context.Context, id uuid.UUID) (approvals.Approval, error) {
	if p == nil || p.pool == nil {
		return approvals.Approval{}, errors.New("store: nil pool")
	}
	return p.scanApproval(p.pool.QueryRow(ctx, approvalSelect, id))
}

const approvalColumns = `
	select id, task_id, action, detail, status, expires_at, created_at, decided_at,
	       coalesce(decided_via, ''), coalesce(chat_id, 0), coalesce(session_id, '00000000-0000-0000-0000-000000000000'::uuid)
	from agent.approvals`

const approvalSelect = approvalColumns + ` where id=$1`

// shortIDSelect uses $1, not $2: the old query inherited approvalSelect's id=$1
// and passed only the short ID, so pgx rejected every callback lookup.
const shortIDSelect = approvalColumns + `
	where left(replace(id::text, '-', ''), 8) = $1
	order by created_at desc limit 1`

func (p *PG) DecideApproval(ctx context.Context, id uuid.UUID, approve bool, via string) (approvals.Approval, error) {
	if p == nil || p.pool == nil {
		return approvals.Approval{}, errors.New("store: nil pool")
	}
	if via == "expired" {
		if err := p.ExpireApproval(ctx, id, time.Now().UTC()); err != nil {
			return approvals.Approval{}, err
		}
		return p.GetApproval(ctx, id)
	}
	status := approvals.StatusRejected
	if approve {
		status = approvals.StatusApproved
	}
	a, err := p.scanApproval(p.pool.QueryRow(ctx, `
		update agent.approvals
		set status=$2, decided_at=now(), decided_via=$3
		where id=$1 and status='pending' and expires_at>now()
		returning id, task_id, action, detail, status, expires_at, created_at, decided_at,
		          coalesce(decided_via, ''), coalesce(chat_id, 0), coalesce(session_id, '00000000-0000-0000-0000-000000000000'::uuid)`,
		id, status, via))
	if errors.Is(err, pgx.ErrNoRows) {
		current, getErr := p.GetApproval(ctx, id)
		if getErr != nil {
			return approvals.Approval{}, errors.New("store: decide approval: already decided")
		}
		if current.Status == approvals.StatusExpired || !time.Now().Before(current.ExpiresAt) {
			return approvals.Approval{}, errors.New("store: decide approval: expired")
		}
		return approvals.Approval{}, errors.New("store: decide approval: already decided")
	}
	if err != nil {
		return approvals.Approval{}, fmt.Errorf("store: decide approval: %w", err)
	}
	return a, nil
}

func (p *PG) ExpireApproval(ctx context.Context, id uuid.UUID, at time.Time) error {
	if p == nil || p.pool == nil {
		return errors.New("store: nil pool")
	}
	_, err := p.pool.Exec(ctx, `
		update agent.approvals
		set status='expired', decided_at=$2, decided_via='expired'
		where id=$1 and status='pending'`, id, at)
	if err != nil {
		return fmt.Errorf("store: expire approval: %w", err)
	}
	return nil
}

func (p *PG) ListExpired(ctx context.Context, now time.Time) ([]approvals.Approval, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("store: nil pool")
	}
	rows, err := p.pool.Query(ctx, approvalSelect+` and status='pending' and expires_at <= $2 order by expires_at`, now)
	if err != nil {
		return nil, fmt.Errorf("store: list expired approvals: %w", err)
	}
	defer rows.Close()
	out := []approvals.Approval{}
	for rows.Next() {
		a, err := p.scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ApprovalByShortID resolves the eight-hex-prefix used in Telegram callbacks.
func (p *PG) ApprovalByShortID(ctx context.Context, shortID string) (approvals.Approval, error) {
	if p == nil || p.pool == nil {
		return approvals.Approval{}, errors.New("store: nil pool")
	}
	shortID = strings.ToLower(shortID)
	if len(shortID) != 8 {
		return approvals.Approval{}, approvals.ErrInvalidVia
	}
	if _, err := hex.DecodeString(shortID); err != nil {
		return approvals.Approval{}, approvals.ErrInvalidVia
	}
	row := p.pool.QueryRow(ctx, shortIDSelect, shortID)
	return p.scanApproval(row)
}

func (p *PG) HasApproved(ctx context.Context, taskID uuid.UUID, action approvals.Action) (bool, error) {
	if p == nil || p.pool == nil {
		return false, errors.New("store: nil pool")
	}
	var ok bool
	err := p.pool.QueryRow(ctx, `
		select exists (
		  select 1 from agent.approvals
		  where task_id=$1 and action=$2 and status='approved')`,
		taskID, string(action)).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: approval gate: %w", err)
	}
	return ok, nil
}

func (p *PG) Audit(ctx context.Context, taskID *uuid.UUID, action, detail string) error {
	if p == nil || p.pool == nil {
		return errors.New("store: nil pool")
	}
	var tid any
	if taskID != nil && *taskID != uuid.Nil {
		tid = *taskID
	}
	payload, err := json.Marshal(map[string]string{"detail": detail})
	if err != nil {
		return fmt.Errorf("store: audit detail: %w", err)
	}
	_, err = p.pool.Exec(ctx, `
		insert into agent.audit_log (task_id, actor, action, detail)
		values ($1, 'gateway', $2, $3::jsonb)`, tid, action, string(payload))
	if err != nil {
		return fmt.Errorf("store: audit: %w", err)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (p *PG) scanApproval(row rowScanner) (approvals.Approval, error) {
	var a approvals.Approval
	var action string
	var raw []byte
	var decided sql.NullTime
	var chat sql.NullInt64
	var session uuid.NullUUID
	err := row.Scan(&a.ID, &a.TaskID, &action, &raw, &a.Status, &a.ExpiresAt,
		&a.CreatedAt, &decided, &a.DecidedVia, &chat, &session)
	if errors.Is(err, pgx.ErrNoRows) {
		return approvals.Approval{}, ErrNotFound
	}
	if err != nil {
		return approvals.Approval{}, fmt.Errorf("store: scan approval: %w", err)
	}
	a.Action = approvals.Action(action)
	if decided.Valid {
		t := decided.Time
		a.DecidedAt = &t
	}
	if chat.Valid {
		a.ChatID = chat.Int64
	}
	if session.Valid {
		a.SessionID = session.UUID
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a.Detail); err != nil {
			return approvals.Approval{}, fmt.Errorf("store: approval detail: %w", err)
		}
	}
	return a, nil
}

func cloneDetail(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
