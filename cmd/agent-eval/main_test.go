package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func evalOptions(baseURL string) options {
	return options{
		Router: baseURL, Alias: defaultAlias, TaskID: "task-1", SessionID: "session-1",
		Prompt: "judge this answer", TokenEnv: "TEST_ROUTER_TOKEN", Timeout: time.Second,
	}
}

func TestParseArgsDefaultsAndAliases(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want options
	}{
		{"defaults", nil, options{Router: "http://127.0.0.1:8090/v1", Alias: defaultAlias, TokenEnv: "ROUTER_TOKEN", Timeout: 90 * time.Second}},
		{"overrides", []string{"--router", "http://router/v1", "--alias", "agent-small", "--token-env", "ALT", "--timeout", "5s"}, options{Router: "http://router/v1", Alias: "agent-small", TokenEnv: "ALT", Timeout: 5 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseArgs(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if got.Router != tc.want.Router || got.Alias != tc.want.Alias || got.TokenEnv != tc.want.TokenEnv || got.Timeout != tc.want.Timeout {
				t.Fatalf("opts=%+v want %+v", got, tc.want)
			}
		})
	}
	if _, err := parseArgs([]string{"unexpected"}); err == nil {
		t.Fatal("expected argument error")
	}
}

func TestEvaluateUsesIndependentProviderAndSafeHeaders(t *testing.T) {
	var gotAuth, gotTask, gotSession, gotAlias string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTask = r.Header.Get("X-Task-Id")
		gotSession = r.Header.Get("X-Session-Id")
		var decoded struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&decoded)
		gotAlias = decoded.Model
		w.Header().Set("X-Padi-Alias", defaultAlias)
		w.Header().Set("X-Padi-Provider", "groq")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"PASS verdict"}}]}`))
	}))
	defer srv.Close()

	lookup := func(string) string { return "SUPERSECRET" }
	got, err := evaluate(context.Background(), evalOptions(srv.URL), strings.NewReader(""), lookup, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if got.Alias != defaultAlias || got.Provider != "groq" || got.Content != "PASS verdict" {
		t.Fatalf("result=%+v", got)
	}
	if gotAuth != "Bearer SUPERSECRET" || gotTask != "task-1" || gotSession != "session-1" || gotAlias != defaultAlias {
		t.Fatalf("headers auth=%q task=%q session=%q alias=%q", gotAuth, gotTask, gotSession, gotAlias)
	}
}

func TestEvaluateErrorsTable(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		provider   string
		build      string
		body       string
		wantPhrase string
	}{
		{"router isolation error", http.StatusServiceUnavailable, "", "", `{"error":{"code":"eval_provider_isolation"}}`, "eval_provider_isolation"},
		{"no provider header", http.StatusOK, "", "", `{"choices":[{"message":{"content":"ok"}}]}`, "did not report"},
		{"same provider as build", http.StatusOK, "nvidia", "nvidia", `{"choices":[{"message":{"content":"ok"}}]}`, "isolation failed"},
		{"empty completion", http.StatusOK, "groq", "", `{"choices":[]}`, "no content"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.provider != "" {
					w.Header().Set("X-Padi-Provider", tc.provider)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			lookup := func(name string) string {
				if name == "TEST_ROUTER_TOKEN" {
					return "SUPERSECRET"
				}
				if name == "PADI_BUILD_PROVIDER" {
					return tc.build
				}
				return ""
			}
			_, err := evaluate(context.Background(), evalOptions(srv.URL), strings.NewReader(""), lookup, srv.Client())
			if err == nil || !strings.Contains(err.Error(), tc.wantPhrase) {
				t.Fatalf("err=%v want phrase %q", err, tc.wantPhrase)
			}
			if strings.Contains(err.Error(), "SUPERSECRET") {
				t.Fatalf("error leaked token: %v", err)
			}
		})
	}
}

func TestRunReadsPromptFromStdinAndEmitsJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var decoded struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&decoded)
		if len(decoded.Messages) != 1 || decoded.Messages[0].Content != "stdin verdict request" {
			t.Fatalf("messages=%+v", decoded.Messages)
		}
		w.Header().Set("X-Padi-Provider", "groq")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"DONE"}}]}`))
	}))
	defer srv.Close()

	o := evalOptions(srv.URL)
	o.Prompt = ""
	args := []string{"--router", o.Router, "--token-env", o.TokenEnv, "--task-id", o.TaskID, "--session-id", o.SessionID}
	var stdout, stderr bytes.Buffer
	lookup := func(name string) string {
		if name == o.TokenEnv {
			return "token-value"
		}
		return ""
	}
	code := run(args, strings.NewReader("stdin verdict request\n"), &stdout, &stderr, lookup)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var decoded result
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Alias != defaultAlias || decoded.Provider != "groq" || decoded.Content != "DONE" {
		t.Fatalf("result=%+v", decoded)
	}
	if strings.Contains(stdout.String(), "token-value") {
		t.Fatal("stdout leaked token")
	}
}
