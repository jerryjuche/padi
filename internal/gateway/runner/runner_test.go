package runner

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeDocker(t *testing.T) (*Docker, *map[string]any) {
	t.Helper()
	last := &map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/networks/agent-session-net":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/containers/create":
			body := map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			(*last)["create"] = body
			fmt.Fprint(w, `{"Id":"cid1","Warnings":[]}`)
		case r.URL.Path == "/containers/cid1/start":
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/containers/cid1/json":
			fmt.Fprint(w, `{"NetworkSettings":{"Ports":{"4096/tcp":[{"HostIp":"127.0.0.1","HostPort":"49155"}]}}}`)
		case r.URL.Path == "/containers/cid1/wait":
			fmt.Fprint(w, `{"StatusCode":0}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/containers/"):
			(*last)["removed"] = true
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/containers/cid1/exec":
			fmt.Fprint(w, `{"Id":"exec1"}`)
		case r.URL.Path == "/exec/exec1/start":
			var buf bytes.Buffer
			buf.WriteByte(1)
			buf.Write([]byte{0, 0, 0})
			_ = binary.Write(&buf, binary.BigEndian, uint32(3))
			buf.WriteString("ok\n")
			w.Write(buf.Bytes())
		case r.URL.Path == "/containers/json":
			fmt.Fprint(w, `[{"Id":"cid1","Labels":{"sandbox-agent":"true","padi_task_id":"t1"}},{"Id":"cid2","Labels":{"sandbox-agent":"true","padi_task_id":"old"}}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &Docker{BaseURL: srv.URL, HTTP: srv.Client()}, last
}

func TestStartWaitStop(t *testing.T) {
	docker, last := fakeDocker(t)
	r := New(docker, Config{
		Image: "session:latest", Network: "agent-session-net", MemoryMB: 2560,
		CPUs: 1, Pids: 512, Model: "openai/gpt-5-nano",
		RouterToken: "router-only-token", RouterBaseURL: "http://router:8090/v1",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := r.Start(ctx, "task-0001", "/srv/agent/workspaces/task-0001/repo")
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != "cid1" || h.Address != "http://127.0.0.1:49155" || h.Password == "" {
		t.Fatalf("handle=%+v", h)
	}
	create, ok := (*last)["create"].(map[string]any)
	if !ok {
		t.Fatalf("create body missing: %+v", last)
	}
	host := create["HostConfig"].(map[string]any)
	if host["Memory"].(float64) != 2560*1024*1024 || host["NetworkMode"] != "agent-session-net" {
		t.Fatalf("host config=%+v", host)
	}
	env := create["Env"].([]any)
	joined := fmt.Sprint(env)
	for _, want := range []string{"ROUTER_TOKEN=router-only-token", "PADI_MODEL_BASE_URL=http://router:8090/v1", "PADI_TASK_ID=task-0001", "OPENCODE_PASSWORD="} {
		if !bytes.Contains([]byte(joined), []byte(want)) {
			t.Fatalf("env missing %q: %v", want, env)
		}
	}
	for _, forbidden := range []string{"NVIDIA_API_KEY=", "GROQ_API_KEY=", "PADI_MODEL_API_KEY="} {
		if bytes.Contains([]byte(joined), []byte(forbidden)) {
			t.Fatalf("direct provider key leaked into container env: %v", env)
		}
	}
	if code, err := r.Wait(ctx, h.ID); err != nil || code != 0 {
		t.Fatalf("wait=%d err=%v", code, err)
	}
	if out, err := r.Exec(ctx, h.ID, "true"); err != nil || string(out) != "ok\n" {
		t.Fatalf("exec=%q err=%v", out, err)
	}
	if err := r.Stop(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if (*last)["removed"] != true {
		t.Fatal("container was not removed")
	}
}

func TestReconcile(t *testing.T) {
	docker, last := fakeDocker(t)
	r := New(docker, Config{})
	got, err := r.Reconcile(context.Background(), map[string]bool{"t1": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TaskID != "t1" {
		t.Fatalf("reattach=%+v", got)
	}
	if (*last)["removed"] != true {
		t.Fatal("stale container was not removed")
	}
}

func TestWaitHealthy(t *testing.T) {
	attempts := 0
	err := WaitHealthy(context.Background(), func(context.Context) error {
		attempts++
		if attempts < 2 {
			return fmt.Errorf("not ready")
		}
		return nil
	}, time.Second)
	if err != nil || attempts < 2 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}
