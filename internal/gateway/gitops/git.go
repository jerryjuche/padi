// Package gitops implements the M1 subset of git operations. All host-side
// git calls go through Run; no git process is ever given an agent-writable
// workspace as its working directory.
package gitops

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const gitHubAPI = "https://api.github.com"

var (
	ErrForbiddenRepo   = errors.New("gitops: forbidden repo")
	ErrForbiddenBranch = errors.New("gitops: forbidden branch")
	branchPattern      = regexp.MustCompile(`^agent/[a-z0-9._-]{1,60}$`)
)

// Git is the host-side git service. Only the three M1 functions are present.
type Git struct {
	CacheDir      string
	WorkspacesDir string
	HTTPClient    *http.Client
}

// MintInstallationToken exchanges GitHub App credentials for a 10-minute
// installation token. Credentials are read from the environment on each call;
// they are never placed in config, an image or a git config file.
func (g *Git) MintInstallationToken(ctx context.Context, repo string) (string, time.Time, error) {
	appID := os.Getenv("GITHUB_APP_ID")
	installationID := os.Getenv("GITHUB_APP_INSTALLATION_ID")
	if installationID == "" {
		installationID = os.Getenv("GITHUB_INSTALLATION_ID") // M1 alias
	}
	keyPath := os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH")
	if appID == "" || installationID == "" || keyPath == "" {
		return "", time.Time{}, fmt.Errorf("gitops: GitHub App env not set (GITHUB_APP_ID, GITHUB_APP_INSTALLATION_ID, GITHUB_APP_PRIVATE_KEY_PATH)")
	}
	if !validRepo(repo) {
		return "", time.Time{}, fmt.Errorf("%w: %q", ErrForbiddenRepo, repo)
	}
	jwt, err := appJWT(appID, time.Now())
	if err != nil {
		return "", time.Time{}, err
	}
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", gitHubAPI, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	client := g.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("gitops: token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf("gitops: token status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		return "", time.Time{}, fmt.Errorf("gitops: bad token response: %v", err)
	}
	return out.Token, out.ExpiresAt, nil
}

func appJWT(appID string, now time.Time) (string, error) {
	keyPath := os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH")
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("gitops: read private key: %w", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return "", errors.New("gitops: private key is not PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, e2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e2 != nil {
			return "", fmt.Errorf("gitops: parse private key: %w", err)
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("gitops: private key is not RSA")
		}
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]int64{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": mustParseInt(appID),
	})
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return "", fmt.Errorf("gitops: sign JWT: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func mustParseInt(s string) int64 {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

func validRepo(repo string) bool {
	if repo == "" || strings.ContainsAny(repo, "\x00\r\n") {
		return false
	}
	if strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, "file://") || strings.HasPrefix(repo, "./") {
		return true
	}
	parts := strings.Split(repo, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.Contains(repo, "..")
}

func cacheName(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	slug := strings.NewReplacer("/", "-", ".", "-", " ", "-").Replace(repo)
	if len(slug) > 48 {
		slug = slug[:48]
	}
	return slug + "-" + fmt.Sprintf("%x", sum[:6]) + ".git"
}

func (g *Git) cachePath(repo string) string {
	return filepath.Join(g.CacheDir, cacheName(repo))
}

func (g *Git) extraheader(ctx context.Context, repo string) ([]string, error) {
	if isLocal(repo) {
		return nil, nil
	}
	token, _, err := g.MintInstallationToken(ctx, repo)
	if err != nil {
		return nil, err
	}
	return []string{"-c", "http.extraheader=Authorization: Bearer " + token}, nil
}

func isLocal(repo string) bool {
	return strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, "file://") || strings.HasPrefix(repo, "./")
}

func localArgs(repo string) []string {
	if !isLocal(repo) {
		return nil
	}
	return []string{"-c", "protocol.file.allow=always"}
}

