package host

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrUnsupported means the host OS can't be managed (not Debian/Ubuntu with
// systemd). Read-only information still works.
var ErrUnsupported = errors.New("this host is not supported for changes: Docklet manages Ubuntu and Debian servers with systemd")

// InputError is a request the host refuses (bad value, protected target).
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func invalid(format string, args ...any) error { return &InputError{Msg: fmt.Sprintf(format, args...)} }

// Service implements the Server page on top of a Runner.
type Service struct {
	R Runner
}

// OS describes the host.
type OS struct {
	ID         string `json:"id"`
	VersionID  string `json:"versionId"`
	PrettyName string `json:"prettyName"`
	Systemd    bool   `json:"systemd"`
	Supported  bool   `json:"supported"`
}

// Overview is the Server page's summary.
type Overview struct {
	OS         OS     `json:"os"`
	Hostname   string `json:"hostname"`
	Kernel     string `json:"kernel"`
	Arch       string `json:"arch"`
	UptimeSec  int64  `json:"uptimeSec"`
	Timezone   string `json:"timezone"`
	LocalTime  string `json:"localTime"`
	CPUs       int    `json:"cpus"`
	MemTotal   int64  `json:"memTotal"`
	MemAvail   int64  `json:"memAvailable"`
	SwapTotal  int64  `json:"swapTotal"`
	SwapUsed   int64  `json:"swapUsed"`
	Swappiness int    `json:"swappiness"`
	SwapFile   int64  `json:"swapFileBytes"` // size of /swapfile when active, else 0
	DiskTotal  int64  `json:"diskTotal"`
	DiskUsed   int64  `json:"diskUsed"`
	DiskAvail  int64  `json:"diskAvailable"`
	RebootReq  bool   `json:"rebootRequired"`
}

// One helper start gathers everything.
const overviewScript = `
. /etc/os-release 2>/dev/null
echo "os.id=$ID"; echo "os.version=$VERSION_ID"; echo "os.pretty=$PRETTY_NAME"
[ -d /run/systemd/system ] && echo "systemd=1"
echo "hostname=$(hostname)"; echo "kernel=$(uname -r)"; echo "arch=$(uname -m)"
echo "uptime=$(cut -d' ' -f1 /proc/uptime)"
tz=$(timedatectl show -p Timezone --value 2>/dev/null); [ -n "$tz" ] || tz=$(cat /etc/timezone 2>/dev/null)
echo "tz=$tz"; echo "time=$(date '+%Y-%m-%d %H:%M:%S %Z')"
echo "cpus=$(nproc 2>/dev/null)"
awk '/^MemTotal:/{print "memtotal="$2} /^MemAvailable:/{print "memavail="$2} /^SwapTotal:/{print "swaptotal="$2} /^SwapFree:/{print "swapfree="$2}' /proc/meminfo
echo "swappiness=$(cat /proc/sys/vm/swappiness)"
awk '$1=="/swapfile"{print "swapfile="$3}' /proc/swaps
df -P -B1 / | awk 'NR==2{print "disktotal="$2; print "diskused="$3; print "diskavail="$4}'
[ -f /var/run/reboot-required ] && echo "reboot=1"
true
`

func kv(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			m[k] = v
		}
	}
	return m
}

func atoi64(s string) int64 { n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64); return n }

// Overview reads the host's state.
func (s *Service) Overview(ctx context.Context) (Overview, error) {
	out, err := Script(ctx, s.R, overviewScript)
	if err != nil {
		return Overview{}, err
	}
	m := kv(out)
	o := Overview{
		OS:       OS{ID: m["os.id"], VersionID: m["os.version"], PrettyName: m["os.pretty"], Systemd: m["systemd"] == "1"},
		Hostname: m["hostname"], Kernel: m["kernel"], Arch: m["arch"], Timezone: m["tz"], LocalTime: m["time"],
		CPUs: int(atoi64(m["cpus"])), MemTotal: atoi64(m["memtotal"]) << 10, MemAvail: atoi64(m["memavail"]) << 10,
		SwapTotal: atoi64(m["swaptotal"]) << 10, Swappiness: int(atoi64(m["swappiness"])), SwapFile: atoi64(m["swapfile"]) << 10,
		DiskTotal: atoi64(m["disktotal"]), DiskUsed: atoi64(m["diskused"]), DiskAvail: atoi64(m["diskavail"]), RebootReq: m["reboot"] == "1",
	}
	o.SwapUsed = o.SwapTotal - atoi64(m["swapfree"])<<10
	if f, err := strconv.ParseFloat(m["uptime"], 64); err == nil {
		o.UptimeSec = int64(f)
	}
	o.OS.Supported = (o.OS.ID == "ubuntu" || o.OS.ID == "debian") && o.OS.Systemd
	return o, nil
}

