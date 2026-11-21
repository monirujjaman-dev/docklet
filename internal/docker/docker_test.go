package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s))) // #nosec G115 -- tiny test strings
	return append(h, s...)
}

func TestDemux(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "hello\nwor"))
	buf.Write(frame(2, "oops\n"))
	buf.Write(frame(1, "ld\nlast"))
	var got []string
	err := Demux(&buf, func(l LogLine) error {
		got = append(got, l.Stream+":"+l.Text)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "stdout:hello,stderr:oops,stdout:world,stdout:last"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v", got)
	}
}

func TestSplitImageRef(t *testing.T) {
	cases := map[string][2]string{
		"nginx":                 {"nginx", "latest"},
		"nginx:alpine":          {"nginx", "alpine"},
		"ghcr.io/o/app:1.2":     {"ghcr.io/o/app", "1.2"},
		"localhost:5000/app":    {"localhost:5000/app", "latest"},
		"localhost:5000/app:v2": {"localhost:5000/app", "v2"},
		"nginx@sha256:abc":      {"nginx@sha256:abc", ""},
	}
	for in, want := range cases {
		img, tag := SplitImageRef(in)
		if img != want[0] || tag != want[1] {
			t.Errorf("%s: got %s %s", in, img, tag)
		}
	}
}

func TestFirstTCPPort(t *testing.T) {
	var img Image
	img.Config.ExposedPorts = map[string]struct{}{"8080/tcp": {}, "53/udp": {}, "443/tcp": {}}
	if p := img.FirstTCPPort(); p != 443 {
		t.Fatalf("got %d", p)
	}
}

func TestVersionNegotiationAndErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_ping":
			w.Header().Set("Api-Version", "1.47")
		case "/v1.47/containers/missing/json":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"No such container: missing"}`))
		case "/v1.47/images/create":
			_, _ = w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"errorDetail":{"message":"denied"},"error":"denied"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c := newWithHTTP(ts.Client(), ts.URL)
	ctx := context.Background()
	if _, err := c.ContainerInspect(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := c.ImagePull(ctx, "private/app:1", nil); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("pull err = %v", err)
	}
}

func TestRejectsOldDaemon(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Api-Version", "1.41")
	}))
	defer ts.Close()
	if _, err := newWithHTTP(ts.Client(), ts.URL).Ping(context.Background()); err == nil {
		t.Fatal("expected error for old API")
	}
}

// TestRealDocker runs against a live daemon when DOCKLET_TEST_DOCKER=1.
// It needs the local image docklet-test/hello:v1 (any image listening on 8080).
func TestRealDocker(t *testing.T) {
	if os.Getenv("DOCKLET_TEST_DOCKER") == "" {
		t.Skip("set DOCKLET_TEST_DOCKER=1 to run against a real Docker daemon")
	}
	c := New("/var/run/docker.sock")
	ctx := context.Background()
	if _, err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	net := "docklet-test-net"
	if err := c.NetworkEnsure(ctx, net, map[string]string{"docklet.managed": "true"}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.NetworkRemove(ctx, net) }()
	if attach := os.Getenv("DOCKLET_TEST_ATTACH"); attach != "" {
		for i := 0; i < 2; i++ { // second call must be a no-op
			if err := c.NetworkAttach(ctx, net, attach); err != nil {
				t.Fatalf("attach #%d: %v", i+1, err)
			}
		}
	}
	id, err := c.ContainerCreate(ctx, ContainerSpec{
		Name: "docklet-test-hello", Image: "docklet-test/hello:v1", Env: []string{"APP_VERSION=t"},
		Labels: map[string]string{"docklet.managed": "true", "docklet.test": "1"}, Port: 8080,
		Network: net, Aliases: []string{"hello"}, RestartPolicy: "unless-stopped", MemoryBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.ContainerRemove(ctx, id) }()
	if err := c.ContainerStart(ctx, id); err != nil {
		t.Fatal(err)
	}
	info, err := c.ContainerInspect(ctx, id)
	if err != nil || !info.State.Running || info.IP(net) == "" {
		t.Fatalf("inspect: %+v %v", info.State, err)
	}
	list, err := c.ContainerList(ctx, map[string]string{"docklet.test": "1"})
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	time.Sleep(300 * time.Millisecond)
	var lines []string
	lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = c.ContainerLogs(lctx, id, LogOptions{Tail: 10}, func(l LogLine) error {
		lines = append(lines, l.Text)
		return nil
	})
	if len(lines) == 0 || !strings.Contains(lines[0], "listening") {
		t.Fatalf("logs: %v", lines)
	}
	if err := c.ContainerStop(ctx, id, 2); err != nil {
		t.Fatal(err)
	}
}

// TestRealExecAndArchive needs DOCKLET_TEST_DOCKER=1 and the local images
// docklet-test/hello:v1 and docklet-test/probe (a binary that prints and exits).
func TestRealExecAndArchive(t *testing.T) {
	if os.Getenv("DOCKLET_TEST_DOCKER") == "" {
		t.Skip("set DOCKLET_TEST_DOCKER=1")
	}
	c := New("/var/run/docker.sock")
	ctx := context.Background()
	vol := "docklet-test-vol"
	if err := c.VolumeCreate(ctx, vol, map[string]string{"docklet.test": "1"}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.VolumeRemove(ctx, vol) }()
	id, err := c.ContainerCreate(ctx, ContainerSpec{
		Name: "docklet-test-exec", Image: "docklet-test/hello:v1", Network: "bridge", RestartPolicy: "no",
		Volumes: map[string]string{vol: "/data"}, Publish: map[int]int{8080: 18089},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.ContainerRemove(ctx, id) }()
	if err := c.ContainerStart(ctx, id); err != nil {
		t.Fatal(err)
	}

	payload := strings.Repeat("backup-bytes ", 1000)
	if err := c.CopyTo(ctx, id, "/data", "dump.bin", strings.NewReader(payload), int64(len(payload)), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	n, err := c.CopyFrom(ctx, id, "/data/dump.bin", &out)
	if err != nil || n != int64(len(payload)) || out.String() != payload {
		t.Fatalf("copy round trip: n=%d err=%v", n, err)
	}

	// Published port reaches the container from the host.
	time.Sleep(300 * time.Millisecond)
	if res, err := http.Get("http://127.0.0.1:18089/"); err != nil {
		t.Fatalf("published port: %v", err)
	} else {
		_ = res.Body.Close()
	}

	// Exec: the hello image has no shell; running its binary again prints a
	// line and exits at once because port 8080 is taken.
	var stdout bytes.Buffer
	res, err := c.Exec(ctx, id, []string{"/app"}, []string{"APP_VERSION=exec"}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(stdout.String(), "version exec") {
		t.Fatalf("exec: exit=%d stdout=%q stderr=%q", res.ExitCode, stdout.String(), res.Stderr)
	}

	// Volume data survives recreating the container.
	_ = c.ContainerRemove(ctx, id)
	id2, err := c.ContainerCreate(ctx, ContainerSpec{
		Name: "docklet-test-exec2", Image: "docklet-test/hello:v1", Network: "bridge", RestartPolicy: "no",
		Volumes: map[string]string{vol: "/data"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.ContainerRemove(ctx, id2) }()
	out.Reset()
	if _, err := c.CopyFrom(ctx, id2, "/data/dump.bin", &out); err != nil || out.String() != payload {
		t.Fatalf("volume lost data: %v", err)
	}
}
