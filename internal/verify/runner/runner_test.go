package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryjuche/padi/internal/verify/detect"
)

func TestExecute(t *testing.T) {
	cases := []struct {
		name       string
		runner     detect.Runner
		wantOK     bool
		contains   []string
		forbidden  []string
		maxTailLen int
	}{
		{
			name:     "combined output on success",
			runner:   detect.Runner{Name: "echo", Command: "sh", Args: []string{"-c", "printf 'out\\n'; printf 'err\\n' >&2"}},
			wantOK:   true,
			contains: []string{"out", "err"},
		},
		{
			name:     "exit code sets failure",
			runner:   detect.Runner{Name: "fail", Command: "sh", Args: []string{"-c", "printf 'broken\\n'; exit 7"}},
			wantOK:   false,
			contains: []string{"broken"},
		},
		{
			name:      "tokens are redacted",
			runner:    detect.Runner{Name: "secret", Command: "sh", Args: []string{"-c", "printf 'token=ghp_abcdefghijklmnopqrstuvwxyz\\n'"}},
			wantOK:    true,
			contains:  []string{"[REDACTED]"},
			forbidden: []string{"ghp_abcdefghijklmnopqrstuvwxyz"},
		},
		{
			name:       "tail is capped at 4 KB",
			runner:     detect.Runner{Name: "large", Command: "sh", Args: []string{"-c", "i=0; while [ $i -lt 600 ]; do printf '0123456789'; i=$((i+1)); done"}},
			wantOK:     true,
			maxTailLen: MaxTailBytes,
		},
		{
			name:     "empty command fails safely",
			runner:   detect.Runner{Name: "empty"},
			wantOK:   false,
			contains: []string{"empty command"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step := Execute(context.Background(), tc.runner, t.TempDir())
			if step.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (tail: %q)", step.OK, tc.wantOK, step.OutputTail)
			}
			for _, want := range tc.contains {
				if !strings.Contains(step.OutputTail, want) {
					t.Fatalf("tail %q does not contain %q", step.OutputTail, want)
				}
			}
			for _, forbidden := range tc.forbidden {
				if strings.Contains(step.OutputTail, forbidden) {
					t.Fatalf("tail %q leaked %q", step.OutputTail, forbidden)
				}
			}
			if tc.maxTailLen > 0 && len(step.OutputTail) > tc.maxTailLen {
				t.Fatalf("tail length %d exceeds %d", len(step.OutputTail), tc.maxTailLen)
			}
		})
	}
}

func TestRun(t *testing.T) {
	const validGoMod = "module example.com/verifytest\n\ngo 1.26\n"

	t.Run("no recognized project is unverifiable", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "README.md", "nothing to verify\n")
		got := Run(context.Background(), dir, time.Second)
		if got.Verdict != detect.VerdictUnverifiable {
			t.Fatalf("verdict = %q, want unverifiable", got.Verdict)
		}
		if len(got.Steps) != 1 || got.Steps[0].Name != "detect" || got.Steps[0].OK {
			t.Fatalf("steps = %#v, want one failed detect step", got.Steps)
		}
	})

	t.Run("passing project", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "go.mod", validGoMod)
		write(t, dir, "x_test.go", "package verifytest\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n")
		got := Run(context.Background(), dir, 30*time.Second)
		if got.Verdict != detect.VerdictPass {
			t.Fatalf("verdict = %q, want pass (steps: %#v)", got.Verdict, got.Steps)
		}
	})

	t.Run("failing command is fail", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "go.mod", "this is not a valid go.mod\n")
		got := Run(context.Background(), dir, 30*time.Second)
		if got.Verdict != detect.VerdictFail {
			t.Fatalf("verdict = %q, want fail (steps: %#v)", got.Verdict, got.Steps)
		}
	})

	t.Run("total timeout is fail", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "go.mod", validGoMod)
		write(t, dir, "slow_test.go", "package verifytest\n\nimport (\n\"testing\"\n\"time\"\n)\n\nfunc TestSlow(t *testing.T) { time.Sleep(5 * time.Second) }\n")
		got := Run(context.Background(), dir, 50*time.Millisecond)
		if got.Verdict != detect.VerdictFail {
			t.Fatalf("verdict = %q, want fail (steps: %#v)", got.Verdict, got.Steps)
		}
	})

	t.Run("parent cancellation is unverifiable", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "go.mod", validGoMod)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got := Run(ctx, dir, time.Second)
		if got.Verdict != detect.VerdictUnverifiable {
			t.Fatalf("verdict = %q, want unverifiable (steps: %#v)", got.Verdict, got.Steps)
		}
	})
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
