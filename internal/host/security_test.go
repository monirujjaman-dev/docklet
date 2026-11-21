package host

import (
	"slices"
	"strings"
	"testing"
)

func TestParseUFW(t *testing.T) {
	out := `Status: active
Default: deny (incoming), allow (outgoing), disabled (routed)
---
[ 1] 9000                       DENY IN     198.51.100.7
[ 2] 22/tcp                     ALLOW IN    Anywhere                   # SSH (Docklet)
[ 3] 8080/tcp                   ALLOW IN    10.1.2.0/24                # office
[ 4] 6000:6100/udp              LIMIT IN    Anywhere
[ 5] OpenSSH                    ALLOW IN    Anywhere
[ 6] 22/tcp (v6)                ALLOW IN    Anywhere (v6)              # SSH (Docklet)
`
	st := ParseUFW(out)
	if !st.Installed || !st.Active || st.DefaultIncoming != "deny" || st.DefaultOutgoing != "allow" || len(st.Rules) != 6 {
		t.Fatalf("status = %+v", st)
	}
	if r := st.Rules[2]; r.To != "8080/tcp" || r.From != "10.1.2.0/24" || r.Comment != "office" || r.Action != "ALLOW IN" {
		t.Fatalf("rule 3 = %+v", r)
	}
	if r := st.Rules[5]; !r.V6 || r.To != "22/tcp" || r.From != "Anywhere" {
		t.Fatalf("rule 6 = %+v", r)
	}
	if !st.allows(22) || st.allows(8080) || st.allows(9000) || !st.allowsProto(6050, "udp") || st.allowsProto(6050, "tcp") {
		t.Fatal("allows")
	}
	if ParseUFW("NOT_INSTALLED\n").Installed {
		t.Fatal("not installed")
	}
}

func TestRuleArgs(t *testing.T) {
	for _, c := range []struct {
		in   RuleInput
		want string
	}{
		{RuleInput{Action: "allow", Port: "8080", Proto: "tcp"}, "ufw allow from any to any port 8080 proto tcp"},
		{RuleInput{Action: "deny", Port: "6000:6100", Proto: "udp", From: "10.0.0.9/8", Comment: "lab"}, "ufw insert 1 deny from 10.0.0.0/8 to any port 6000:6100 proto udp comment lab"},
		{RuleInput{Action: "limit", Port: "22", Proto: "tcp", From: "203.0.113.7"}, "ufw limit from 203.0.113.7 to any port 22 proto tcp"},
	} {
		argv, err := ruleArgs(c.in, c.in.Action == "deny")
		if err != nil || strings.Join(argv, " ") != c.want {
			t.Errorf("%+v: %q %v", c.in, strings.Join(argv, " "), err)
		}
	}
	for _, bad := range []RuleInput{
		{Action: "drop", Port: "80"}, {Action: "allow", Port: "0"}, {Action: "allow", Port: "90:80", Proto: "tcp"},
		{Action: "allow", Port: "6000:6100"}, {Action: "limit", Port: "53", Proto: "udp"},
		{Action: "allow", Port: "80", From: "evil; rm -rf /"}, {Action: "allow", Port: "80", Comment: "a'b"},
	} {
		if _, err := ruleArgs(bad, false); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestParseSourcesAndAllowEnv(t *testing.T) {
	got, err := ParseSources("203.0.113.7, 10.1.2.3/8 203.0.113.7\n192.0.2.1/32")
	if err != nil || !slices.Equal(got, []string{"203.0.113.7", "10.0.0.0/8", "192.0.2.1"}) {
		t.Fatalf("sources = %v %v", got, err)
	}
	for _, bad := range []string{"0.0.0.0/0", "::1", "foo", "1.2.3.4/33"} {
		if _, err := ParseSources(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	env, err := allowRulesEnv([]PortAllow{{Port: 5433, Sources: []string{"10.0.0.1"}}, {Port: 5432, Sources: []string{"1.2.3.4", "10.0.0.0/8"}}, {Port: 6379}})
	if err != nil || env != "5432=1.2.3.4,10.0.0.0/8 5433=10.0.0.1" {
		t.Fatalf("env = %q %v", env, err)
	}
}

func TestParseF2B(t *testing.T) {
	out := `installed=1
running=1
configured=1
cfg.enabled=true
cfg.maxretry=3
cfg.findtime=600
cfg.bantime=-1
cfg.ignoreip=127.0.0.1/8 ::1 192.0.2.0/24
jail.sshd.Status for the jail=sshd
jail.sshd.Filter=
jail.sshd.Currently failed=1
jail.sshd.Total failed=9
jail.sshd.Journal matches=_COMM=sshd + _COMM=sshd-session
jail.sshd.Actions=
jail.sshd.Currently banned=2
jail.sshd.Total banned=4
jail.sshd.Banned IP list=198.51.100.1 198.51.100.2
`
	st := parseF2B(out)
	if !st.Installed || !st.Running || !st.Settings.Enabled || st.Settings.MaxRetry != 3 || st.Settings.BanTime != -1 ||
		!slices.Equal(st.Settings.IgnoreIPs, []string{"192.0.2.0/24"}) || len(st.Jails) != 1 {
		t.Fatalf("status = %+v", st)
	}
	if j := st.Jails[0]; j.Name != "sshd" || j.TotalFailed != 9 || j.TotalBanned != 4 || !slices.Equal(j.Banned, []string{"198.51.100.1", "198.51.100.2"}) {
		t.Fatalf("jail = %+v", j)
	}
}

func TestSSHConfigAndKeys(t *testing.T) {
	c := sshConfig([]int{22, 2222}, "prohibit-password", false)
	for _, want := range []string{"Port 22\n", "Port 2222\n", "PermitRootLogin prohibit-password\n", "PasswordAuthentication no\n", "KbdInteractiveAuthentication no\n"} {
		if !strings.Contains(c, want) {
			t.Errorf("config lacks %q:\n%s", want, c)
		}
	}
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test@client"
	if !pubKeyRe.MatchString(key) {
		t.Fatal("valid key rejected")
	}
	fp, err := KeyFingerprint(key)
	if err != nil || !strings.HasPrefix(fp, "SHA256:") || len(fp) != 50 {
		t.Fatalf("fingerprint = %q %v", fp, err)
	}
	if pubKeyRe.MatchString("ssh-ed25519 AAAA\nssh-rsa AAAA") || pubKeyRe.MatchString("command=\"x\" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl") {
		t.Fatal("bad key accepted")
	}
}
