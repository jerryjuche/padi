package telegram

import (
	"context"
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
		{"callback only", Update{CallbackQuery: &CallbackQuery{From: User{ID: 7}}}, 7, false},
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
