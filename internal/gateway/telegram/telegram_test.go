package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jerryjuche/padi/internal/gateway/opencode"
)

func TestCallRedactsBotToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // transport error includes the full bot URL
	c := &Client{Token: "super-secret-token", BaseURL: base, PollSeconds: 1}
	err := c.call(context.Background(), "getMe", nil, nil)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), c.Token) {
		t.Fatalf("token leaked in error: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("error not redacted: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = c.call(ctx, "getMe", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is lost after redaction: %v", err)
	}
}

func TestCleanReplyDropsToolJargon(t *testing.T) {
	in := "Updated the file and ran the checks.\noldString: foo\nnewString: bar\nreplaceAll: true\nSend it when you're ready."
	want := "Updated the file and ran the checks.\nSend it when you're ready."
	if got := cleanReply(in); got != want {
		t.Fatalf("cleanReply=%q want %q", got, want)
	}
	if got := cleanReply("oldString: only"); got != "" {
		t.Fatalf("cleanReply fallback=%q", got)
	}
}

func TestAllowed(t *testing.T) {
	cases := []struct {
		name  string
		u     Update
		owner int64
		want  bool
	}{
		{"owner private", Update{Message: &Message{Chat: Chat{Type: "private"}, From: User{ID: 7}}}, 7, true},
		{"other user", Update{Message: &Message{Chat: Chat{Type: "private"}, From: User{ID: 8}}}, 7, false},
		{"group", Update{Message: &Message{Chat: Chat{Type: "group"}, From: User{ID: 7}}}, 7, false},
		{"callback without message", Update{CallbackQuery: &CallbackQuery{From: User{ID: 7}}}, 7, false},
		{"owner callback", Update{CallbackQuery: &CallbackQuery{
			From: User{ID: 7}, Message: &Message{Chat: Chat{ID: 3, Type: "private"}}, Data: "mode:plan",
		}}, 7, true},
		{"zero owner", Update{Message: &Message{Chat: Chat{Type: "private"}, From: User{ID: 7}}}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Allowed(tc.u, tc.owner); got != tc.want {
				t.Fatalf("Allowed=%v want %v", got, tc.want)
			}
		})
	}
}

func TestRender(t *testing.T) {
	cases := []struct {
		ev   opencode.Event
		want string
	}{
		{opencode.Event{Type: "tool_call", Tool: "shell"}, "Running shell..."},
		{opencode.Event{Type: "status", Message: "Working"}, "Working"},
		{opencode.Event{Type: "error", Message: "boom"}, "Error: boom"},
		{opencode.Event{Type: "token", Text: "hi"}, ""},
	}
	for _, tc := range cases {
		if got := Render(tc.ev); got != tc.want {
			t.Fatalf("Render(%+v)=%q want %q", tc.ev, got, tc.want)
		}
	}
}

func TestSplit(t *testing.T) {
	cases := []struct {
		text string
		max  int
		want []string
	}{
		{"short", 10, []string{"short"}},
		{"abcdef", 3, []string{"abc", "def"}},
		{"one\ntwo\nthree", 8, []string{"one\ntwo", "three"}},
	}
	for _, tc := range cases {
		got := Split(tc.text, tc.max)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("Split(%q,%d)=%q want %q", tc.text, tc.max, got, tc.want)
		}
	}
}

type memoryState struct{ id int64 }

func (m *memoryState) LoadLastUpdateID(context.Context) (int64, error) { return m.id, nil }
func (m *memoryState) SaveLastUpdateID(_ context.Context, id int64) error {
	m.id = id
	return nil
}

func TestPollerCursorAndAllowlist(t *testing.T) {
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getUpdates") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		page++
		if page == 1 {
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":4,"message":{"message_id":1,"chat":{"id":7,"type":"private"},"from":{"id":8},"text":"intruder"}},{"update_id":5,"message":{"message_id":2,"chat":{"id":7,"type":"private"},"from":{"id":7},"text":"hello"}}]}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":[]}`)
	}))
	defer srv.Close()
	state := &memoryState{id: 3}
	var handled []string
	p := &Poller{
		Client: &Client{BaseURL: srv.URL, Token: "t", HTTP: srv.Client(), PollSeconds: 0},
		Store:  state, OwnerID: 7,
		Handle: func(_ context.Context, u Update) error {
			handled = append(handled, u.Message.Text)
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(handled) != "[hello]" {
		t.Fatalf("handled=%v", handled)
	}
	if state.id != 5 {
		t.Fatalf("cursor=%d want 5", state.id)
	}
}

func TestSendMessageKeyboard(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.Form.Get("reply_markup")
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "t", HTTP: srv.Client()}
	rows := [][]InlineButton{{{Text: "Plan", CallbackData: "mode:plan"}, {Text: "Research", CallbackData: "mode:research"}}}
	if _, err := c.SendMessageKeyboard(context.Background(), 7, "Choose", rows); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"inline_keyboard"`) || !strings.Contains(got, `"callback_data":"mode:plan"`) {
		t.Fatalf("reply_markup=%q", got)
	}
}

func TestEditMessageTextNotModifiedIsNoOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message"}`)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "t", HTTP: srv.Client()}
	if err := c.EditMessageText(context.Background(), 7, 1, "same"); err != nil {
		t.Fatalf("benign edit returned %v", err)
	}
}

func TestPollerAdvancesOnReportedError(t *testing.T) {
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page++
		if page == 1 {
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":9,"message":{"message_id":1,"chat":{"id":7,"type":"private"},"from":{"id":7},"text":"bad"}}]}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":[]}`)
	}))
	defer srv.Close()
	state := &memoryState{id: 8}
	var handled int
	p := &Poller{
		Client: &Client{BaseURL: srv.URL, Token: "t", HTTP: srv.Client()},
		Store:  state, OwnerID: 7,
		Handle: func(_ context.Context, _ Update) error {
			handled++
			return &ReportedError{Err: fmt.Errorf("already told the owner")}
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if handled != 1 {
		t.Fatalf("handled %d times, want 1 (no replay)", handled)
	}
	if state.id != 9 {
		t.Fatalf("cursor=%d want 9", state.id)
	}
}

func TestRendererRelay(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		calls = append(calls, r.URL.Path+" "+r.Form.Get("text"))
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":99}}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	defer srv.Close()
	events := make(chan opencode.Event, 4)
	events <- opencode.Event{Type: "tool_call", Tool: "shell"}
	events <- opencode.Event{Type: "token", Text: "done <x>"}
	events <- opencode.Event{Type: "done"}
	close(events)
	r := &Renderer{
		Client:       &Client{BaseURL: srv.URL, Token: "t", HTTP: srv.Client()},
		EditInterval: time.Hour, MaxChars: 3800,
	}
	summary, err := r.Relay(context.Background(), 7, uuid.New(), nil, events)
	if err != nil {
		t.Fatal(err)
	}
	if summary != "done <x>" {
		t.Fatalf("summary=%q", summary)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 2 || !strings.Contains(calls[len(calls)-1], "done &lt;x&gt;") {
		t.Fatalf("calls=%v", calls)
	}
}

func TestRendererRelayTreatsNotModifiedAsNoOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":99}}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message"}`)
	}))
	defer srv.Close()
	events := make(chan opencode.Event, 3)
	events <- opencode.Event{Type: "status", Message: "same"}
	events <- opencode.Event{Type: "token", Text: "same"}
	events <- opencode.Event{Type: "done"}
	close(events)
	r := &Renderer{
		Client:       &Client{BaseURL: srv.URL, Token: "t", HTTP: srv.Client()},
		EditInterval: 0, MaxChars: 3800,
	}
	rows := [][]InlineButton{{{Text: "Build", CallbackData: "mode:build"}}}
	summary, err := r.RelayWithKeyboard(context.Background(), 7, uuid.New(), nil, events, rows)
	if err != nil {
		t.Fatalf("RelayWithKeyboard returned %v", err)
	}
	if summary != "same" {
		t.Fatalf("summary=%q", summary)
	}
}

func TestRendererRelayFailsOnTerminalError(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		calls = append(calls, r.URL.Path+" "+r.Form.Get("text"))
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":101}}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	defer srv.Close()
	events := make(chan opencode.Event, 2)
	events <- opencode.Event{Type: "token", Text: "partial"}
	events <- opencode.Event{Type: "error", Terminal: true, Message: "boom"}
	close(events)
	r := &Renderer{
		Client:       &Client{BaseURL: srv.URL, Token: "t", HTTP: srv.Client()},
		EditInterval: time.Hour, MaxChars: 3800,
	}
	summary, err := r.Relay(context.Background(), 7, uuid.New(), nil, events)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err=%v summary=%q", err, summary)
	}
	if summary != "" {
		t.Fatalf("summary=%q", summary)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 || !strings.Contains(calls[len(calls)-1], "Error: boom") {
		t.Fatalf("calls=%v", calls)
	}
}
