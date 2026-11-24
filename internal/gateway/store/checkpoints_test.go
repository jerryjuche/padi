package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestValidateCheckpoint(t *testing.T) {
	t.Parallel()
	task := uuid.New()
	session := uuid.New()
	cases := []struct {
		name    string
		c       Checkpoint
		wantErr bool
	}{
		{"valid", Checkpoint{TaskID: task, SessionID: session, N: 0, Ref: "refs/agent/checkpoints/0"}, false},
		{"negative n", Checkpoint{TaskID: task, SessionID: session, N: -1, Ref: "ref"}, true},
		{"empty ref", Checkpoint{TaskID: task, SessionID: session, N: 1}, true},
		{"missing task", Checkpoint{SessionID: session, N: 1, Ref: "ref"}, true},
		{"missing session", Checkpoint{TaskID: task, N: 1, Ref: "ref"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCheckpoint(tc.c)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidCheckpoint) {
				t.Fatalf("want ErrInvalidCheckpoint, got %v", err)
			}
		})
	}
}

func TestCheckpointStoreRejectsNilPool(t *testing.T) {
	t.Parallel()
	p := &PG{}
	ctx := context.Background()
	task := uuid.New()
	cases := []struct {
		name string
		call func() error
	}{
		{"save", func() error {
			_, err := p.SaveCheckpoint(ctx, Checkpoint{TaskID: task, SessionID: uuid.New(), N: 1, Ref: "ref"})
			return err
		}},
		{"load", func() error { _, err := p.LoadCheckpoint(ctx, task, 1); return err }},
		{"latest", func() error { _, err := p.LatestCheckpoint(ctx, task); return err }},
		{"rewind", func() error { _, err := p.RewindCheckpoint(ctx, task, 1); return err }},
		{"list", func() error { _, err := p.ListCheckpoints(ctx, task); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Fatal("expected nil-pool error")
			}
		})
	}
}
