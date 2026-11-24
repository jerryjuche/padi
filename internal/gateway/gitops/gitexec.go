package gitops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const defaultGitTimeout = 60 * time.Second

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)\S+`),
	regexp.MustCompile(`(?i)(authorization:\s*basic\s+)[A-Za-z0-9+/=]+`),
	regexp.MustCompile(`\b(?:ghs|gho|ghu|ghr|github_pat)_[A-Za-z0-9_]+\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`),
}

// scrub removes credential-shaped strings before git output reaches an error,
// a database failure_reason or a Telegram reply.
func scrub(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

// Run is the ONLY way the gateway invokes git. It never uses a shell, always
// neutralises user/system git configuration, and defaults to HTTPS-only.
// Callers that need the local file transport append the explicit
// "-c protocol.file.allow=always" argument themselves.
// shellMeta is rejected defensively: exec.Command never uses a shell, but
// callers must not be able to smuggle command syntax into a git argument.
var shellMeta = strings.NewReplacer(
	";", "", "&", "", "|", "", "`", "", "$", "", "\n", "", "\r", "",
	">", "", "<", "", "(", "", ")", "", "{", "", "}", "", "\x00", "",
)

func rejectShellArg(arg string) error {
	if shellMeta.Replace(arg) != arg {
		return fmt.Errorf("gitexec: shell metacharacter in argument %q", arg)
	}
	return nil
}

func Run(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("gitexec: no git arguments")
	}
	if err := rejectShellArg(dir); err != nil {
		return nil, err
	}
	for _, arg := range args {
		if err := rejectShellArg(arg); err != nil {
			return nil, err
		}
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
		detail := scrub(strings.TrimSpace(string(out)))
		return []byte(detail), fmt.Errorf("gitexec: git %s: %w: %s", strings.Join(args, " "), err, detail)
	}
	return out, nil
}
