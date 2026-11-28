package secrets

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// fill builds a fixture of n characters so this file never contains a literal
// that a secret scanner (or the audit) would flag.
func fill(ch byte, n int) string { return strings.Repeat(string(ch), n) }

func TestRedact(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		leaks []string
	}{
		{
			name:  "github personal token",
			in:    "remote: token=" + "ghp_" + fill('a', 24),
			leaks: []string{"ghp_" + fill('a', 24)},
		},
		{
			name:  "github server token",
			in:    "using ghs_" + fill('b', 20),
			leaks: []string{"ghs_" + fill('b', 20)},
		},
		{
			name:  "github fine-grained token",
			in:    "pat github_pat_" + fill('c', 22),
			leaks: []string{"github_pat_" + fill('c', 22)},
		},
		{
			name:  "openai key",
			in:    "export OPENAI_API_KEY=sk-" + fill('d', 32),
			leaks: []string{"sk-" + fill('d', 32)},
		},
		{
			name:  "anthropic key",
			in:    "key sk-ant-api03-" + fill('e', 20),
			leaks: []string{"sk-ant-api03-" + fill('e', 20)},
		},
		{
			name:  "google api key",
			in:    "AIza" + fill('f', 35),
			leaks: []string{"AIza" + fill('f', 35)},
		},
		{
			name:  "aws access key id",
			in:    "AKIA" + strings.Repeat("Q", 16),
			leaks: []string{"AKIA" + strings.Repeat("Q", 16)},
		},
		{
			name:  "telegram bot token",
			in:    "https://api.telegram.org/bot" + "123456789:" + fill('g', 35) + "/sendMessage",
			leaks: []string{"123456789:" + fill('g', 35)},
		},
		{
			name:  "authorization header",
			in:    "Authorization: Bearer " + fill('h', 24),
			leaks: []string{fill('h', 24)},
		},
		{
			name:  "basic authorization header",
			in:    "authorization: Basic " + fill('i', 24),
			leaks: []string{fill('i', 24)},
		},
		{
			name:  "credential in url userinfo",
			in:    "https://x-access-token:" + fill('j', 20) + "@github.com/o/r.git",
			leaks: []string{fill('j', 20)},
		},
		{
			name:  "env assignment",
			in:    "SUPABASE_SERVICE_ROLE_KEY=" + fill('k', 24),
			leaks: []string{fill('k', 24)},
		},
		{
			name:  "json assignment",
			in:    `{"password": "` + fill('l', 20) + `"}`,
			leaks: []string{fill('l', 20)},
		},
		{
			name:  "jwt",
			in:    "token eyJ" + fill('m', 12) + "." + fill('n', 12) + "." + fill('o', 12),
			leaks: []string{"eyJ" + fill('m', 12)},
		},
		{
			name:  "nvidia key",
			in:    "nvapi-" + fill('p', 20),
			leaks: []string{"nvapi-" + fill('p', 20)},
		},
		{
			name:  "stripe key",
			in:    "sk_live_" + fill('q', 24),
			leaks: []string{"sk_live_" + fill('q', 24)},
		},
		{
			name:  "slack token",
			in:    "xoxb-" + fill('r', 20),
			leaks: []string{"xoxb-" + fill('r', 20)},
		},
		{
			name:  "private key block",
			in:    "-----BEGIN RSA PRIVATE KEY-----\n" + fill('s', 64) + "\n-----END RSA PRIVATE KEY-----",
			leaks: []string{fill('s', 64)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			if !strings.Contains(got, Marker) {
				t.Fatalf("Redact(%q) = %q, missing %s", tc.in, got, Marker)
			}
			for _, leak := range tc.leaks {
				if strings.Contains(got, leak) {
					t.Fatalf("Redact(%q) = %q, leaked %q", tc.in, got, leak)
				}
			}
			if again := Redact(got); again != got {
				t.Fatalf("Redact is not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestRedactKeepsOrdinaryOutput(t *testing.T) {
	cases := []string{
		"ok  github.com/jerryjuche/padi/internal/verify/secrets    1.173s",
		"--- FAIL: TestOpenPR (0.02s)",
		"commit 2f76163e1c4a0f2a9c8f37dd6f0b5c3b1a4d9e77",
		"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"built in 1.25s from /workspace/padi/internal/verify/runner/runner.go:13",
		"tests passed: 42 skipped: 0",
		"COVERAGE=87.5%",
		"installed 18 packages, 0 vulnerabilities",
	}
	for _, in := range cases {
		if got := Redact(in); got != in {
			t.Fatalf("Redact(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestCapTail(t *testing.T) {
	t.Run("shorter than the cap is unchanged", func(t *testing.T) {
		if got := CapTail("hello", 4096); got != "hello" {
			t.Fatalf("CapTail = %q, want hello", got)
		}
	})

	t.Run("longer than the cap keeps the tail", func(t *testing.T) {
		body := fill('a', 100) + "END"
		got := CapTail(body, 10)
		if got != "aaaaaaaEND" {
			t.Fatalf("CapTail = %q, want the last 10 bytes", got)
		}
		if len(got) > 10 {
			t.Fatalf("CapTail length %d exceeds 10", len(got))
		}
	})

	t.Run("never splits a rune", func(t *testing.T) {
		body := strings.Repeat("é", 40) // two bytes each
		got := CapTail(body, 9)
		if !utf8.ValidString(got) {
			t.Fatalf("CapTail returned invalid UTF-8: %q", got)
		}
		if len(got) > 9 {
			t.Fatalf("CapTail length %d exceeds 9", len(got))
		}
		if !strings.HasSuffix(body, got) {
			t.Fatalf("CapTail = %q is not a suffix of the input", got)
		}
	})

	t.Run("non-positive cap is empty", func(t *testing.T) {
		for _, max := range []int{0, -1} {
			if got := CapTail("anything", max); got != "" {
				t.Fatalf("CapTail(max=%d) = %q, want empty", max, got)
			}
		}
	})
}
