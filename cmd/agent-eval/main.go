// Command agent-eval asks the router for an independent evaluation. The router
// excludes every provider that can serve agent-build, so this command never
// judges a build with the same upstream that produced it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultAlias = "agent-eval"

type options struct {
	Router    string
	Alias     string
	TaskID    string
	SessionID string
	Prompt    string
	TokenEnv  string
	Timeout   time.Duration
}

type result struct {
	Alias    string `json:"alias"`
	Provider string `json:"provider"`
	Content  string `json:"content"`
}

func parseArgs(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("agent-eval", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.Router, "router", "http://127.0.0.1:8090/v1", "router /v1 base URL")
	fs.StringVar(&o.Alias, "alias", defaultAlias, "evaluation alias")
	fs.StringVar(&o.TaskID, "task-id", "", "task ID sent as X-Task-Id")
	fs.StringVar(&o.SessionID, "session-id", "", "session ID sent as X-Session-Id")
	fs.StringVar(&o.Prompt, "prompt", "", "evaluation prompt (or read stdin)")
	fs.StringVar(&o.TokenEnv, "token-env", "ROUTER_TOKEN", "environment variable holding the router token")
	fs.DurationVar(&o.Timeout, "timeout", 90*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("agent-eval: unexpected argument %q", fs.Arg(0))
	}
	o.Router = strings.TrimSpace(o.Router)
	o.Alias = strings.TrimSpace(o.Alias)
	if o.Router == "" || o.Alias == "" {
		return options{}, fmt.Errorf("agent-eval: router and alias are required")
	}
	if o.Timeout <= 0 {
		return options{}, fmt.Errorf("agent-eval: timeout must be positive")
	}
	return o, nil
}

// evaluate sends one non-streaming completion through the alias and returns
// only safe response metadata. The caller's token is never echoed.
func evaluate(ctx context.Context, o options, stdin io.Reader, lookup func(string) string, client *http.Client) (result, error) {
	if o.Prompt == "" {
		raw, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
		if err != nil {
			return result{}, fmt.Errorf("agent-eval: read prompt: %w", err)
		}
		o.Prompt = strings.TrimSpace(string(raw))
	}
	if o.Prompt == "" {
		return result{}, fmt.Errorf("agent-eval: prompt is empty")
	}
	if lookup == nil {
		lookup = os.Getenv
	}
	token := lookup(o.TokenEnv)
	if token == "" {
		return result{}, fmt.Errorf("agent-eval: %s is empty", o.TokenEnv)
	}

	body, err := json.Marshal(map[string]any{
		"model": o.Alias, "stream": false,
		"messages": []map[string]string{{"role": "user", "content": o.Prompt}},
	})
	if err != nil {
		return result{}, fmt.Errorf("agent-eval: encode request: %w", err)
	}
	endpoint := strings.TrimRight(o.Router, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return result{}, fmt.Errorf("agent-eval: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if o.TaskID != "" {
		req.Header.Set("X-Task-Id", o.TaskID)
	}
	if o.SessionID != "" {
		req.Header.Set("X-Session-Id", o.SessionID)
	}
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return result{}, fmt.Errorf("agent-eval: request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return result{}, fmt.Errorf("agent-eval: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(raw))
		if len(message) > 300 {
			message = message[:300]
		}
		return result{}, fmt.Errorf("agent-eval: router status %d: %s", resp.StatusCode, message)
	}

	provider := strings.TrimSpace(resp.Header.Get("X-Padi-Provider"))
	if provider == "" {
		return result{}, fmt.Errorf("agent-eval: router did not report the selected provider")
	}
	if got := strings.TrimSpace(resp.Header.Get("X-Padi-Alias")); got != "" && got != o.Alias {
		return result{}, fmt.Errorf("agent-eval: router served alias %q, want %q", got, o.Alias)
	}
	if buildProvider := strings.TrimSpace(lookup("PADI_BUILD_PROVIDER")); buildProvider != "" &&
		strings.EqualFold(buildProvider, provider) {
		return result{}, fmt.Errorf("agent-eval: provider isolation failed (%s served both build and eval)", provider)
	}

	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return result{}, fmt.Errorf("agent-eval: decode response: %w", err)
	}
	if decoded.Error != nil && decoded.Error.Message != "" {
		return result{}, fmt.Errorf("agent-eval: router error: %s", decoded.Error.Message)
	}
	if len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		return result{}, fmt.Errorf("agent-eval: response has no content")
	}
	return result{Alias: o.Alias, Provider: provider, Content: decoded.Choices[0].Message.Content}, nil
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, lookup func(string) string) int {
	o, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
	defer cancel()
	out, err := evaluate(ctx, o, stdin, lookup, nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(stderr, "agent-eval: write output:", err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}
