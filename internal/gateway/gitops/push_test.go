package gitops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type pushFixture struct {
	g      *Git
	repo   string
	source string
	remote string
	root   string
}

// newPushFixture builds a local bare remote, a source repo whose agent branch
// has one commit not yet on the remote, and a gateway-owned cache at main.
func newPushFixture(t *testing.T, branch string) pushFixture {
	t.Helper()
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	remote := filepath.Join(root, "remote.git")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	mustRun(t, ctx, source, "init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("# fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustRun(t, ctx, source, "add", "README.md")
	mustRun(t, ctx, source,
		"-c", "user.name=sandbox-agent", "-c", "user.email=noreply@sandbox.local",
		"commit", "--quiet", "-m", "initial")
	mustRun(t, ctx, root, "init", "--bare", "--quiet", "--initial-branch=main", remote)
	mustRun(t, ctx, root, append(localArgs(remote),
		"-C", source, "push", remote, "refs/heads/main:refs/heads/main")...)

	mustRun(t, ctx, source, "checkout", "--quiet", "-b", branch)
	if err := os.WriteFile(filepath.Join(source, "branch.txt"), []byte(branch+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustRun(t, ctx, source, "add", "branch.txt")
	mustRun(t, ctx, source,
		"-c", "user.name=sandbox-agent", "-c", "user.email=noreply@sandbox.local",
		"commit", "--quiet", "-m", "agent change")

	g := &Git{
		CacheDir:      filepath.Join(root, "cache"),
		QuarantineDir: filepath.Join(root, "quarantine"),
	}
	if _, err := g.EnsureCache(ctx, remote); err != nil {
		t.Fatalf("ensure cache: %v", err)
	}
	return pushFixture{g: g, repo: remote, source: source, remote: remote, root: root}
}

// seedQuarantine clones the source repo into the gateway-owned quarantine
// path. The source commit is deliberately not on the remote yet.
func (f pushFixture) seedQuarantine(t *testing.T, branch string) string {
	t.Helper()
	qdir := f.g.quarantinePath(f.repo, branch)
	if err := os.MkdirAll(filepath.Dir(qdir), 0o700); err != nil {
		t.Fatal(err)
	}
	mustRun(t, context.Background(), filepath.Dir(qdir), append(localArgs(f.repo),
		"clone", "--bare", "--quiet", f.source, qdir)...)
	return qdir
}

func (f pushFixture) remoteHead(t *testing.T, branch string) string {
	t.Helper()
	out := mustRun(t, context.Background(), f.remote, "rev-parse", "--verify", "refs/heads/"+branch)
	return strings.TrimSpace(string(out))
}

func (f pushFixture) remoteHasBranch(t *testing.T, branch string) bool {
	t.Helper()
	_, err := mustRunNoFail(context.Background(), f.remote, "rev-parse", "--verify", "refs/heads/"+branch)
	return err == nil
}

func mustRunNoFail(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return Run(ctx, dir, nil, args...)
}

func TestPushAgentBranchPublishesQuarantinedBranch(t *testing.T) {
	const branch = "agent/push-ok"
	f := newPushFixture(t, branch)
	qdir := f.seedQuarantine(t, branch)
	want := strings.TrimSpace(string(mustRun(t, context.Background(), qdir, "rev-parse", "refs/heads/"+branch)))

	// Push is the BUILD_SPEC signature and must derive the same quarantine path.
	if err := f.g.Push(context.Background(), f.repo, branch); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := f.remoteHead(t, branch); got != want {
		t.Fatalf("remote head=%q want %q", got, want)
	}

	// An explicit gateway-owned directory is accepted too.
	if err := f.g.PushAgentBranch(context.Background(), f.repo, branch, qdir); err != nil {
		t.Fatalf("explicit push: %v", err)
	}
}

func TestPushAgentBranchRefusesForbiddenBranches(t *testing.T) {
	const validBranch = "agent/forbidden-fixture"
	f := newPushFixture(t, validBranch)
	branches := []string{
		"main",
		"master",
		"refs/heads/main",
		"HEAD",
		"agent/../main",
		"agent/UPPER",
		"agent/",
		"",
	}
	for _, branch := range branches {
		t.Run(strings.ReplaceAll(branch, "/", "_"), func(t *testing.T) {
			err := f.g.Push(context.Background(), f.repo, branch)
			if !errors.Is(err, ErrForbiddenBranch) {
				t.Fatalf("branch %q err=%v want ErrForbiddenBranch", branch, err)
			}
		})
	}
	if f.remoteHasBranch(t, validBranch) {
		t.Fatal("forbidden push modified the remote")
	}
}

func TestPushAgentBranchRefusesCacheDefaultBranch(t *testing.T) {
	const branch = "agent/is-default"
	f := newPushFixture(t, branch)
	f.seedQuarantine(t, branch)
	cache := f.g.cachePath(f.repo)
	mustRun(t, context.Background(), f.g.CacheDir,
		"-C", cache, "symbolic-ref", "HEAD", "refs/heads/"+branch)

	err := f.g.Push(context.Background(), f.repo, branch)
	if !errors.Is(err, ErrForbiddenBranch) {
		t.Fatalf("err=%v want ErrForbiddenBranch for default branch", err)
	}
	if f.remoteHasBranch(t, branch) {
		t.Fatal("default-branch push modified the remote")
	}
}

func TestPushAgentBranchRefusesHostileQuarantinePaths(t *testing.T) {
	const branch = "agent/hostile-path"
	f := newPushFixture(t, branch)
	f.seedQuarantine(t, branch)

	link := filepath.Join(f.g.QuarantineDir, "escape.git")
	if err := os.Symlink(f.source, link); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(f.root, "workspaces", "task-1", "repo"),
		filepath.Join(f.g.QuarantineDir, "..", "escape.git"),
		filepath.Join("quarantine", "relative.git"),
		link,
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			err := f.g.PushAgentBranch(context.Background(), f.repo, branch, path)
			if err == nil {
				t.Fatal("hostile quarantine path accepted")
			}
			if errors.Is(err, ErrForbiddenBranch) {
				t.Fatalf("wrong error for path %q: %v", path, err)
			}
			if !strings.Contains(err.Error(), "unsafe quarantine dir") {
				t.Fatalf("err=%v want unsafe quarantine dir", err)
			}
		})
	}
	if f.remoteHasBranch(t, branch) {
		t.Fatal("hostile path pushed to the remote")
	}
}

// TestPushDelegatesRemoteAuthToGitexec guards the audit rule: the token is
// supplied through g.authEnv, never assembled into a git command argument.
// The gitexec lint test separately proves every git call goes through Run.
func TestPushDelegatesRemoteAuthToGitexec(t *testing.T) {
	src, err := os.ReadFile("push.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	if !strings.Contains(code, "g.authEnv(ctx, repo)") {
		t.Fatal("push must obtain credentials from g.authEnv")
	}
	for _, bad := range []string{`http.extraheader`, `"Bearer "`, `basicAuthValue(`} {
		if strings.Contains(code, bad) {
			t.Fatalf("credential material found in push arguments: %s", bad)
		}
	}
}
