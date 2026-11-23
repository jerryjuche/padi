package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidTransition(t *testing.T) {
	cases := []struct {
		name     string
		from, to State
		want     bool
	}{
		{"queued to preparing", StateQueued, StatePreparing, true},
		{"queued to cancelled", StateQueued, StateCancelled, true},
		{"queued to executing", StateQueued, StateExecuting, false},
		{"preparing to executing", StatePreparing, StateExecuting, true},
		{"preparing to failed", StatePreparing, StateFailed, true},
		{"preparing to queued", StatePreparing, StateQueued, false},
		{"executing to done", StateExecuting, StateDone, true},
		{"executing to failed", StateExecuting, StateFailed, true},
		{"executing to cancelled", StateExecuting, StateCancelled, true},
		{"executing to preparing", StateExecuting, StatePreparing, false},
		{"done terminal", StateDone, StateFailed, false},
		{"failed terminal", StateFailed, StateDone, false},
		{"cancelled terminal", StateCancelled, StateDone, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidTransition(tc.from, tc.to); got != tc.want {
				t.Fatalf("ValidTransition(%s,%s)=%v want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

func TestTransitionRejectsBadStateWithoutDB(t *testing.T) {
	p := &PG{}
	err := p.Transition(context.Background(), uuid.New(), StateDone, StateExecuting, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errorsIs(err, ErrBadState) {
		t.Fatalf("want ErrBadState, got %v", err)
	}
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestPostgresRoundTrip is opt-in so unit tests never require a live database.
// Run with PADI_TEST_DSN pointing at a throwaway database with M0 migrations.
func TestPostgresRoundTrip(t *testing.T) {
	dsn := os.Getenv("PADI_TEST_DSN")
	if dsn == "" {
		t.Skip("PADI_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := db.CreateSession(ctx, "telegram", "m1-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `delete from agent.events where session_id=$1`, s.ID)
		_, _ = db.pool.Exec(context.Background(), `delete from agent.tasks where session_id=$1`, s.ID)
		_, _ = db.pool.Exec(context.Background(), `delete from agent.sessions where id=$1`, s.ID)
	})
	task, err := db.CreateTask(ctx, NewTask{SessionID: s.ID, Repo: "local/fixture", Prompt: "add a line", IdempotencyKey: "test-" + uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	seq1, err := db.AppendEvent(ctx, s.ID, &task.ID, "status", map[string]string{"state": "QUEUED"})
	if err != nil {
		t.Fatal(err)
	}
	seq2, err := db.AppendEvent(ctx, s.ID, &task.ID, "status", map[string]string{"state": "PREPARING"})
	if err != nil {
		t.Fatal(err)
	}
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("seq = %d,%d want 1,2", seq1, seq2)
	}
	events, err := db.EventsAfter(ctx, s.ID, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Seq != 2 {
		t.Fatalf("events = %+v", events)
	}
	if err := db.Transition(ctx, task.ID, StateQueued, StatePreparing, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, task.ID, StateQueued, StateExecuting, ""); err != ErrStaleState {
		t.Fatalf("want ErrStaleState, got %v", err)
	}
}
