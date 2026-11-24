// Package detect identifies the verification commands that apply to a project
// checkout. It is intentionally dependency-free and never executes anything.
package detect

import (
	"os"
	"path/filepath"
	"strings"
)

// Verdict values used by the verifier and the approval card.
const (
	VerdictPass         = "pass"
	VerdictFail         = "fail"
	VerdictUnverifiable = "unverifiable"
)

// Step is one executed verification command. OutputTail is always redacted and
// capped by the runner before it reaches this struct.
type Step struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	DurationMS int64  `json:"duration_ms"`
	OutputTail string `json:"output_tail"`
}

// Verdict is the complete result of a verification run.
type Verdict struct {
	Verdict string `json:"verdict"`
	Steps   []Step `json:"steps"`
}

// Runner describes a command to execute in a checkout directory.
type Runner struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// String renders the command line for logs and error messages.
func (r Runner) String() string {
	parts := make([]string, 0, 1+len(r.Args))
	if r.Command != "" {
		parts = append(parts, r.Command)
	}
	parts = append(parts, r.Args...)
	return strings.Join(parts, " ")
}

// Detect returns the runners recognized in dir. Order is fixed: Go, Node,
// Python, Rust. An empty result means there is nothing to verify; callers must
// report "unverifiable" rather than "pass".
func Detect(dir string) []Runner {
	var out []Runner
	if Exists(dir, "go.mod") {
		out = append(out, Runner{
			Name:    "go test",
			Command: "go",
			Args:    []string{"test", "./..."},
		})
	}
	if Exists(dir, "package.json") {
		out = append(out, Runner{
			Name:    "npm test",
			Command: "npm",
			Args:    []string{"test"},
		})
	}
	if Exists(dir, "pyproject.toml") || Exists(dir, "setup.py") || Exists(dir, "requirements.txt") {
		out = append(out, Runner{
			Name:    "pytest",
			Command: "pytest",
		})
	}
	if Exists(dir, "Cargo.toml") {
		out = append(out, Runner{
			Name:    "cargo test",
			Command: "cargo",
			Args:    []string{"test"},
		})
	}
	return out
}

// Recognized reports whether Detect would return at least one runner.
func Recognized(dir string) bool {
	return len(Detect(dir)) > 0
}

// Exists reports whether a regular file named name is present at the top level
// of dir.
func Exists(dir, name string) bool {
	if dir == "" {
		dir = "."
	}
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !info.IsDir()
}
