package upstream

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Outcome is the coarse result of one upstream attempt.
type Outcome string

const (
	OK            Outcome = "ok"
	RateLimited   Outcome = "rate_limited"
	AuthFailed    Outcome = "auth_failed"
	ContextTooBig Outcome = "context_too_big"
	BadRequest    Outcome = "bad_request"
	Retryable     Outcome = "retryable"
)

// Classify maps upstream responses and transport errors to router outcomes.
func Classify(status int, headers http.Header, body []byte, err error) Outcome {
	if err != nil {
		return Retryable
	}
	switch {
	case status >= 200 && status < 300:
		return OK
	case status == http.StatusTooManyRequests:
		return RateLimited
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return AuthFailed
	case status == http.StatusBadRequest && contextLengthBody(body):
		return ContextTooBig
	case status >= 400 && status < 500:
		return BadRequest
	case status >= 500:
		return Retryable
	default:
		return Retryable
	}
}

func contextLengthBody(body []byte) bool {
	s := strings.ToLower(string(body))
	for _, needle := range []string{"context_length", "context length", "maximum context", "max_context", "too many tokens"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// RetryAfter parses a Retry-After header in seconds or HTTP-date form. It
// returns 0 when the header is absent or malformed.
func RetryAfter(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		d := time.Until(when)
		if d > 0 {
			return d
		}
	}
	return 0
}
