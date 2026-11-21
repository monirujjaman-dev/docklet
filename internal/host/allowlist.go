package host

import (
	"context"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// Ports that Docker publishes skip ufw: Docker's NAT rules send the traffic
// to the FORWARD chain, which ufw doesn't filter. Docker leaves DOCKER-USER
// for admins; Docklet keeps its rules in its own chain jumped from there.
//
// Rules match on conntrack's original destination port and state DNAT, so
// only traffic that came in through a published port is filtered; container
// to container traffic on the same port is untouched.

// AllowChain is Docklet's iptables chain.
const AllowChain = "DOCKLET-USER"

// PortAllow limits a published TCP port to some sources.
type PortAllow struct {
	Port    int      `json:"port"`
	Sources []string `json:"sources"`
}

// ParseSources validates a comma/space separated list of IPv4 addresses or
// CIDRs and returns it normalised.
func ParseSources(list string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		var p netip.Prefix
		if strings.Contains(f, "/") {
			var err error
			if p, err = netip.ParsePrefix(f); err != nil {
				return nil, invalid("%q is not an IP address or CIDR", f)
			}
			p = p.Masked()
		} else {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, invalid("%q is not an IP address or CIDR", f)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if !p.Addr().Is4() {
			return nil, invalid("%q: only IPv4 sources are supported", f)
		}
		if p.Bits() == 0 {
			return nil, invalid("0.0.0.0/0 allows everyone; clear the list instead")
		}
		s := p.String()
		if p.Bits() == 32 {
			s = p.Addr().String()
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) > 50 {
		return nil, invalid("at most 50 sources")
	}
	return out, nil
}

const allowScript = `set -e
command -v iptables >/dev/null || { echo "iptables is not installed on the host" >&2; exit 3; }
if ! iptables -n -L DOCKER-USER >/dev/null 2>&1; then
  [ -z "$RULES" ] && exit 0
  echo "Docker's DOCKER-USER chain is missing (is Docker managing iptables?)" >&2; exit 3
fi
iptables -N DOCKLET-USER 2>/dev/null || true
iptables -F DOCKLET-USER
for r in $RULES; do
  port=${r%%=*}; srcs=$(echo "${r#*=}" | tr ',' ' ')
  for s in $srcs; do
    iptables -A DOCKLET-USER -p tcp -m conntrack --ctstate DNAT --ctdir ORIGINAL --ctorigdstport "$port" -s "$s" -j RETURN
  done
  iptables -A DOCKLET-USER -p tcp -m conntrack --ctstate DNAT --ctdir ORIGINAL --ctorigdstport "$port" -j DROP
done
iptables -C DOCKER-USER -j DOCKLET-USER 2>/dev/null || iptables -I DOCKER-USER 1 -j DOCKLET-USER
iptables -S DOCKLET-USER | grep -c -- '-A DOCKLET-USER' || true`

// allowRulesEnv renders rules for allowScript (sorted, so output is stable).
func allowRulesEnv(rules []PortAllow) (string, error) {
	var parts []string
	for _, r := range rules {
		if r.Port < 1 || r.Port > 65535 {
			return "", invalid("invalid port %d", r.Port)
		}
		if len(r.Sources) == 0 {
			continue
		}
		srcs, err := ParseSources(strings.Join(r.Sources, ","))
		if err != nil {
			return "", err
		}
		parts = append(parts, strconv.Itoa(r.Port)+"="+strings.Join(srcs, ","))
	}
	sort.Strings(parts)
	return strings.Join(parts, " "), nil
}

// ApplyPortAllowlist replaces Docklet's chain with rules (idempotent).
func (s *Service) ApplyPortAllowlist(ctx context.Context, rules []PortAllow) error {
	env, err := allowRulesEnv(rules)
	if err != nil {
		return err
	}
	_, err = Script(ctx, s.R, allowScript, "RULES="+env)
	return err
}