// requireSupported refuses changes on unsupported hosts.
func (s *Service) requireSupported(ctx context.Context) error {
	out, err := Script(ctx, s.R, `. /etc/os-release 2>/dev/null; echo "$ID"; [ -d /run/systemd/system ] && echo systemd; true`)
	if err != nil {
		return err
	}
	lines := strings.Fields(out)
	if len(lines) < 2 || (lines[0] != "ubuntu" && lines[0] != "debian") || lines[1] != "systemd" {
		return ErrUnsupported
	}
	return nil
}

// --- Hostname ----------------------------------------------------------------

var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// ValidHostname checks an RFC 1123 host name (lowercase, ≤ 253 chars).
func ValidHostname(h string) bool { return len(h) <= 253 && hostnameRe.MatchString(h) }

const hostnameScript = `set -e
H="${HOSTS_FILE:-/etc/hosts}"
hostnamectl set-hostname "$NEW_HOSTNAME"
short="${NEW_HOSTNAME%%.*}"
if grep -q '^127\.0\.1\.1[[:space:]]' "$H"; then
  sed -i "s/^127\.0\.1\.1[[:space:]].*/127.0.1.1\t$NEW_HOSTNAME $short/" "$H"
else
  printf '127.0.1.1\t%s %s\n' "$NEW_HOSTNAME" "$short" >> "$H"
fi
hostname`

// SetHostname changes the host name persistently and keeps /etc/hosts in step.
func (s *Service) SetHostname(ctx context.Context, name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if !ValidHostname(name) {
		return invalid("host name: lowercase letters, digits, '-' and '.', each part 1-63 characters")
	}
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	_, err := Script(ctx, s.R, hostnameScript, "NEW_HOSTNAME="+name)
	return err
}

// --- Time zone ---------------------------------------------------------------

