package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jerryjuche/padi/internal/gateway/telegram"
)

func TestRouterChatClientSendsHistoryAndToken(t *testing.T) {
	var mu sync.Mutex
	var got chatCompletionRequest
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		auth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	client := &routerChatClient{
		BaseURL: srv.URL, Token: "router-secret", Model: "padi/agent-chat", HTTP: srv.Client(),
	}
	reply, err := client.Complete(context.Background(), "SYSTEM", []directTurn{
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	}, "latest question")
	if err != nil || reply != "ok" {
		t.Fatalf("Complete()=%q,%v", reply, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "Bearer router-secret" {
		t.Fatalf("Authorization=%q", auth)
	}
	if got.Model != "padi/agent-chat" || got.Stream {
		t.Fatalf("request model=%q stream=%v", got.Model, got.Stream)
	}
	wantRoles := []string{"system", "user", "assistant", "user"}
	if len(got.Messages) != len(wantRoles) {
		t.Fatalf("messages=%d want %d", len(got.Messages), len(wantRoles))
	}
	for i, role := range wantRoles {
		if got.Messages[i].Role != role {
			t.Fatalf("message %d role=%q want %q", i, got.Messages[i].Role, role)
		}
	}
	if got.Messages[0].Content != "SYSTEM" || got.Messages[3].Content != "latest question" {
		t.Fatalf("system/user messages lost: %+v", got.Messages)
	}
}

func TestClassifyDirectReplyMarkers(t *testing.T) {
	cases := []struct {
		name, raw, fallback string
		kind                directKind
		message             string
	}{
		{"answer", "Here is the answer.", "prompt", directAnswer, "Here is the answer."},
		{"workspace", "WORKSPACE_TASK: run the tests", "prompt", directWorkspace, "run the tests"},
		{"lowercase marker", "workspace_task: fix parser", "prompt", directWorkspace, "fix parser"},
		{"repo", "NEEDS_REPO: Open a repo or keep chatting?", "prompt", directNeedsRepo, "Open a repo or keep chatting?"},
		{"empty workspace marker", "WORKSPACE_TASK:", "fix parser", directWorkspace, "fix parser"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, message := classifyDirectReply(tc.raw, tc.fallback)
			if kind != tc.kind || message != tc.message {
				t.Fatalf("got %q/%q want %q/%q", kind, message, tc.kind, tc.message)
			}
		})
	}
}

func newDirectChatApp(t *testing.T, reply string, capture func(r *http.Request)) (*application, func() []string, func()) {
	t.Helper()
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			capture(r)
		}
		payload := fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, reply)
		fmt.Fprint(w, payload)
	}))
	var mu sync.Mutex
	var texts []string
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		texts = append(texts, r.Form.Get("text"))
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	a := &application{
		cfg: defaults(),
		tg:  &telegram.Client{BaseURL: tg.URL, Token: "test", HTTP: tg.Client()},
		chatClient: &routerChatClient{
			BaseURL: router.URL, Token: "router-secret", Model: "padi/agent-chat", HTTP: router.Client(),
		},
		pending: make(map[int64]string),
		modes:   make(map[int64]string),
		chats:   make(map[int64]*chatSession),
		direct:  make(map[int64]*directChatState),
	}
	textSnapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), texts...)
	}
	return a, textSnapshot, func() {
		router.Close()
		tg.Close()
	}
}

func TestDirectChatAnswersWithoutRepo(t *testing.T) {
	a, texts, closeServers := newDirectChatApp(t, "Chat works directly; no repo was opened.", nil)
	defer closeServers()
	u := telegram.Update{Message: &telegram.Message{
		Chat: telegram.Chat{ID: 42, Type: "private"}, Text: "How's this project working?",
	}}
	if err := a.handleUpdate(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if a.getChat(42) != nil {
		t.Fatal("normal chat must not open a container")
	}
	if repo := a.forgetRepo(42); repo != "" {
		t.Fatalf("normal chat must not choose a repo, got %q", repo)
	}
	if len(texts()) != 1 || !strings.Contains(texts()[0], "Chat works directly") {
		t.Fatalf("reply=%q", texts())
	}
	history := a.getDirect(42).snapshot()
	if len(history) != 2 || history[0].Role != "user" || history[1].Role != "assistant" {
		t.Fatalf("history=%+v", history)
	}
}

func TestDirectChatAsksRepoOnlyForWorkspaceWork(t *testing.T) {
	a, texts, closeServers := newDirectChatApp(t, "NEEDS_REPO: Should I open a repository, or keep this chat-only?", nil)
	defer closeServers()
	u := telegram.Update{Message: &telegram.Message{
		Chat: telegram.Chat{ID: 42, Type: "private"}, Text: "fix the login bug",
	}}
	if err := a.handleUpdate(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if a.getChat(42) != nil {
		t.Fatal("asking for a repo must not start a workspace task")
	}
	if len(texts()) != 1 || !strings.Contains(texts()[0], "clone owner/repo") {
		t.Fatalf("reply=%q", texts())
	}
	history := a.getDirect(42).snapshot()
	if len(history) != 2 || !strings.Contains(history[1].Content, "clone owner/repo") {
		t.Fatalf("repo question was not recorded: %+v", history)
	}
}

func TestTaskAgentEnforcesPlanBeforeEdits(t *testing.T) {
	if got := taskAgent(modeBuild, command{}); got != "plan" {
		t.Fatalf("plain build turn uses %q agent, want plan", got)
	}
	prompt := taskPrompt(modeBuild, command{})
	if !strings.HasPrefix(prompt, "PLAN MODE:") || !strings.Contains(prompt, "PROPOSAL:") {
		t.Fatalf("plain build prompt=%q", prompt)
	}
	if got := taskAgent(modeBuild, command{approved: true}); got != "build" {
		t.Fatalf("approved plan uses %q agent, want build", got)
	}
	if !strings.Contains(taskPrompt(modeBuild, command{approved: true}), "already approved") {
		t.Fatal("approved execution prompt must suppress a second approval ask")
	}
	if got := taskAgent(modeBuild, command{readOnly: true}); got != "plan" {
		t.Fatalf("read-only action uses %q agent, want plan", got)
	}
	if !strings.HasPrefix(taskPrompt(modeBuild, command{readOnly: true}), "READ-ONLY MODE:") {
		t.Fatal("read-only action prompt lost its contract")
	}
}
