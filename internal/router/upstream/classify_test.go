package upstream

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		err    error
		want   Outcome
	}{
		{"ok", 200, "{}", nil, OK},
		{"rate limited", 429, "{}", nil, RateLimited},
		{"auth", 401, "{}", nil, AuthFailed},
		{"forbidden", 403, "{}", nil, AuthFailed},
		{"context length", 400, `{"error":"maximum context length"}`, nil, ContextTooBig},
		{"context length snake", 400, `{"error":"context_length exceeded"}`, nil, ContextTooBig},
		{"bad request", 400, `{"error":"bad model"}`, nil, BadRequest},
		{"server error", 503, "{}", nil, Retryable},
		{"transport", 0, "", errors.New("dial timeout"), Retryable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, nil, []byte(tc.body), tc.err); got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestRetryAfterSecondsAndDate(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "7")
	if got := RetryAfter(h); got != 7*time.Second {
		t.Fatalf("seconds=%v", got)
	}
	h.Set("Retry-After", time.Now().Add(3*time.Second).UTC().Format(http.TimeFormat))
	got := RetryAfter(h)
	if got < 2*time.Second || got > 3*time.Second {
		t.Fatalf("date=%v", got)
	}
	h.Set("Retry-After", "nonsense")
	if got := RetryAfter(h); got != 0 {
		t.Fatalf("invalid=%v", got)
	}
}