// Timezones lists the zones the host knows.
func (s *Service) Timezones(ctx context.Context) ([]string, error) {
	out, err := Check(ctx, s.R, []string{"timedatectl", "list-timezones", "--no-pager"})
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// SetTimezone sets the host's time zone (must be in Timezones).
func (s *Service) SetTimezone(ctx context.Context, tz string) error {
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	zones, err := s.Timezones(ctx)
	if err != nil {
		return err
	}
	i := sort.SearchStrings(zones, tz)
	if !sort.StringsAreSorted(zones) {
		i = -1
		for j, z := range zones {
			if z == tz {
				i = j
			}
		}
	}
	if i < 0 || i >= len(zones) || zones[i] != tz {
		return invalid("unknown time zone %q", tz)
	}
	_, err = Check(ctx, s.R, []string{"timedatectl", "set-timezone", tz})
	return err
}

// --- Swap ------------------------------------------------------------------------

const swapScript = `set -e
F=/swapfile
if grep -q "^$F " /proc/swaps; then swapoff "$F"; fi
rm -f "$F"
if [ "$SWAP_MB" -gt 0 ]; then
  avail=$(df -P -m / | awk 'NR==2{print $4}')
  if [ "$avail" -lt $((SWAP_MB + 1024)) ]; then echo "not enough free disk space: ${avail} MB free, need ${SWAP_MB} MB plus 1 GB to spare" >&2; exit 3; fi
  fallocate -l "${SWAP_MB}M" "$F" 2>/dev/null || dd if=/dev/zero of="$F" bs=1M count="$SWAP_MB" status=none
  chmod 600 "$F"
  mkswap "$F" >/dev/null
  swapon "$F"
  grep -q "^$F[[:space:]]" /etc/fstab || printf '%s none swap sw 0 0\n' "$F" >> /etc/fstab
else
  sed -i "\|^$F[[:space:]]|d" /etc/fstab
fi
cat /proc/swaps`

// SetSwap creates or resizes /swapfile (sizeMB > 0) or removes it (0).
func (s *Service) SetSwap(ctx context.Context, sizeMB int) error {
	if sizeMB < 0 || (sizeMB > 0 && sizeMB < 256) || sizeMB > 64*1024 {
		return invalid("swap size must be 0 (remove) or 256 MB - 64 GB")
	}
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	_, err := Script(ctx, s.R, swapScript, "SWAP_MB="+strconv.Itoa(sizeMB))
	return err
}

const swappinessScript = `set -e
sysctl -q -w "vm.swappiness=$SWAPPINESS"
f=/etc/sysctl.d/99-docklet.conf
touch "$f"
sed -i '/^vm\.swappiness[[:space:]]*=/d' "$f"
echo "vm.swappiness = $SWAPPINESS" >> "$f"
cat /proc/sys/vm/swappiness`

// SetSwappiness sets vm.swappiness now and across reboots.
func (s *Service) SetSwappiness(ctx context.Context, v int) error {
	if v < 0 || v > 100 {
		return invalid("swappiness must be 0-100")
	}
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	_, err := Script(ctx, s.R, swappinessScript, "SWAPPINESS="+strconv.Itoa(v))
	return err
}

// --- Reboot --------------------------------------------------------------------

// Reboot schedules a reboot a few seconds out, so the request can finish.
func (s *Service) Reboot(ctx context.Context) error {
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	_, err := Check(ctx, s.R, []string{"systemd-run", "--on-active=5", "--unit=docklet-reboot-" + strconv.FormatInt(time.Now().Unix(), 10),
		"--description=Reboot requested in Docklet", "systemctl", "reboot"})
	return err
}

// --- Listening ports --------------------------------------------------------------

// Port is a listening socket.
type Port struct {
	Proto   string `json:"proto"` // tcp, udp
	Address string `json:"address"`
	Port    int    `json:"port"`
	Process string `json:"process,omitempty"`
	PID     int    `json:"pid,omitempty"`
	// Filled in by the caller from Docker.
	Container string `json:"container,omitempty"`
	Owner     string `json:"owner,omitempty"` // e.g. "app web", "database main", "Docklet"
}

var ssUsers = regexp.MustCompile(`\("([^"]+)",pid=(\d+)`)

// ListeningPorts lists TCP and UDP listeners with their processes.
func (s *Service) ListeningPorts(ctx context.Context) ([]Port, error) {
	out, err := Script(ctx, s.R, `if command -v ss >/dev/null; then ss -H -tulpn; else echo NO_SS; cat /proc/net/tcp /proc/net/tcp6 /proc/net/udp /proc/net/udp6 2>/dev/null; true; fi`)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(out, "NO_SS") {
		return parseProcNet(out), nil
	}
	return ParseSS(out), nil
}

// ParseSS parses `ss -H -tulpn` output.
func ParseSS(out string) []Port {
	var ports []Port
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		proto := f[0]
		if proto != "tcp" && proto != "udp" {
			continue
		}
		if proto == "tcp" && f[1] != "LISTEN" {
			continue
		}
		local := f[4]
		i := strings.LastIndexByte(local, ':')
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(local[i+1:])
		if err != nil {
			continue
		}
		addr := strings.Trim(local[:i], "[]")
		if j := strings.IndexByte(addr, '%'); j >= 0 {
			addr = addr[:j]
		}
		p := Port{Proto: proto, Address: addr, Port: port}
		if m := ssUsers.FindStringSubmatch(l); m != nil {
			p.Process = m[1]
			p.PID, _ = strconv.Atoi(m[2])
		}
		key := fmt.Sprintf("%s/%s/%d/%s", p.Proto, p.Address, p.Port, p.Process)
		if !seen[key] {
			seen[key] = true
			ports = append(ports, p)
		}
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Port != ports[j].Port {
			return ports[i].Port < ports[j].Port
		}
		return ports[i].Proto < ports[j].Proto
	})
	return ports
}

