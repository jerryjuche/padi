package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jerryjuche/padi/internal/router/config"
	"github.com/jerryjuche/padi/internal/router/route"
)

func targetAndProvider(baseURL string) (config.Target, config.Provider) {
	return config.Target{Provider: "fake", Model: "upstream/model", Enabled: true}, config.Provider{
		Name: "fake", BaseURL: baseURL + "/v1", APIKey: "provider-key", RPM: 10, Concurrency: 1,
		SupportsStream: true, SupportsTools: true, Enabled: true,
	}
}

func TestDoRewritesModelAndPassesToolsAndHeaders(t *testing.T) {
	var saw map[string]any
	var auth, task, session string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		task = r.Header.Get("X-Task-Id")
		session = r.Header.Get("X-Session-Id")
		_ = json.NewDecoder(r.Body).Decode(&saw)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	target, provider := targetAndProvider(srv.URL)
	body := []byte(`{"model":"alias","stream":true,"tools":[{"type":"function"}],"messages":[]}`)
	rec := httptest.NewRecorder()
	res, err := New(time.Second, 5*time.Second).Do(context.Background(), target, provider, route.Request{Body: body, Stream: true, Headers: map[string]string{"X-Task-Id": "task-42", "X-Session-Id": "session-42"}}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OK {
		t.Fatalf("outcome=%v", res.Outcome)
	}
	if saw["model"] != "upstream/model" {
		t.Fatalf("model=%v", saw["model"])
	}
	if len(saw["tools"].([]any)) != 1 {
		t.Fatalf("tools=%v", saw["tools"])
	}
	opts, ok := saw["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options=%v", saw["stream_options"])
	}
	if auth != "Bearer provider-key" || task != "task-42" || session != "session-42" {
		t.Fatalf("auth/task/session=%q/%q/%q", auth, task, session)
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("body=%q", rec.Body.String())
	}
}

func TestDoPreFirstByteTimeoutIsFailoverable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("late"))
	}))
	defer srv.Close()

	target, provider := targetAndProvider(srv.URL)
	rec := httptest.NewRecorder()
	res, _ := New(50*time.Millisecond, 2*time.Second).Do(context.Background(), target, provider, route.Request{Body: []byte(`{"model":"alias","stream":false}`)}, rec)
	if res.Outcome != Retryable {
		t.Fatalf("outcome=%v", res.Outcome)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("wrote before first byte: %q", rec.Body.String())
	}
}

func TestDoMidStreamErrorDoesNotFailOver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"boom\"}}\n\n"))
	}))
	defer srv.Close()

	target, provider := targetAndProvider(srv.URL)
	rec := httptest.NewRecorder()
	res, err := New(time.Second, 5*time.Second).Do(context.Background(), target, provider, route.Request{Body: []byte(`{"model":"alias","stream":true}`), Stream: true}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Retryable || !res.MidStream {
		t.Fatalf("result=%+v", res)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "partial") || !strings.Contains(body, `"error"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("body=%q", body)
	}
}
