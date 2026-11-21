package server

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/monirujjaman-dev/docklet/internal/docker/dockertest"
	"github.com/monirujjaman-dev/docklet/internal/host"
)

// fakeHost answers the host commands the Server page runs.
type fakeHost struct {
	mu       sync.Mutex
	os       string
	hostname string
	tz       string
	calls    []string
}

func (f *fakeHost) Run(_ context.Context, argv, env []string) (host.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := strings.Join(argv, " ")
	f.calls = append(f.calls, cmd+" "+strings.Join(env, " "))
	envv := func(k string) string {
		for _, e := range env {
			if v, ok := strings.CutPrefix(e, k+"="); ok {
				return v
			}
		}
		return ""
	}
	switch {
	case strings.Contains(cmd, "echo \"os.id=$ID\""):
		return host.Result{Stdout: "os.id=" + f.os + "\nos.pretty=Test OS\nsystemd=1\nhostname=" + f.hostname + "\ntz=" + f.tz +
			"\nmemtotal=1000000\nmemavail=500000\nswaptotal=0\nswapfree=0\nswappiness=60\ndisktotal=100\ndiskused=40\ndiskavail=60\nuptime=123.4\n"}, nil
	case strings.Contains(cmd, `echo "$ID"`):
		return host.Result{Stdout: f.os + "\nsystemd\n"}, nil
	case strings.Contains(cmd, "hostnamectl set-hostname"):
		f.hostname = envv("NEW_HOSTNAME")
		return host.Result{Stdout: f.hostname}, nil
	case argv[0] == "timedatectl" && argv[1] == "list-timezones":
		return host.Result{Stdout: "Africa/Abidjan\nAsia/Dhaka\nEurope/Berlin\nUTC\n"}, nil
	case argv[0] == "timedatectl" && argv[1] == "set-timezone":
		f.tz = argv[2]
		return host.Result{}, nil
	case argv[0] == "systemd-run":
		return host.Result{}, nil
	case strings.Contains(cmd, "ss -H -tulpn"):
		return host.Result{Stdout: `tcp LISTEN 0 4096 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=812,fd=3))` + "\n"}, nil
	case strings.Contains(cmd, "journalctl --disk-usage"):
		return host.Result{Stdout: "Archived and active journals take up 1.5G in the file system.\n\n40000000\n"}, nil
	case argv[0] == "journalctl" || argv[0] == "apt-get":
		return host.Result{}, nil
	}
	return host.Result{Exit: 1, Stderr: "unexpected command: " + cmd}, nil
}

func TestServerPageAPI(t *testing.T) {
	fh := &fakeHost{os: "ubuntu", hostname: "vps-old", tz: "UTC"}
	dk := dockertest.New()
	dk.AddImage("old/unused:1", 80)
	env := startPanelServer(t, filepath.Join(t.TempDir(), "d.db"), true, WithDocker(dk), WithHostRunner(fh))
	c := env.client(t)

	_, o := c.do("GET", "/api/server/overview", nil)
	if o["hostname"] != "vps-old" || o["os"].(map[string]any)["supported"] != true || o["memTotal"] != float64(1000000<<10) {
		t.Fatalf("overview = %v", o)
	}
	if code, _ := c.do("PUT", "/api/server/hostname", map[string]any{"hostname": "bad name"}); code != 400 {
		t.Fatalf("bad hostname: %d", code)
	}
	if code, o := c.do("PUT", "/api/server/hostname", map[string]any{"hostname": "web-1"}); code != 200 || o["hostname"] != "web-1" {
		t.Fatalf("hostname: %d %v", code, o)
	}
	if code, _ := c.do("PUT", "/api/server/timezone", map[string]any{"timezone": "Mars/Base"}); code != 400 {
		t.Fatalf("unknown zone: %d", code)
	}
	if code, o := c.do("PUT", "/api/server/timezone", map[string]any{"timezone": "Asia/Dhaka"}); code != 200 || o["timezone"] != "Asia/Dhaka" {
		t.Fatalf("timezone: %d %v", code, o)
	}
	if code, _ := c.do("PUT", "/api/server/swap", map[string]any{"sizeMb": 100}); code != 400 {
		t.Fatalf("tiny swap: %d", code)
	}

	// Reboot needs the password and the typed host name.
	if code, _ := c.do("POST", "/api/server/reboot", map[string]any{"hostname": "web-1", "password": "nope"}); code < 400 {
		t.Fatalf("reboot without password: %d", code)
	}
	if code, _ := c.do("POST", "/api/server/reboot", map[string]any{"hostname": "vps-old", "password": adminPW}); code != 400 {
		t.Fatalf("reboot with wrong hostname: %d", code)
	}
	if code, _ := c.do("POST", "/api/server/reboot", map[string]any{"hostname": "web-1", "password": adminPW}); code != 200 {
		t.Fatalf("reboot: %d", code)
	}
	if !strings.Contains(strings.Join(fh.calls, "\n"), "systemd-run --on-active=5") {
		t.Fatal("reboot not scheduled")
	}

	_, ports := c.do("GET", "/api/server/ports", nil)
	if p := ports["ports"].([]any); len(p) != 1 || p[0].(map[string]any)["process"] != "sshd" {
		t.Fatalf("ports = %v", ports)
	}
	if code, _ := c.do("POST", "/api/server/services/docker.service/stop", nil); code != 400 {
		t.Fatalf("stop docker: %d", code)
	}

	_, prev := c.do("GET", "/api/server/cleanup", nil)
	cats := prev["categories"].([]any)
	var names []string
	for _, cat := range cats {
		names = append(names, cat.(map[string]any)["key"].(string))
	}
	if strings.Join(names, ",") != "images,build-cache,containers,volumes,journal,apt" {
		t.Fatalf("categories = %v", names)
	}
	if j := cats[4].(map[string]any); j["size"].(float64) < 1.3e9 {
		t.Fatalf("journal = %v", j)
	}
	if code, res := c.do("POST", "/api/server/cleanup", map[string]any{"categories": []string{"images", "journal"}}); code != 200 || len(res["results"].([]any)) != 2 {
		t.Fatalf("cleanup: %d %v", code, res)
	}
	if dk.HasImage("old/unused:1") {
		t.Fatal("unused image kept")
	}

	// Unsupported hosts refuse changes but still show the overview.
	fh.os = "alpine"
	_, o = c.do("GET", "/api/server/overview", nil)
	if o["os"].(map[string]any)["supported"] != false {
		t.Fatalf("alpine supported: %v", o["os"])
	}
	if code, _ := c.do("PUT", "/api/server/hostname", map[string]any{"hostname": "x"}); code != 409 {
		t.Fatalf("change on unsupported host: %d", code)
	}

	acts := strings.Join(actionsOf(t, env.st), ",")
	for _, want := range []string{"server.hostname_changed", "server.timezone_changed", "server.reboot_requested", "server.disk_cleanup"} {
		if !strings.Contains(acts, want) {
			t.Errorf("audit missing %s", want)
		}
	}
}
