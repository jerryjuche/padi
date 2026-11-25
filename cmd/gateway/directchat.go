package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// directSystem tells the model when a message belongs in the workspace.
	// The marker lines are a gateway protocol, never owner-facing copy.
	directSystemPrompt = `You are Padi, a concise conversational assistant talking to the owner on Telegram.

This chat connection has no repository tools. Do not claim that you inspected files, ran commands, made changes, pushed, or opened a PR unless the gateway has handed the request to a workspace.

Use the conversation history. Answer the current message directly in 2-4 short sentences: the result or thought, one useful detail, then one concrete next step tied to this request. Keep it under 600 characters. Never repeat a settled answer or re-ask a settled question.

Routing protocol — follow it exactly:
- If the request needs work in the active repository (edits, file inspection, tests, commands, diffs, or repo-specific facts), reply with exactly one line: WORKSPACE_TASK: <the owner's request, unchanged>. Do not add any other text.
- If no repository is open and the request needs code or file changes, reply with exactly one line: NEEDS_REPO: <one short question offering clone owner/repo and a chat-only answer>. Do not add any other text.
- Otherwise answer normally. Questions about how this assistant or workflow works are normal chat, not workspace tasks.`

	maxDirectTurns = 12
	maxDirectChars = 4000
)

type directKind string

const (
	directAnswer    directKind = "answer"
	directWorkspace directKind = "workspace"
	directNeedsRepo directKind = "needs_repo"
)

type directTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type routerChatClient struct {
	BaseURL string
	Token   string
	Model   string
	HTTP    *http.Client
}

type chatCompletionRequest struct {
	Model    string       `json:"model"`
	Stream   bool         `json:"stream"`
	Messages []directTurn `json:"messages"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends one non-streaming turn through the existing model router.
// The router owns provider selection, retry, rate limits and key isolation.
func (c *routerChatClient) Complete(ctx context.Context, system string, history []directTurn, user string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("gateway: chat client is not configured")
	}
	base := strings.TrimSpace(c.BaseURL)
	if base == "" {
		return "", fmt.Errorf("gateway: chat client base URL is empty")
	}
	model := strings.TrimSpace(c.Model)
	if model == "" {
		return "", fmt.Errorf("gateway: chat client model is empty")
	}

	messages := make([]directTurn, 0, len(history)+2)
	messages = append(messages, directTurn{Role: "system", Content: system})
	for _, turn := range history {
		if turn.Content == "" {
			continue
		}
		if turn.Role != "user" && turn.Role != "assistant" {
			continue
		}
		messages = append(messages, turn)
	}
	messages = append(messages, directTurn{Role: "user", Content: user})

	body, err := json.Marshal(chatCompletionRequest{Model: model, Stream: false, Messages: messages})
	if err != nil {
		return "", fmt.Errorf("gateway: encode chat request: %w", err)
	}
	endpoint := strings.TrimRight(base, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("gateway: create chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gateway: chat request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", fmt.Errorf("gateway: read chat response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gateway: chat status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var decoded chatCompletionResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("gateway: decode chat response: %w", err)
	}
	if decoded.Error != nil && decoded.Error.Message != "" {
		return "", fmt.Errorf("gateway: chat error: %s", decoded.Error.Message)
	}
	if len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("gateway: chat response has no content")
	}
	return decoded.Choices[0].Message.Content, nil
}

// directChatState is the per-Telegram-chat conversation memory used when no
// workspace container is involved. Workspace tasks keep their own OpenCode
// session history; this memory covers normal chat and follow-up questions.
type directChatState struct {
	mu       sync.Mutex
	history  []directTurn
	lastUsed time.Time
}

func (d *directChatState) add(role, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	runes := []rune(content)
	if len(runes) > maxDirectChars {
		content = string(runes[:maxDirectChars])
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.history = append(d.history, directTurn{Role: role, Content: content})
	if len(d.history) > maxDirectTurns {
		d.history = append([]directTurn(nil), d.history[len(d.history)-maxDirectTurns:]...)
	}
	d.lastUsed = time.Now()
}

func (d *directChatState) snapshot() []directTurn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]directTurn(nil), d.history...)
}

func (d *directChatState) stale() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.lastUsed.IsZero() && time.Since(d.lastUsed) > chatIdleTTL
}

func (a *application) getDirect(chatID int64) *directChatState {
	a.directMu.Lock()
	defer a.directMu.Unlock()
	if a.direct == nil {
		a.direct = make(map[int64]*directChatState)
	}
	state := a.direct[chatID]
	if state == nil || state.stale() {
		state = &directChatState{}
		a.direct[chatID] = state
	}
	return state
}

func (a *application) clearDirect(chatID int64) {
	a.directMu.Lock()
	defer a.directMu.Unlock()
	delete(a.direct, chatID)
}

// classifyDirectReply reads the gateway routing markers and returns owner-safe
// text. An unmarked reply is an answer; a marker never reaches Telegram.
func classifyDirectReply(raw, fallback string) (directKind, string) {
	trimmed := strings.TrimSpace(raw)
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "WORKSPACE_TASK:"):
			body := strings.TrimSpace(line[len("WORKSPACE_TASK:"):])
			if body == "" {
				body = strings.TrimSpace(fallback)
			}
			return directWorkspace, body
		case strings.HasPrefix(upper, "NEEDS_REPO:"):
			body := strings.TrimSpace(line[len("NEEDS_REPO:"):])
			if body == "" {
				body = defaultRepoQuestion(fallback)
			}
			return directNeedsRepo, body
		}
	}
	if trimmed == "" {
		trimmed = "I'm here. Tell me what you'd like to work on."
	}
	return directAnswer, trimmed
}

// ensureRepoQuestion keeps the model's natural wording but guarantees the
// owner always gets the concrete clone option when a repo is required.
func ensureRepoQuestion(question, request string) string {
	question = strings.TrimSpace(question)
	if question == "" || strings.Contains(strings.ToLower(question), "clone") {
		return question
	}
	return question + " " + defaultRepoQuestion(request)
}

func defaultRepoQuestion(request string) string {
	preview := strings.TrimSpace(request)
	runes := []rune(preview)
	if len(runes) > 90 {
		preview = string(runes[:90]) + "…"
	}
	if preview == "" {
		preview = "this change"
	}
	return "That needs a workspace. Send `clone owner/repo and " + preview + "` to open one, or tell me if you want a chat-only answer."
}
