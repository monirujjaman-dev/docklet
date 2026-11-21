package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker"
)

type fakeDK struct {
	mu        sync.Mutex
	names     map[string]string // name -> id
	running   map[string]bool
	created   map[string]map[string]any
	versionOf map[string]string // id -> version it reports ("" = crashes)
	n         int
	dataDir   string // a new container "migrates" the database here
}

func (f *fakeDK) ContainerInspectRaw(_ context.Context, id string) (map[string]any, error) {
	return map[string]any{
		"Id": "old123456789abc", "Image": "sha256:oldimg",
		"Config": map[string]any{
			"Hostname": "old123456789", "Image": "ghcr.io/x/docklet:1.0.0",
			"Env":        []any{"DOCKLET_ADDR=172.30.0.3:3000", "PATH=/bin", "DOCKLET_CONTAINER=1"},
			"Entrypoint": []any{"docklet"}, "Cmd": []any{"serve"},
			"Labels": map[string]any{"com.docker.compose.service": "docklet", "org.opencontainers.image.version": "1.0.0"},
		},
		"HostConfig": map[string]any{"NetworkMode": "docklet", "Binds": []any{"/var/run/docker.sock:/var/run/docker.sock"}},
		"NetworkSettings": map[string]any{"Networks": map[string]any{
			"docklet":    map[string]any{"IPAMConfig": map[string]any{"IPv4Address": "172.30.0.3"}, "Aliases": []any{"docklet", "old123456789"}},
			"docklet-p1": map[string]any{"IPAMConfig": nil},
		}},
	}, nil
}

func (f *fakeDK) ImageInspectRaw(context.Context, string) (map[string]any, error) {
	return map[string]any{"Config": map[string]any{
		"Env": []any{"PATH=/bin", "DOCKLET_CONTAINER=1"}, "Entrypoint": []any{"docklet"}, "Cmd": []any{"serve"},
		"Labels": map[string]any{"org.opencontainers.image.version": "1.0.0"},
	}}, nil
}

func (f *fakeDK) ContainerInspect(_ context.Context, id string) (docker.ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var i docker.ContainerInfo
	i.State.Running = f.running[id]
	if !i.State.Running {
		i.State.Status = "exited"
		i.State.ExitCode = 3
	}
	return i, nil
}

func (f *fakeDK) ContainerCreateRaw(_ context.Context, name string, body map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, taken := f.names[name]; taken {
		return "", errors.New("name in use")
	}
	f.n++
	id := "new" + string(rune('0'+f.n))
	f.names[name] = id
	f.created[id] = body
	img, _ := body["Image"].(string)
	f.versionOf[id] = strings.TrimPrefix(img[strings.LastIndex(img, ":")+1:], "")
	if strings.Contains(img, "broken") {
		f.versionOf[id] = ""
	}
	return id, nil
}

func (f *fakeDK) ContainerRename(_ context.Context, id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for n, i := range f.names {
		if i == id {
			delete(f.names, n)
		}
	}
	f.names[name] = id
	return nil
}

func (f *fakeDK) ContainerStart(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[id] = id == "old123456789abc" || f.versionOf[id] != ""
	if id != "old123456789abc" && f.dataDir != "" {
		_ = os.WriteFile(filepath.Join(f.dataDir, "docklet.db"), []byte("migrated"), 0o600)
	}
	return nil
}

func (f *fakeDK) ContainerStop(_ context.Context, id string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[id] = false
	return nil
}

func (f *fakeDK) ContainerRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for n, i := range f.names {
		if i == id {
			delete(f.names, n)
		}
	}
	return nil
}

func (f *fakeDK) ContainerLogs(_ context.Context, _ string, _ docker.LogOptions, fn func(docker.LogLine) error) error {
	return fn(docker.LogLine{Text: "boom"})
}

func (f *fakeDK) NetworkConnect(context.Context, string, string, map[string]any) error { return nil }

