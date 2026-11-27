// Package runner manages the single M1 task container through the Docker API.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is supplied from config/gateway.yaml plus environment-only secrets.
type Config struct {
	Image           string
	Network         string
	Runtime         string // runc or runsc, chosen from runtime-bench evidence
	InternalNetwork bool   // M3a: no default route out of the session network
	MemoryMB        int
	CPUs            float64
	Pids            int
	Model           string
	RouterToken     string // only credential passed to the session container
	RouterBaseURL   string // OpenAI-compatible router endpoint, always /v1
	RouterHostIP    string // host bridge IP mapped to the "router" hostname
	ProxyURL        string // egress-proxy build port
	BundleMaxMB     int
	TaskIDHeader    string // optional per-provider header value source
}

// Handle is the live container plus the address/password of its OpenCode API.
type Handle struct {
	ID       string
	Address  string
	Password string
	TaskID   string
}

// Runner starts, waits for and stops task containers.
type Runner struct {
	Docker *Docker
	Cfg    Config
}

func New(docker *Docker, cfg Config) *Runner {
	return &Runner{Docker: docker, Cfg: cfg}
}

func randomPassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Start clones no code itself: workDir is the gateway-owned independent clone.
// The router token is the only secret passed into the container, as a process
// environment variable; provider keys never leave the router host.
func (r *Runner) Start(ctx context.Context, taskID, workDir string) (Handle, error) {
	if taskID == "" || strings.ContainsAny(taskID, "/\\..") {
		return Handle{}, fmt.Errorf("runner: invalid task id %q", taskID)
	}
	if err := r.Docker.EnsureNetwork(ctx, r.Cfg.Network, r.Cfg.InternalNetwork); err != nil {
		return Handle{}, err
	}
	password, err := randomPassword()
	if err != nil {
		return Handle{}, err
	}
	env := []string{
		"PATH=/home/agent/.opencode/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/home/agent",
		"OPENCODE_PASSWORD=" + password,
		"PADI_TASK_ID=" + taskID,
		"TASK_ID=" + taskID,
		"SESSION_ID=" + taskID,
		"PADI_MODEL=" + r.Cfg.Model,
		"PADI_MODEL_BASE_URL=" + r.Cfg.RouterBaseURL,
	}
	if r.Cfg.ProxyURL != "" {
		env = append(env, "HTTP_PROXY="+r.Cfg.ProxyURL, "HTTPS_PROXY="+r.Cfg.ProxyURL,
			"http_proxy="+r.Cfg.ProxyURL, "https_proxy="+r.Cfg.ProxyURL,
			"NO_PROXY=router,egress-proxy,localhost,127.0.0.1",
			"no_proxy=router,egress-proxy,localhost,127.0.0.1")
	}
	if r.Cfg.RouterToken != "" {
		env = append(env, "ROUTER_TOKEN="+r.Cfg.RouterToken)
	}
	spec := r.hardenedSpec(ContainerSpec{
		Name:       "padi-task-" + taskID,
		Image:      r.Cfg.Image,
		Env:        env,
		Binds:      []string{workDir + ":/workspace"},
		Network:    r.Cfg.Network,
		MemoryMB:   r.Cfg.MemoryMB,
		CPUs:       r.Cfg.CPUs,
		Pids:       r.Cfg.Pids,
		ExposePort: "4096/tcp",
		Labels: map[string]string{
			"sandbox-agent": "true",
			"padi_task_id":  taskID,
			"padi_role":     "session",
		},
		PublishPorts: true,
	})
	id, err := r.Docker.Create(ctx, spec)
	if err != nil {
		return Handle{}, err
	}
	if err := r.Docker.Start(ctx, id); err != nil {
		_ = r.Docker.Remove(context.Background(), id)
		return Handle{}, err
	}
	address, err := r.Docker.Inspect(ctx, id)
	if err != nil {
		_ = r.Docker.Remove(context.Background(), id)
		return Handle{}, err
	}
	return Handle{ID: id, Address: address, Password: password, TaskID: taskID}, nil
}

// hardenedSpec applies the M3a boundary to every container. The rootfs is
// read-only, all capabilities are dropped, no new privileges are allowed, and
// only /tmp plus the agent home are writable tmpfs mounts.
func (r *Runner) hardenedSpec(spec ContainerSpec) ContainerSpec {
	spec.Runtime = r.Cfg.Runtime
	if spec.Runtime == "" {
		spec.Runtime = "runc"
	}
	spec.ReadonlyRootfs = true
	// OpenCode writes config, state and cache under $HOME at startup. All three
	// XDG dirs must be writable; a read-only rootfs otherwise kills the
	// container before the health endpoint can answer.
	tmpfsOpts := "rw,nosuid,nodev,size=64m,uid=1001,gid=1001,mode=0700"
	spec.Tmpfs = map[string]string{
		"/tmp":                "rw,nosuid,nodev,exec,size=512m",
		"/home/agent/.config": tmpfsOpts,
		"/home/agent/.local":  tmpfsOpts,
		"/home/agent/.cache":  tmpfsOpts,
	}
	spec.CapDrop = []string{"ALL"}
	spec.SecurityOpt = []string{"no-new-privileges"}
	// The gateway owns the bind-mounted workspace. Give the agent its host
	// group so git can write objects there; safe.directory in the fixed scripts
	// already trusts this exact path. No extra capability or root access.
	if gid := os.Getgid(); gid > 0 {
		spec.GroupAdd = []string{strconv.Itoa(gid)}
	}
	// The router runs on the host bridge, not as a container on
	// agent-session-net. Docker's embedded DNS has no "router" record, so the
	// session would retry forever without this alias. No egress is opened:
	// the network remains internal and only this one hostname is pinned.
	if ip := strings.TrimSpace(r.Cfg.RouterHostIP); ip != "" {
		host := "router"
		if u, err := url.Parse(r.Cfg.RouterBaseURL); err == nil && u.Hostname() != "" {
			host = u.Hostname()
		}
		spec.ExtraHosts = []string{host + ":" + ip}
	}
	spec.User = "agent"
	return spec
}

