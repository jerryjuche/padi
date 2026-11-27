package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
			fmt.Fprint(w, `{"Name":"agent-session-net","Internal":false}`)
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
		case r.URL.Path == "/exec/exec1/json":
			fmt.Fprint(w, `{"ExitCode":0}`)
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
		Image: "session:latest", Network: "agent-session-net", Runtime: "runc", MemoryMB: 2560,
		CPUs: 1, Pids: 512, Model: "padi/agent-chat",
		RouterToken: "router-only-token", RouterBaseURL: "http://router:8090/v1",
		ProxyURL: "http://egress-proxy:3128", BundleMaxMB: 200,
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
	if host["ReadonlyRootfs"] != true || host["Runtime"] != "runc" || create["User"] != "agent" {
		t.Fatalf("hardening missing: %+v", create)
	}
	if fmt.Sprint(host["CapDrop"]) != "[ALL]" || fmt.Sprint(host["SecurityOpt"]) != "[no-new-privileges]" {
		t.Fatalf("capabilities not dropped: %+v", host)
	}
	if fmt.Sprint(host["GroupAdd"]) != fmt.Sprintf("[%d]", os.Getgid()) {
		t.Fatalf("workspace group not passed: %+v", host)
	}
	tmpfs := host["Tmpfs"].(map[string]any)
	if !strings.Contains(fmt.Sprint(tmpfs["/tmp"]), "exec") {
		t.Fatalf("tmpfs /tmp missing exec: %+v", tmpfs)
	}
	for _, dir := range []string{"/home/agent/.config", "/home/agent/.local", "/home/agent/.cache"} {
		if !strings.Contains(fmt.Sprint(tmpfs[dir]), "uid=1001") {
			t.Fatalf("tmpfs %s missing agent-writable options: %+v", dir, tmpfs)
		}
	}
	env := create["Env"].([]any)
	joined := fmt.Sprint(env)
	for _, want := range []string{"ROUTER_TOKEN=router-only-token", "PADI_MODEL_BASE_URL=http://router:8090/v1", "PADI_TASK_ID=task-0001", "OPENCODE_PASSWORD=", "HTTP_PROXY=http://egress-proxy:3128", "NO_PROXY=router,egress-proxy,localhost,127.0.0.1"} {
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

func TestStartVerifyUsesFreshHardenedContainer(t *testing.T) {
	docker, last := fakeDocker(t)
	r := New(docker, Config{Image: "session:latest", Network: "agent-session-net", Runtime: "runc", MemoryMB: 2560, CPUs: 1, Pids: 512})
	h, err := r.StartVerify(context.Background(), "task-0002", "/srv/agent/verify/task-0002")
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != "cid1" || h.Address != "" {
		t.Fatalf("handle=%+v", h)
	}
	create, ok := (*last)["create"].(map[string]any)
	if !ok {
		t.Fatalf("create body missing: %+v", last)
	}
	if create["User"] != "agent" || create["Cmd"] == nil || create["Entrypoint"] == nil {
		t.Fatalf("verify command missing: %+v", create)
	}
	if fmt.Sprint(create["Entrypoint"]) != `[/usr/local/bin/agent-verify]` ||
		fmt.Sprint(create["Cmd"]) != `[--format json --timeout 10m /workspace]` {
		t.Fatalf("unexpected verify command: %v %v", create["Entrypoint"], create["Cmd"])
	}
	host := create["HostConfig"].(map[string]any)
	if !strings.Contains(fmt.Sprint(host["Binds"]), "/srv/agent/verify/task-0002:/workspace:ro") {
		t.Fatalf("verify bind is not read-only: %v", host["Binds"])
	}
	if host["ReadonlyRootfs"] != true || host["PublishAllPorts"] != false {
		t.Fatalf("verify container not isolated: %+v", host)
	}
}

func TestOOMDetection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/wait"):
			fmt.Fprint(w, `{"StatusCode":137}`)
		case strings.HasSuffix(r.URL.Path, "/json"):
			fmt.Fprint(w, `{"State":{"OOMKilled":true}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	r := New(&Docker{BaseURL: srv.URL, HTTP: srv.Client()}, Config{})
	info, err := r.WaitInfo(context.Background(), "cid-oom")
	if err != nil {
		t.Fatal(err)
	}
	if info.ExitCode != 137 || !info.OOMKilled || !OOMKilled(info) {
		t.Fatalf("oom info=%+v", info)
	}
}

func TestCopyOutRefusesNonRegularFiles(t *testing.T) {
	cases := []struct {
		name     string
		typeflag byte
		size     int64
		wantErr  bool
	}{
		{"regular", tar.TypeReg, 3, false},
		{"symlink", tar.TypeSymlink, 0, true},
		{"directory", tar.TypeDir, 0, true},
		{"over cap", tar.TypeReg, 64, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var buf bytes.Buffer
				tw := tar.NewWriter(&buf)
				_ = tw.WriteHeader(&tar.Header{Name: "file", Mode: 0o600, Size: tc.size, Typeflag: tc.typeflag})
				if tc.typeflag == tar.TypeReg {
					_, _ = tw.Write(make([]byte, tc.size))
				}
				_ = tw.Close()
				_, _ = w.Write(buf.Bytes())
			}))
			defer srv.Close()
			dest := filepath.Join(t.TempDir(), "out")
			err := New(&Docker{BaseURL: srv.URL, HTTP: srv.Client()}, Config{}).CopyOut(context.Background(), "cid", "/tmp/file", dest, 8)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestExecRequiresZeroExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec") && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"Id":"eid1"}`)
		case strings.HasSuffix(r.URL.Path, "/exec/eid1/start"):
			var frame [8]byte
			binary.BigEndian.PutUint32(frame[4:], 5)
			_, _ = w.Write(frame[:])
			_, _ = w.Write([]byte("boom!"))
		case strings.HasSuffix(r.URL.Path, "/exec/eid1/json"):
			fmt.Fprint(w, `{"ExitCode":7}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	r := New(&Docker{BaseURL: srv.URL, HTTP: srv.Client()}, Config{})
	out, err := r.Exec(context.Background(), "cid", "/bin/false")
	if err == nil || !strings.Contains(err.Error(), "exit 7") {
		t.Fatalf("err=%v out=%q want exit 7", err, out)
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
