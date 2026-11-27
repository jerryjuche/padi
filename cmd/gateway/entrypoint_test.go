package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEntrypointRendersConfigWithoutKey(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(bin, "opencode")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s' \"$*\" > \"$FAKE_ARGS\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(tmp, "args")
	cmd := exec.Command("sh", filepath.Join(root, "deploy", "session", "entrypoint.sh"))
	cmd.Env = append(os.Environ(),
		"HOME="+tmp,
		"PATH="+bin+":"+os.Getenv("PATH"),
		"FAKE_ARGS="+argsFile,
		"OPENCODE_BASE_CONFIG="+filepath.Join(root, "deploy", "session", "opencode.base.json"),
		"PADI_MODEL=padi/agent-chat",
		"PADI_TASK_ID=task-42",
		"SESSION_ID=ses-42",
		"PADI_MODEL_BASE_URL=http://router:9999/v1",
		"ROUTER_TOKEN=DO_NOT_WRITE_ME",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entrypoint: %v: %s", err, out)
	}
	rendered, err := os.ReadFile(filepath.Join(tmp, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "DO_NOT_WRITE_ME") {
		t.Fatal("model key leaked into rendered config")
	}
	var cfg map[string]any
	if err := json.Unmarshal(rendered, &cfg); err != nil {
		t.Fatal(err)
	}
	providers := cfg["providers"].(map[string]any)
	if len(providers) != 1 {
		t.Fatalf("providers=%v want router only", providers)
	}
	padi, ok := providers["padi"].(map[string]any)
	if !ok {
		t.Fatalf("padi provider missing: %v", providers)
	}
	models := padi["models"].(map[string]any)
	for _, alias := range []string{
		"agent-build", "agent-plan", "agent-chat", "agent-small", "agent-vision", "agent-embed", "agent-eval",
	} {
		if _, ok := models[alias]; !ok {
			t.Fatalf("model %s missing from rendered provider: %v", alias, models)
		}
	}
	agents := cfg["agent"].(map[string]any)
	if got := agents["build"].(map[string]any)["model"]; got != "padi/agent-build" {
		t.Fatalf("build model=%v", got)
	}
	if got := agents["plan"].(map[string]any)["model"]; got != "padi/agent-plan" {
		t.Fatalf("plan model=%v", got)
	}
	headers := padi["headers"].(map[string]any)
	if headers["X-Task-Id"] != "task-42" {
		t.Fatalf("headers=%+v", headers)
	}
	settings, ok := padi["settings"].(map[string]any)
	if !ok || settings["baseURL"] != "http://router:9999/v1" {
		t.Fatalf("settings=%+v", padi["settings"])
	}
	if cfg["model"] != "padi/agent-chat" {
		t.Fatalf("model=%v", cfg["model"])
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "serve --hostname 0.0.0.0 --port 4096" {
		t.Fatalf("args=%q", args)
	}
}