func setup(t *testing.T) (*fakeDK, string, Options) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docklet.db"), []byte("v1-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, ".env")
	_ = os.WriteFile(env, []byte("DOCKLET_PORT=3000\nDOCKLET_VERSION=1.0.0\n"), 0o600)
	f := &fakeDK{names: map[string]string{"docklet": "old123456789abc"}, running: map[string]bool{"old123456789abc": true},
		created: map[string]map[string]any{}, versionOf: map[string]string{"old123456789abc": "1.0.0"}}
	o := Options{Container: "docklet", DataDir: dir, EnvFile: env, Health: 3 * time.Second,
		Probe: func(_ context.Context, addr string) (string, error) {
			if addr != "172.30.0.3:3000" {
				return "", errors.New("wrong address " + addr)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.versionOf[f.names["docklet"]], nil
		}}
	return f, dir, o
}

func TestUpdateSucceeds(t *testing.T) {
	f, dir, o := setup(t)
	o.Image, o.To = "ghcr.io/x/docklet:1.1.0", "1.1.0"
	rec := NewRecorder(dir, Status{State: StateRunning, From: "1.0.0", To: "1.1.0"}, nil)
	if err := Run(context.Background(), f, rec, o); err != nil {
		t.Fatal(err)
	}
	st, _ := ReadStatus(dir)
	if st.State != StateSucceeded {
		t.Fatalf("state = %s %s", st.State, st.Error)
	}
	body := f.created[f.names["docklet"]]
	if _, ok := body["Hostname"]; ok {
		t.Error("hostname copied")
	}
	if _, ok := body["Entrypoint"]; ok {
		t.Error("image entrypoint copied")
	}
	if env := body["Env"].([]any); len(env) != 1 || env[0] != "DOCKLET_ADDR=172.30.0.3:3000" {
		t.Errorf("env = %v", env)
	}
	if l := body["Labels"].(map[string]any); len(l) != 1 || l["com.docker.compose.service"] != "docklet" {
		t.Errorf("labels = %v", l)
	}
	ep := body["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)["docklet"].(map[string]any)
	if ep["IPAMConfig"].(map[string]any)["IPv4Address"] != "172.30.0.3" || len(ep["Aliases"].([]any)) != 1 {
		t.Errorf("endpoint = %v", ep)
	}
	if b, _ := os.ReadFile(o.EnvFile); !strings.Contains(string(b), "DOCKLET_VERSION=1.1.0\n") || !strings.Contains(string(b), "DOCKLET_PORT=3000") {
		t.Errorf(".env = %q", b)
	}
	if _, ok := f.names["docklet-old-"]; ok || len(f.names) != 1 {
		t.Errorf("containers left: %v", f.names)
	}
}

func TestUpdateRollsBack(t *testing.T) {
	f, dir, o := setup(t)
	o.Image, o.To = "ghcr.io/x/docklet:1.1.0-broken", "1.1.0-broken"
	rec := NewRecorder(dir, Status{State: StateRunning, From: "1.0.0", To: o.To}, nil)
	f.dataDir = dir // the broken version migrates the database before crashing
	if err := Run(context.Background(), f, rec, o); err == nil {
		t.Fatal("broken update succeeded")
	}
	st, _ := ReadStatus(dir)
	if st.State != StateRolledBack || !strings.Contains(st.Error, "stopped") {
		t.Fatalf("state = %s %q", st.State, st.Error)
	}
	if f.names["docklet"] != "old123456789abc" || !f.running["old123456789abc"] || len(f.names) != 1 {
		t.Fatalf("after rollback: %v %v", f.names, f.running)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "docklet.db")); string(b) != "v1-data" { //nolint:gosec // test temp dir
		t.Fatalf("database not restored: %q", b)
	}
	if b, _ := os.ReadFile(o.EnvFile); !strings.Contains(string(b), "DOCKLET_VERSION=1.0.0") {
		t.Errorf(".env changed: %q", b)
	}
	var logged bool
	for _, s := range st.Steps {
		logged = logged || strings.Contains(s.Msg, "boom")
	}
	if !logged {
		t.Error("new container's logs not recorded")
	}
}
