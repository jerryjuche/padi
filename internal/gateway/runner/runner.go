// Package runner manages the single M1 task container through the Docker API.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// Config is supplied from config/gateway.yaml plus environment-only secrets.
type Config struct {
	Image         string
	Network       string
	MemoryMB      int
	CPUs          float64
	Pids          int
	Model         string
	RouterToken   string // only credential passed to the session container
	RouterBaseURL string // OpenAI-compatible router endpoint, always /v1
	TaskIDHeader  string // optional per-provider header value source
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
	if err := r.Docker.EnsureNetwork(ctx, r.Cfg.Network); err != nil {
		return Handle{}, err
	}
	password, err := randomPassword()
	if err != nil {
		return Handle{}, err
	}
	env := []string{
		"OPENCODE_PASSWORD=" + password,
		"PADI_TASK_ID=" + taskID,
		"PADI_MODEL=" + r.Cfg.Model,
		"PADI_MODEL_BASE_URL=" + r.Cfg.RouterBaseURL,
	}
	if r.Cfg.RouterToken != "" {
		env = append(env, "ROUTER_TOKEN="+r.Cfg.RouterToken)
	}
	spec := ContainerSpec{
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
		},
	}
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

func (r *Runner) Wait(ctx context.Context, id string) (int, error) {
	return r.Docker.Wait(ctx, id)
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
