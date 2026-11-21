package host

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker"
)

// TestRealHost runs the Server features against a systemd container that
// stands in for the host:
//
//	docker run -d --name fakehost --hostname vps-old --privileged --cgroupns=host \
//	  -v /sys/fs/cgroup:/sys/fs/cgroup:rw --restart unless-stopped jrei/systemd-ubuntu:24.04
//	DOCKLET_TEST_FAKEHOST=fakehost go test ./internal/host/ -run TestRealHost -v
func TestRealHost(t *testing.T) {
	name := os.Getenv("DOCKLET_TEST_FAKEHOST")
	if name == "" {
		t.Skip("set DOCKLET_TEST_FAKEHOST to a systemd container (see the comment)")
	}
	ctx := context.Background()
	dk := docker.New("/var/run/docker.sock")
	pidOf := func() int {
		info, err := dk.ContainerInspect(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return info.State.Pid
	}
	h := &Helper{DK: dk, Image: "alpine:3.20", TargetPID: pidOf()}
	s := &Service{R: h}
	sh := func(cmd string) string {
		out, _ := exec.Command("docker", "exec", name, "sh", "-c", cmd).CombinedOutput() //nolint:gosec // test
		return strings.TrimSpace(string(out))
	}

	start := time.Now()
	o, err := s.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("overview in %s: %s, %s, kernel %s, tz %s, %d CPUs, mem %d MB, disk %d/%d GB",
		time.Since(start).Round(time.Millisecond), o.OS.PrettyName, o.Hostname, o.Kernel, o.Timezone, o.CPUs, o.MemTotal>>20, o.DiskUsed>>30, o.DiskTotal>>30)
	if !o.OS.Supported || o.OS.ID != "ubuntu" || o.Hostname == "" || o.MemTotal == 0 || o.DiskTotal == 0 {
		t.Fatalf("overview = %+v", o)
	}

	// Time zone.
	zones, err := s.Timezones(ctx)
	if err != nil || len(zones) < 300 {
		t.Fatalf("timezones: %d %v", len(zones), err)
	}
	if err := s.SetTimezone(ctx, "Mars/Olympus"); err == nil {
		t.Fatal("unknown zone accepted")
	}
	if err := s.SetTimezone(ctx, "Asia/Dhaka"); err != nil {
		t.Fatal(err)
	}
	if got := sh("readlink /etc/localtime"); !strings.HasSuffix(got, "Asia/Dhaka") {
		t.Fatalf("localtime -> %s", got)
	}

	// Hostname (inside Docker /etc/hostname is a bind mount; on a VM it's a file).
	if err := s.SetHostname(ctx, "Bad_Name"); err == nil {
		t.Fatal("bad hostname accepted")
	}
	herr := s.SetHostname(ctx, "web-1.example.com")
	t.Logf("set hostname: %v; now %q; hosts: %q", herr, sh("hostname"), sh("grep 127.0.1.1 /etc/hosts"))

	// Swappiness (a kernel-wide setting: restore it afterwards).
	prev := sh("cat /proc/sys/vm/swappiness")
	if err := s.SetSwappiness(ctx, 17); err != nil {
		t.Fatal(err)
	}
	if sh("cat /proc/sys/vm/swappiness") != "17" || !strings.Contains(sh("cat /etc/sysctl.d/99-docklet.conf"), "vm.swappiness = 17") {
		t.Fatal("swappiness not applied/persisted")
	}
	if n, _ := strconv.Atoi(prev); n > 0 {
		_ = s.SetSwappiness(ctx, n)
	}
	// Swap on the container's overlay filesystem fails; the error must be clear.
	if err := s.SetSwap(ctx, 300); err != nil {
		t.Logf("swap in a container (expected to fail on overlayfs): %v", err)
	} else {
		t.Logf("swap created: %s", sh("cat /proc/swaps"))
		_ = s.SetSwap(ctx, 0)
	}

	// A demo service that listens on 8099.
	sh(`cat > /etc/systemd/system/demo.service <<'EOF'
[Unit]
Description=Docklet demo listener
[Service]
ExecStart=/usr/bin/perl -MIO::Socket::INET -e '$$s=IO::Socket::INET->new(LocalPort=>8099,Listen=>5,ReuseAddr=>1) or die; sleep 1000 while 1'
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload`)
	if err := s.ServiceAction(ctx, "demo.service", "enable"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	units, err := s.Services(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var demo Unit
	for _, u := range units {
		if u.Name == "demo.service" {
			demo = u
		}
	}
	if demo.Active != "active" || demo.Enabled != "enabled" {
		t.Fatalf("demo unit = %+v (of %d units)", demo, len(units))
	}
	if err := s.ServiceAction(ctx, "docker.service", "stop"); err == nil {
		t.Fatal("stopping docker allowed")
	}
	if err := s.ServiceAction(ctx, "demo.service; reboot", "stop"); err == nil {
		t.Fatal("bad unit name accepted")
	}

	ports, err := s.ListeningPorts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range ports {
		if p.Port == 8099 && p.Proto == "tcp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("8099 not in listening ports: %+v", ports)
	}

	procs, err := s.Processes(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	var perl Process
	for _, p := range procs {
		if p.Comm == "perl" {
			perl = p
		}
		if p.PID == 1 && !p.Protect {
			t.Fatal("pid 1 not protected")
		}
	}
	if perl.PID == 0 {
		t.Fatalf("perl not in process list (%d processes)", len(procs))
	}
	if _, err := s.Signal(ctx, 1, "KILL"); err == nil {
		t.Fatal("killing pid 1 allowed")
	}
	if _, err := s.Signal(ctx, perl.PID, "TERM"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	logs, err := s.ServiceLogs(ctx, "demo.service", 50)
	if err != nil || !strings.Contains(logs, "demo") {
		t.Fatalf("logs: %v\n%s", err, logs)
	}
	if err := s.ServiceAction(ctx, "demo.service", "disable"); err != nil {
		t.Fatal(err)
	}

	// Reboot: the container exits and its restart policy brings it back.
	before := sh("cat /proc/1/stat | cut -d' ' -f22")
	if err := s.Reboot(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if st := sh("systemctl is-system-running 2>/dev/null; cat /proc/1/stat | cut -d' ' -f22"); strings.Contains(st, "running") && !strings.HasSuffix(st, before) {
			break
		}
	}
	h.TargetPID = pidOf()
	o2, err := s.Overview(ctx)
	if err != nil {
		t.Fatalf("after reboot: %v", err)
	}
	t.Logf("after reboot: uptime %ds, tz %s, hostname %s", o2.UptimeSec, o2.Timezone, o2.Hostname)
	if o2.Timezone != "Asia/Dhaka" {
		t.Fatalf("time zone not kept across reboot: %s", o2.Timezone)
	}
	if v := sh("cat /etc/sysctl.d/99-docklet.conf"); !strings.Contains(v, "vm.swappiness") {
		t.Fatal("swappiness file lost")
	}
}

// TestRealCleanup checks the protections against the real Docker daemon
// without removing anything it didn't create.
func TestRealCleanup(t *testing.T) {
	if os.Getenv("DOCKLET_TEST_DOCKER") == "" {
		t.Skip("set DOCKLET_TEST_DOCKER=1")
	}
	ctx := context.Background()
	dk := docker.New("/var/run/docker.sock")
	run := func(args ...string) {
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil { //nolint:gosec // test
			t.Fatalf("docker %v: %v %s", args, err, out)
		}
	}
	run("create", "--name", "cleanup-test-ctr", "alpine:3.20", "true")
	run("create", "--name", "docklet-app-keepme", "--label", "docklet.managed=true", "alpine:3.20", "true")
	run("volume", "create", "cleanup-test-vol")
	run("volume", "create", "docklet-db-999")
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "cleanup-test-ctr", "docklet-app-keepme").Run()
		_ = exec.Command("docker", "volume", "rm", "cleanup-test-vol", "docklet-db-999").Run()
	})
	s := &Service{R: Direct{}}
	cats, err := s.CleanupPreview(ctx, dk, nil)
	if err != nil {
		t.Fatal(err)
	}
	has := func(key, name string) bool {
		for _, c := range cats {
			if c.Key == key {
				for _, it := range c.Items {
					if it.Name == name {
						return true
					}
				}
			}
		}
		return false
	}
	if !has("containers", "cleanup-test-ctr") || has("containers", "docklet-app-keepme") {
		t.Fatal("container selection wrong")
	}
	if !has("volumes", "cleanup-test-vol") || has("volumes", "docklet-db-999") {
		t.Fatal("volume selection wrong")
	}
	for _, c := range cats {
		t.Logf("%s: %d items, %d MB", c.Key, len(c.Items), c.Size>>20)
	}
	// Remove only our container and the one volume we pick.
	res, err := (&Service{R: Direct{}}).cleanupOnly(ctx, dk, "cleanup-test-ctr", "cleanup-test-vol")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cleanup: %+v", res)
	if out, _ := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}").Output(); strings.Contains(string(out), "cleanup-test-ctr") || !strings.Contains(string(out), "docklet-app-keepme") {
		t.Fatalf("containers after cleanup: %s", out)
	}
	if out, _ := exec.Command("docker", "volume", "ls", "-q").Output(); strings.Contains(string(out), "cleanup-test-vol") || !strings.Contains(string(out), "docklet-db-999") {
		t.Fatalf("volumes after cleanup: %s", out)
	}
}

