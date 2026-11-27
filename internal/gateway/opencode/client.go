// Package opencode is the M1 client for the real OpenCode v2 HTTP API.
// Endpoint and auth details were verified against opencode v2.4.0; see
// docs/decisions.md VERIFY V1.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client talks to one session container's `opencode serve` process.
type Client struct {
	BaseURL  string
	Password string // OPENCODE_PASSWORD in the container; never logged
	HTTP     *http.Client
}

// RawEvent is one SSE frame from GET /api/event.
type RawEvent struct {
	Event string
	Data  json.RawMessage
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// Deadlines come from the caller's context; a fixed client timeout would
	// cut off long-lived /api/event streams.
	return &http.Client{}
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("opencode: marshal request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Password != "" {
		req.SetBasicAuth("opencode", c.Password)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode: %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("opencode: %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// Healthy probes GET /api/info.
func (c *Client) Healthy(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/api/info", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// CreateSession calls POST /api/session and returns the new session ID.
func (c *Client) CreateSession(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, "/api/session", map[string]any{})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("opencode: decode session: %w", err)
	}
	if out.Data.ID == "" {
		return "", fmt.Errorf("opencode: empty session id")
	}
	return out.Data.ID, nil
}

// SessionActivity is a bounded view of what an OpenCode session is doing.
// OpenCode keeps provider transport failures in assistant messages as retry
// metadata; without reading them the gateway cannot tell a silent retry loop
// from a healthy long-running turn.
type SessionActivity struct {
	Retry     int
	LastError string
	Tokens    int
}

// Activity reads the session message list and returns the latest retry state.
// It never exposes message content to callers, only counters and the transport
// error needed for the live card.
func (c *Client) Activity(ctx context.Context, sessionID string) (SessionActivity, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sessionID)+"/message", nil)
	if err != nil {
		return SessionActivity{}, err
	}
	defer resp.Body.Close()
	var envelope struct {
		Data []struct {
			Type  string `json:"type"`
			Retry *struct {
				Attempt int `json:"attempt"`
				Error   *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"retry"`
			Tokens *struct {
				Input     int `json:"input"`
				Output    int `json:"output"`
				Reasoning int `json:"reasoning"`
			} `json:"tokens"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return SessionActivity{}, fmt.Errorf("opencode: decode session activity: %w", err)
	}
	var out SessionActivity
	for _, msg := range envelope.Data {
		if msg.Tokens != nil {
			out.Tokens += msg.Tokens.Input + msg.Tokens.Output + msg.Tokens.Reasoning
		}
		if msg.Type != "assistant" || msg.Retry == nil {
			continue
		}
		if msg.Retry.Attempt > out.Retry {
			out.Retry = msg.Retry.Attempt
		}
		if msg.Retry.Error != nil && msg.Retry.Error.Message != "" && msg.Retry.Attempt >= out.Retry {
			out.LastError = msg.Retry.Error.Message
		}
	}
	return out, nil
}

// SetModel is per-session in v2; there is no per-message model field.
func (c *Client) SetModel(ctx context.Context, sessionID, model string) error {
	if model == "" {
		return nil
	}
	provider, id, ok := strings.Cut(model, "/")
	if !ok || provider == "" || id == "" {
		return fmt.Errorf("opencode: model must be provider/id, got %q", model)
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sessionID)+"/model",
		map[string]any{"model": map[string]string{"providerID": provider, "id": id}})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// SetAgent selects the primary agent for a session. OpenCode v2 exposes this
// as POST /api/session/{id}/agent; the built-in plan agent and any custom
// agents declared in opencode.base.json are valid values.
func (c *Client) SetAgent(ctx context.Context, sessionID, agent string) error {
	if agent == "" {
		return nil
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sessionID)+"/agent",
		map[string]any{"agent": agent})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Prompt posts a user message. Model and agent are applied to the session
// first when set, so a follow-up can switch between build, plan and research
// without losing the OpenCode conversation history.
func (c *Client) PromptWithAgent(ctx context.Context, sessionID, text, model, agent string) error {
	if err := c.SetModel(ctx, sessionID, model); err != nil {
		return err
	}
	if err := c.SetAgent(ctx, sessionID, agent); err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sessionID)+"/prompt",
		map[string]any{"text": text})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Prompt posts a user message with the session's current agent.
func (c *Client) Prompt(ctx context.Context, sessionID, text, model string) error {
	return c.PromptWithAgent(ctx, sessionID, text, model, "")
}

// Abort interrupts a running session (POST .../interrupt in v2).
func (c *Client) Abort(ctx context.Context, sessionID string) error {
	resp, err := c.do(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sessionID)+"/interrupt", map[string]any{})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Events opens GET /api/event and streams decoded SSE frames. The stream is
// volatile by contract; reconnects must be handled by the caller.
func (c *Client) Events(ctx context.Context) (<-chan RawEvent, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/event", nil)
	if err != nil {
		return nil, err
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		resp.Body.Close()
		return nil, fmt.Errorf("opencode: expected text/event-stream, got %q", ct)
	}
	out := make(chan RawEvent, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		var name string
		var data bytes.Buffer
		flush := func() bool {
			if name == "" && data.Len() == 0 {
				return true
			}
			payload := bytes.TrimSpace(data.Bytes())
			// OpenCode v2 emits data-only frames with the event name inside
			// the JSON "type" field. Translate resolves that when Event is
			// empty, so never discard a payload just because no event: line
			// was present.
			if len(payload) > 0 {
				ev := RawEvent{Event: name, Data: json.RawMessage(payload)}
				select {
				case out <- ev:
				case <-ctx.Done():
					return false
				}
			}
			name, data = "", bytes.Buffer{}
			return true
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if !flush() {
					return
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			key, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch key {
			case "event":
				name = value
			case "data":
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(value)
			}
		}
		flush()
	}()
	return out, nil
}
