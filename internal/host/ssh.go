package host

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SSH settings are written to a drop-in that sorts first, so it wins over
// cloud-init's 50-cloud-init.conf and the main sshd_config (sshd uses the
// first value it reads for each keyword).
const sshDropIn = "/etc/ssh/sshd_config.d/00-docklet.conf"

// SSHStatus is the effective sshd configuration.
type SSHStatus struct {
	Installed       bool           `json:"installed"`
	Ports           []int          `json:"ports"`
	PermitRootLogin string         `json:"permitRootLogin"` // yes, prohibit-password, no, …
	PasswordAuth    bool           `json:"passwordAuthentication"`
	PubkeyAuth      bool           `json:"pubkeyAuthentication"`
	SocketActivated bool           `json:"socketActivated"`
	Managed         bool           `json:"managed"` // Docklet's drop-in exists
	Keys            map[string]int `json:"keys"`    // user -> authorized keys (root + sudo users)
}

// SSHSettings is what the admin can change.
type SSHSettings struct {
	Port            int    `json:"port"`
	PermitRootLogin string `json:"permitRootLogin"` // yes, prohibit-password, no
	PasswordAuth    bool   `json:"passwordAuthentication"`
}

const sshStatusScript = `mkdir -p /run/sshd
command -v sshd >/dev/null || { echo installed=0; exit 0; }
echo installed=1
sshd -T 2>/dev/null | awk '$1=="port"||$1=="permitrootlogin"||$1=="passwordauthentication"||$1=="pubkeyauthentication"{print $1"="$2}'
systemctl is-active -q ssh.socket 2>/dev/null && echo socket=1
[ -f ` + sshDropIn + ` ] && echo managed=1
for u in root $(getent group sudo admin wheel | cut -d: -f4 | tr ',' ' '); do
  h=$(getent passwd "$u" | cut -d: -f6); f="$h/.ssh/authorized_keys"; n=0
  [ -f "$f" ] && n=$(grep -cE '^[[:space:]]*(ssh-|ecdsa-|sk-)' "$f" || true)
  echo "keys.$u=$n"
done
true`

// SSHStatus reads the effective sshd settings with sshd -T.
func (s *Service) SSHStatus(ctx context.Context) (SSHStatus, error) {
	out, err := Script(ctx, s.R, sshStatusScript)
	if err != nil {
		return SSHStatus{}, err
	}
	st := SSHStatus{Keys: map[string]int{}}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		switch {
		case k == "installed":
			st.Installed = v == "1"
		case k == "port":
			if p, err := strconv.Atoi(v); err == nil {
				st.Ports = append(st.Ports, p)
			}
		case k == "permitrootlogin":
			if v == "without-password" { // the old spelling sshd -T still prints
				v = "prohibit-password"
			}
			st.PermitRootLogin = v
		case k == "passwordauthentication":
			st.PasswordAuth = v == "yes"
		case k == "pubkeyauthentication":
			st.PubkeyAuth = v == "yes"
		case k == "socket":
			st.SocketActivated = true
		case k == "managed":
			st.Managed = true
		case strings.HasPrefix(k, "keys."):
			st.Keys[strings.TrimPrefix(k, "keys.")] = int(atoi64(v))
		}
	}
	sort.Ints(st.Ports)
	return st, nil
}

// reloadSSH restarts sshd; Ubuntu ≥ 22.10 starts it from ssh.socket, whose
// ports a generator derives from sshd_config, so it needs a daemon-reload.
const reloadSSH = `
reload_ssh() {
  if systemctl is-active -q ssh.socket 2>/dev/null || systemctl is-enabled -q ssh.socket 2>/dev/null; then
    systemctl daemon-reload
    systemctl stop ssh.service 2>/dev/null || true
    systemctl restart ssh.socket
  elif systemctl cat ssh.service >/dev/null 2>&1; then
    systemctl restart ssh.service
  else
    systemctl restart sshd.service
  fi
}
`

