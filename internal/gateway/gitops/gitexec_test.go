package gitops

import (
	"context"
	"strings"
	"testing"
)

func TestGitexecRejectsShellInjection(t *testing.T) {
	cases := []string{
		"status; touch /tmp/pwned",
		"status && rm -rf /",
		"$(touch /tmp/pwned)",
		"`touch /tmp/pwned`",
		"status\nreset --hard",
		"status | sh",
		"status > /tmp/out",
		"status\x00reset",
	}
	for _, arg := range cases {
		t.Run(strings.ReplaceAll(arg, "\n", "\\n"), func(t *testing.T) {
			if _, err := Run(context.Background(), t.TempDir(), nil, arg); err == nil {
				t.Fatalf("accepted shell metacharacters in %q", arg)
			}
		})
	}
	if err := rejectShellArg("status"); err != nil {
		t.Fatalf("ordinary argument rejected: %v", err)
	}
	if err := rejectShellArg("/tmp/a b"); err != nil {
		t.Fatalf("space in path rejected: %v", err)
	}
}
