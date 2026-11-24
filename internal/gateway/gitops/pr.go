package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type prRequest struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
	Draft bool   `json:"draft"`
}

// OpenPR opens a non-draft PR when DefaultDraft is false. Use OpenPRDraft to
// choose the draft flag explicitly.
func (g *Git) OpenPR(ctx context.Context, repo, head, base, title, body string) (url string, err error) {
	return g.OpenPRDraft(ctx, repo, head, base, title, body, g.DefaultDraft)
}

// OpenPRDraft creates a pull request through the GitHub REST API. The head is
// restricted to the same agent/* allowlist used by Push; base may be main or
// master, but may never be another agent branch or the head itself.
//
// The installation token goes only into the Authorization header. Errors run
// through scrub plus an explicit token replacement, so a server echoing the
// credential cannot leak it into logs or audit records.
func (g *Git) OpenPRDraft(ctx context.Context, repo, head, base, title, body string, draft bool) (url string, err error) {
	if !validRepo(repo) {
		return "", fmt.Errorf("%w: %q", ErrForbiddenRepo, repo)
	}
	if err := validateAgentBranch(head); err != nil {
		return "", err
	}
	if base == "" || branchPattern.MatchString(base) || base == head {
		return "", fmt.Errorf("%w: base %q", ErrForbiddenBranch, base)
	}
	token, err := g.prToken(ctx, repo)
	if err != nil {
		return "", err
	}

	api := g.APIURL
	if api == "" {
		api = gitHubAPI
	}
	endpoint := strings.TrimRight(api, "/") + "/repos/" + repo + "/pulls"
	payload, err := json.Marshal(prRequest{
		Title: title,
		Head:  head,
		Base:  base,
		Body:  body,
		Draft: draft,
	})
	if err != nil {
		return "", fmt.Errorf("gitops: encode PR payload: %s", redactToken(err.Error(), token))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("gitops: PR request: %s", redactToken(err.Error(), token))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	client := g.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gitops: open PR: %s", redactToken(err.Error(), token))
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gitops: open PR status %d: %s",
			resp.StatusCode, redactToken(string(bodyBytes), token))
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return "", fmt.Errorf("gitops: bad PR response: %s", redactToken(err.Error(), token))
	}
	if out.HTMLURL == "" {
		return "", fmt.Errorf("gitops: PR response missing html_url")
	}
	return out.HTMLURL, nil
}

// prToken resolves the optional test hook first. Production leaves the hook
// nil and therefore uses MintInstallationToken exactly as before.
func (g *Git) prToken(ctx context.Context, repo string) (string, error) {
	f := g.tokenFunc
	if f == nil {
		f = g.MintInstallationToken
	}
	token, _, err := f(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("gitops: installation token: %s", redactToken(err.Error(), token))
	}
	if token == "" {
		return "", fmt.Errorf("gitops: empty installation token")
	}
	return token, nil
}

// SetTokenFuncForTest installs a fake installation-token source for hermetic
// tests. It must not be used in production code.
func (g *Git) SetTokenFuncForTest(f func(context.Context, string) (string, time.Time, error)) {
	g.tokenFunc = f
}

func redactToken(s, token string) string {
	s = scrub(s)
	if token == "" {
		return s
	}
	s = strings.ReplaceAll(s, "Bearer "+token, "[REDACTED]")
	return strings.ReplaceAll(s, token, "[REDACTED]")
}
