package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testPRToken = "dummy-token-very-secret-42"

func fakeTokenFunc() func(context.Context, string) (string, time.Time, error) {
	return func(context.Context, string) (string, time.Time, error) {
		return testPRToken, time.Now().Add(time.Hour), nil
	}
}

type prCapture struct {
	method      string
	path        string
	auth        string
	contentType string
	payload     prRequest
}

func TestOpenPRPayloadAndAuth(t *testing.T) {
	const wantURL = "https://github.com/owner/repo/pull/7"
	captures := make(chan prCapture, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload prRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		captures <- prCapture{
			method:      r.Method,
			path:        r.URL.Path,
			auth:        r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			payload:     payload,
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"html_url":%q}`, wantURL)
	}))
	defer srv.Close()

	draftTrue, draftFalse := true, false
	cases := []struct {
		name         string
		localDraft   *bool
		defaultDraft bool
		wantDraft    bool
	}{
		{name: "openpr default", wantDraft: false},
		{name: "openpr default draft", defaultDraft: true, wantDraft: true},
		{name: "draft explicit true", localDraft: &draftTrue, wantDraft: true},
		{name: "draft explicit false", localDraft: &draftFalse, defaultDraft: true, wantDraft: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &Git{
				APIURL:       srv.URL,
				DefaultDraft: tc.defaultDraft,
				HTTPClient:   srv.Client(),
			}
			g.SetTokenFuncForTest(fakeTokenFunc())

			var gotURL string
			var err error
			if tc.localDraft == nil {
				gotURL, err = g.OpenPR(context.Background(), "owner/repo",
					"agent/pr-ok", "main", "Fix title", "Fix body")
			} else {
				gotURL, err = g.OpenPRDraft(context.Background(), "owner/repo",
					"agent/pr-ok", "main", "Fix title", "Fix body", *tc.localDraft)
			}
			if err != nil {
				t.Fatalf("open PR: %v", err)
			}
			if gotURL != wantURL {
				t.Fatalf("url=%q want %q", gotURL, wantURL)
			}

			cap := <-captures
			if cap.method != http.MethodPost {
				t.Errorf("method=%q", cap.method)
			}
			if cap.path != "/repos/owner/repo/pulls" {
				t.Errorf("path=%q", cap.path)
			}
			if cap.auth != "Bearer "+testPRToken {
				t.Errorf("auth=%q", cap.auth)
			}
			if !strings.HasPrefix(cap.contentType, "application/json") {
				t.Errorf("content type=%q", cap.contentType)
			}
			want := prRequest{
				Title: "Fix title",
				Head:  "agent/pr-ok",
				Base:  "main",
				Body:  "Fix body",
				Draft: tc.wantDraft,
			}
			if cap.payload != want {
				t.Errorf("payload=%+v want %+v", cap.payload, want)
			}
		})
	}
}

func TestOpenPRNon2xxRedactsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, "denied: %s token=%s", r.Header.Get("Authorization"), testPRToken)
	}))
	defer srv.Close()

	g := &Git{APIURL: srv.URL, HTTPClient: srv.Client()}
	g.SetTokenFuncForTest(fakeTokenFunc())
	_, err := g.OpenPR(context.Background(), "owner/repo", "agent/pr-ok", "main", "t", "b")
	if err == nil {
		t.Fatal("unauthorized response accepted")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("err=%v want status 401", err)
	}
	if strings.Contains(err.Error(), testPRToken) {
		t.Fatalf("token leaked in error: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("token not redacted: %v", err)
	}
}

func TestOpenPRRefusesForbiddenBranchPairs(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"html_url":"https://github.com/owner/repo/pull/1"}`)
	}))
	defer srv.Close()

	cases := []struct{ head, base string }{
		{head: "main", base: "release"},
		{head: "master", base: "release"},
		{head: "refs/heads/main", base: "main"},
		{head: "agent/pr-ok", base: "agent/base"},
		{head: "agent/pr-ok", base: "agent/pr-ok"},
		{head: "agent/Upper", base: "main"},
		{head: "", base: "main"},
	}
	for _, tc := range cases {
		t.Run(tc.head+"_to_"+tc.base, func(t *testing.T) {
			g := &Git{APIURL: srv.URL, HTTPClient: srv.Client()}
			g.SetTokenFuncForTest(fakeTokenFunc())
			_, err := g.OpenPR(context.Background(), "owner/repo", tc.head, tc.base, "t", "b")
			if !errors.Is(err, ErrForbiddenBranch) {
				t.Fatalf("head=%q base=%q err=%v want ErrForbiddenBranch", tc.head, tc.base, err)
			}
		})
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("forbidden PRs made %d HTTP requests", n)
	}
}

func TestOpenPRHonorsContextCancellation(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"html_url":"https://github.com/owner/repo/pull/1"}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := &Git{APIURL: srv.URL, HTTPClient: srv.Client()}
	g.SetTokenFuncForTest(fakeTokenFunc())
	_, err := g.OpenPR(ctx, "owner/repo", "agent/pr-ok", "main", "t", "b")
	if err == nil {
		t.Fatal("canceled context accepted")
	}
	if strings.Contains(err.Error(), testPRToken) {
		t.Fatalf("token leaked in error: %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("canceled request reached server %d times", n)
	}
}
