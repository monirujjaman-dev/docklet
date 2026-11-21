package host

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker"
)

// TestRealSecurity runs SSH, firewall and Fail2Ban changes against a systemd
// container with openssh-server, ufw and fail2ban installed, and logs in to
// it over SSH from a second container:
//
//	docker run -d --name sechost --hostname sechost --privileged --cgroupns=host \
//	  -v /sys/fs/cgroup:/sys/fs/cgroup:rw docklet-test/ubuntu-ssh:24.04
//	docker run -d --name sshclient --entrypoint sleep docklet-test/ubuntu-ssh:24.04 infinity
//	DOCKLET_TEST_SECHOST=sechost DOCKLET_TEST_SSHCLIENT=sshclient go test ./internal/host/ -run TestRealSecurity -v
//
// (docklet-test/ubuntu-ssh is jrei/systemd-ubuntu:24.04 plus openssh-server,
// ufw, fail2ban, sshpass, a root password and a sudo user "deploy"; see
// docs/server.md.)
func TestRealSecurity(t *testing.T) {
	name, client := os.Getenv("DOCKLET_TEST_SECHOST"), os.Getenv("DOCKLET_TEST_SSHCLIENT")
	if name == "" || client == "" {
		t.Skip("set DOCKLET_TEST_SECHOST and DOCKLET_TEST_SSHCLIENT (see the comment)")
	}
	ctx := context.Background()
	dk := docker.New("/var/run/docker.sock")
	info, err := dk.ContainerInspect(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	ip := ""
	for _, n := range info.NetworkSettings.Networks {
		ip = n.IPAddress
	}
	s := &Service{R: &Helper{DK: dk, Image: "alpine:3.20", TargetPID: info.State.Pid}}
	run := func(c string, args ...string) (string, error) {
		out, err := exec.Command("docker", append([]string{"exec", c}, args...)...).CombinedOutput() //nolint:gosec // test
		return strings.TrimSpace(string(out)), err
	}
	sshOpts := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=4", "-o", "LogLevel=ERROR"}
	pwLogin := func(user, pass string, port string) error {
		args := append([]string{"sshpass", "-p", pass, "ssh", "-o", "PreferredAuthentications=password,keyboard-interactive", "-o", "PubkeyAuthentication=no"}, sshOpts...)
		_, err := run(client, append(args, "-p", port, user+"@"+ip, "true")...)
		return err
	}
	keyLogin := func(user, port string) error {
		args := append([]string{"ssh", "-i", "/root/.ssh/id_ed25519", "-o", "BatchMode=yes"}, sshOpts...)
		_, err := run(client, append(args, "-p", port, user+"@"+ip, "true")...)
		return err
	}
	want := func(desc string, err error, ok bool) {
		t.Helper()
		if (err == nil) != ok {
			t.Fatalf("%s: err=%v, want success=%v", desc, err, ok)
		}
	}
	protected := func() []ProtectedPort {
		st, err := s.SSHStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return Protected(st.Ports, 80, 443, 3000)
	}

	// --- SSH status and lockout guard -------------------------------------------------
	st, err := s.SSHStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ssh: %+v", st)
	if !st.Installed || !slices.Equal(st.Ports, []int{22}) || !st.SocketActivated {
		t.Fatalf("status = %+v", st)
	}
	want("root password login before", pwLogin("root", "rootpass-123", "22"), st.PermitRootLogin == "yes")
	want("deploy password login", pwLogin("deploy", "deploypass-123", "22"), true)

	if _, err := s.ChangeSSH(ctx, SSHSettings{Port: 22, PermitRootLogin: "prohibit-password", PasswordAuth: false}, nil, time.Minute); err == nil ||
		!strings.Contains(err.Error(), "add an SSH key first") {
		t.Fatalf("password off without keys: %v", err)
	}
	_, _ = run(client, "sh", "-c", "rm -f /root/.ssh/id_ed25519*; ssh-keygen -q -t ed25519 -N '' -C test@client -f /root/.ssh/id_ed25519")
	pub, err := run(client, "cat", "/root/.ssh/id_ed25519.pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddAuthorizedKey(ctx, "root", "ssh-ed25519 notakey"); err == nil {
		t.Fatal("bad key accepted")
	}
	if err := s.AddAuthorizedKey(ctx, "nobody", pub); err == nil {
		t.Fatal("key for a non-sudo user accepted")
	}
	if err := s.AddAuthorizedKey(ctx, "root", pub); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAuthorizedKey(ctx, "root", pub); err != nil { // idempotent
		t.Fatal(err)
	}
	keys, err := s.AuthorizedKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].User != "root" || keys[0].Comment != "test@client" || keys[0].Type != "ED25519" {
		t.Fatalf("keys = %+v %v", keys, err)
	}
	want("root key login", keyLogin("root", "22"), true)

	// --- Firewall -------------------------------------------------------------------------
	fw, err := s.FirewallStatus(ctx)
	if err != nil || !fw.Installed || fw.Active {
		t.Fatalf("firewall = %+v %v", fw, err)
	}
	p, err := s.EnableFirewall(ctx, protected(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if pend, _ := s.RevertPending(ctx); !pend {
		t.Fatal("no revert scheduled after enabling the firewall")
	}
	s.dropRevert(ctx) // confirm
	_ = p
	fw, _ = s.FirewallStatus(ctx)
	t.Logf("firewall on: in=%s out=%s, %d rules", fw.DefaultIncoming, fw.DefaultOutgoing, len(fw.Rules))
	if !fw.Active || fw.DefaultIncoming != "deny" || !fw.allows(22) || !fw.allows(80) || !fw.allows(443) || !fw.allowsProto(443, "udp") || !fw.allows(3000) {
		t.Fatalf("firewall = %+v", fw)
	}
	want("key login with firewall on", keyLogin("root", "22"), true)
	if err := s.AddRule(ctx, RuleInput{Action: "deny", Port: "20:30", Proto: "tcp"}, protected()); err == nil {
		t.Fatal("deny covering SSH accepted")
	}
	if err := s.AddRule(ctx, RuleInput{Action: "allow", Port: "8080", Proto: "tcp", From: "10.1.2.0/24", Comment: "office"}, protected()); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRule(ctx, RuleInput{Action: "deny", Port: "9000", From: "198.51.100.7"}, protected()); err != nil {
		t.Fatal(err)
	}
	fw, _ = s.FirewallStatus(ctx)
	var office, ssh FWRule
	for _, r := range fw.Rules {
		if r.Comment == "office" {
			office = r
		}
		if r.To == "22/tcp" && !r.V6 {
			ssh = r
		}
	}
	if office.From != "10.1.2.0/24" || fw.Rules[0].Action != "DENY IN" {
		t.Fatalf("rules = %+v", fw.Rules)
	}
	if _, err := s.DeleteRule(ctx, ssh.Num, ssh.Text, protected()); err == nil || !strings.Contains(err.Error(), "SSH") {
		t.Fatalf("deleting the SSH rule: %v", err)
	}
	if _, err := s.DeleteRule(ctx, office.Num, "stale text", protected()); err == nil {
		t.Fatal("stale delete accepted")
	}
	if _, err := s.DeleteRule(ctx, office.Num, office.Text, protected()); err != nil {
		t.Fatal(err)
	}

	// --- SSH port change: not confirmed → host reverts ------------------------------------
	if _, err := s.ChangeSSH(ctx, SSHSettings{Port: 2222, PermitRootLogin: st.PermitRootLogin, PasswordAuth: true}, []int{3000}, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	st2, _ := s.SSHStatus(ctx)
	fw, _ = s.FirewallStatus(ctx)
	if !slices.Equal(st2.Ports, []int{22, 2222}) || !fw.allows(2222) {
		t.Fatalf("transitional ports = %v, firewall allows 2222: %v", st2.Ports, fw.allows(2222))
	}
	want("key login on the new port", keyLogin("root", "2222"), true)
	want("key login on the old port (still open)", keyLogin("root", "22"), true)
	t.Log("waiting for the automatic revert…")
	time.Sleep(25 * time.Second)
	st2, _ = s.SSHStatus(ctx)
	fw, _ = s.FirewallStatus(ctx)
	if !slices.Equal(st2.Ports, []int{22}) || fw.allows(2222) {
		t.Fatalf("after revert ports = %v, firewall allows 2222: %v", st2.Ports, fw.allows(2222))
	}
	want("new port closed after revert", keyLogin("root", "2222"), false)
	want("old port works after revert", keyLogin("root", "22"), true)

	// --- Port change confirmed ------------------------------------------------------------
	if _, err := s.ChangeSSH(ctx, SSHSettings{Port: 3000, PermitRootLogin: "yes", PasswordAuth: true}, []int{3000}, time.Minute); err == nil {
		t.Fatal("panel port accepted for SSH")
	}
	pend, err := s.ChangeSSH(ctx, SSHSettings{Port: 2222, PermitRootLogin: "yes", PasswordAuth: true}, []int{3000}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmSSH(ctx, pend); err != nil {
		t.Fatal(err)
	}
	st2, _ = s.SSHStatus(ctx)
	fw, _ = s.FirewallStatus(ctx)
	if !slices.Equal(st2.Ports, []int{2222}) || fw.allows(22) || !fw.allows(2222) {
		t.Fatalf("confirmed ports = %v, firewall allows 22: %v", st2.Ports, fw.allows(22))
	}
	if pending, _ := s.RevertPending(ctx); pending {
		t.Fatal("revert still scheduled after confirming")
	}
	want("old port closed", keyLogin("root", "22"), false)
	want("root password on 2222", pwLogin("root", "rootpass-123", "2222"), true)

	// --- Root password login off (key still works) ---------------------------------------
	pend, err = s.ChangeSSH(ctx, SSHSettings{Port: 2222, PermitRootLogin: "prohibit-password", PasswordAuth: true}, []int{3000}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmSSH(ctx, pend); err != nil {
		t.Fatal(err)
	}
	want("root password rejected", pwLogin("root", "rootpass-123", "2222"), false)
	want("root key login", keyLogin("root", "2222"), true)
	want("deploy password login", pwLogin("deploy", "deploypass-123", "2222"), true)

	// Password login off for everyone.
	pend, err = s.ChangeSSH(ctx, SSHSettings{Port: 2222, PermitRootLogin: "prohibit-password", PasswordAuth: false}, []int{3000}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmSSH(ctx, pend); err != nil {
		t.Fatal(err)
	}
	want("deploy password rejected", pwLogin("deploy", "deploypass-123", "2222"), false)
	want("root key login", keyLogin("root", "2222"), true)
	if err := s.RemoveAuthorizedKey(ctx, "root", keys[0].Fingerprint); err == nil || !strings.Contains(err.Error(), "last SSH key") {
		t.Fatalf("removing the last key: %v", err)
	}

	// --- Fail2Ban -------------------------------------------------------------------------
	pend, err = s.ChangeSSH(ctx, SSHSettings{Port: 2222, PermitRootLogin: "prohibit-password", PasswordAuth: true}, []int{3000}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmSSH(ctx, pend); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureFail2Ban(ctx, Fail2BanSettings{Enabled: true, MaxRetry: 3, FindTime: 600, BanTime: 600, IgnoreIPs: []string{"bad"}}, []int{2222}); err == nil {
		t.Fatal("bad ignoreip accepted")
	}
	if err := s.ConfigureFail2Ban(ctx, Fail2BanSettings{Enabled: true, MaxRetry: 3, FindTime: 600, BanTime: 600, IgnoreIPs: []string{"192.0.2.0/24"}}, []int{2222}); err != nil {
		t.Fatal(err)
	}
	f2b, err := s.Fail2BanStatus(ctx)
	if err != nil || !f2b.Running || !f2b.Settings.Configured || f2b.Settings.MaxRetry != 3 || len(f2b.Jails) != 1 || !slices.Equal(f2b.Settings.IgnoreIPs, []string{"192.0.2.0/24"}) {
		t.Fatalf("fail2ban = %+v %v", f2b, err)
	}
	clientIP := ""
	if ci, err := dk.ContainerInspect(ctx, client); err == nil {
		for _, n := range ci.NetworkSettings.Networks {
			clientIP = n.IPAddress
		}
	}
	for i := 0; i < 5; i++ {
		_ = pwLogin("deploy", "wrong-password", "2222")
	}
	var banned []string
	for i := 0; i < 20 && !slices.Contains(banned, clientIP); i++ {
		time.Sleep(time.Second)
		f2b, _ = s.Fail2BanStatus(ctx)
		if len(f2b.Jails) > 0 {
			banned = f2b.Jails[0].Banned
		}
	}
	t.Logf("fail2ban jails: %+v", f2b.Jails)
	if !slices.Contains(banned, clientIP) {
		t.Fatalf("client %s not banned: %+v", clientIP, f2b.Jails)
	}
	want("banned client can't log in", keyLogin("root", "2222"), false)
	if err := s.Unban(ctx, "sshd", "not-an-ip"); err == nil {
		t.Fatal("bad unban accepted")
	}
	if err := s.Unban(ctx, "sshd", clientIP); err != nil {
		t.Fatal(err)
	}
	want("unbanned client logs in", keyLogin("root", "2222"), true)

	// --- Clean up: back to port 22, firewall off ------------------------------------------
	pend, err = s.ChangeSSH(ctx, SSHSettings{Port: 22, PermitRootLogin: "yes", PasswordAuth: true}, []int{3000}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmSSH(ctx, pend); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableFirewall(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAuthorizedKey(ctx, "root", keys[0].Fingerprint); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.AuthorizedKeys(ctx); len(k) != 0 {
		t.Fatalf("keys after remove = %+v", k)
	}
}

// TestRealAllowlist limits a published container port to one source on the
// machine running the tests (it must be the Docker host, as root). Two network
// namespaces stand in for outside clients:
//
//	DOCKLET_TEST_ALLOWLIST=1 DOCKLET_HOST_PID=$(pgrep -x dockerd) go test ./internal/host/ -run TestRealAllowlist -v
func TestRealAllowlist(t *testing.T) {
	if os.Getenv("DOCKLET_TEST_ALLOWLIST") == "" {
		t.Skip("set DOCKLET_TEST_ALLOWLIST=1 and DOCKLET_HOST_PID (see the comment)")
	}
	ctx := context.Background()
	dk := docker.New("/var/run/docker.sock")
	pid := 1
	if v := os.Getenv("DOCKLET_HOST_PID"); v != "" {
		pid = int(atoi64(v))
	}
	s := &Service{R: &Helper{DK: dk, Image: "alpine:3.20", TargetPID: pid}}
	sh := func(c string) (string, error) {
		out, err := exec.Command("sh", "-c", c).CombinedOutput() //nolint:gosec // test
		return strings.TrimSpace(string(out)), err
	}
	_, _ = sh("docker rm -f allow-target; ip netns del ext1; ip netns del ext2")
	if out, err := sh(`docker run -d --name allow-target -p 15432:80 nginx:alpine`); err != nil {
		t.Fatal(out)
	}
	t.Cleanup(func() {
		_, _ = sh("docker rm -f allow-target; ip netns del ext1; ip netns del ext2")
		_ = s.ApplyPortAllowlist(ctx, nil)
	})
	for i, n := range []string{"ext1", "ext2"} {
		c := strings.NewReplacer("N", n, "I", string(rune('1'+i))).Replace(`set -e
ip netns add N; ip link add vN type veth peer name eth0 netns N
ip addr add 10.9I.0.1/24 dev vN; ip link set vN up
ip netns exec N ip addr add 10.9I.0.2/24 dev eth0; ip netns exec N ip link set eth0 up; ip netns exec N ip link set lo up
ip netns exec N ip route add default via 10.9I.0.1`)
		if out, err := sh(c); err != nil {
			t.Fatal(out)
		}
	}
	reach := func(ns string) bool {
		_, err := sh("ip netns exec " + ns + " timeout 4 sh -c 'exec 3<>/dev/tcp/10.91.0.1/15432' 2>/dev/null || ip netns exec " + ns + " timeout 4 bash -c 'exec 3<>/dev/tcp/10.91.0.1/15432'")
		return err == nil
	}
	time.Sleep(time.Second)
	if !reach("ext1") || !reach("ext2") {
		t.Fatal("published port not reachable before the allowlist")
	}
	if err := s.ApplyPortAllowlist(ctx, []PortAllow{{Port: 15432, Sources: []string{"10.91.0.2"}}}); err != nil {
		t.Fatal(err)
	}
	if !reach("ext1") || reach("ext2") {
		t.Fatalf("with allowlist: ext1=%v ext2=%v, want true false", reach("ext1"), reach("ext2"))
	}
	// Re-applying is idempotent and keeps one jump.
	if err := s.ApplyPortAllowlist(ctx, []PortAllow{{Port: 15432, Sources: []string{"10.91.0.2"}}}); err != nil {
		t.Fatal(err)
	}
	if out, _ := sh("iptables -S DOCKER-USER | grep -c DOCKLET-USER"); out != "1" {
		t.Fatalf("jumps = %s", out)
	}
	// Containers still reach each other on the container port.
	if out, err := sh(`docker run --rm alpine:3.20 wget -q -T 4 -O /dev/null http://$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' allow-target)/ && echo ok`); err != nil || out != "ok" {
		t.Fatalf("container to container: %s %v", out, err)
	}
	if err := s.ApplyPortAllowlist(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !reach("ext2") {
		t.Fatal("still blocked after clearing the allowlist")
	}
}
