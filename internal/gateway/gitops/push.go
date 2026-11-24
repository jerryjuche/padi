package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Push publishes a quarantined agent branch using the gateway-owned quarantine
// path derived from repo and branch. It keeps the M3b BUILD_SPEC signature.
func (g *Git) Push(ctx context.Context, repo, branch string) error {
	return g.PushAgentBranch(ctx, repo, branch, "")
}

// PushAgentBranch pushes exactly one validated agent branch from a
// gateway-owned quarantine repository. An empty quarantineDir selects
// g.quarantinePath(repo, branch); an explicit directory must still live under
// the gateway quarantine root.
//
// The installation token is supplied by g.authEnv through the process
// environment only. No shell is involved and no agent workspace is used as a
// git working directory.
func (g *Git) PushAgentBranch(ctx context.Context, repo, branch, quarantineDir string) error {
	if !validRepo(repo) {
		return fmt.Errorf("%w: %q", ErrForbiddenRepo, repo)
	}
	if err := validateAgentBranch(branch); err != nil {
		return err
	}
	if err := g.refuseDefaultBranch(ctx, repo, branch); err != nil {
		return err
	}
	dir, err := g.safeQuarantineDir(repo, branch, quarantineDir)
	if err != nil {
		return err
	}
	if _, err := Run(ctx, filepath.Dir(dir), nil,
		"-C", dir, "rev-parse", "--verify", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("gitops: quarantine branch: %w", err)
	}

	auth, err := g.authEnv(ctx, repo)
	if err != nil {
		return err
	}
	args := append(localArgs(repo),
		"-C", dir,
		"push", "--set-upstream",
		gitSource(repo),
		"refs/heads/"+branch+":refs/heads/"+branch,
	)
	if _, err := Run(ctx, filepath.Dir(dir), auth, args...); err != nil {
		return fmt.Errorf("gitops: push branch: %w", err)
	}
	return nil
}

// validateAgentBranch is the single allowlist for publish branches. main,
// master, refs/heads/* and every non-agent name are refused with the same
// sentinel so callers can audit the refusal uniformly.
func validateAgentBranch(branch string) error {
	if branch == "main" || branch == "master" || !branchPattern.MatchString(branch) {
		return fmt.Errorf("%w: %q", ErrForbiddenBranch, branch)
	}
	return nil
}

// refuseDefaultBranch compares the branch with the default branch recorded in
// the gateway-owned cache. A detached or missing cache cannot widen the
// allowlist: validateAgentBranch has already ruled out main/master.
func (g *Git) refuseDefaultBranch(ctx context.Context, repo, branch string) error {
	cache := g.cachePath(repo)
	st, err := os.Stat(cache)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gitops: stat cache: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("gitops: cache %q is not a directory", cache)
	}
	out, err := Run(ctx, g.CacheDir, nil, "-C", cache, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("gitops: cache default branch: %w", err)
	}
	def := strings.TrimSpace(string(out))
	if def == "" || strings.ContainsAny(def, "\r\n") {
		return fmt.Errorf("gitops: invalid default branch %q", def)
	}
	if def == branch {
		return fmt.Errorf("%w: %q is the default branch", ErrForbiddenBranch, branch)
	}
	return nil
}

// safeQuarantineDir keeps caller-supplied paths inside the gateway quarantine
// root and rejects symlink escapes. filepath.Rel is applied to both the lexical
// and resolved paths so ".." and a symlink cannot redirect the push.
func (g *Git) safeQuarantineDir(repo, branch, dir string) (string, error) {
	if dir == "" {
		dir = g.quarantinePath(repo, branch)
	}
	if !filepath.IsAbs(dir) || strings.ContainsRune(dir, 0) {
		return "", fmt.Errorf("gitops: unsafe quarantine dir %q", dir)
	}
	clean := filepath.Clean(dir)
	root := filepath.Clean(g.quarantineRoot())
	if !filepath.IsAbs(root) || !withinRoot(root, clean) {
		return "", fmt.Errorf("gitops: unsafe quarantine dir %q", dir)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return "", fmt.Errorf("gitops: quarantine dir: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("gitops: unsafe quarantine dir %q", dir)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("gitops: quarantine root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("gitops: quarantine dir: %w", err)
	}
	if !withinRoot(resolvedRoot, resolved) {
		return "", fmt.Errorf("gitops: unsafe quarantine dir %q", dir)
	}
	return clean, nil
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	return true
}
