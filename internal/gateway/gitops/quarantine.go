package gitops

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrHostileBundle marks content that must never reach a verify container.
var ErrHostileBundle = errors.New("gitops: hostile bundle")

var hookNames = map[string]bool{
	"pre-commit": true, "prepare-commit-msg": true, "commit-msg": true,
	"post-commit": true, "pre-push": true, "post-checkout": true,
	"post-merge": true, "pre-receive": true, "post-receive": true,
}

func (g *Git) quarantineRoot() string {
	if g.QuarantineDir != "" {
		return g.QuarantineDir
	}
	return filepath.Join(g.WorkspacesDir, "..", "quarantine")
}

func (g *Git) verifyRoot() string {
	if g.VerifyDir != "" {
		return g.VerifyDir
	}
	return filepath.Join(g.WorkspacesDir, "..", "verify")
}

func (g *Git) quarantinePath(repo, branch string) string {
	sum := sha256.Sum256([]byte(repo + "\n" + branch))
	return filepath.Join(g.quarantineRoot(), fmt.Sprintf("%x", sum[:12])+".git")
}

// Quarantine verifies a bundle and fetches exactly one validated branch into a
// gateway-owned bare repository. All git invocations use gitexec.Run with
// hooks disabled and object checking enabled; the agent workspace is untouched.
func (g *Git) Quarantine(ctx context.Context, repo, bundlePath, branch string) (string, error) {
	if !validRepo(repo) {
		return "", fmt.Errorf("%w: %q", ErrForbiddenRepo, repo)
	}
	if !branchPattern.MatchString(branch) {
		return "", fmt.Errorf("%w: %q", ErrForbiddenBranch, branch)
	}
	cleanBundle := filepath.Clean(bundlePath)
	info, err := os.Lstat(cleanBundle)
	if err != nil {
		return "", fmt.Errorf("%w: stat bundle: %v", ErrExportFailed, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: bundle is not a regular file", ErrExportFailed)
	}
	if max := int64(g.BundleMaxMB) * 1024 * 1024; max > 0 && info.Size() > max {
		return "", fmt.Errorf("%w: bundle too large: %d", ErrExportFailed, info.Size())
	}
	qdir := g.quarantinePath(repo, branch)
	if err := os.MkdirAll(filepath.Dir(qdir), 0o700); err != nil {
		return "", err
	}
	if _, err := os.Stat(qdir); errors.Is(err, os.ErrNotExist) {
		if _, err := Run(ctx, filepath.Dir(qdir), nil, "init", "--bare", "--quiet", qdir); err != nil {
			return "", fmt.Errorf("gitops: init quarantine: %w", err)
		}
	}
	// A thin bundle needs its prerequisite base commits. Seed them from the
	// gateway-owned cache under remote refs; the agent's branch is never
	// fetched from the workspace.
	if cache := g.cachePath(repo); g.CacheDir != "" {
		if _, err := os.Stat(cache); err == nil {
			seed := []string{"-c", "protocol.file.allow=always", "-C", qdir,
				"fetch", "--no-tags", cache, "+refs/heads/*:refs/remotes/cache/*"}
			if _, err := Run(ctx, filepath.Dir(qdir), nil, seed...); err != nil {
				return "", fmt.Errorf("gitops: seed quarantine: %w", err)
			}
		}
	}
	if _, err := Run(ctx, qdir, nil, "bundle", "verify", cleanBundle); err != nil {
		return "", fmt.Errorf("%w: verify: %v", ErrExportFailed, err)
	}
	fetch := []string{"-c", "transfer.fsckObjects=true", "-c", "protocol.file.allow=always",
		"-C", qdir, "fetch", "--no-tags", cleanBundle,
		"refs/heads/" + branch + ":refs/heads/" + branch}
	if _, err := Run(ctx, filepath.Dir(qdir), nil, fetch...); err != nil {
		return "", fmt.Errorf("gitops: quarantine fetch: %w", err)
	}
	out, err := Run(ctx, filepath.Dir(qdir), nil, "-C", qdir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("gitops: quarantine head: %w", err)
	}
	head := strings.TrimSpace(string(out))
	if !shaPattern.MatchString(head) {
		return "", fmt.Errorf("gitops: invalid quarantine head %q", head)
	}
	return head, nil
}

// CheckoutForVerify clones the quarantined branch into a fresh directory and
// refuses hostile trees before any verify container is started.
func (g *Git) CheckoutForVerify(ctx context.Context, repo, branch, dest string) error {
	if !validRepo(repo) || !branchPattern.MatchString(branch) {
		return fmt.Errorf("%w: %q / %q", ErrForbiddenBranch, repo, branch)
	}
	if dest == "" || !filepath.IsAbs(dest) || strings.Contains(dest, "..") {
		return fmt.Errorf("gitops: unsafe verify destination %q", dest)
	}
	qdir := g.quarantinePath(repo, branch)
	if _, err := os.Stat(filepath.Join(qdir, "HEAD")); err != nil {
		return fmt.Errorf("gitops: no quarantine for %s: %w", branch, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	_ = os.RemoveAll(dest)
	clone := append(localArgs(qdir), "clone", "--no-hardlinks", "--quiet",
		"--branch", branch, qdir, dest)
	if _, err := Run(ctx, filepath.Dir(dest), nil, clone...); err != nil {
		return fmt.Errorf("gitops: verify checkout: %w", err)
	}
	return InspectTree(dest)
}

// InspectTree rejects symlinks, embedded git metadata, setuid/setgid files and
// executable hook names. It is pure filesystem inspection: no file is opened
// for execution and no git process runs in the checked-out tree.
func InspectTree(root string) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: verify root is not a plain directory", ErrHostileBundle)
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel != "." {
			parts := strings.Split(rel, string(os.PathSeparator))
			for i, part := range parts {
				lower := strings.ToLower(part)
				// The clone's own top-level .git is expected; a nested .git or
				// any .git_* file is metadata smuggled in by the bundle.
				if (i > 0 && lower == ".git") || strings.HasPrefix(lower, ".git_") {
					return fmt.Errorf("%w: git metadata %q", ErrHostileBundle, rel)
				}
			}
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink %q", ErrHostileBundle, rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return fmt.Errorf("%w: privileged mode on %q", ErrHostileBundle, rel)
		}
		if hookNames[strings.ToLower(d.Name())] && info.Mode()&0o111 != 0 && d.Type().IsRegular() {
			return fmt.Errorf("%w: executable hook %q", ErrHostileBundle, rel)
		}
		return nil
	})
}
