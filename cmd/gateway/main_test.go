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
	for _, text := range []string{"/help", "/status", "hello", "clone"} {
		u := telegram.Update{Message: &telegram.Message{
			Chat: telegram.Chat{ID: 1}, Text: text,
		}}
		if err := a.handleUpdate(ctx, u); err != nil {
			t.Fatalf("handleUpdate(%q): %v", text, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 4 {
		t.Fatalf("sent %d messages, want 4: %q", len(texts), texts)
	}
	for _, got := range texts {
		if strings.Contains(got, "<repo>") || strings.Contains(got, "<prompt>") {
			t.Fatalf("unescaped placeholder in %q", got)
		}
	}
	if !strings.Contains(texts[0], "&lt;repo&gt;") {
		t.Fatalf("help text missing escaped placeholder: %q", texts[0])
	}
}

func TestNormalizeRepo(t *testing.T) {
	cases := map[string]string{
		"GitHub.com/jerryjuche/koder":             "jerryjuche/koder",
		"https://github.com/jerryjuche/padi.git":  "jerryjuche/padi",
		"https://user:pass@github.com/owner/repo": "owner/repo",
		"git@github.com:jerryjuche/koder.git":     "jerryjuche/koder",
		"jerryjuche/padi":                         "jerryjuche/padi",
		"/tmp/opencode/padi-m1-loop/source":       "/tmp/opencode/padi-m1-loop/source",
		"  owner/repo/  ":                         "owner/repo",
		"not-a-repo":                              "not-a-repo",
	}
	for in, want := range cases {
		if got := normalizeRepo(in); got != want {
			t.Errorf("normalizeRepo(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseCommandConversation(t *testing.T) {
	a := &application{cfg: defaults(), pending: make(map[int64]string)}
	const chat = int64(42)

	c := a.parseCommand(chat, "clone GitHub.com/jerryjuche/koder")
	if c.repo != "jerryjuche/koder" || !c.awaitingPrompt || c.prompt != "" {
		t.Fatalf("step 1 = %+v", c)
	}
	a.rememberRepo(chat, c.repo)

	c = a.parseCommand(chat, "Let me know what you found")
	if c.repo != "jerryjuche/koder" || c.prompt != "Let me know what you found" {
		t.Fatalf("step 2 = %+v", c)
	}
	if a.forgetRepo(chat) != "" {
		t.Fatal("pending repo should be consumed")
	}

	c = a.parseCommand(chat, "clone jerryjuche/padi and add a line to README")
	if c.repo != "jerryjuche/padi" || c.prompt != "add a line to README" {
		t.Fatalf("one-shot = %+v", c)
	}

	c = a.parseCommand(chat, "clone nonsense")
	if !c.askRepo {
		t.Fatalf("invalid repo should ask again: %+v", c)
	}
}

func TestModesAndButtons(t *testing.T) {
	if modeAgent(modePlan) != "plan" || modeAgent(modeResearch) != "plan" || modeAgent(modeBuild) != "build" {
		t.Fatal("wrong agent mapping")
	}
	if !strings.Contains(modePrompt(modeResearch, "check auth"), "RESEARCH MODE") || !strings.Contains(modePrompt(modeBuild, "check auth"), "check auth") {
		t.Fatal("wrong mode prompt")
	}
	if !validMode(modeResearch) || validMode("nonsense") {
		t.Fatal("validMode is wrong")
	}
	rows := chatButtons(modePlan)
	want := map[string]bool{"mode:build": false, "mode:plan": false, "mode:research": false, "act:summary": false, "status": false, "new": false}
	for _, row := range rows {
		for _, b := range row {
			if _, ok := want[b.CallbackData]; ok {
				want[b.CallbackData] = true
			}
		}
	}
	for data, seen := range want {
		if !seen {
			t.Errorf("missing button %q", data)
		}
	}
}

func TestModeCommandsAndCallbacks(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if strings.Contains(r.URL.Path, "sendMessage") {
			mu.Lock()
			texts = append(texts, r.Form.Get("text"))
			mu.Unlock()
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer srv.Close()

	a := &application{cfg: defaults(), tg: &telegram.Client{
		BaseURL: srv.URL, Token: "test", HTTP: srv.Client(),
	}, pending: map[int64]string{}, modes: map[int64]string{}, chats: map[int64]*chatSession{}}
	ctx := context.Background()
	if err := a.handleUpdate(ctx, telegram.Update{Message: &telegram.Message{
		Chat: telegram.Chat{ID: 42, Type: "private"}, Text: "/plan",
	}}); err != nil {
		t.Fatal(err)
	}
	if a.currentMode(42) != modePlan {
		t.Fatalf("mode=%q want plan", a.currentMode(42))
	}
	q := &telegram.CallbackQuery{
		ID: "cb-1", From: telegram.User{ID: 42}, Data: "mode:research",
		Message: &telegram.Message{Chat: telegram.Chat{ID: 42, Type: "private"}},
	}
	if err := a.handleUpdate(ctx, telegram.Update{CallbackQuery: q}); err != nil {
		t.Fatal(err)
	}
	if a.currentMode(42) != modeResearch {
		t.Fatalf("mode=%q want research", a.currentMode(42))
	}
	if err := a.handleUpdate(ctx, telegram.Update{Message: &telegram.Message{
		Chat: telegram.Chat{ID: 42, Type: "private"}, Text: "/new",
	}}); err != nil {
		t.Fatal(err)
	}
	if a.currentMode(42) != modeBuild {
		t.Fatalf("mode=%q want build after /new", a.currentMode(42))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 3 {
		t.Fatalf("sent %d messages, want 3: %q", len(texts), texts)
	}
	if !strings.Contains(texts[0], "plan mode on") || !strings.Contains(texts[1], "research mode on") || !strings.Contains(texts[2], "New topic") {
		t.Fatalf("unexpected replies: %q", texts)
	}
}

func TestScrubForUser(t *testing.T) {
	got := scrubForUser(fmt.Errorf("git Authorization: Bearer ghs_SECRET123 failed"))
	if strings.Contains(got, "ghs_SECRET123") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("scrub=%q", got)
	}
}

func TestValidRepo(t *testing.T) {
	for _, in := range []string{"owner/repo", "/tmp/source", "a/b/c"} {
		if !validRepo(in) {
			t.Errorf("validRepo(%q)=false", in)
		}
	}
	for _, in := range []string{"", "owner", "owner repo", "owner/re po"} {
		if validRepo(in) {
			t.Errorf("validRepo(%q)=true", in)
		}
	}
}
