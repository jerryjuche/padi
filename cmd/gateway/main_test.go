package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

	// A plain message with no pending repo is a conversation turn, not a
	// repo selection failure. No implicit default repo is ever applied.
	c = a.parseCommand(99, "How's this project working?")
	if c.askRepo || c.repo != "" || c.prompt != "How's this project working?" {
		t.Fatalf("plain chat = %+v", c)
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
	if !strings.Contains(modePrompt(modeBuild, "check auth"), chatStyle) {
		t.Fatal("build prompt is missing the chat style contract")
	}
	rows := buttonLayout("empty", "")
	if len(rows) != 1 || len(rows[0]) != 2 || rows[0][0].Static != "help" || rows[0][1].Static != "status" {
		t.Fatalf("empty buttons=%v", rows)
	}
}

func TestButtonsMatchTaskState(t *testing.T) {
	const id = "task-42"
	cases := []struct {
		kind string
		want []string
	}{
		{"running", []string{"s:status", "s:new"}},
		{"decision", []string{"a:execute", "a:revise", "s:new"}},
		{"done", []string{"a:checks", "a:explain", "a:diff", "a:undo", "s:new"}},
		{"failed", []string{"a:continue", "a:checks", "a:undo", "s:new"}},
		{"workspace", []string{"a:summary", "a:checks", "s:new"}},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			rows := buttonLayout(tc.kind, id)
			var got []string
			for _, row := range rows {
				for _, b := range row {
					if b.Action != "" {
						got = append(got, "a:"+b.Action)
					} else {
						got = append(got, "s:"+b.Static)
					}
				}
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("buttons=%q want %q", got, tc.want)
			}
		})
	}
	if rows := buttonLayout("done", "other-task"); strings.Contains(fmt.Sprint(rows), id) {
		t.Fatalf("stale task id leaked into button layout: %v", rows)
	}
}

func keyboardTokens(rows [][]telegram.InlineButton) string {
	var b strings.Builder
	for _, row := range rows {
		for _, button := range row {
			b.WriteString(button.CallbackData)
			b.WriteString(" ")
		}
	}
	return b.String()
}

func TestActionButtonsAreSingleUse(t *testing.T) {
	var mu sync.Mutex
	var sends, edits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			sends++
		} else if strings.HasSuffix(r.URL.Path, "/editMessageText") {
			edits++
		}
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":99}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	}))
	defer srv.Close()

	chat := &chatSession{state: "done", lastUsed: time.Now(), lastTask: taskRecord{ID: "task-42", Prompt: "fix parser"}}
	a := &application{cfg: defaults(), tg: &telegram.Client{
		BaseURL: srv.URL, Token: "test", HTTP: srv.Client(),
	}, chats: map[int64]*chatSession{42: chat}}
	rows := a.buttons(42, "done", "task-42")
	token := ""
	for candidate, tap := range a.taps {
		if tap.Action == "undo" {
			token = candidate
		}
	}
	if token == "" || !strings.HasPrefix(token, "x:") || strings.Contains(keyboardTokens(rows), "task-42") {
		t.Fatalf("dynamic token missing or leaked task id: token=%q rows=%v", token, rows)
	}
	q := &telegram.CallbackQuery{
		ID: "cb-1", From: telegram.User{ID: 42}, Data: token,
		Message: &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 42, Type: "private"}},
	}
	if err := a.handleCallback(context.Background(), q); err != nil {
		t.Fatalf("first tap: %v", err)
	}
	if _, ok := a.taps[token]; ok {
		t.Fatal("token must be consumed on first tap")
	}
	q.ID = "cb-2"
	if err := a.handleCallback(context.Background(), q); err != nil {
		t.Fatalf("replayed tap: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sends != 1 {
		t.Fatalf("replayed button started %d runs, want 1", sends)
	}
	if edits < 2 {
		t.Fatalf("tapped card was not spent: %d edits", edits)
	}
}

func TestParseProposal(t *testing.T) {
	cases := []struct {
		name    string
		summary string
		want    bool
	}{
		{"valid", "Here is the plan.\nPROPOSAL: add tests; internal/foo.go; run go test; low", true},
		{"lowercase", "\tproposal: fix parser; main.go; unit tests; none", true},
		{"inline prose is ignored", "I mentioned PROPOSAL: inline but did not end with one.", false},
		{"empty body", "PROPOSAL:", false},
		{"none", "Done, no approval needed.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parseProposal(tc.summary, "task-1")
			if (p != nil) != tc.want {
				t.Fatalf("proposal=%+v want=%v", p, tc.want)
			}
			if p != nil && (p.TaskID != "task-1" || !strings.Contains(p.Prompt, "add tests") && !strings.Contains(p.Prompt, "fix parser")) {
				t.Fatalf("proposal lost its plan: %+v", p)
			}
		})
	}
}

