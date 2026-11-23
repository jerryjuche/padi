package telegram

import (
	"context"
	"fmt"
	"log"
	"time"
)

// StateStore persists the Telegram update cursor (M0 agent.telegram_state).
type StateStore interface {
	LoadLastUpdateID(ctx context.Context) (int64, error)
	SaveLastUpdateID(ctx context.Context, id int64) error
}

// Allowed is true only for a private chat from the configured owner.
func Allowed(u Update, ownerID int64) bool {
	if ownerID == 0 || u.Message == nil {
		return false
	}
	return u.Message.Chat.Type == "private" && u.Message.From.ID == ownerID
}

// Handler processes one authorised update.
type Handler func(ctx context.Context, u Update) error

// Poller owns exactly one Telegram getUpdates loop.
type Poller struct {
	Client   *Client
	Store    StateStore
	OwnerID  int64
	Handle   Handler
	Interval time.Duration
}

// Run blocks until the context is cancelled. The cursor is persisted only
// after a successful handle, so a crash replay is safe (at-least-once).
func (p *Poller) Run(ctx context.Context) error {
	last, err := p.Store.LoadLastUpdateID(ctx)
	if err != nil {
		return fmt.Errorf("telegram: load cursor: %w", err)
	}
	interval := p.Interval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		updates, err := p.Client.GetUpdates(ctx, last+1)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("telegram getUpdates: %v", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID <= last {
				continue
			}
			if !Allowed(u, p.OwnerID) {
				last = u.UpdateID
				if err := p.Store.SaveLastUpdateID(ctx, last); err != nil {
					return fmt.Errorf("telegram: save cursor: %w", err)
				}
				continue
			}
			if err := p.Handle(ctx, u); err != nil {
				log.Printf("telegram handle update %d: %v", u.UpdateID, err)
				continue
			}
			last = u.UpdateID
			if err := p.Store.SaveLastUpdateID(ctx, last); err != nil {
				return fmt.Errorf("telegram: save cursor: %w", err)
			}
		}
		if len(updates) == 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(interval):
			}
		}
	}
}