// applySSHScript writes CONF_B64 as the drop-in, keeps the previous version
// for revert, checks it with sshd -t (restoring on failure) and restarts sshd.
const applySSHScript = `set -e` + reloadSSH + `
D=` + sshDropIn + `; S=/var/lib/docklet-host; mkdir -p "$S" /run/sshd
grep -qiE '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config.d/\*\.conf' /etc/ssh/sshd_config || {
  echo "/etc/ssh/sshd_config does not include sshd_config.d/*.conf, so Docklet can't manage SSH settings" >&2; exit 4; }
if [ "$KEEP_PREV" != 1 ]; then
  if [ -f "$D" ]; then cp -p "$D" "$S/sshd.prev"; echo present > "$S/sshd.prev.state"; else echo absent > "$S/sshd.prev.state"; fi
fi
printf '%s' "$CONF_B64" | base64 -d > "$D.new"; chmod 644 "$D.new"
[ -f "$D" ] && cp -p "$D" "$D.bak" || rm -f "$D.bak"
mv "$D.new" "$D"
if ! sshd -t 2>"$S/sshd.err"; then
  if [ -f "$D.bak" ]; then mv "$D.bak" "$D"; else rm -f "$D"; fi
  echo "sshd rejected the configuration: $(cat "$S/sshd.err")" >&2; exit 3
fi
rm -f "$D.bak"
reload_ssh
sleep 1
ss -H -tlnp 2>/dev/null | grep -E 'sshd|systemd' | awk '{print $4}' || true`

// revertSSHScript restores the drop-in saved by the first apply and removes
// firewall rules added for new ports (FW_PORTS, space separated).
const revertSSHScript = reloadSSH + `
D=` + sshDropIn + `; S=/var/lib/docklet-host; mkdir -p /run/sshd
if [ "$(cat "$S/sshd.prev.state" 2>/dev/null)" = present ]; then cp -p "$S/sshd.prev" "$D"; else rm -f "$D"; fi
sshd -t && reload_ssh
if [ -n "$FW_PORTS" ] && command -v ufw >/dev/null; then
  for p in $FW_PORTS; do ufw delete allow "$p/tcp" >/dev/null 2>&1 || true; done
fi
true`

func validRootLogin(v string) bool { return v == "yes" || v == "prohibit-password" || v == "no" }

// sshConfig renders the drop-in.
func sshConfig(ports []int, rootLogin string, password bool) string {
	var b strings.Builder
	b.WriteString("# Managed by Docklet (Server → SSH). Changes here are overwritten.\n")
	for _, p := range ports {
		fmt.Fprintf(&b, "Port %d\n", p)
	}
	fmt.Fprintf(&b, "PermitRootLogin %s\n", rootLogin)
	if password {
		b.WriteString("PasswordAuthentication yes\n")
	} else {
		b.WriteString("PasswordAuthentication no\nKbdInteractiveAuthentication no\n")
	}
	b.WriteString("PubkeyAuthentication yes\n")
	return b.String()
}