func TestActionPromptCarriesTaskContext(t *testing.T) {
	task := taskRecord{ID: "task-1", Prompt: "fix the flaky test in router", Outcome: "completed"}
	for _, action := range []string{"checks", "explain", "continue", "diff", "summary", "revise"} {
		prompt, ok := actionPrompt(action, task)
		if !ok || !strings.Contains(prompt, task.Prompt) {
			t.Errorf("action %s: prompt=%q ok=%v", action, prompt, ok)
		}
	}
	if _, ok := actionPrompt("unknown", task); ok {
		t.Fatal("unknown action should be rejected")
	}
}

func TestTaskContextDoesNotRepeatQuestions(t *testing.T) {
	chat := &chatSession{lastTask: taskRecord{
		ID: "task-1", Prompt: "add a retry", Outcome: "completed", Evidence: "go test passed", Files: []string{"a.go"},
	}}
	got := chat.taskContext()
	for _, want := range []string{"add a retry", "completed", "a.go", "do not re-ask settled questions"} {
		if !strings.Contains(got, want) {
			t.Errorf("context %q missing %q", got, want)
		}
	}
	chat.setLastTask(taskRecord{})
	if got := chat.taskContext(); got != "" {
		t.Fatalf("empty task should have empty context, got %q", got)
	}
}

func TestMarkActionRejectsDuplicateTap(t *testing.T) {
	a := &application{}
	if !a.markAction("a:task-1:checks") {
		t.Fatal("first tap should start")
	}
	if a.markAction("a:task-1:checks") {
		t.Fatal("duplicate tap should be ignored")
	}
	if !a.markAction("a:task-1:explain") {
		t.Fatal("different action on same task should start")
	}
}

func TestStaleAndUnapprovedCallbacks(t *testing.T) {
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
	}, pending: map[int64]string{}, modes: map[int64]string{}, chats: map[int64]*chatSession{}, actions: map[string]time.Time{}}
	chat := &chatSession{lastUsed: time.Now(), lastTask: taskRecord{ID: "task-1", Prompt: "fix parser", Outcome: "completed"}}
	a.putChat(42, chat)
	callback := func(data string) error {
		return a.handleCallback(context.Background(), &telegram.CallbackQuery{
			ID: "cb-1", From: telegram.User{ID: 42}, Data: data,
			Message: &telegram.Message{Chat: telegram.Chat{ID: 42, Type: "private"}},
		})
	}
	if err := callback("a:task-99:checks"); err != nil {
		t.Fatal(err)
	}
	if err := callback("exec:task-1"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 2 || !strings.Contains(texts[0], "older task") || !strings.Contains(texts[1], "no longer active") {
		t.Fatalf("replies=%q", texts)
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

func TestCallbackButtonsEditInsteadOfStackingMessages(t *testing.T) {
	var mu sync.Mutex
	var sends, edits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		if strings.Contains(r.URL.Path, "editMessageText") {
			edits++
		} else if strings.Contains(r.URL.Path, "sendMessage") {
			sends++
		}
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer srv.Close()
	a := &application{cfg: defaults(), tg: &telegram.Client{
		BaseURL: srv.URL, Token: "test", HTTP: srv.Client(),
	}, pending: map[int64]string{}, modes: map[int64]string{}, chats: map[int64]*chatSession{}}
	q := &telegram.CallbackQuery{
		ID: "cb-edit", From: telegram.User{ID: 42}, Data: "mode:research",
		Message: &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 42, Type: "private"}},
	}
	if err := a.handleUpdate(context.Background(), telegram.Update{CallbackQuery: q}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if edits != 1 || sends != 0 {
		t.Fatalf("button sent %d messages and made %d edits; want 0/1", sends, edits)
	}
	if a.currentMode(42) != modeResearch {
		t.Fatalf("mode=%q want research", a.currentMode(42))
	}
}

func TestUndoCallbackKeepsTaskScopedData(t *testing.T) {
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
	chat := &chatSession{}
	chat.setLastTask(taskRecord{ID: "task-42"})
	chat.begin()
	chat.end()
	a := &application{cfg: defaults(), tg: &telegram.Client{
		BaseURL: srv.URL, Token: "test", HTTP: srv.Client(),
	}, pending: map[int64]string{}, modes: map[int64]string{}, chats: map[int64]*chatSession{42: chat}}
	if err := a.undoCallback(context.Background(), 42, "undo:task-42:latest"); err != nil {
		t.Fatalf("undoCallback: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 1 || !strings.Contains(texts[0], "No live workspace") {
		t.Fatalf("undo replies=%q, want one no-workspace reply", texts)
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
