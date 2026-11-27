package gitops

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHostileRepoIsRejectedWithoutExecution(t *testing.T) {
	marker := "/tmp/padi-hostile-marker"
	_ = os.Remove(marker)
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	copyFixture(t, "testdata/hostile-repo", source)

	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		if _, err := Run(ctx, source, nil, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	run("init", "--quiet")
	run("-c", "user.name=sandbox-agent", "-c", "user.email=noreply@sandbox.local",
		"commit", "--allow-empty", "--quiet", "-m", "base")
	base := strings.TrimSpace(string(mustRun(t, ctx, source, "rev-parse", "HEAD")))
	run("add", "-A")
	run("-c", "user.name=sandbox-agent", "-c", "user.email=noreply@sandbox.local",
		"commit", "--quiet", "-m", "hostile fixture")
	branch := "agent/hostile"
	run("checkout", "--quiet", "-b", branch)
	bundle := filepath.Join(root, "hostile.bundle")
	run("bundle", "create", bundle, "refs/heads/"+branch, "--not", base)

	g := &Git{CacheDir: filepath.Join(root, "cache"), QuarantineDir: filepath.Join(root, "quarantine"), VerifyDir: filepath.Join(root, "verify"), BundleMaxMB: 10}
	if _, err := g.EnsureCache(ctx, source); err != nil {
		t.Fatalf("ensure cache: %v", err)
	}
	if _, err := g.Quarantine(ctx, source, bundle, branch); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	dest := filepath.Join(g.verifyRoot(), "hostile-checkout")
	err := g.CheckoutForVerify(ctx, source, branch, dest)
	if !errors.Is(err, ErrHostileBundle) {
		t.Fatalf("checkout err=%v want hostile bundle", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hostile hook executed on host: marker exists (%v)", err)
	}
	t.Cleanup(func() { _ = os.Remove(marker) })
}

func mustRun(t *testing.T, ctx context.Context, dir string, args ...string) []byte {
	t.Helper()
	out, err := Run(ctx, dir, nil, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func TestInspectTreeTable(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, root string)
		hostile bool
	}{
		{"clean", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"symlink", func(t *testing.T, root string) {
			if err := os.Symlink("/etc/passwd", filepath.Join(root, "escape")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"embedded git metadata", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, ".git_config"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"executable hook", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, "hooks"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "hooks", "post-checkout"), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"gitmodules manifest", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte("[submodule \"x\"]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.prepare(t, root)
			err := InspectTree(root)
			if tc.hostile && !errors.Is(err, ErrHostileBundle) {
				t.Fatalf("err=%v want hostile", err)
			}
			if !tc.hostile && err != nil {
				t.Fatalf("clean tree rejected: %v", err)
			}
		})
	}
}
