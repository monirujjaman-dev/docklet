package server

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/monirujjaman-dev/docklet/internal/docker/dockertest"
	"github.com/monirujjaman-dev/docklet/internal/host"
)

// fakeSecHost simulates sshd and ufw well enough for the API flow.
type fakeSecHost struct {
	mu      sync.Mutex
	ports   string // "22" or "22 2222"
	timer   bool   // revert scheduled
	ufwOn   bool
	reverts int
}

var portLine = regexp.MustCompile(`(?m)^Port (\d+)$`)

func (f *fakeSecHost) Run(_ context.Context, argv, env []string) (host.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := strings.Join(argv, " ")
	envv := func(k string) string {
		for _, e := range env {
			if v, ok := strings.CutPrefix(e, k+"="); ok {
				return v
			}
		}
		return ""
	}
	switch {
	case strings.Contains(cmd, `echo "$ID"`):
		return host.Result{Stdout: "ubuntu\nsystemd\n"}, nil
	case strings.Contains(cmd, "sshd -T"):
		out := "installed=1\npermitrootlogin=yes\npasswordauthentication=yes\npubkeyauthentication=yes\nsocket=1\nkeys.root=1\n"
		for _, p := range strings.Fields(f.ports) {
			out += "port=" + p + "\n"
		}
		return host.Result{Stdout: out}, nil
	case strings.Contains(cmd, "ssh-keygen -lf"):
		return host.Result{Stdout: "root|256 SHA256:abc me@laptop (ED25519)\n"}, nil
	case strings.Contains(cmd, `base64 -d > "$D.new"`):
		conf, _ := base64.StdEncoding.DecodeString(envv("CONF_B64"))
		var ps []string
		for _, m := range portLine.FindAllStringSubmatch(string(conf), -1) {
			ps = append(ps, m[1])
		}
		f.ports = strings.Join(ps, " ")
		out := ""
		for _, p := range ps {
			out += "0.0.0.0:" + p + "\n"
		}
		return host.Result{Stdout: out}, nil
	case strings.Contains(cmd, "ufw status verbose"):
		st := "inactive"
		if f.ufwOn {
			st = "active"
		}
		return host.Result{Stdout: "Status: " + st + "\nDefault: deny (incoming), allow (outgoing), disabled (routed)\n---\n"}, nil
	case argv[0] == "ufw":
		return host.Result{}, nil
	case strings.Contains(cmd, "ufw --force enable"):
		f.ufwOn = true
		return host.Result{}, nil
	case argv[0] == "systemd-run":
		f.timer = true
		return host.Result{}, nil
	case strings.Contains(cmd, `printf '%s' "$REVERT"`):
		return host.Result{}, nil
	case strings.Contains(cmd, "--state=active '"+host.RevertUnit):
		if f.timer {
			return host.Result{Stdout: "1\n"}, nil
		}
		return host.Result{Stdout: "0\n"}, nil
	case strings.Contains(cmd, "/bin/sh /var/lib/docklet-host/revert.sh; fi"):
		f.timer = false
		f.reverts++
		f.ports, f.ufwOn = "22", false
		return host.Result{}, nil
	case strings.Contains(cmd, `systemctl stop "$t"`):
		f.timer = false
		return host.Result{}, nil
	case strings.Contains(cmd, "ss -H -tulpn"):
		return host.Result{Stdout: `tcp LISTEN 0 4096 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=812,fd=3))` + "\n"}, nil
	}
	return host.Result{Exit: 1, Stderr: "unexpected command: " + cmd}, nil
}

func TestSecurityAPI(t *testing.T) {
	fh := &fakeSecHost{ports: "22"}
	env := startPanelServer(t, filepath.Join(t.TempDir(), "d.db"), true, WithDocker(dockertest.New()), WithHostRunner(fh))
	c := env.client(t)

	_, o := c.do("GET", "/api/server/ssh", nil)
	if ssh := o["ssh"].(map[string]any); ssh["ports"].([]any)[0] != float64(22) || len(o["keys"].([]any)) != 1 {
		t.Fatalf("ssh = %v", o)
	}
	// Re-authentication.
	if code, _ := c.do("PUT", "/api/server/ssh", map[string]any{"port": 2222, "permitRootLogin": "yes", "passwordAuthentication": true, "password": "nope"}); code < 400 {
		t.Fatalf("without password: %d", code)
	}
	if code, _ := c.do("PUT", "/api/server/ssh", map[string]any{"port": 80, "permitRootLogin": "yes", "passwordAuthentication": true, "password": adminPW}); code != 400 {
		t.Fatalf("ssh on port 80: %d", code)
	}
	code, o := c.do("PUT", "/api/server/ssh", map[string]any{"port": 2222, "permitRootLogin": "yes", "passwordAuthentication": true, "password": adminPW})
	if code != 200 || o["pending"] == nil || fh.ports != "22 2222" || !fh.timer {
		t.Fatalf("change: %d %v ports=%q timer=%v", code, o, fh.ports, fh.timer)
	}
	// One pending change at a time.
	if code, _ := c.do("POST", "/api/server/firewall/enable", map[string]any{"password": adminPW}); code != 409 {
		t.Fatalf("second change while pending: %d", code)
	}
	if code, _ := c.do("POST", "/api/server/security/pending/confirm", nil); code != 200 || fh.ports != "2222" || fh.timer {
		t.Fatalf("confirm: %d ports=%q timer=%v", code, fh.ports, fh.timer)
	}
	if _, o := c.do("GET", "/api/server/security/pending", nil); o["pending"] != nil {
		t.Fatalf("pending after confirm: %v", o)
	}
	if code, _ := c.do("POST", "/api/server/security/pending/confirm", nil); code != 409 {
		t.Fatalf("confirm twice: %d", code)
	}

	// Firewall enable, then undo.
	if code, _ := c.do("POST", "/api/server/firewall/enable", map[string]any{"password": adminPW}); code != 200 || !fh.ufwOn {
		t.Fatalf("enable: %d", code)
	}
	if code, _ := c.do("POST", "/api/server/security/pending/undo", nil); code != 200 || fh.ufwOn || fh.reverts != 1 {
		t.Fatalf("undo: %d on=%v", code, fh.ufwOn)
	}
	// The host reverted on its own: confirming must fail.
	if code, _ := c.do("POST", "/api/server/firewall/enable", map[string]any{"password": adminPW}); code != 200 {
		t.Fatalf("enable again: %d", code)
	}
	fh.mu.Lock()
	fh.timer, fh.ufwOn = false, false
	fh.mu.Unlock()
	if code, _ := c.do("POST", "/api/server/security/pending/confirm", nil); code != 409 {
		t.Fatalf("confirm after host revert: %d", code)
	}
	if code, _ := c.do("POST", "/api/server/firewall/rules", map[string]any{"action": "deny", "port": "22", "proto": "tcp"}); code != 400 {
		t.Fatalf("deny on the SSH port: %d", code)
	}
	if code, _ := c.do("POST", "/api/server/firewall/rules", map[string]any{"action": "allow", "port": "8080", "from": "nope"}); code != 400 {
		t.Fatalf("bad source: %d", code)
	}

	acts := strings.Join(actionsOf(t, env.st), ",")
	for _, want := range []string{"server.ssh_changed", "server.change_confirmed", "server.firewall_changed", "server.change_undone"} {
		if !strings.Contains(acts, want) {
			t.Errorf("audit missing %s", want)
		}
	}
}
