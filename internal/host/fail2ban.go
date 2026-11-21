package host

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

const f2bJail = "/etc/fail2ban/jail.d/docklet.local"

// Fail2BanSettings configures the sshd jail.
type Fail2BanSettings struct {
	Enabled    bool     `json:"enabled"`
	MaxRetry   int      `json:"maxRetry"`
	FindTime   int      `json:"findTimeSec"`
	BanTime    int      `json:"banTimeSec"` // -1 = forever
	IgnoreIPs  []string `json:"ignoreIps"`
	Configured bool     `json:"configured"` // Docklet's jail file exists
}

// Fail2BanStatus is fail2ban's state.
type Fail2BanStatus struct {
	Installed bool             `json:"installed"`
	Running   bool             `json:"running"`
	Settings  Fail2BanSettings `json:"settings"`
	Jails     []Jail           `json:"jails"`
}

// Jail is an active jail with its bans.
type Jail struct {
	Name        string   `json:"name"`
	Failed      int      `json:"currentlyFailed"`
	TotalFailed int      `json:"totalFailed"`
	Banned      []string `json:"banned"`
	TotalBanned int      `json:"totalBanned"`
}

const f2bStatusScript = `command -v fail2ban-client >/dev/null || { echo installed=0; exit 0; }
echo installed=1
systemctl is-active -q fail2ban && echo running=1
if [ -f ` + f2bJail + ` ]; then echo configured=1; sed -n 's/^[[:space:]]*\(enabled\|maxretry\|findtime\|bantime\|ignoreip\)[[:space:]]*=[[:space:]]*/cfg.\1=/p' ` + f2bJail + `; fi
if systemctl is-active -q fail2ban; then
  for j in $(fail2ban-client status 2>/dev/null | sed -n 's/.*Jail list:[[:space:]]*//p' | tr ',' ' '); do
    fail2ban-client status "$j" 2>/dev/null | sed -e "s/^[^A-Za-z]*/jail.$j./" -e 's/:[[:space:]]*/=/'
  done
fi
true`

// Fail2BanStatus reads fail2ban's configuration and bans.
func (s *Service) Fail2BanStatus(ctx context.Context) (Fail2BanStatus, error) {
	out, err := Script(ctx, s.R, f2bStatusScript)
	if err != nil {
		return Fail2BanStatus{}, err
	}
	return parseF2B(out), nil
}

func parseF2B(out string) Fail2BanStatus {
	st := Fail2BanStatus{Settings: Fail2BanSettings{MaxRetry: 5, FindTime: 600, BanTime: 3600}}
	jails := map[string]*Jail{}
	var order []string
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch {
		case k == "installed":
			st.Installed = v == "1"
		case k == "running":
			st.Running = true
		case k == "configured":
			st.Settings.Configured = true
		case k == "cfg.enabled":
			st.Settings.Enabled = v == "true"
		case k == "cfg.maxretry":
			st.Settings.MaxRetry = int(atoi64(v))
		case k == "cfg.findtime":
			st.Settings.FindTime = int(atoi64(v))
		case k == "cfg.bantime":
			st.Settings.BanTime = int(atoi64(v))
		case k == "cfg.ignoreip":
			for _, ip := range strings.Fields(v) {
				if ip != "127.0.0.1/8" && ip != "::1" {
					st.Settings.IgnoreIPs = append(st.Settings.IgnoreIPs, ip)
				}
			}
		case strings.HasPrefix(k, "jail."):
			rest := strings.TrimPrefix(k, "jail.")
			name, field, ok := strings.Cut(rest, ".")
			if !ok {
				continue
			}
			j := jails[name]
			if j == nil {
				j = &Jail{Name: name, Banned: []string{}}
				jails[name] = j
				order = append(order, name)
			}
			switch field {
			case "Currently failed":
				j.Failed = int(atoi64(v))
			case "Total failed":
				j.TotalFailed = int(atoi64(v))
			case "Total banned":
				j.TotalBanned = int(atoi64(v))
			case "Banned IP list":
				j.Banned = append(j.Banned, strings.Fields(v)...)
			}
		}
	}
	for _, n := range order {
		st.Jails = append(st.Jails, *jails[n])
	}
	return st
}

// ConfigureFail2Ban writes Docklet's sshd jail and restarts fail2ban.
func (s *Service) ConfigureFail2Ban(ctx context.Context, in Fail2BanSettings, sshPorts []int) error {
	if in.MaxRetry < 1 || in.MaxRetry > 100 {
		return invalid("max retries must be 1-100")
	}
	if in.FindTime < 60 || in.FindTime > 7*86400 {
		return invalid("find time must be between 1 minute and 7 days")
	}
	if in.BanTime != -1 && (in.BanTime < 60 || in.BanTime > 365*86400) {
		return invalid("ban time must be between 1 minute and 1 year, or -1 for permanent")
	}
	ignore := []string{"127.0.0.1/8", "::1"}
	for _, ip := range in.IgnoreIPs {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if p, err := netip.ParsePrefix(ip); err == nil {
			ignore = append(ignore, p.Masked().String())
		} else if a, err := netip.ParseAddr(ip); err == nil {
			ignore = append(ignore, a.String())
		} else {
			return invalid("%q is not an IP address or CIDR", ip)
		}
	}
	if len(ignore) > 52 {
		return invalid("at most 50 ignored addresses")
	}
	if err := s.requireSupported(ctx); err != nil {
		return err
	}
	var ports []string
	for _, p := range sshPorts {
		ports = append(ports, strconv.Itoa(p))
	}
	if len(ports) == 0 {
		ports = []string{"ssh"}
	}
	conf := fmt.Sprintf(`# Managed by Docklet (Server → Security). Changes here are overwritten.
[sshd]
enabled = %t
backend = systemd
journalmatch = _COMM=sshd + _COMM=sshd-session
port = %s
maxretry = %d
findtime = %d
bantime = %d
ignoreip = %s
`, in.Enabled, strings.Join(ports, ","), in.MaxRetry, in.FindTime, in.BanTime, strings.Join(ignore, " "))
	_, err := Script(ctx, s.R, `set -e
command -v fail2ban-client >/dev/null || { echo "Fail2Ban is not installed" >&2; exit 3; }
mkdir -p /etc/fail2ban/jail.d
# Ban with whatever firewall tooling the host has.
if command -v nft >/dev/null; then ba=nftables-multiport; else ba=iptables-multiport; fi
printf '%sbanaction = %s\n' "$CONF" "$ba" > `+f2bJail+`.new && mv `+f2bJail+`.new `+f2bJail+`
fail2ban-client -t >/tmp/docklet-f2b.log 2>&1 || { tail -5 /tmp/docklet-f2b.log >&2; exit 3; }
systemctl enable -q fail2ban
systemctl restart fail2ban
for i in 1 2 3 4 5 6 7 8 9 10; do fail2ban-client ping >/dev/null 2>&1 && exit 0; sleep 1; done
echo "fail2ban did not start: $(journalctl -u fail2ban -n 5 --no-pager 2>/dev/null)" >&2; exit 1`, "CONF="+conf)
	return err
}

// Unban removes ip from jail.
func (s *Service) Unban(ctx context.Context, jail, ip string) error {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return invalid("invalid IP address")
	}
	if !userRe.MatchString(jail) {
		return invalid("invalid jail name")
	}
	_, err = Check(ctx, s.R, []string{"fail2ban-client", "set", jail, "unbanip", a.String()})
	return err
}
