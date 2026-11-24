package gitops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct {
	bundle    []byte
	copyErr   error
	execCalls int
}

func (f *fakeRunner) Exec(ctx context.Context, id string, cmd ...string) ([]byte, error) {
	f.execCalls++
	if len(cmd) == 0 || cmd[0] != bundleScript {
		return nil, errors.New("unexpected container command: " + strings.Join(cmd, " "))
	}
	return []byte("ok"), nil
}

func (f *fakeRunner) CopyOut(ctx context.Context, id, path, dest string, maxBytes int64) error {
	if f.copyErr != nil {
		return f.copyErr
	}
	if int64(len(f.bundle)) > maxBytes {
		return errors.New("fake copy: too large")
	}
	return os.WriteFile(dest, f.bundle, 0o600)
}

// makeLocalRepo builds a source repo through gitexec.Run and returns the base
// SHA and a validated agent branch carrying one extra commit.
func makeLocalRepo(t *testing.T, dir, branch string) (string, string) {
	t.Helper()
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		out, err := Run(ctx, dir, nil, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	run("init", "--quiet")
	run("-c", "user.name=sandbox-agent", "-c", "user.email=noreply@sandbox.local",
		"commit", "--allow-empty", "--quiet", "-m", "base")
	base := run("rev-parse", "HEAD")
	run("checkout", "--quiet", "-b", branch)
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "feature.txt")
	run("-c", "user.name=sandbox-agent", "-c", "user.email=noreply@sandbox.local",
		"commit", "--quiet", "-m", "feature")
	return base, run("rev-parse", "HEAD")
}

func makeBundle(t *testing.T, repo, dest, branch, base string) []byte {
	t.Helper()
	ctx := context.Background()
	if _, err := Run(ctx, repo, nil, "bundle", "create", dest, "refs/heads/"+branch, "--not", base); err != nil {
		t.Fatalf("bundle create: %v", err)
	}
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestExportBundleRoundTrip(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "source")
	branch := "agent/test-export"
	base, _ := makeLocalRepo(t, repo, branch)
	raw := makeBundle(t, repo, filepath.Join(root, "source.bundle"), branch, base)

	g := &Git{CacheDir: filepath.Join(root, "cache"), QuarantineDir: filepath.Join(root, "quarantine"), BundleMaxMB: 10}
	dest := filepath.Join(root, "exported.bundle")
	fr := &fakeRunner{bundle: raw}
	if err := g.ExportBundle(context.Background(), fr, "container-1", base, branch, dest); err != nil {
		t.Fatal(err)
	}
	if fr.execCalls != 1 {
		t.Fatalf("container exec calls=%d", fr.execCalls)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("exported bundle differs: err=%v", err)
	}
	if _, err := g.EnsureCache(context.Background(), repo); err != nil {
		t.Fatalf("ensure cache: %v", err)
	}
	head, err := g.Quarantine(context.Background(), repo, dest, branch)
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if head == "" {
		t.Fatal("empty quarantine head")
	}
	checkout := filepath.Join(root, "verify", "checkout")
	if err := g.CheckoutForVerify(context.Background(), repo, branch, checkout); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(checkout, "feature.txt")); err != nil || string(data) != "feature\n" {
		t.Fatalf("checkout content=%q err=%v", data, err)
	}
}

func TestExportBundleRejectsBadInputs(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "source")
	branch := "agent/test-reject"
	base, _ := makeLocalRepo(t, repo, branch)
	raw := makeBundle(t, repo, filepath.Join(root, "source.bundle"), branch, base)
	g := &Git{BundleMaxMB: 10}
	dest := filepath.Join(root, "out.bundle")
	cases := []struct {
		name   string
		base   string
		branch string
	}{
		{"bad base sha", "not-a-sha", branch},
		{"bad branch", base, "main"},
		{"missing branch", base, "agent/nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := g.ExportBundle(context.Background(), &fakeRunner{bundle: raw}, "cid", tc.base, tc.branch, dest)
			if !errors.Is(err, ErrExportFailed) {
				t.Fatalf("err=%v want export_failed", err)
			}
		})
	}
	big := filepath.Join(root, "oversize.bundle")
	if err := os.WriteFile(big, make([]byte, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	g.BundleMaxMB = 1
	if err := g.verifyBundle(context.Background(), big, branch); !errors.Is(err, ErrExportFailed) {
		t.Fatalf("size cap error=%v", err)
	}
	g.BundleMaxMB = 10
	fr := &fakeRunner{bundle: raw, copyErr: errors.New("copy failed")}
	if err := g.ExportBundle(context.Background(), fr, "cid", base, branch, dest); !errors.Is(err, ErrExportFailed) {
		t.Fatalf("copy error: %v", err)
	}
	if err := g.ExportBundle(context.Background(), fr, "cid", base, branch, "relative.bundle"); !errors.Is(err, ErrExportFailed) {
		t.Fatalf("relative dest: %v", err)
	}
}
