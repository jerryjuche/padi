// Package telegram implements the M1 long-polling bot and event renderer.
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a small Telegram Bot API client. BaseURL is overridable so the
// whole loop can be tested locally with httptest.
type Client struct {
	BaseURL     string
	Token       string
	HTTP        *http.Client
	PollSeconds int
}

// Update is the subset of a Telegram update used by the gateway.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	Chat      Chat   `json:"chat"`
	From      User   `json:"from"`
	Text      string `json:"text"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type User struct {
	ID int64 `json:"id"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

// InlineButton is one Telegram inline-keyboard button. CallbackData is sent
// back verbatim in a callback_query, never displayed to the user.
type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: time.Duration(c.PollSeconds+10) * time.Second}
}

// redactedError hides the bot token that net/http embeds in transport errors
// while keeping the original error unwrap-able for errors.Is.
type redactedError struct {
	err   error
	token string
}

func (e *redactedError) Error() string {
	if e.token == "" {
		return e.err.Error()
	}
	return strings.ReplaceAll(e.err.Error(), e.token, "<redacted>")
}

func (e *redactedError) Unwrap() error { return e.err }

func (c *Client) redact(err error) error {
	return &redactedError{err: err, token: c.Token}
}

func (c *Client) call(ctx context.Context, method string, params url.Values, out any) error {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	endpoint := base + "/bot" + c.Token + "/" + method
	var req *http.Request
	var err error
	if len(params) > 0 {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(params.Encode()))
		if err != nil {
			return c.redact(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", method, c.redact(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if method == "editMessageText" && isMessageNotModified(body) {
			return nil
		}
		return fmt.Errorf("telegram: %s: HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("telegram: decode %s: %w", method, err)
	}
	if !envelope.OK {
		return fmt.Errorf("telegram: %s: %s", method, envelope.Description)
	}
	if out == nil || len(envelope.Result) == 0 {
		return nil
	}
	return json.Unmarshal(envelope.Result, out)
}

// GetUpdates long-polls from the given offset (first update ID to return).
func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	params := url.Values{}
	params.Set("offset", strconv.FormatInt(offset, 10))
	params.Set("timeout", strconv.Itoa(c.PollSeconds))
	params.Set("allowed_updates", `["message","callback_query"]`)
	var updates []Update
	if err := c.call(ctx, "getUpdates", params, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// SendMessage sends an HTML message and returns its message ID.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	return c.SendMessageKeyboard(ctx, chatID, text, nil)
}

// SendMessageKeyboard sends an HTML message with an optional inline keyboard.
// Each inner slice is one row of buttons.
func (c *Client) SendMessageKeyboard(ctx context.Context, chatID int64, text string, rows [][]InlineButton) (int64, error) {
	params := url.Values{}
	params.Set("chat_id", strconv.FormatInt(chatID, 10))
	params.Set("text", text)
	params.Set("parse_mode", "HTML")
	if markup, err := keyboardJSON(rows); err != nil {
		return 0, err
	} else if markup != "" {
		params.Set("reply_markup", markup)
	}
	var msg Message
	if err := c.call(ctx, "sendMessage", params, &msg); err != nil {
		return 0, err
	}
	return msg.MessageID, nil
}

// isMessageNotModified recognises Telegram's benign no-op edit response. It
// happens when a streamed status is identical to the final text/keyboard, and
// must not turn an otherwise successful task into a failure.
func isMessageNotModified(body []byte) bool {
	var envelope struct {
		Description string `json:"description"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	return strings.Contains(envelope.Description, "message is not modified")
}

func keyboardJSON(rows [][]InlineButton) (string, error) {
	if len(rows) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(map[string]any{"inline_keyboard": rows})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// EditMessageText updates a message in place; used for streamed tokens.
func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text string) error {
	return c.EditMessageTextKeyboard(ctx, chatID, messageID, text, nil)
}

// EditMessageTextKeyboard updates a message and its optional inline keyboard.
func (c *Client) EditMessageTextKeyboard(ctx context.Context, chatID, messageID int64, text string, rows [][]InlineButton) error {
	params := url.Values{}
	params.Set("chat_id", strconv.FormatInt(chatID, 10))
	params.Set("message_id", strconv.FormatInt(messageID, 10))
	params.Set("text", text)
	params.Set("parse_mode", "HTML")
	if markup, err := keyboardJSON(rows); err != nil {
		return err
	} else if markup != "" {
		params.Set("reply_markup", markup)
	}
	return c.call(ctx, "editMessageText", params, nil)
}

// AnswerCallbackQuery acknowledges an inline button press.
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	params := url.Values{}
	params.Set("callback_query_id", id)
	if text != "" {
		params.Set("text", text)
	}
	return c.call(ctx, "answerCallbackQuery", params, nil)
}
