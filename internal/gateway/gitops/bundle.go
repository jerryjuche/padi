package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// ErrExportFailed is returned when the container bundle is missing, too
	// large, or does not verify against the expected branch.
	ErrExportFailed = errors.New("gitops: export_failed")
	shaPattern      = regexp.MustCompile(`^[0-9a-fA-F]{40}(?:[0-9a-fA-F]{24})?$`)
)

// Runner is the subset of the container runner needed for bundle export. It is
// deliberately small so the git package can be tested without Docker.
type Runner interface {
	Exec(ctx context.Context, id string, cmd ...string) ([]byte, error)
	CopyOut(ctx context.Context, id, path, dest string, maxBytes int64) error
}

// bundleScript is the fixed container-side command. It lives in the session
// image, never in a writable workspace, and accepts only validated arguments.
const bundleScript = "/usr/local/bin/padi-export-bundle"

// Docker's archive endpoint cannot read a runtime tmpfs path, so the fixed
// script writes into the bind-mounted workspace. The file is created after the
// commit and removed before every export, so it never enters the bundle.
const bundleInContainer = "/workspace/.padi-task.bundle"

// ExportBundle commits the agent's work inside its container, exports one
// bundle through the fixed script, copies out exactly one file under the size
// cap, and verifies it on the host. No host git call touches the workspace.
func (g *Git) ExportBundle(ctx context.Context, r Runner, id, base, branch, dest string) error {
	if !shaPattern.MatchString(base) {
		return fmt.Errorf("%w: invalid base %q", ErrExportFailed, base)
	}
	if !branchPattern.MatchString(branch) {
		return fmt.Errorf("%w: %w: %q", ErrExportFailed, ErrForbiddenBranch, branch)
	}
	cleanDest, err := safeBundleDest(dest)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrExportFailed, err)
	}
	if r == nil {
		return fmt.Errorf("%w: no runner", ErrExportFailed)
	}
	if _, err := r.Exec(ctx, id, bundleScript, base, branch, bundleInContainer); err != nil {
		return fmt.Errorf("%w: container export: %v", ErrExportFailed, err)
	}
	capBytes := int64(g.BundleMaxMB) * 1024 * 1024
	if capBytes <= 0 {
		capBytes = 200 * 1024 * 1024
	}
	if err := r.CopyOut(ctx, id, bundleInContainer, cleanDest, capBytes); err != nil {
		return fmt.Errorf("%w: copy out: %v", ErrExportFailed, err)
	}
	return g.verifyBundle(ctx, cleanDest, branch)
}

func safeBundleDest(dest string) (string, error) {
	if dest == "" || filepath.IsAbs(dest) == false || strings.Contains(dest, "\x00") {
		return "", errors.New("bundle destination must be an absolute path")
	}
	clean := filepath.Clean(dest)
	if clean == "/" || strings.Contains(clean, "..") {
		return "", errors.New("unsafe bundle destination")
	}
	if _, err := os.Lstat(filepath.Dir(clean)); err != nil {
		return "", err
	}
	return clean, nil
}

func (g *Git) verifyBundle(ctx context.Context, path, branch string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: stat: %v", ErrExportFailed, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: bundle is not a regular file", ErrExportFailed)
	}
	capBytes := int64(g.BundleMaxMB) * 1024 * 1024
	if capBytes <= 0 {
		capBytes = 200 * 1024 * 1024
	}
	if info.Size() > capBytes {
		return fmt.Errorf("%w: bundle %d bytes exceeds %d", ErrExportFailed, info.Size(), capBytes)
	}
	// Full prerequisite verification happens in Quarantine, where the
	// gateway-owned cache supplies the base commits. Here we check the file
	// shape and advertised refs without touching an agent workspace.
	out, err := Run(ctx, filepath.Dir(path), nil, "bundle", "list-heads", path)
	if err != nil {
		return fmt.Errorf("%w: list heads: %v", ErrExportFailed, err)
	}
	want := "refs/heads/" + branch
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == want {
			return nil
		}
	}
	return fmt.Errorf("%w: branch %q missing from bundle", ErrExportFailed, branch)
}
