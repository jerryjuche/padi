// Command agent-verify verifies a checkout and exits 0 only on a pass verdict.
// It is designed to run in a fresh verify container against a quarantine
// checkout, never inside the agent's container.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jerryjuche/padi/internal/verify/detect"
	"github.com/jerryjuche/padi/internal/verify/report"
	"github.com/jerryjuche/padi/internal/verify/runner"
	"github.com/jerryjuche/padi/internal/verify/tampering"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent-verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "json", "output format: json, text or markdown")
	diffPath := fs.String("diff", "", "optional path to a unified diff for the tampering guard")
	timeout := fs.Duration("timeout", 0, "total verification timeout (default 10m)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dir := fs.Arg(0)
	if dir == "" {
		dir = "."
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(stderr, "agent-verify: not a directory: %s\n", dir)
		return 2
	}

	var diff tampering.Diff
	if *diffPath != "" {
		raw, err := os.ReadFile(*diffPath)
		if err != nil {
			fmt.Fprintf(stderr, "agent-verify: read diff: %v\n", err)
			return 2
		}
		diff = tampering.ParseDiff(string(raw))
	}
	testFiles := tampering.ScanTestFiles(dir)
	if len(testFiles) > 0 {
		diff.HasTestFiles = true
		diff.TestFiles = appendUnique(diff.TestFiles, testFiles...)
	}

	verdict := runner.Run(context.Background(), dir, *timeout)
	findings := tampering.Check(diff, verdict)
	verdict = tampering.Apply(verdict, findings)

	switch *format {
	case "json":
		out := struct {
			Verdict  string              `json:"verdict"`
			Steps    []detect.Step       `json:"steps"`
			Findings []tampering.Finding `json:"findings,omitempty"`
		}{
			Verdict:  verdict.Verdict,
			Steps:    verdict.Steps,
			Findings: findings,
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "agent-verify: encode verdict: %v\n", err)
			return 2
		}
	case "text":
		fmt.Fprint(stdout, report.RenderText(verdict, findings))
	case "markdown":
		fmt.Fprint(stdout, report.RenderMarkdown(verdict, findings))
	default:
		fmt.Fprintf(stderr, "agent-verify: unknown format %q (want json, text or markdown)\n", *format)
		return 2
	}

	if verdict.Verdict == detect.VerdictPass {
		return 0
	}
	return 1
}

func appendUnique(dst []string, values ...string) []string {
	seen := make(map[string]bool, len(dst)+len(values))
	for _, v := range dst {
		seen[v] = true
	}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			dst = append(dst, v)
		}
	}
	return dst
}
