// Package secrets removes credential-shaped strings from captured command
// output and caps that output to a bounded tail before it is stored on a step,
// shown on an approval card, sent to the owner or written to a log.
//
// Redaction is deliberately pattern-based. Verification output is arbitrary
// program text, so an entropy-only heuristic would mangle commit SHAs,
// checksums, package hashes and identifiers and destroy the evidence a failed
// verdict depends on. A false positive here costs a single [REDACTED] marker;
// a false negative leaks a live credential, so the patterns favour the known
// credential shapes the contract names (ghs_, ghp_, sk-, AIza, bearer values)
// plus the other providers this system holds.
package secrets

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Marker replaces every redacted value. Callers and tests assert on it.
const Marker = "[REDACTED]"

// rule is one ordered substitution. Order matters: structural shapes (a PEM
// block, a URL with userinfo, an Authorization header) are collapsed before the
// individual token shapes inside them are rewritten.
type rule struct {
	re   *regexp.Regexp
	repl string
}

// rules is the complete redaction table. It is built once at init and never
// written afterwards.
var rules = []rule{
	// Private keys, in whole blocks. A partial key is still a key.
	{
		re:   regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		repl: "-----BEGIN PRIVATE KEY-----\n" + Marker + "\n-----END PRIVATE KEY-----",
	},
	// Credentials embedded in a URL: https://x-access-token:<token>@github.com/o/r
	{
		re:   regexp.MustCompile(`://[^/\s@]+@`),
		repl: "://" + Marker + "@",
	},
	// Explicit headers, which carry Basic and Bearer payloads.
	{
		re:   regexp.MustCompile(`(?i)((?:proxy-)?authorization\s*:\s*(?:bearer|basic|token|digest)\s+)\S+`),
		repl: "${1}" + Marker,
	},
	// NAME=value, "name": "value" and name: value for anything named like a
	// credential. The quotes are optional so JSON, YAML, shell and log forms all
	// match, and they are kept so the redacted line stays readable.
	{
		re:   regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:token|key|secret|password|passwd|passphrase)[a-z0-9_.-]*)(["']?\s*[:=]\s*)(["']?)([^\s"',;&]{6,})(["']?)`),
		repl: "${1}${2}${3}" + Marker + "${5}",
	},
	// Provider token shapes.
	{re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}\b`), repl: Marker},
	{re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{16,}\b`), repl: Marker},
	{re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`), repl: Marker},
	{re: regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b`), repl: Marker},
	{re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}\b`), repl: Marker},
	{re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), repl: Marker},
	{re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`), repl: Marker},
	{re: regexp.MustCompile(`\bnvapi-[A-Za-z0-9_-]{16,}\b`), repl: Marker},
	{re: regexp.MustCompile(`\bsbp_[A-Za-z0-9]{16,}\b`), repl: Marker},
	// Telegram bot tokens follow "bot" in the URL, so no leading word boundary.
	{re: regexp.MustCompile(`\d{6,12}:[A-Za-z0-9_-]{30,}`), repl: Marker},
	// A bare bearer/basic value that lost its header, and JWT triples.
	{re: regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`), repl: "Bearer " + Marker},
	{re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), repl: Marker},
}

// Redact returns s with every recognized credential replaced by Marker. It is
// idempotent, and it leaves ordinary verification output (SHAs, checksums,
// package paths, timings) untouched.
//
// Redact the complete output, then CapTail it — in that order. Capping first
// could move a credential across the cut so that only a suffix remained, and a
// suffix no longer matches its pattern.
func Redact(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// CapTail returns the last max bytes of s, the shape a stored OutputTail takes.
// It never splits a UTF-8 rune, and max <= 0 yields an empty string.
func CapTail(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	tail := s[len(s)-max:]
	// s is normally valid UTF-8, but output is arbitrary bytes; drop leading
	// continuation bytes so the tail starts on a rune boundary when it can.
	for i := 0; i < utf8.UTFMax && len(tail) > 0; i++ {
		r, size := utf8.DecodeRuneInString(tail)
		if r != utf8.RuneError || size > 1 {
			break
		}
		tail = tail[1:]
	}
	return strings.TrimLeft(tail, "\x00")
}
