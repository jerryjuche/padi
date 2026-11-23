package gitops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const defaultGitTimeout = 60 * time.Second

// Run is the ONLY way the gateway invokes git. It never uses a shell, always
// neutralises user/system git configuration, and defaults to HTTPS-only.
// Callers that need the local file transport append the explicit
// "-c protocol.file.allow=always" argument themselves.
func Run(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("gitexec: no git arguments")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultGitTimeout)
		defer cancel()
	}

	cmdArgs := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
	}
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = dir

	// Inherited GIT_* variables can redirect hooks, config or credentials.
	base := make([]string, 0, len(os.Environ())+len(env)+8)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GIT_") {
			continue
		}
		base = append(base, e)
	}
	base = append(base, env...)
	base = append(base,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"GIT_ASKPASS=/bin/false",
	)
	cmd.Env = base

	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("gitexec: git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