// EnsureCache creates or refreshes a gateway-owned bare cache. Local source
// paths are permitted only here, for the local spike.
func (g *Git) EnsureCache(ctx context.Context, repo string) (string, error) {
	if !validRepo(repo) {
		return "", fmt.Errorf("%w: %q", ErrForbiddenRepo, repo)
	}
	if err := os.MkdirAll(g.CacheDir, 0o700); err != nil {
		return "", err
	}
	cache := g.cachePath(repo)
	auth, err := g.extraheader(ctx, repo)
	if err != nil {
		return "", err
	}
	args := append([]string{}, append(localArgs(repo), auth...)...)
	source := repo
	if strings.HasPrefix(repo, "file://") {
		source = strings.TrimPrefix(repo, "file://")
	}
	if st, err := os.Stat(cache); err == nil && st.IsDir() {
		fetch := append([]string{}, args...)
		fetch = append(fetch, "-C", cache, "fetch", "--prune", "origin")
		if _, err := Run(ctx, g.CacheDir, nil, fetch...); err != nil {
			return "", fmt.Errorf("gitops: fetch cache: %w", err)
		}
		return cache, nil
	}
	clone := append([]string{}, args...)
	clone = append(clone, "clone", "--bare", "--no-hardlinks", "--quiet", source, cache)
	if _, err := Run(ctx, g.CacheDir, nil, clone...); err != nil {
		return "", fmt.Errorf("gitops: clone cache: %w", err)
	}
	return cache, nil
}

// BranchName builds the single per-task branch pattern allowed by spec.
func BranchName(repo, taskID string) string {
	slug := strings.ToLower(strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(repo))
	slug = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "repo"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}
	short := taskID
	if len(short) > 8 {
		short = short[:8]
	}
	branch := "agent/" + slug + "-" + short
	if !branchPattern.MatchString(branch) {
		branch = "agent/repo-" + short
	}
	return branch
}

// CloneForTask clones the cache into a gateway-owned staging directory, creates
// the task branch there, then atomically renames it into workspaces/<taskID>/repo.
// No git command ever runs with an agent-writable workspace as its working dir.
func (g *Git) CloneForTask(ctx context.Context, repo, taskID string) (string, string, error) {
	if !validRepo(repo) || strings.ContainsAny(taskID, "/\\..") {
		return "", "", fmt.Errorf("%w: %q / %q", ErrForbiddenRepo, repo, taskID)
	}
	cache, err := g.EnsureCache(ctx, repo)
	if err != nil {
		return "", "", err
	}
	stagingRoot := filepath.Join(g.WorkspacesDir, ".staging")
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		return "", "", err
	}
	staged := filepath.Join(stagingRoot, taskID)
	finalDir := filepath.Join(g.WorkspacesDir, taskID, "repo")
	_ = os.RemoveAll(staged)
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(finalDir), 0o700); err != nil {
		return "", "", err
	}

	headOut, err := Run(ctx, g.CacheDir, nil, "-C", cache, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", "", err
	}
	defaultBranch := strings.TrimSpace(string(headOut))
	if defaultBranch == "" || strings.ContainsAny(defaultBranch, "\r\n") {
		return "", "", fmt.Errorf("gitops: invalid default branch %q", defaultBranch)
	}

	cloneArgs := append(localArgs(cache), "clone", "--no-hardlinks", "--quiet",
		"--branch", defaultBranch, cache, staged)
	if _, err := Run(ctx, stagingRoot, nil, cloneArgs...); err != nil {
		return "", "", fmt.Errorf("gitops: clone for task: %w", err)
	}
	branch := BranchName(repo, taskID)
	if !branchPattern.MatchString(branch) {
		return "", "", fmt.Errorf("%w: %q", ErrForbiddenBranch, branch)
	}
	if _, err := Run(ctx, stagingRoot, nil,
		"-c", "user.name=sandbox-agent",
		"-c", "user.email=noreply@sandbox.local",
		"-C", staged, "checkout", "--quiet", "-b", branch); err != nil {
		_ = os.RemoveAll(staged)
		return "", "", fmt.Errorf("gitops: create branch: %w", err)
	}
	_ = os.RemoveAll(finalDir)
	if err := os.Rename(staged, finalDir); err != nil {
		return "", "", fmt.Errorf("gitops: publish clone: %w", err)
	}
	return finalDir, branch, nil
}
