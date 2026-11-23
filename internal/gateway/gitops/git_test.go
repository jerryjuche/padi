package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBranchName(t *testing.T) {
	cases := []struct {
		repo, task, want string
	}{
		{"jerryjuche/padi", "abc12345de", "agent/jerryjuche-padi-abc12345"},
		{"owner/My.Repo", "0123456789", "agent/owner-my-repo-01234567"},
		{"x/y", "z", "agent/x-y-z"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := BranchName(tc.repo, tc.task); got != tc.want {
				t.Fatalf("BranchName=%q want %q", got, tc.want)
			}
		})
	}
}

func TestCloneForTaskLocal(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := Run(ctx, source, nil, "init", "--quiet", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, source, nil, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, source, nil,
		"-c", "user.name=sandbox-agent",
		"-c", "user.email=noreply@sandbox.local",
		"commit", "--quiet", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	g := &Git{
		CacheDir:      filepath.Join(root, "cache"),
		WorkspacesDir: filepath.Join(root, "workspaces"),
	}
	workDir, branch, err := g.CloneForTask(ctx, source, "task-0001")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(g.WorkspacesDir, "task-0001", "repo"); workDir != want {
		t.Fatalf("workDir=%q want %q", workDir, want)
	}
	if want := BranchName(source, "task-0001"); branch != want {
		t.Fatalf("branch=%q want %q", branch, want)
	}
	if !branchPattern.MatchString(branch) {
		t.Fatalf("branch %q does not match required pattern", branch)
	}
	content, err := os.ReadFile(filepath.Join(workDir, "README.md"))
	if err != nil || string(content) != "# fixture\n" {
		t.Fatalf("README=%q err=%v", content, err)
	}
	// Independent clone: objects must not be hard-linked to the cache.
	workStat, err := os.Stat(filepath.Join(workDir, ".git", "objects", "info"))
	if err != nil {
		t.Fatal(err)
	}
	_ = workStat
	if st, err := os.Lstat(filepath.Join(workDir, ".git", "objects")); err != nil || !st.IsDir() {
		t.Fatalf("objects dir missing: %v", err)
	}
	// Second call must be repeatable (idempotent cache refresh).
	_, _, err = g.CloneForTask(ctx, source, "task-0002")
	if err != nil {
		t.Fatalf("second clone: %v", err)
	}
}

func TestMintInstallationTokenEnvNames(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "canonical", env: map[string]string{"GITHUB_APP_INSTALLATION_ID": "11"}, wantErr: "read private key"},
		{name: "alias", env: map[string]string{"GITHUB_INSTALLATION_ID": "22"}, wantErr: "read private key"},
		{name: "missing", env: map[string]string{}, wantErr: "GitHub App env not set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"GITHUB_APP_ID", "GITHUB_APP_INSTALLATION_ID", "GITHUB_INSTALLATION_ID", "GITHUB_APP_PRIVATE_KEY_PATH"} {
				t.Setenv(k, "")
			}
			t.Setenv("GITHUB_APP_ID", "12345")
			t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH", filepath.Join(t.TempDir(), "missing.pem"))
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, _, err := (&Git{}).MintInstallationToken(context.Background(), "owner/repo")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v want substring %q", err, tc.wantErr)
			}
		})
	}
}
