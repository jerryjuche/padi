package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunUnrecognizedProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{dir}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for unverifiable", code)
	}
	var out struct {
		Verdict string `json:"verdict"`
		Steps   []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON %q: %v", stdout.String(), err)
	}
	if out.Verdict != "unverifiable" {
		t.Fatalf("verdict = %q, want unverifiable", out.Verdict)
	}
	if len(out.Steps) != 1 || out.Steps[0].Name != "detect" || out.Steps[0].OK {
		t.Fatalf("steps = %#v, want one failed detect step", out.Steps)
	}
}

func TestRunBadDirectoryAndFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{filepath.Join(t.TempDir(), "missing")}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2 for a bad directory", code)
	}
	stderr.Reset()
	if code := run([]string{"-format", "yaml", t.TempDir()}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2 for a bad format", code)
	}
	if !strings.Contains(stderr.String(), "unknown format") {
		t.Fatalf("stderr = %q, want an unknown format message", stderr.String())
	}
}

func TestRunTextFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-format", "text", t.TempDir()}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "verdict: unverifiable") {
		t.Fatalf("stdout = %q, want a text verdict", stdout.String())
	}
}
