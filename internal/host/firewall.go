package host

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The firewall is ufw. Docklet never lets a change block SSH, HTTP(S) or the
// panel: those ports are allowed before ufw is enabled, rules that would deny
// them are refused, and enabling ufw is confirm-or-revert.

// FWRule is one numbered ufw rule.
type FWRule struct {
	Num     int    `json:"num"`
	To      string `json:"to"`     // 22/tcp, 80, 6000:6100/udp, OpenSSH
	Action  string `json:"action"` // ALLOW IN, DENY IN, LIMIT IN, REJECT IN, ...
	From    string `json:"from"`   // Anywhere, 1.2.3.4, 10.0.0.0/8
	V6      bool   `json:"v6"`
	Comment string `json:"comment,omitempty"`
	Text    string `json:"text"` // the line as ufw printed it, used to confirm deletes
}

// FirewallStatus is ufw's state.
type FirewallStatus struct {
	Installed       bool     `json:"installed"`
	Active          bool     `json:"active"`
	DefaultIncoming string   `json:"defaultIncoming"`
	DefaultOutgoing string   `json:"defaultOutgoing"`
	Rules           []FWRule `json:"rules"`
}

const fwStatusScript = `command -v ufw >/dev/null || { echo NOT_INSTALLED; exit 0; }
ufw status verbose | grep -E '^(Status|Default):'
echo ---
ufw status numbered | grep -E '^\[ *[0-9]+\]'
true`

var fwLineRe = regexp.MustCompile(`^\[\s*(\d+)\]\s+(.+?)\s{2,}((?:ALLOW|DENY|REJECT|LIMIT)(?: IN| OUT| FWD)?)\s+(.+?)\s*$`)

// FirewallStatus reads ufw's status and numbered rules.
func (s *Service) FirewallStatus(ctx context.Context) (FirewallStatus, error) {
	out, err := Script(ctx, s.R, fwStatusScript)
	if err != nil {
		return FirewallStatus{}, err
	}
	return ParseUFW(out), nil
}

// ParseUFW parses fwStatusScript's output.
func ParseUFW(out string) FirewallStatus {
	var st FirewallStatus
	if strings.HasPrefix(strings.TrimSpace(out), "NOT_INSTALLED") {
		return st
	}
	st.Installed = true
	head, rules, _ := strings.Cut(out, "---")
	for _, l := range strings.Split(head, "\n") {
		l = strings.TrimSpace(l)
		if v, ok := strings.CutPrefix(l, "Status:"); ok {
			st.Active = strings.TrimSpace(v) == "active"
		}
		if v, ok := strings.CutPrefix(l, "Default:"); ok {
			// deny (incoming), allow (outgoing), disabled (routed)
			for _, part := range strings.Split(v, ",") {
				f := strings.Fields(part)
				if len(f) == 2 {
					switch strings.Trim(f[1], "()") {
					case "incoming":
						st.DefaultIncoming = f[0]
					case "outgoing":
						st.DefaultOutgoing = f[0]
					}
				}
			}
		}
	}
	for _, l := range strings.Split(rules, "\n") {
		l = strings.TrimRight(l, " \r")
		body, comment, _ := strings.Cut(l, " # ")
		m := fwLineRe.FindStringSubmatch(strings.TrimRight(body, " "))
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		r := FWRule{Num: n, To: strings.TrimSpace(m[2]), Action: m[3], From: strings.TrimSpace(m[4]), Comment: strings.TrimSpace(comment), Text: strings.TrimSpace(l)}
		if strings.HasSuffix(r.To, "(v6)") {
			r.V6 = true
			r.To = strings.TrimSpace(strings.TrimSuffix(r.To, "(v6)"))
			r.From = strings.TrimSpace(strings.TrimSuffix(r.From, "(v6)"))
		}
		st.Rules = append(st.Rules, r)
	}
	return st
}