// parseProcNet is the fallback without ss: no process names.
func parseProcNet(out string) []Port {
	var ports []Port
	seen := map[string]bool{}
	section := ""
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		if f[0] == "sl" {
			section = ""
			if strings.Contains(l, "tx_queue") {
				section = "?"
			}
			continue
		}
		local, state := f[1], f[3]
		hexAddr, hexPort, ok := strings.Cut(local, ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseInt(hexPort, 16, 32)
		if err != nil || port == 0 {
			continue
		}
		proto := "udp"
		if state == "0A" {
			proto = "tcp"
		} else if state != "07" {
			continue
		}
		addr := "0.0.0.0"
		if len(hexAddr) == 32 {
			addr = "::"
		}
		if strings.Trim(hexAddr, "0") != "" {
			addr = decodeHexAddr(hexAddr)
		}
		_ = section
		key := fmt.Sprintf("%s/%s/%d", proto, addr, port)
		if !seen[key] {
			seen[key] = true
			ports = append(ports, Port{Proto: proto, Address: addr, Port: int(port)})
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
	return ports
}

func decodeHexAddr(h string) string {
	if len(h) == 8 {
		b := make([]byte, 4)
		for i := 0; i < 4; i++ {
			v, _ := strconv.ParseUint(h[i*2:i*2+2], 16, 8)
			b[3-i] = byte(v)
		}
		return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
	}
	return h
}

// --- Processes ----------------------------------------------------------------------

// Process is one row of the process list.
type Process struct {
	PID     int     `json:"pid"`
	PPID    int     `json:"ppid"`
	User    string  `json:"user"`
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	RSS     int64   `json:"rss"`
	Elapsed int64   `json:"elapsedSec"`
	Comm    string  `json:"comm"`
	Args    string  `json:"args"`
	Protect bool    `json:"protected"`
}

// Protected processes can't be signalled from the UI.
var protectedComms = map[string]bool{
	"systemd": true, "init": true, "kthreadd": true, "sshd": true, "dockerd": true, "containerd": true,
	"docker-proxy": true, "docklet": true, "caddy": true, "systemd-journal": true, "systemd-logind": true,
	"dbus-daemon": true, "nsenter": true, "runc": true,
}

func protectedProcess(p Process) bool {
	return p.PID <= 2 || p.PPID == 2 || protectedComms[p.Comm] || strings.HasPrefix(p.Comm, "containerd-shim")
}

// Processes returns the top processes by CPU.
func (s *Service) Processes(ctx context.Context, limit int) ([]Process, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out, err := Check(ctx, s.R, []string{"ps", "-eo", "pid=,ppid=,user:32=,pcpu=,pmem=,rss=,etimes=,comm=,args=", "--sort=-pcpu", "--cols", "400"})
	if err != nil {
		return nil, err
	}
	var list []Process
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 8 {
			continue
		}
		p := Process{User: f[2], Comm: f[7]}
		p.PID, _ = strconv.Atoi(f[0])
		p.PPID, _ = strconv.Atoi(f[1])
		p.CPU, _ = strconv.ParseFloat(f[3], 64)
		p.Mem, _ = strconv.ParseFloat(f[4], 64)
		p.RSS = atoi64(f[5]) << 10
		p.Elapsed = atoi64(f[6])
		if len(f) > 8 {
			p.Args = strings.Join(f[8:], " ")
		}
		if p.Comm == "ps" || p.Comm == "nsenter" {
			continue // ourselves
		}
		p.Protect = protectedProcess(p)
		list = append(list, p)
		if len(list) >= limit {
			break
		}
	}
	return list, nil
}

// Signal sends TERM or KILL to a process unless it is protected.
func (s *Service) Signal(ctx context.Context, pid int, sig string) (Process, error) {
	if sig != "TERM" && sig != "KILL" {
		return Process{}, invalid("signal must be TERM or KILL")
	}
	out, err := Check(ctx, s.R, []string{"ps", "-o", "pid=,ppid=,user:32=,comm=", "-p", strconv.Itoa(pid)})
	if err != nil {
		return Process{}, invalid("no such process")
	}
	f := strings.Fields(out)
	if len(f) < 4 {
		return Process{}, invalid("no such process")
	}
	p := Process{PID: pid, User: f[2], Comm: f[3]}
	p.PPID, _ = strconv.Atoi(f[1])
	if protectedProcess(p) {
		return p, invalid("%s (pid %d) is protected: Docker, Docklet, Caddy, SSH and system processes can't be stopped here", p.Comm, pid)
	}
	_, err = Check(ctx, s.R, []string{"kill", "-s", sig, strconv.Itoa(pid)})
	return p, err
}

// --- systemd services ----------------------------------------------------------------

// Unit is a systemd service.
type Unit struct {
	Name        string `json:"name"`
	Load        string `json:"load"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
	Description string `json:"description"`
	Enabled     string `json:"enabled"` // enabled, disabled, static, masked, …
	Protected   string `json:"protected,omitempty"`
}

var unitRe = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]{1,200}\.service$`)

// ValidUnit checks a service unit name.
func ValidUnit(u string) bool { return unitRe.MatchString(u) }

// unitGuard says which actions are refused for a unit and why.
func unitGuard(name, action string) error {
	base := strings.TrimSuffix(name, ".service")
	core := base == "docker" || base == "containerd" || base == "dbus" || strings.HasPrefix(base, "systemd-") || strings.HasPrefix(base, "docklet")
	if core && action != "status" && action != "logs" {
		return invalid("%s is needed by the server or by Docklet and can't be changed here", name)
	}
	if (base == "ssh" || base == "sshd") && (action == "stop" || action == "disable") {
		return invalid("stopping %s could lock you out of the server; it can be restarted", name)
	}
	return nil
}

// Services lists service units with their enablement.
func (s *Service) Services(ctx context.Context) ([]Unit, error) {
	out, err := Script(ctx, s.R, `systemctl list-units --type=service --all --no-legend --no-pager --plain
echo ---
systemctl list-unit-files --type=service --no-legend --no-pager`)
	if err != nil {
		return nil, err
	}
	units, files, _ := strings.Cut(out, "---\n")
	enabled := map[string]string{}
	for _, l := range strings.Split(files, "\n") {
		if f := strings.Fields(l); len(f) >= 2 {
			enabled[f[0]] = f[1]
		}
	}
	var list []Unit
	seen := map[string]bool{}
	for _, l := range strings.Split(units, "\n") {
		f := strings.Fields(strings.TrimLeft(l, "● "))
		if len(f) < 4 || !strings.HasSuffix(f[0], ".service") {
			continue
		}
		u := Unit{Name: f[0], Load: f[1], Active: f[2], Sub: f[3], Description: strings.Join(f[4:], " "), Enabled: enabled[f[0]]}
		if err := unitGuard(u.Name, "stop"); err != nil {
			u.Protected = err.Error()
		}
		seen[u.Name] = true
		list = append(list, u)
	}
	// Installed but never loaded (e.g. disabled and inactive).
	for name, st := range enabled {
		if !seen[name] && !strings.Contains(name, "@") {
			list = append(list, Unit{Name: name, Load: "not-loaded", Active: "inactive", Sub: "dead", Enabled: st})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// ServiceAction runs start/stop/restart/enable/disable on a unit.
func (s *Service) ServiceAction(ctx context.Context, unit, action string) error {
	if !ValidUnit(unit) {
		return invalid("invalid unit name")
	}
	switch action {
	case "start", "stop", "restart", "enable", "disable":
	default:
		return invalid("action must be start, stop, restart, enable or disable")
	}
	if err := unitGuard(unit, action); err != nil {
		return err
	}
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	argv := []string{"systemctl", action, unit}
	if action == "enable" || action == "disable" {
		argv = []string{"systemctl", action, "--now", unit}
	}
	_, err := Check(ctx, s.R, argv)
	return err
}

// ServiceLogs returns the unit's last journal lines.
func (s *Service) ServiceLogs(ctx context.Context, unit string, lines int) (string, error) {
	if !ValidUnit(unit) {
		return "", invalid("invalid unit name")
	}
	if lines <= 0 || lines > 2000 {
		lines = 200
	}
	return Check(ctx, s.R, []string{"journalctl", "-u", unit, "-n", strconv.Itoa(lines), "--no-pager", "-o", "short-iso"})
}