// ChangeSSH applies new SSH settings with confirm-or-revert:
//   - a new port is opened (firewall first) while the old port stays open;
//   - the host reverts everything after window unless ConfirmSSH runs.
func (s *Service) ChangeSSH(ctx context.Context, want SSHSettings, reservedPorts []int, window time.Duration) (Pending, error) {
	if err := s.requireSupported(ctx); err != nil {
		return Pending{}, err
	}
	cur, err := s.SSHStatus(ctx)
	if err != nil {
		return Pending{}, err
	}
	if !cur.Installed {
		return Pending{}, invalid("OpenSSH server is not installed on this host")
	}
	if !validRootLogin(want.PermitRootLogin) {
		return Pending{}, invalid("root login must be yes, prohibit-password or no")
	}
	if want.Port < 1 || want.Port > 65535 {
		return Pending{}, invalid("SSH port must be 1-65535")
	}
	for _, r := range reservedPorts {
		if want.Port == r {
			return Pending{}, invalid("port %d is used by Docklet or Caddy", r)
		}
	}
	portChange := len(cur.Ports) != 1 || cur.Ports[0] != want.Port
	if portChange {
		if ports, err := s.ListeningPorts(ctx); err == nil {
			for _, p := range ports {
				if p.Port == want.Port && p.Proto == "tcp" && p.Process != "" && p.Process != "sshd" && p.Process != "systemd" {
					return Pending{}, invalid("port %d is already used by %s", want.Port, p.Process)
				}
			}
		}
	}
	// Lockout guard: key-only logins need a key for someone who can log in.
	if !want.PasswordAuth || want.PermitRootLogin != "yes" {
		root := cur.Keys["root"] > 0 && want.PermitRootLogin != "no"
		sudo := false
		for u, n := range cur.Keys {
			if u != "root" && n > 0 {
				sudo = true
			}
		}
		if !want.PasswordAuth && !root && !sudo {
			return Pending{}, invalid("add an SSH key first: with password login off, nobody could log in (no authorized_keys for root or a sudo user)")
		}
		if want.PermitRootLogin == "no" && !sudo && !want.PasswordAuth {
			return Pending{}, invalid("root login can't be turned off: no sudo user has an SSH key")
		}
	}

	// Transitional config keeps the old ports open next to the new one.
	ports := []int{want.Port}
	if portChange {
		seen := map[int]bool{want.Port: true}
		for _, p := range cur.Ports {
			if !seen[p] {
				ports = append(ports, p)
				seen[p] = true
			}
		}
	}
	sort.Ints(ports)
	final := sshConfig([]int{want.Port}, want.PermitRootLogin, want.PasswordAuth)
	trans := sshConfig(ports, want.PermitRootLogin, want.PasswordAuth)

	var fwAdded []string
	if portChange {
		if fw, err := s.FirewallStatus(ctx); err == nil && fw.Active && !fw.allows(want.Port) {
			if _, err := Check(ctx, s.R, []string{"ufw", "allow", strconv.Itoa(want.Port) + "/tcp", "comment", "SSH (Docklet)"}); err != nil {
				return Pending{}, fmt.Errorf("open port %d in the firewall: %w", want.Port, err)
			}
			fwAdded = append(fwAdded, strconv.Itoa(want.Port))
		}
	}
	revertEnv := []string{"FW_PORTS=" + strings.Join(fwAdded, " ")}
	out, err := Script(ctx, s.R, applySSHScript, "CONF_B64="+base64.StdEncoding.EncodeToString([]byte(trans)), "KEEP_PREV=0")
	if err != nil {
		_, _ = Script(ctx, s.R, revertSSHScript, revertEnv...)
		return Pending{}, err
	}
	if portChange && !strings.Contains(out, ":"+strconv.Itoa(want.Port)) {
		_, _ = Script(ctx, s.R, revertSSHScript, revertEnv...)
		return Pending{}, fmt.Errorf("sshd is not listening on port %d after the change; reverted", want.Port)
	}
	if err := s.scheduleRevert(ctx, revertSSHScript, revertEnv, window); err != nil {
		_, _ = Script(ctx, s.R, revertSSHScript, revertEnv...)
		return Pending{}, fmt.Errorf("schedule the automatic revert: %w", err)
	}
	var closePorts []string
	for _, p := range cur.Ports {
		if p != want.Port {
			closePorts = append(closePorts, strconv.Itoa(p))
		}
	}
	desc := fmt.Sprintf("SSH: port %d, root login %s, password login %s", want.Port, want.PermitRootLogin, map[bool]string{true: "on", false: "off"}[want.PasswordAuth])
	return Pending{
		Kind: "ssh", Description: desc, Deadline: time.Now().Add(window),
		ConfirmData: map[string]string{"conf": base64.StdEncoding.EncodeToString([]byte(final)), "close": strings.Join(closePorts, " ")},
	}, nil
}

// ConfirmSSH keeps a pending SSH change: the revert is cancelled, old ports
// are closed in sshd and in the firewall.
func (s *Service) ConfirmSSH(ctx context.Context, p Pending) error {
	s.dropRevert(ctx)
	if _, err := Script(ctx, s.R, applySSHScript, "CONF_B64="+p.ConfirmData["conf"], "KEEP_PREV=1"); err != nil {
		return err
	}
	if fw, err := s.FirewallStatus(ctx); err == nil && fw.Active {
		for _, port := range strings.Fields(p.ConfirmData["close"]) {
			_, _ = Check(ctx, s.R, []string{"ufw", "delete", "allow", port + "/tcp"})
			if port == "22" {
				_, _ = Check(ctx, s.R, []string{"ufw", "delete", "allow", "OpenSSH"})
			}
		}
	}
	return nil
}

// --- Authorized keys ------------------------------------------------------------

var pubKeyRe = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/=]{40,}( [^\r\n]{0,200})?$`)

var userRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// AuthorizedKey is one line of authorized_keys.
type AuthorizedKey struct {
	User        string `json:"user"`
	Fingerprint string `json:"fingerprint"`
	Type        string `json:"type"`
	Comment     string `json:"comment"`
}

// KeyFingerprint is ssh-keygen's SHA256 fingerprint of a public key line.
func KeyFingerprint(key string) (string, error) {
	f := strings.Fields(key)
	if len(f) < 2 {
		return "", invalid("not a public key")
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return "", invalid("not a public key")
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

const listKeysScript = `for u in root $(getent group sudo admin wheel | cut -d: -f4 | tr ',' ' '); do
  h=$(getent passwd "$u" | cut -d: -f6); f="$h/.ssh/authorized_keys"
  [ -f "$f" ] || continue
  grep -E '^[[:space:]]*(ssh-|ecdsa-|sk-)' "$f" | while IFS= read -r line; do
    printf '%s\n' "$line" > /tmp/.docklet-key
    fp=$(ssh-keygen -lf /tmp/.docklet-key 2>/dev/null) && echo "$u|$fp"
  done