// ruleCovers reports whether rule target `to` includes port/proto.
func ruleCovers(to string, port int, proto string) bool {
	switch to {
	case "OpenSSH":
		return port == 22 && proto == "tcp"
	case "Anywhere":
		return true
	}
	spec, p, hasProto := strings.Cut(to, "/")
	if hasProto && p != proto {
		return false
	}
	// "Anywhere on eth0" and IP targets: not port-specific, ignore.
	for _, part := range strings.Split(spec, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		if port >= a && port <= b {
			return true
		}
	}
	return false
}

// allows reports whether an IPv4 allow/limit rule from anywhere covers port/tcp.
func (st FirewallStatus) allows(port int) bool { return st.allowsProto(port, "tcp") }

func (st FirewallStatus) allowsProto(port int, proto string) bool {
	for _, r := range st.Rules {
		if r.V6 || r.From != "Anywhere" || (!strings.HasPrefix(r.Action, "ALLOW") && !strings.HasPrefix(r.Action, "LIMIT")) {
			continue
		}
		if strings.HasSuffix(r.Action, "OUT") {
			continue
		}
		if ruleCovers(r.To, port, proto) {
			return true
		}
	}
	return false
}

// ProtectedPort is a port the firewall must always let in.
type ProtectedPort struct {
	Port  int    `json:"port"`
	Proto string `json:"proto"`
	Why   string `json:"why"`
}

// Protected returns the ports Docklet keeps open: SSH, HTTP(S) and the panel.
func Protected(sshPorts []int, httpPort, httpsPort, panelPort int) []ProtectedPort {
	var p []ProtectedPort
	for _, sp := range sshPorts {
		p = append(p, ProtectedPort{sp, "tcp", "SSH"})
	}
	p = append(p, ProtectedPort{httpPort, "tcp", "HTTP"}, ProtectedPort{httpsPort, "tcp", "HTTPS"}, ProtectedPort{httpsPort, "udp", "HTTP/3"})
	if panelPort > 0 && panelPort != httpPort && panelPort != httpsPort {
		p = append(p, ProtectedPort{panelPort, "tcp", "Docklet panel"})
	}
	return p
}

var pkgRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{1,60}$`)

// InstallPackages installs apt packages (needs a long context deadline).
func (s *Service) InstallPackages(ctx context.Context, pkgs ...string) error {
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	for _, p := range pkgs {
		if !pkgRe.MatchString(p) {
			return invalid("invalid package name %q", p)
		}
	}
	_, err := Script(ctx, s.R, `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -q >/dev/null 2>&1 || apt-get update -q
apt-get install -y -q --no-install-recommends $PKGS >/tmp/docklet-apt.log 2>&1 || { tail -20 /tmp/docklet-apt.log >&2; exit 1; }`, "PKGS="+strings.Join(pkgs, " "))
	return err
}

// ensureAllowed adds allow rules for every protected port not yet allowed.
func (s *Service) ensureAllowed(ctx context.Context, st FirewallStatus, protected []ProtectedPort) ([]string, error) {
	var added []string
	for _, p := range protected {
		if st.allowsProto(p.Port, p.Proto) {
			continue
		}
		spec := strconv.Itoa(p.Port) + "/" + p.Proto
		if _, err := Check(ctx, s.R, []string{"ufw", "allow", spec, "comment", p.Why + " (Docklet)"}); err != nil {
			return added, err
		}
		added = append(added, spec)
	}
	return added, nil
}

// EnableFirewall turns ufw on (default deny incoming) with the protected
// ports allowed, and schedules ufw to switch off again unless confirmed.
func (s *Service) EnableFirewall(ctx context.Context, protected []ProtectedPort, window time.Duration) (Pending, error) {
	if err := s.requireSupported(ctx); err != nil {
		return Pending{}, err
	}
	st, err := s.FirewallStatus(ctx)
	if err != nil {
		return Pending{}, err
	}
	if !st.Installed {
		return Pending{}, invalid("ufw is not installed; install it first")
	}
	if st.Active {
		return Pending{}, invalid("the firewall is already on")
	}
	if _, err := s.ensureAllowed(ctx, st, protected); err != nil {
		return Pending{}, err
	}
	if err := s.scheduleRevert(ctx, `ufw --force disable`, nil, window); err != nil {
		return Pending{}, fmt.Errorf("schedule the automatic revert: %w", err)
	}
	if _, err := Script(ctx, s.R, `set -e