// StartVerify starts a fresh, short-lived container on a quarantine checkout.
// It never runs in the agent's container and is removed by the caller on exit.
func (r *Runner) StartVerify(ctx context.Context, taskID, checkoutDir string) (Handle, error) {
	return r.StartVerifyWithOutput(ctx, taskID, checkoutDir, "")
}

// StartVerifyWithOutput is the M3b form: the verifier writes its JSON verdict
// to a gateway-owned directory outside the read-only checkout. The output bind
// is writable only to the short-lived verify container.
func (r *Runner) StartVerifyWithOutput(ctx context.Context, taskID, checkoutDir, outputDir string) (Handle, error) {
	if taskID == "" || strings.ContainsAny(taskID, "/\\..") || checkoutDir == "" {
		return Handle{}, fmt.Errorf("runner: invalid verify request %q / %q", taskID, checkoutDir)
	}
	if err := r.Docker.EnsureNetwork(ctx, r.Cfg.Network, r.Cfg.InternalNetwork); err != nil {
		return Handle{}, err
	}
	binds := []string{checkoutDir + ":/workspace:ro"}
	cmd := []string{"--format", "json", "--timeout", "10m"}
	if outputDir != "" {
		clean := filepath.Clean(outputDir)
		if !filepath.IsAbs(clean) || clean == "/" || strings.Contains(clean, "..") {
			return Handle{}, fmt.Errorf("runner: unsafe verify output directory %q", outputDir)
		}
		if err := os.MkdirAll(clean, 0o700); err != nil {
			return Handle{}, err
		}
		binds = append(binds, clean+":/tmp/padi-out:rw")
		cmd = append(cmd, "--output", "/tmp/padi-out/verdict.json")
	}
	cmd = append(cmd, "/workspace")
	spec := r.hardenedSpec(ContainerSpec{
		Name:  "padi-verify-" + taskID,
		Image: r.Cfg.Image,
		Env: []string{
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"PADI_TASK_ID=" + taskID, "TASK_ID=" + taskID, "HOME=/home/agent",
			"GOCACHE=/tmp/go-build", "GOMODCACHE=/tmp/go-mod", "XDG_CACHE_HOME=/tmp/cache",
		},
		Binds:    binds,
		Network:  r.Cfg.Network,
		MemoryMB: r.Cfg.MemoryMB, CPUs: r.Cfg.CPUs, Pids: r.Cfg.Pids,
		ExposePort: "4097/tcp",
		Labels: map[string]string{
			"sandbox-agent": "true",
			"padi_task_id":  taskID,
			"padi_role":     "verify",
		},
		Entrypoint: []string{"/usr/local/bin/agent-verify"},
		Cmd:        cmd,
	})
	id, err := r.Docker.Create(ctx, spec)
	if err != nil {
		return Handle{}, err
	}
	if err := r.Docker.Start(ctx, id); err != nil {
		_ = r.Docker.Remove(context.Background(), id)
		return Handle{}, err
	}
	return Handle{ID: id, TaskID: taskID}, nil
}

func (r *Runner) Wait(ctx context.Context, id string) (int, error) {
	return r.Docker.Wait(ctx, id)
}

// WaitInfo reports both the exit code and OOM kill flag. OOM is always a task
// failure, whether it happened in the session or the verify container.
func (r *Runner) WaitInfo(ctx context.Context, id string) (ExitInfo, error) {
	return r.Docker.WaitInfo(ctx, id)
}

func OOMKilled(info ExitInfo) bool {
	return info.OOMKilled || info.ExitCode == 137
}

// CopyOut retrieves ONE file from a container under an explicit byte cap.
func (r *Runner) CopyOut(ctx context.Context, id, path, dest string, maxBytes int64) error {
	return r.Docker.CopyOut(ctx, id, path, dest, maxBytes)
}

func (r *Runner) Stop(ctx context.Context, id string) error {
	return r.Docker.Remove(ctx, id)
}

func (r *Runner) Exec(ctx context.Context, id string, cmd ...string) ([]byte, error) {
	return r.Docker.Exec(ctx, id, cmd)
}

// Reconcile removes containers left behind by a gateway restart whose task is
// no longer active. Running tasks are re-attached by returning their IDs.
func (r *Runner) Reconcile(ctx context.Context, active map[string]bool) (reattach []Handle, err error) {
	items, err := r.Docker.ListContainers(ctx, `{"label":["sandbox-agent=true"]}`)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		taskID := item.Labels["padi_task_id"]
		if active[taskID] {
			address, inspectErr := r.Docker.Inspect(ctx, item.ID)
			if inspectErr != nil {
				continue
			}
			reattach = append(reattach, Handle{ID: item.ID, Address: address, TaskID: taskID})
			continue
		}
		if err := r.Docker.Remove(ctx, item.ID); err != nil {
			return nil, err
		}
	}
	return reattach, nil
}

// WaitHealthy polls the OpenCode server inside the container.
func WaitHealthy(ctx context.Context, check func(context.Context) error, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		last = check(probeCtx)
		cancel()
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("runner: opencode not healthy: %w", last)
}