// cleanupOnly runs Cleanup restricted to one container and one volume, so the
// test never removes unrelated things on the developer's machine.
func (s *Service) cleanupOnly(ctx context.Context, dk CleanupDocker, ctr, vol string) ([]CleanupResult, error) {
	return s.Cleanup(ctx, onlyDocker{CleanupDocker: dk, ctr: ctr}, nil, []string{"containers", "volumes"}, []string{vol})
}

type onlyDocker struct {
	CleanupDocker
	ctr string
}

func (o onlyDocker) DiskUsage(ctx context.Context) (docker.DiskUsage, error) {
	du, err := o.CleanupDocker.DiskUsage(ctx)
	keep := du.Containers[:0]
	for _, c := range du.Containers {
		if strings.TrimPrefix(strings.Join(c.Names, ""), "/") == o.ctr {
			keep = append(keep, c)
		}
	}
	du.Containers = keep
	return du, err
}

// TestRealSwapScript runs the swap script directly on this machine (ext4,
// root): DOCKLET_TEST_SWAP_DIRECT=1. It adds a 300 MB /swapfile and removes it.
func TestRealSwapScript(t *testing.T) {
	if os.Getenv("DOCKLET_TEST_SWAP_DIRECT") == "" {
		t.Skip("set DOCKLET_TEST_SWAP_DIRECT=1 to create and remove a real /swapfile here")
	}
	ctx := context.Background()
	out, err := Script(ctx, Direct{}, swapScript, "SWAP_MB=300")
	if err != nil {
		t.Fatal(err)
	}
	fstab, _ := os.ReadFile("/etc/fstab")
	if !strings.Contains(out, "/swapfile") || !strings.Contains(string(fstab), "/swapfile none swap sw 0 0") {
		t.Fatalf("swap not active/persisted:\n%s\n%s", out, fstab)
	}
	// Resize.
	if out, err = Script(ctx, Direct{}, swapScript, "SWAP_MB=400"); err != nil || !strings.Contains(out, "409596") {
		t.Fatalf("resize: %v\n%s", err, out)
	}
	if _, err := Script(ctx, Direct{}, swapScript, "SWAP_MB=999999"); err == nil || !strings.Contains(err.Error(), "not enough free disk space") {
		t.Fatalf("oversized swap: %v", err)
	}
	if out, err = Script(ctx, Direct{}, swapScript, "SWAP_MB=0"); err != nil || strings.Contains(out, "/swapfile") {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	fstab, _ = os.ReadFile("/etc/fstab")
	if strings.Contains(string(fstab), "/swapfile") {
		t.Fatal("fstab entry left behind")
	}
	if _, err := os.Stat("/swapfile"); err == nil {
		t.Fatal("swapfile left behind")
	}
}
