package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNetworkNotInternal identifies the safe failure when an existing session
// network would allow outbound routes. Callers can turn it into an actionable
// owner message without exposing raw Docker errors to Telegram.
var ErrNetworkNotInternal = errors.New("runner: session network is not internal")

// Docker is a minimal Docker Engine API client over the rootless socket.
// It uses only stdlib and the unversioned API paths supported by Docker 29.
type Docker struct {
	BaseURL string
	HTTP    *http.Client
}

// NewDocker builds a client from DOCKER_HOST. Empty means the default socket.
func NewDocker(host string) (*Docker, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	if strings.HasPrefix(host, "unix://") {
		path := strings.TrimPrefix(host, "unix://")
		client := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", path)
				},
			},
			Timeout: 10 * time.Minute,
		}
		return &Docker{BaseURL: "http://docker", HTTP: client}, nil
	}
	if !strings.Contains(host, "://") {
		host = "tcp://" + host
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("runner: bad DOCKER_HOST: %w", err)
	}
	if u.Scheme == "tcp" {
		u.Scheme = "http"
	}
	return &Docker{BaseURL: strings.TrimRight(u.String(), "/"), HTTP: &http.Client{Timeout: 10 * time.Minute}}, nil
}

func (d *Docker) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (d *Docker) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("runner: docker %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("runner: docker %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// ContainerSpec is the hardened container creation shape used from M3a.
type ContainerSpec struct {
	Name           string
	Image          string
	Env            []string
	Binds          []string
	Network        string
	MemoryMB       int
	CPUs           float64
	Pids           int
	Labels         map[string]string
	ExposePort     string
	Runtime        string
	ReadonlyRootfs bool
	Tmpfs          map[string]string
	CapDrop        []string
	SecurityOpt    []string
	GroupAdd       []string
	User           string
	Entrypoint     []string
	Cmd            []string
	PublishPorts   bool
}

func (s ContainerSpec) body() map[string]any {
	port := s.ExposePort
	if port == "" {
		port = "4096/tcp"
	}
	host := map[string]any{
		"Binds":           s.Binds,
		"NetworkMode":     s.Network,
		"Memory":          int64(s.MemoryMB) * 1024 * 1024,
		"NanoCpus":        int64(s.CPUs * 1e9),
		"PidsLimit":       s.Pids,
		"PublishAllPorts": s.PublishPorts,
	}
	if s.Runtime != "" {
		host["Runtime"] = s.Runtime
	}
	if s.ReadonlyRootfs {
		host["ReadonlyRootfs"] = true
	}
	if len(s.Tmpfs) > 0 {
		host["Tmpfs"] = s.Tmpfs
	}
	if len(s.CapDrop) > 0 {
		host["CapDrop"] = s.CapDrop
	}
	if len(s.SecurityOpt) > 0 {
		host["SecurityOpt"] = s.SecurityOpt
	}
	if len(s.GroupAdd) > 0 {
		host["GroupAdd"] = s.GroupAdd
	}
	body := map[string]any{
		"Image":        s.Image,
		"Env":          s.Env,
		"Labels":       s.Labels,
		"ExposedPorts": map[string]any{port: map[string]any{}},
		"HostConfig":   host,
	}
	if s.User != "" {
		body["User"] = s.User
	}
	if len(s.Entrypoint) > 0 {
		body["Entrypoint"] = s.Entrypoint
	}
	if len(s.Cmd) > 0 {
		body["Cmd"] = s.Cmd
	}
	return body
}

// Create creates a stopped container and returns its ID.
func (d *Docker) Create(ctx context.Context, spec ContainerSpec) (string, error) {
	path := "/containers/create"
	if spec.Name != "" {
		path += "?name=" + url.QueryEscape(spec.Name)
	}
	resp, err := d.do(ctx, http.MethodPost, path, spec.body())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("runner: decode create: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("runner: empty container id")
	}
	return out.ID, nil
}

func (d *Docker) Start(ctx context.Context, id string) error {
	resp, err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Wait blocks until the container stops and returns its exit code.
func (d *Docker) Wait(ctx context.Context, id string) (int, error) {
	resp, err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/wait?condition=not-running", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		StatusCode int `json:"StatusCode"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return 0, fmt.Errorf("runner: decode wait: %w", err)
	}
	return out.StatusCode, nil
}

// ExitInfo records why a container stopped. OOMKilled comes from inspect,
// because the Docker wait response only carries the numeric exit status.
type ExitInfo struct {
	ExitCode  int
	OOMKilled bool
}

func (d *Docker) WaitInfo(ctx context.Context, id string) (ExitInfo, error) {
	code, err := d.Wait(ctx, id)
	if err != nil {
		return ExitInfo{}, err
	}
	resp, err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		return ExitInfo{}, err
	}
	defer resp.Body.Close()
	var info struct {
		State struct {
			OOMKilled bool `json:"OOMKilled"`
		} `json:"State"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&info); err != nil {
		return ExitInfo{}, fmt.Errorf("runner: decode state: %w", err)
	}
	return ExitInfo{ExitCode: code, OOMKilled: info.State.OOMKilled}, nil
}

// CopyOut reads exactly one regular file from a container archive stream. It
// refuses directories and symlinks and never writes more than maxBytes.
func (d *Docker) CopyOut(ctx context.Context, id, path, dest string, maxBytes int64) error {
	if path == "" || dest == "" || !filepath.IsAbs(dest) {
		return fmt.Errorf("runner: invalid copy path")
	}
	resp, err := d.do(ctx, http.MethodGet,
		"/containers/"+url.PathEscape(id)+"/archive?path="+url.QueryEscape(path), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	archive := tar.NewReader(resp.Body)
	header, err := archive.Next()
	if err != nil {
		return fmt.Errorf("runner: empty archive: %w", err)
	}
	if header.Typeflag != tar.TypeReg || header.Size > maxBytes || header.Size < 0 {
		return fmt.Errorf("runner: refused archive entry type=%c size=%d", header.Typeflag, header.Size)
	}
	var body bytes.Buffer
	if _, err := io.CopyN(&body, archive, header.Size); err != nil {
		return fmt.Errorf("runner: copy body: %w", err)
	}
	if _, err := archive.Next(); err != io.EOF {
		return fmt.Errorf("runner: archive contains more than one entry")
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := body.WriteTo(out); err != nil {
		return fmt.Errorf("runner: write copy: %w", err)
	}
	return nil
}

func (d *Docker) Remove(ctx context.Context, id string) error {
	resp, err := d.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?force=1", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Inspect provides the published host port used by the local gateway.
func (d *Docker) Inspect(ctx context.Context, id string) (string, error) {
	resp, err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var info struct {
		NetworkSettings struct {
			Ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&info); err != nil {
		return "", fmt.Errorf("runner: decode inspect: %w", err)
	}
	for _, binding := range info.NetworkSettings.Ports["4096/tcp"] {
		if binding.HostPort != "" {
			host := binding.HostIP
			if host == "" || host == "0.0.0.0" {
				host = "127.0.0.1"
			}
			return "http://" + net.JoinHostPort(host, binding.HostPort), nil
		}
	}
	for _, netInfo := range info.NetworkSettings.Networks {
		if netInfo.IPAddress != "" {
			return "http://" + net.JoinHostPort(netInfo.IPAddress, "4096"), nil
		}
	}
	return "", fmt.Errorf("runner: container %s has no 4096 address", id)
}

// Exec runs a fixed command inside the container and returns combined output.
func (d *Docker) Exec(ctx context.Context, id string, cmd []string) ([]byte, error) {
	resp, err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Cmd":          cmd,
	})
	if err != nil {
		return nil, err
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		resp.Body.Close()
		return nil, err
	}
	resp.Body.Close()
	if created.ID == "" {
		return nil, fmt.Errorf("runner: empty exec id")
	}
	start, err := d.do(ctx, http.MethodPost, "/exec/"+url.PathEscape(created.ID)+"/start", map[string]any{
		"Detach": false,
		"Tty":    false,
	})
	if err != nil {
		return nil, err
	}
	defer start.Body.Close()
	out, err := readMultiplexed(start.Body)
	if err != nil {
		return nil, err
	}
	state, err := d.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(created.ID)+"/json", nil)
	if err != nil {
		return nil, err
	}
	defer state.Body.Close()
	var result struct {
		ExitCode int `json:"ExitCode"`
	}
	if err := json.NewDecoder(io.LimitReader(state.Body, 1<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("runner: decode exec result: %w", err)
	}
	if result.ExitCode != 0 {
		return out, fmt.Errorf("runner: exec exit %d: %s", result.ExitCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// readMultiplexed parses Docker's 8-byte stream frame header.
func readMultiplexed(r io.Reader) ([]byte, error) {
	header := make([]byte, 8)
	var out bytes.Buffer
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if err == io.EOF {
				return out.Bytes(), nil
			}
			return nil, err
		}
		n := binary.BigEndian.Uint32(header[4:8])
		if n > 32<<20 {
			return nil, fmt.Errorf("runner: docker stream frame too large: %d", n)
		}
		if _, err := io.CopyN(&out, r, int64(n)); err != nil {
			return nil, err
		}
	}
}

// ListedContainer is the subset of `docker ps` used by Reconcile.
type ListedContainer struct {
	ID     string
	Labels map[string]string
}

// ListContainers returns containers matching a Docker filter string.
func (d *Docker) ListContainers(ctx context.Context, filters string) ([]ListedContainer, error) {
	path := "/containers/json?all=1"
	if filters != "" {
		path += "&filters=" + url.QueryEscape(filters)
	}
	resp, err := d.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var items []struct {
		ID     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}
	out := make([]ListedContainer, 0, len(items))
	for _, it := range items {
		out = append(out, ListedContainer{ID: it.ID, Labels: it.Labels})
	}
	return out, nil
}

// EnsureNetwork creates the session network if absent. From M3a it must be
// internal (no default route); an existing non-internal network is rejected
// rather than silently weakening the sandbox.
func (d *Docker) EnsureNetwork(ctx context.Context, name string, internal bool) error {
	if name == "" || name == "bridge" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.BaseURL+"/networks/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		var info struct {
			Internal bool `json:"Internal"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info)
		resp.Body.Close()
		if decodeErr != nil {
			return decodeErr
		}
		if internal && !info.Internal {
			return fmt.Errorf("%w: %q", ErrNetworkNotInternal, name)
		}
		return nil
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("runner: inspect network: HTTP %d", resp.StatusCode)
	}
	create, err := d.do(ctx, http.MethodPost, "/networks/create", map[string]any{
		"Name":     name,
		"Driver":   "bridge",
		"Internal": internal,
	})
	if err != nil {
		return err
	}
	return create.Body.Close()
}