ufw default deny incoming >/dev/null
ufw default allow outgoing >/dev/null
ufw --force enable`); err != nil {
		_ = s.runRevertNow(ctx)
		return Pending{}, err
	}
	return Pending{Kind: "firewall", Description: "Firewall turned on (incoming traffic denied unless allowed)", Deadline: time.Now().Add(window)}, nil
}

// DisableFirewall turns ufw off.
func (s *Service) DisableFirewall(ctx context.Context) error {
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	s.dropRevert(ctx)
	_, err := Check(ctx, s.R, []string{"ufw", "--force", "disable"})
	return err
}

// RuleInput is a new firewall rule.
type RuleInput struct {
	Action  string `json:"action"` // allow, deny, reject, limit
	Port    string `json:"port"`   // "8080" or "6000:6100"
	Proto   string `json:"proto"`  // tcp, udp, any
	From    string `json:"from"`   // "" / "any" or an IP / CIDR
	Comment string `json:"comment"`
}

var commentRe = regexp.MustCompile(`^[A-Za-z0-9 ._:/()@+-]{0,60}$`)

// parsePorts validates "80" or "6000:6100".
func parsePorts(spec string) (lo, hi int, err error) {
	a, b, isRange := strings.Cut(spec, ":")
	lo, err1 := strconv.Atoi(a)
	hi = lo
	var err2 error
	if isRange {
		hi, err2 = strconv.Atoi(b)
	}
	if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
		return 0, 0, invalid("port must be 1-65535 or a range like 6000:6100")
	}
	return lo, hi, nil
}

// ValidFrom normalises a source address ("" means anywhere).
func ValidFrom(from string) (string, error) {
	from = strings.TrimSpace(from)
	if from == "" || strings.EqualFold(from, "any") || strings.EqualFold(from, "anywhere") {
		return "", nil
	}
	if p, err := netip.ParsePrefix(from); err == nil {
		return p.Masked().String(), nil
	}
	if a, err := netip.ParseAddr(from); err == nil {
		return a.String(), nil
	}
	return "", invalid("source must be an IP address or CIDR (e.g. 203.0.113.7 or 10.0.0.0/8)")
}

// ruleArgs builds the ufw argv for a rule.
func ruleArgs(in RuleInput, insertFirst bool) ([]string, error) {
	action := strings.ToLower(in.Action)
	switch action {
	case "allow", "deny", "reject", "limit":
	default:
		return nil, invalid("action must be allow, deny, reject or limit")
	}
	lo, hi, err := parsePorts(strings.TrimSpace(in.Port))
	if err != nil {
		return nil, err
	}
	proto := strings.ToLower(in.Proto)
	if proto == "" {
		proto = "any"
	}
	if proto != "tcp" && proto != "udp" && proto != "any" {
		return nil, invalid("protocol must be tcp, udp or any")
	}
	if lo != hi && proto == "any" {
		return nil, invalid("a port range needs a protocol (tcp or udp)")
	}
	if action == "limit" && proto != "tcp" {
		return nil, invalid("rate limiting works for tcp only")
	}
	from, err := ValidFrom(in.From)
	if err != nil {
		return nil, err
	}
	if !commentRe.MatchString(in.Comment) {
		return nil, invalid("comment may only use letters, digits, spaces and . _ : / ( ) @ + -")
	}
	argv := []string{"ufw"}
	if insertFirst {
		argv = append(argv, "insert", "1")
	}
	src := "any"
	if from != "" {
		src = from
	}
	port := strconv.Itoa(lo)
	if hi != lo {
		port += ":" + strconv.Itoa(hi)
	}
	argv = append(argv, action, "from", src, "to", "any", "port", port)
	if proto != "any" {
		argv = append(argv, "proto", proto)
	}
	if c := strings.TrimSpace(in.Comment); c != "" {
		argv = append(argv, "comment", c)
	}
	return argv, nil
}

// AddRule adds a ufw rule. Deny/reject rules go first so they take effect
// before broader allows, and may not cover a protected port from anywhere.
func (s *Service) AddRule(ctx context.Context, in RuleInput, protected []ProtectedPort) error {
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	action := strings.ToLower(in.Action)
	blocking := action == "deny" || action == "reject"
	if blocking {
		lo, hi, err := parsePorts(strings.TrimSpace(in.Port))
		if err != nil {
			return err
		}
		proto := strings.ToLower(in.Proto)
		for _, p := range protected {
			if p.Port >= lo && p.Port <= hi && (proto == "" || proto == "any" || proto == p.Proto) {
				return invalid("this rule would block %s (port %d/%s); Docklet keeps it open", p.Why, p.Port, p.Proto)
			}
		}
	}
	st, err := s.FirewallStatus(ctx)
	if err != nil {
		return err
	}
	if !st.Installed {
		return invalid("ufw is not installed")
	}
	argv, err := ruleArgs(in, blocking && len(st.Rules) > 0)
	if err != nil {
		return err
	}
	_, err = Check(ctx, s.R, argv)
	return err
}

// DeleteRule deletes rule num, provided it still reads text (rule numbers
// shift when rules change) and doesn't remove the last allow of a protected port.
func (s *Service) DeleteRule(ctx context.Context, num int, text string, protected []ProtectedPort) (FWRule, error) {
	if err := s.requireSupported(ctx); err != nil {
		return FWRule{}, err
	}
	st, err := s.FirewallStatus(ctx)
	if err != nil {
		return FWRule{}, err
	}
	idx := -1
	for i, r := range st.Rules {
		if r.Num == num {
			idx = i
		}
	}
	if idx < 0 {
		return FWRule{}, invalid("rule %d doesn't exist", num)
	}
	rule := st.Rules[idx]
	if strings.TrimSpace(text) != rule.Text {
		return FWRule{}, invalid("the rules changed since you loaded them; reload and try again")
	}
	if st.Active {
		rest := st
		rest.Rules = append(append([]FWRule{}, st.Rules[:idx]...), st.Rules[idx+1:]...)
		for _, p := range protected {
			if st.allowsProto(p.Port, p.Proto) && !rest.allowsProto(p.Port, p.Proto) {
				return FWRule{}, invalid("this is the rule that lets %s (port %d/%s) in; Docklet keeps it", p.Why, p.Port, p.Proto)
			}
		}
	}
	_, err = Check(ctx, s.R, []string{"ufw", "--force", "delete", strconv.Itoa(num)})
	return rule, err
}

// SetDefaults sets the default incoming/outgoing policies.
func (s *Service) SetDefaults(ctx context.Context, incoming, outgoing string, protected []ProtectedPort) error {
	ok := func(v string) bool { return v == "allow" || v == "deny" || v == "reject" }
	if !ok(incoming) || !ok(outgoing) {
		return invalid("policies must be allow, deny or reject")
	}
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	st, err := s.FirewallStatus(ctx)
	if err != nil {
		return err
	}
	if incoming != "allow" {
		if _, err := s.ensureAllowed(ctx, st, protected); err != nil {
			return err
		}
	}
	_, err = Script(ctx, s.R, `set -e; ufw default "$IN" incoming >/dev/null; ufw default "$OUT" outgoing >/dev/null`, "IN="+incoming, "OUT="+outgoing)
	return err
}
