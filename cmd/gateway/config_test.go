package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	body := `
telegram:
  poll_timeout_s: 5
container:
  network: "test-net"
  memory_mb: 512
opencode:
  model: "openai/gpt-5-nano"
git:
  cache_dir: "/tmp/cache"
  workspaces_dir: "/tmp/workspaces"
store:
  dsn_env: "MY_DSN"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Telegram.PollTimeoutS != 5 || c.Telegram.EditIntervalMS != 1200 {
		t.Fatalf("telegram cfg=%+v", c.Telegram)
	}
	if c.Container.Network != "test-net" || c.Container.MemoryMB != 512 || c.Container.CPUs != 1 {
		t.Fatalf("container cfg=%+v", c.Container)
	}
	if c.Store.DSNEnv != "MY_DSN" {
		t.Fatalf("store cfg=%+v", c.Store)
	}
}

func TestValidateRejectsConcurrency(t *testing.T) {
	c := defaults()
	c.Sessions.MaxSessions = 2
	if err := c.validate(); err == nil {
		t.Fatal("expected error")
	}
	c = defaults()
	if err := c.validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}
}

func TestSecretFromEnvAny(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{name: "canonical", env: map[string]string{"TELEGRAM_ALLOWED_USER_ID": "123"}, want: "123"},
		{name: "alias", env: map[string]string{"TELEGRAM_OWNER_ID": "456"}, want: "456"},
		{name: "canonical wins", env: map[string]string{"TELEGRAM_ALLOWED_USER_ID": "123", "TELEGRAM_OWNER_ID": "456"}, want: "123"},
		{name: "missing", env: map[string]string{}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TELEGRAM_ALLOWED_USER_ID", "")
			t.Setenv("TELEGRAM_OWNER_ID", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := secretFromEnvAny("Telegram owner ID", "TELEGRAM_ALLOWED_USER_ID", "TELEGRAM_OWNER_ID")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got=%q want %q", got, tc.want)
			}
		})
	}
}

func TestParseRepo(t *testing.T) {
	a := &application{cfg: defaults()}
	a.cfg.Git.DefaultRepo = "/srv/local/default"
	cases := []struct {
		text, repo, prompt string
	}{
		{"clone /tmp/source and add a line", "/tmp/source", "clone /tmp/source and add a line"},
		{"add a line to README", "/srv/local/default", "add a line to README"},
	}
	for _, tc := range cases {
		repo, prompt := a.parseRepo(tc.text)
		if repo != tc.repo || prompt != tc.prompt {
			t.Fatalf("parseRepo(%q)=%q,%q want %q,%q", tc.text, repo, prompt, tc.repo, tc.prompt)
		}
	}
}
