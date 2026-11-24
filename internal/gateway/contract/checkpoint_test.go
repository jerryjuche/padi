package contract

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	out   string
	err   error
}

func (f *fakeRunner) Exec(_ context.Context, id string, cmd ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := append([]string{id}, cmd...)
	f.calls = append(f.calls, row)
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.out), nil
}

func (f *fakeRunner) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, row := range f.calls {
		out[i] = append([]string(nil), row...)
	}
	return out
}

func TestCheckpointArgv(t *testing.T) {
	tests := []struct {
		name string
		n    int
		out  string
		want string
	}{
		{name: "baseline zero", n: 0, out: "refs/agent/checkpoints/0\n", want: "refs/agent/checkpoints/0"},
		{name: "tenth checkpoint", n: 10, out: "  refs/agent/checkpoints/10 \t\n", want: "refs/agent/checkpoints/10"},
		{name: "large integer", n: 2147483647, out: "ref-42", want: "ref-42"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{out: tc.out}
			ref, err := Checkpoint(context.Background(), r, "container-1", tc.n)
			if err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			if ref != tc.want {
				t.Fatalf("ref = %q, want %q", ref, tc.want)
			}
			wantCalls := [][]string{{
				"container-1", checkpointScript, checkpointSave, strconv.Itoa(tc.n),
			}}
			if got := r.Calls(); !reflect.DeepEqual(got, wantCalls) {
				t.Fatalf("calls = %q, want %q", got, wantCalls)
			}
		})
	}
}

func TestRollbackAndAliasesArgv(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, Runner, string, int) error
	}{
		{name: "rollback", call: Rollback},
		{name: "resume", call: Resume},
		{name: "rewind", call: Rewind},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{out: "ignored output"}
			if err := tc.call(context.Background(), r, "container-2", 7); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			wantCalls := [][]string{{
				"container-2", checkpointScript, checkpointRollback, "7",
			}}
			if got := r.Calls(); !reflect.DeepEqual(got, wantCalls) {
				t.Fatalf("calls = %q, want %q", got, wantCalls)
			}
		})
	}
}

func TestSaveAliasArgv(t *testing.T) {
	r := &fakeRunner{out: "ref\n"}
	ref, err := Save(context.Background(), r, "container-3", 5)
	if err != nil || ref != "ref" {
		t.Fatalf("Save = (%q, %v)", ref, err)
	}
	want := [][]string{{"container-3", checkpointScript, checkpointSave, "5"}}
	if got := r.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestCheckpointRejectsNegativeN(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, Runner, string, int) error
	}{
		{name: "checkpoint", call: func(ctx context.Context, r Runner, id string, n int) error {
			_, err := Checkpoint(ctx, r, id, n)
			return err
		}},
		{name: "save", call: func(ctx context.Context, r Runner, id string, n int) error {
			_, err := Save(ctx, r, id, n)
			return err
		}},
		{name: "rollback", call: Rollback},
		{name: "resume", call: Resume},
		{name: "rewind", call: Rewind},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{}
			err := tc.call(context.Background(), r, "container-4", -1)
			if !errors.Is(err, ErrInvalidCheckpoint) {
				t.Fatalf("err = %v, want ErrInvalidCheckpoint", err)
			}
			if got := r.Calls(); len(got) != 0 {
				t.Fatalf("runner must not be called, got %q", got)
			}
		})
	}
}

func TestCheckpointRejectsNilRunner(t *testing.T) {
	if _, err := Checkpoint(context.Background(), nil, "container-5", 1); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Checkpoint err = %v", err)
	}
	if err := Rollback(context.Background(), nil, "container-5", 1); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Rollback err = %v", err)
	}
}

func TestCheckpointRunnerError(t *testing.T) {
	boom := errors.New("boom")
	r := &fakeRunner{err: boom}
	ref, err := Checkpoint(context.Background(), r, "container-6", 2)
	if ref != "" || !errors.Is(err, boom) || !errors.Is(err, ErrCheckpointFailed) {
		t.Fatalf("Checkpoint = (%q, %v)", ref, err)
	}
	if err := Rollback(context.Background(), r, "container-6", 2); !errors.Is(err, boom) {
		t.Fatalf("Rollback err = %v", err)
	}
}
