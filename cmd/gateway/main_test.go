package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jerryjuche/padi/internal/gateway/telegram"
)

// Telegram client sends with parse_mode=HTML. Placeholders such as <repo>
// must be escaped or Telegram rejects the whole message with HTTP 400.
func TestHintMessagesAreValidTelegramHTML(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		texts = append(texts, r.Form.Get("text"))
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer srv.Close()

	a := &application{cfg: defaults(), tg: &telegram.Client{
		BaseURL: srv.URL, Token: "test", HTTP: srv.Client(),
	}}
	ctx := context.Background()
	for _, text := range []string{"/help", "/status", "hello"} {
		u := telegram.Update{Message: &telegram.Message{
			Chat: telegram.Chat{ID: 1}, Text: text,
		}}
		if err := a.handleUpdate(ctx, u); err != nil {
			t.Fatalf("handleUpdate(%q): %v", text, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 3 {
		t.Fatalf("sent %d messages, want 3: %q", len(texts), texts)
	}
	for _, got := range texts {
		if strings.Contains(got, "<repo>") || strings.Contains(got, "<prompt>") {
			t.Fatalf("unescaped placeholder in %q", got)
		}
		if !strings.Contains(got, "&lt;repo&gt;") {
			t.Fatalf("missing escaped placeholder in %q", got)
		}
	}
}
