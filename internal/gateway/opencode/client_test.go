package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientEndpoints(t *testing.T) {
	var gotModel, gotPrompt map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "opencode" || p != "throwaway" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/info":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"version":"2.4.0"}`)
		case r.URL.Path == "/api/session" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":{"id":"ses_test1"}}`)
		case strings.HasSuffix(r.URL.Path, "/model"):
			_ = json.NewDecoder(r.Body).Decode(&gotModel)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/prompt"):
			_ = json.NewDecoder(r.Body).Decode(&gotPrompt)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/interrupt"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Password: "throwaway", HTTP: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Healthy(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := c.CreateSession(ctx)
	if err != nil || id != "ses_test1" {
		t.Fatalf("CreateSession=%q err=%v", id, err)
	}
	if err := c.Prompt(ctx, id, "add a line", "openai/gpt-5-nano"); err != nil {
		t.Fatal(err)
	}
	model, _ := gotModel["model"].(map[string]any)
	if model["providerID"] != "openai" || model["id"] != "gpt-5-nano" {
		t.Fatalf("model body=%+v", gotModel)
	}
	if gotPrompt["text"] != "add a line" {
		t.Fatalf("prompt body=%+v", gotPrompt)
	}
	if err := c.Abort(ctx, id); err != nil {
		t.Fatal(err)
	}

	bad := &Client{BaseURL: srv.URL, Password: "wrong", HTTP: srv.Client()}
	if err := bad.Healthy(ctx); err == nil {
		t.Fatal("expected auth failure")
	}
}

func TestEventsSSE(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		want  []string
		wantT string
	}{
		{
			name: "event and data lines",
			body: "event: server.connected\ndata: {\"type\":\"server.connected\"}\n\n" +
				"event: session.text.delta\ndata: {\"type\":\"session.text.delta\",\"data\":{\"delta\":\"hi\",\"sessionID\":\"s1\"}}\n\n",
			want:  []string{"server.connected", "session.text.delta"},
			wantT: "token",
		},
		{
			name: "data-only frames from opencode v2",
			body: "data: {\"type\":\"server.connected\",\"data\":{}}\n\n" +
				": heartbeat\n\n" +
				"data: {\"type\":\"session.text.delta\",\"data\":{\"delta\":\"hi\",\"sessionID\":\"s1\"}}\n\n",
			want:  []string{"", ""},
			wantT: "token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			c := &Client{BaseURL: srv.URL, Password: "x", HTTP: srv.Client()}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			events, err := c.Events(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var got []RawEvent
			for ev := range events {
				got = append(got, ev)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("events=%+v want %d", got, len(tc.want))
			}
			for i, name := range tc.want {
				if got[i].Event != name {
					t.Fatalf("event[%d]=%q want %q", i, got[i].Event, name)
				}
			}
			last := got[len(got)-1]
			ev, ok := Translate(last)
			if !ok || ev.Type != tc.wantT || ev.Text != "hi" {
				t.Fatalf("translated=%+v ok=%v", ev, ok)
			}
		})
	}
}