done
rm -f /tmp/.docklet-key; true`

// AuthorizedKeys lists the keys of root and sudo users.
func (s *Service) AuthorizedKeys(ctx context.Context) ([]AuthorizedKey, error) {
	out, err := Script(ctx, s.R, listKeysScript)
	if err != nil {
		return nil, err
	}
	var keys []AuthorizedKey
	for _, l := range strings.Split(out, "\n") {
		u, fp, ok := strings.Cut(strings.TrimSpace(l), "|")
		if !ok {
			continue
		}
		// "256 SHA256:abc… comment (ED25519)"
		f := strings.Fields(fp)
		if len(f) < 3 {
			continue
		}
		k := AuthorizedKey{User: u, Fingerprint: f[1], Type: strings.Trim(f[len(f)-1], "()")}
		if len(f) > 3 {
			k.Comment = strings.Join(f[2:len(f)-1], " ")
		}
		keys = append(keys, k)
	}
	return keys, nil
}

const addKeyScript = `set -e
getent passwd "$KEY_USER" >/dev/null || { echo "no such user" >&2; exit 3; }
if [ "$KEY_USER" != root ] && ! id -nG "$KEY_USER" | tr ' ' '\n' | grep -qxE 'sudo|admin|wheel'; then echo "not a sudo user" >&2; exit 3; fi
h=$(getent passwd "$KEY_USER" | cut -d: -f6); g=$(id -gn "$KEY_USER")
install -d -m 700 -o "$KEY_USER" -g "$g" "$h/.ssh"
f="$h/.ssh/authorized_keys"; touch "$f"; chown "$KEY_USER:$g" "$f"; chmod 600 "$f"
grep -qxF "$PUBKEY" "$f" || printf '%s\n' "$PUBKEY" >> "$f"`

// AddAuthorizedKey adds a public key for root or a sudo user.
func (s *Service) AddAuthorizedKey(ctx context.Context, user, key string) error {
	key = strings.TrimSpace(key)
	if len(key) > 16384 {
		return invalid("key is too long")
	}
	if !userRe.MatchString(user) {
		return invalid("invalid user name")
	}
	if !pubKeyRe.MatchString(key) {
		return invalid("that doesn't look like an OpenSSH public key (ssh-ed25519 AAAA… comment)")
	}
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	_, err := Script(ctx, s.R, addKeyScript, "KEY_USER="+user, "PUBKEY="+key)
	return err
}

const removeKeyScript = `set -e
h=$(getent passwd "$KEY_USER" | cut -d: -f6); f="$h/.ssh/authorized_keys"
[ -f "$f" ] || exit 0
tmp=$(mktemp); : > "$tmp"
while IFS= read -r line; do
  printf '%s\n' "$line" > /tmp/.docklet-key
  fp=$(ssh-keygen -lf /tmp/.docklet-key 2>/dev/null | awk '{print $2}') || fp=""
  [ "$fp" = "$FINGERPRINT" ] || printf '%s\n' "$line" >> "$tmp"
done < "$f"
cat "$tmp" > "$f"; rm -f "$tmp" /tmp/.docklet-key`

// RemoveAuthorizedKey removes the key with fingerprint from user's file.
func (s *Service) RemoveAuthorizedKey(ctx context.Context, user, fingerprint string) error {
	if !userRe.MatchString(user) || !strings.HasPrefix(fingerprint, "SHA256:") || strings.ContainsAny(fingerprint, " \n'\"") {
		return invalid("invalid user or fingerprint")
	}
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	st, err := s.SSHStatus(ctx)
	if err == nil && !st.PasswordAuth {
		total := 0
		for _, n := range st.Keys {
			total += n
		}
		if total <= 1 {
			return invalid("this is the last SSH key and password login is off: removing it would lock everyone out")
		}
	}
	_, err = Script(ctx, s.R, removeKeyScript, "KEY_USER="+user, "FINGERPRINT="+fingerprint)
	return err
}
