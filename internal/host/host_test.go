package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monirujjaman-dev/docklet/internal/docker"
)

func TestHostnameScriptUpdatesHosts(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o750)
	// Fake hostnamectl/hostname that record the new name.
	_ = os.WriteFile(filepath.Join(bin, "hostnamectl"), []byte("#!/bin/sh\necho \"$2\" > "+dir+"/name\n"), 0o755) //nolint:gosec // test
	_ = os.WriteFile(filepath.Join(bin, "hostname"), []byte("#!/bin/sh\ncat "+dir+"/name\n"), 0o755)              //nolint:gosec // test
	hosts := filepath.Join(dir, "hosts")
	for name, before := range map[string]string{
		"existing line": "127.0.0.1 localhost\n127.0.1.1\told-name old\n::1 localhost\n",
		"no line":       "127.0.0.1 localhost\n",
	} {
		_ = os.WriteFile(hosts, []byte(before), 0o600)
		out, err := Script(context.Background(), Direct{}, hostnameScript,
			"NEW_HOSTNAME=web-1.example.com", "HOSTS_FILE="+hosts, "PATH="+bin+":"+os.Getenv("PATH"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, _ := os.ReadFile(hosts) //nolint:gosec // test
		if strings.TrimSpace(out) != "web-1.example.com" || !strings.Contains(string(got), "127.0.1.1\tweb-1.example.com web-1\n") ||
			strings.Count(string(got), "127.0.1.1") != 1 || !strings.Contains(string(got), "127.0.0.1 localhost") {
			t.Fatalf("%s: out=%q hosts=%q", name, out, got)
		}
	}
}

func TestValidation(t *testing.T) {
	for h, ok := range map[string]bool{"web-1": true, "web-1.example.com": true, "-web": false, "Web": false, "a_b": false, "a..b": false, strings.Repeat("a", 64): false, "": false} {
		if ValidHostname(h) != ok {
			t.Errorf("ValidHostname(%q) != %v", h, ok)
		}
	}
	for u, ok := range map[string]bool{"nginx.service": true, "getty@tty1.service": true, "nginx": false, "a;b.service": false, "x y.service": false, "../x.service": false} {
		if ValidUnit(u) != ok {
			t.Errorf("ValidUnit(%q) != %v", u, ok)
		}
	}
	for _, c := range []struct {
		unit, action string
		ok           bool
	}{
		{"nginx.service", "stop", true}, {"docker.service", "restart", false}, {"containerd.service", "stop", false},
		{"systemd-journald.service", "restart", false}, {"ssh.service", "stop", false}, {"ssh.service", "restart", true},
		{"docklet.service", "stop", false}, {"dbus.service", "stop", false},
	} {
		if (unitGuard(c.unit, c.action) == nil) != c.ok {
			t.Errorf("unitGuard(%s, %s) ok=%v", c.unit, c.action, !c.ok)
		}
	}
}

func TestParseSS(t *testing.T) {
	out := `tcp   LISTEN 0      4096        0.0.0.0:22        0.0.0.0:*    users:(("sshd",pid=812,fd=3))
tcp   LISTEN 0      4096           [::]:22           [::]:*    users:(("sshd",pid=812,fd=4))
tcp   LISTEN 0      4096        0.0.0.0:443       0.0.0.0:*    users:(("docker-proxy",pid=2201,fd=4))
tcp   ESTAB  0      0         10.0.0.5:22      10.0.0.9:5000  users:(("sshd",pid=900,fd=4))
udp   UNCONN 0      0     127.0.0.53%lo:53        0.0.0.0:*    users:(("systemd-resolve",pid=500,fd=13))
tcp   LISTEN 0      511       127.0.0.1:3000      0.0.0.0:*`
	p := ParseSS(out)
	if len(p) != 5 || p[0].Port != 22 || p[0].Process != "sshd" || p[0].PID != 812 || p[2].Address != "127.0.0.53" || p[2].Proto != "udp" ||
		p[3].Port != 443 || p[4].Port != 3000 || p[4].Process != "" {
		t.Fatalf("parsed = %+v", p)
	}
}

func TestProcNetFallback(t *testing.T) {
	out := `NO_SS
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1
   1: 0500000A:0016 0900000A:1388 01 00000000:00000000 00:00000000 00000000     0        0 2 1
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 1`
	p := parseProcNet(out)
	if len(p) != 2 || p[0].Port != 22 || p[0].Address != "::" || p[1].Port != 3000 || p[1].Address != "127.0.0.1" {
		t.Fatalf("parsed = %+v", p)
	}
}

type fakeCleanupDocker struct{ du docker.DiskUsage }

func (f fakeCleanupDocker) DiskUsage(context.Context) (docker.DiskUsage, error) { return f.du, nil }
func (fakeCleanupDocker) ImageRemove(context.Context, string) error             { return nil }
func (fakeCleanupDocker) BuildPrune(context.Context) (int64, error)             { return 0, nil }
func (fakeCleanupDocker) ContainerRemove(context.Context, string) error         { return nil }
func (fakeCleanupDocker) VolumeRemove(context.Context, string) error            { return nil }

func TestCleanupKeepsProtectedImages(t *testing.T) {
	var du docker.DiskUsage
	add := func(id string, tags []string, containers int64) {
		du.Images = append(du.Images, struct {
			ID         string   `json:"Id"`
			RepoTags   []string `json:"RepoTags"`
			Size       int64    `json:"Size"`
			SharedSize int64    `json:"SharedSize"`
			Containers int64    `json:"Containers"`
			Created    int64    `json:"Created"`
		}{ID: id, RepoTags: tags, Size: 100, SharedSize: 40, Containers: containers})
	}
	add("sha256:unused", []string{"old/app:1"}, 0)
	add("sha256:inuse", []string{"nginx:alpine"}, 1)
	add("sha256:rollback", []string{"docklet/app-3:5"}, 0)
	add("sha256:self", []string{"ghcr.io/monirujjaman-dev/docklet:1.0.0"}, 0)
	add("sha256:caddy", []string{"caddy:2.10-alpine"}, 0)
	cats, err := (&Service{R: Direct{}}).CleanupPreview(context.Background(), fakeCleanupDocker{du}, map[string]bool{"sha256:rollback": true})
	if err != nil {
		t.Fatal(err)
	}
	img := cats[0]
	if img.Key != "images" || len(img.Items) != 1 || img.Items[0].ID != "sha256:unused" || img.Size != 60 {
		t.Fatalf("images = %+v", img)
	}
}
