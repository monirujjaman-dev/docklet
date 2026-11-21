package server

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/host"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// securityState holds the change waiting for confirmation. Only Docklet's
// copy is lost on restart; the host's revert job still runs.
type securityState struct {
	mu      sync.Mutex
	pending *host.Pending

	allowMu   sync.Mutex
	allowLast string // last applied allowlist, "" = none
}

func (s *Server) confirmWindow() time.Duration {
	// For end-to-end tests: waiting 5 minutes for a revert is slow.
	if v, err := strconv.Atoi(os.Getenv("DOCKLET_CONFIRM_WINDOW_SEC")); err == nil && v >= 10 && v <= 3600 {
		return time.Duration(v) * time.Second
	}
	return host.DefaultConfirmWindow
}

// envPort reads a port number from the environment.
func envPort(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 && v < 65536 {
		return v
	}
	return def
}

// webPorts are the host ports Docklet's HTTP(S) and panel listen on. In the
// container they are the published ports, passed in by compose.yaml.
func (s *Server) webPorts() (httpPort, httpsPort, panel int) {
	return envPort("DOCKLET_HTTP_PORT", 80), envPort("DOCKLET_HTTPS_PORT", 443), envPort("DOCKLET_PANEL_PORT", s.lm.port())
}

func (s *Server) protected(sshPorts []int) []host.ProtectedPort {
	hp, hs, pp := s.webPorts()
	return host.Protected(sshPorts, hp, hs, pp)
}

// reservedForSSH are ports SSH may not move to.
func (s *Server) reservedForSSH(ctx context.Context) []int {
	hp, hs, pp := s.webPorts()
	out := []int{hp, hs, pp}
	if dbs, err := s.st.Q.ListDatabases(ctx); err == nil {
		for _, d := range dbs {
			if d.PublicPort > 0 {
				out = append(out, int(d.PublicPort))
			}
		}
	}
	return out
}

// pendingView is what the UI shows for a pending change.
type pendingView struct {
	Kind        string    `json:"kind"`
	Description string    `json:"description"`
	Deadline    time.Time `json:"deadline"`
	SecondsLeft int       `json:"secondsLeft"`
}

func (s *Server) currentPending() *pendingView {
	s.sec.mu.Lock()
	defer s.sec.mu.Unlock()
	p := s.sec.pending
	if p == nil {
		return nil
	}
	left := time.Until(p.Deadline)
	if left <= 0 {
		s.sec.pending = nil // the host has reverted it
		return nil
	}
	return &pendingView{Kind: p.Kind, Description: p.Description, Deadline: p.Deadline, SecondsLeft: int(left.Seconds())}
}

func (s *Server) setPending(p host.Pending) {
	s.sec.mu.Lock()
	s.sec.pending = &p
	s.sec.mu.Unlock()
}

func (s *Server) requireNoPending() error {
	if p := s.currentPending(); p != nil {
		return errf(http.StatusConflict, "confirm or undo the pending change first (%s)", p.Description)
	}
	return nil
}

// GET /api/server/security/pending
func (s *Server) handleSecurityPending(w http.ResponseWriter, _ *http.Request) error {
	writeJSON(w, http.StatusOK, map[string]any{"pending": s.currentPending()})
	return nil
}

// POST /api/server/security/pending/confirm — called from the browser that
// can still reach Docklet after the change, which proves it works.
func (s *Server) handleSecurityConfirm(w http.ResponseWriter, r *http.Request) error {
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		s.sec.mu.Lock()
		p := s.sec.pending
		s.sec.mu.Unlock()
		if p == nil || time.Now().After(p.Deadline) {
			return nil, errf(http.StatusConflict, "nothing to confirm: the change was already undone")
		}
		if still, err := h.RevertPending(r.Context()); err == nil && !still {
			s.sec.mu.Lock()
			s.sec.pending = nil
			s.sec.mu.Unlock()
			return nil, errf(http.StatusConflict, "nothing to confirm: the host already undid the change")
		}
		var err error
		switch p.Kind {
		case "ssh":
			err = h.ConfirmSSH(r.Context(), *p)
		default:
			err = h.CancelRevert(r.Context())
		}
		if err != nil {
			return nil, err
		}
		s.sec.mu.Lock()
		s.sec.pending = nil
		s.sec.mu.Unlock()
		if err := s.auditServer(r, audit.ActionServerChangeConfirmed, map[string]any{"kind": p.Kind, "change": p.Description}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
}

// POST /api/server/security/pending/undo — revert now.
func (s *Server) handleSecurityUndo(w http.ResponseWriter, r *http.Request) error {
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		s.sec.mu.Lock()
		p := s.sec.pending
		s.sec.pending = nil
		s.sec.mu.Unlock()
		if err := h.RevertNow(r.Context()); err != nil {
			return nil, err
		}
		desc := ""
		if p != nil {
			desc = p.Description
		}
		if err := s.auditServer(r, audit.ActionServerChangeUndone, map[string]any{"change": desc}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
}

// --- SSH ------------------------------------------------------------------------------

// GET /api/server/ssh
func (s *Server) handleSSHStatus(w http.ResponseWriter, r *http.Request) error {
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		st, err := h.SSHStatus(r.Context())
		if err != nil {
			return nil, err
		}
		keys, err := h.AuthorizedKeys(r.Context())
		if keys == nil {
			keys = []host.AuthorizedKey{}
		}
		return map[string]any{"ssh": st, "keys": keys, "pending": s.currentPending()}, err
	})
}

// PUT /api/server/ssh {port, permitRootLogin, passwordAuthentication, password}
func (s *Server) handleSSHChange(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		host.SSHSettings
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := checkPassword(authFrom(r.Context()).User, in.Password); err != nil {
		return err
	}
	if err := s.requireNoPending(); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		before, err := h.SSHStatus(r.Context())
		if err != nil {
			return nil, err
		}
		p, err := h.ChangeSSH(r.Context(), in.SSHSettings, s.reservedForSSH(r.Context()), s.confirmWindow())
		if err != nil {
			return nil, err
		}
		s.setPending(p)
		if err := s.auditServer(r, audit.ActionServerSSH, map[string]any{
			"from": map[string]any{"ports": before.Ports, "permitRootLogin": before.PermitRootLogin, "passwordAuthentication": before.PasswordAuth},
			"to":   in.SSHSettings, "confirmBy": p.Deadline,
		}); err != nil {
			return nil, err
		}
		return map[string]any{"pending": s.currentPending()}, nil
	})
}

// POST /api/server/ssh/keys {user, key}
func (s *Server) handleSSHAddKey(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		User string `json:"user"`
		Key  string `json:"key"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		if err := h.AddAuthorizedKey(r.Context(), in.User, in.Key); err != nil {
			return nil, err
		}
		keys, err := h.AuthorizedKeys(r.Context())
		fp, _ := host.KeyFingerprint(in.Key)
		if aerr := s.auditServer(r, audit.ActionServerSSHKeyAdded, map[string]any{"user": in.User, "fingerprint": fp}); aerr != nil {
			return nil, aerr
		}
		return map[string]any{"keys": keys}, err
	})
}

// POST /api/server/ssh/keys/delete {user, fingerprint}
func (s *Server) handleSSHRemoveKey(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		User        string `json:"user"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		if err := h.RemoveAuthorizedKey(r.Context(), in.User, in.Fingerprint); err != nil {
			return nil, err
		}
		if err := s.auditServer(r, audit.ActionServerSSHKeyRemoved, map[string]any{"user": in.User, "fingerprint": in.Fingerprint}); err != nil {
			return nil, err
		}
		keys, err := h.AuthorizedKeys(r.Context())
		return map[string]any{"keys": keys}, err
	})
}

// --- Firewall -------------------------------------------------------------------------

func (s *Server) sshPorts(ctx context.Context, h *host.Service) []int {
	if st, err := h.SSHStatus(ctx); err == nil && len(st.Ports) > 0 {
		return st.Ports
	}
	return []int{22}
}

// GET /api/server/firewall
func (s *Server) handleFirewall(w http.ResponseWriter, r *http.Request) error {
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		st, err := h.FirewallStatus(r.Context())
		if err != nil {
			return nil, err
		}
		if st.Rules == nil {
			st.Rules = []host.FWRule{}
		}
		return map[string]any{"firewall": st, "protected": s.protected(s.sshPorts(r.Context(), h)), "pending": s.currentPending()}, nil
	})
}

// POST /api/server/firewall/install
func (s *Server) handleFirewallInstall(w http.ResponseWriter, r *http.Request) error {
	return s.installPackages(w, r, "firewall", "ufw")
}

func (s *Server) installPackages(w http.ResponseWriter, r *http.Request, what string, pkgs ...string) error {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		if err := h.InstallPackages(ctx, pkgs...); err != nil {
			return nil, err
		}
		if err := s.auditServer(r, audit.ActionServerPackageInstalled, map[string]any{"for": what, "packages": pkgs}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
}

// POST /api/server/firewall/enable {password}
func (s *Server) handleFirewallEnable(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := checkPassword(authFrom(r.Context()).User, in.Password); err != nil {
		return err
	}
	if err := s.requireNoPending(); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		prot := s.protected(s.sshPorts(r.Context(), h))
		p, err := h.EnableFirewall(r.Context(), prot, s.confirmWindow())
		if err != nil {
			return nil, err
		}
		s.setPending(p)
		if err := s.auditServer(r, audit.ActionServerFirewall, map[string]any{"from": "inactive", "to": "active", "allowed": prot, "confirmBy": p.Deadline}); err != nil {
			return nil, err
		}
		return map[string]any{"pending": s.currentPending()}, nil
	})
}

// POST /api/server/firewall/disable {password}
func (s *Server) handleFirewallDisable(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := checkPassword(authFrom(r.Context()).User, in.Password); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		if err := h.DisableFirewall(r.Context()); err != nil {
			return nil, err
		}
		s.sec.mu.Lock()
		if s.sec.pending != nil && s.sec.pending.Kind == "firewall" {
			s.sec.pending = nil
		}
		s.sec.mu.Unlock()
		if err := s.auditServer(r, audit.ActionServerFirewall, map[string]any{"from": "active", "to": "inactive"}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
}

// POST /api/server/firewall/rules {action, port, proto, from, comment}
func (s *Server) handleFirewallAddRule(w http.ResponseWriter, r *http.Request) error {
	var in host.RuleInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		if err := h.AddRule(r.Context(), in, s.protected(s.sshPorts(r.Context(), h))); err != nil {
			return nil, err
		}
		if err := s.auditServer(r, audit.ActionServerFirewallRuleAdded, map[string]any{"rule": in}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
}

// POST /api/server/firewall/rules/delete {num, text}
func (s *Server) handleFirewallDeleteRule(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Num  int    `json:"num"`
		Text string `json:"text"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		rule, err := h.DeleteRule(r.Context(), in.Num, in.Text, s.protected(s.sshPorts(r.Context(), h)))
		if err != nil {
			return nil, err
		}
		if err := s.auditServer(r, audit.ActionServerFirewallRuleDeleted, map[string]any{"rule": rule.Text}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
}

// PUT /api/server/firewall/defaults {incoming, outgoing}
func (s *Server) handleFirewallDefaults(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Incoming string `json:"incoming"`
		Outgoing string `json:"outgoing"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		before, err := h.FirewallStatus(r.Context())
		if err != nil {
			return nil, err
		}
		if err := h.SetDefaults(r.Context(), in.Incoming, in.Outgoing, s.protected(s.sshPorts(r.Context(), h))); err != nil {
			return nil, err
		}
		if err := s.auditServer(r, audit.ActionServerFirewallDefaults, map[string]any{
			"from": map[string]string{"incoming": before.DefaultIncoming, "outgoing": before.DefaultOutgoing}, "to": in,
		}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
}

// --- Fail2Ban -------------------------------------------------------------------------

// GET /api/server/fail2ban
func (s *Server) handleFail2Ban(w http.ResponseWriter, r *http.Request) error {
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		st, err := h.Fail2BanStatus(r.Context())
		if st.Jails == nil {
			st.Jails = []host.Jail{}
		}
		if st.Settings.IgnoreIPs == nil {
			st.Settings.IgnoreIPs = []string{}
		}
		return st, err
	})
}

// POST /api/server/fail2ban/install
func (s *Server) handleFail2BanInstall(w http.ResponseWriter, r *http.Request) error {
	return s.installPackages(w, r, "fail2ban", "fail2ban", "python3-systemd")
}

// PUT /api/server/fail2ban {enabled, maxRetry, findTimeSec, banTimeSec, ignoreIps}
func (s *Server) handleFail2BanConfigure(w http.ResponseWriter, r *http.Request) error {
	var in host.Fail2BanSettings
	if err := readJSON(r, &in); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		before, _ := h.Fail2BanStatus(r.Context())
		if err := h.ConfigureFail2Ban(r.Context(), in, s.sshPorts(r.Context(), h)); err != nil {
			return nil, err
		}
		in.Configured = true
		if err := s.auditServer(r, audit.ActionServerFail2Ban, map[string]any{"from": before.Settings, "to": in}); err != nil {
			return nil, err
		}
		return h.Fail2BanStatus(r.Context())
	})
}

// POST /api/server/fail2ban/unban {jail, ip}
func (s *Server) handleFail2BanUnban(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Jail string `json:"jail"`
		IP   string `json:"ip"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.Jail == "" {
		in.Jail = "sshd"
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		if err := h.Unban(r.Context(), in.Jail, in.IP); err != nil {
			return nil, err
		}
		if err := s.auditServer(r, audit.ActionServerUnban, map[string]any{"jail": in.Jail, "ip": in.IP}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
}

// --- Database port allowlist ----------------------------------------------------------

// PUT /api/databases/{id}/allowlist {"sources": "203.0.113.7, 10.0.0.0/8"}
func (s *Server) handleDatabaseAllowlist(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	d, err := s.dbFromPath(r)
	if err != nil {
		return err
	}
	var in struct {
		Sources string `json:"sources"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	srcs, err := host.ParseSources(in.Sources)
	if err != nil {
		return hostErr(err)
	}
	h, err := s.hostService(r.Context())
	if err != nil && len(srcs) > 0 {
		return err
	}
	ctx := r.Context()
	allow := strings.Join(srcs, ",")
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.SetDatabasePublicAllow(ctx, db.SetDatabasePublicAllowParams{PublicAllow: allow, UpdatedAt: s.now().Unix(), ID: d.ID}); err != nil {
			return err
		}
		return s.auditDB(ctx, q, r, d.ID, audit.ActionDatabaseAllowlist, map[string]any{"from": d.PublicAllow, "to": allow})
	}); err != nil {
		return err
	}
	if h != nil {
		if err := s.applyAllowlist(ctx, h, true); err != nil {
			return errf(http.StatusBadGateway, "saved, but the firewall rules couldn't be applied: %v", err)
		}
	}
	return s.handleGetDatabase(w, r)
}

// refreshAllowlist re-applies the allowlist after a public port changed.
func (s *Server) refreshAllowlist(ctx context.Context) {
	h, err := s.hostService(ctx)
	if err != nil {
		return
	}
	if err := s.applyAllowlist(ctx, h, false); err != nil {
		s.log.Warn("database port allowlist", "err", err)
	}
}

// allowRules builds the allowlist from the databases table.
func (s *Server) allowRules(ctx context.Context) ([]host.PortAllow, error) {
	dbs, err := s.st.Q.ListDatabases(ctx)
	if err != nil {
		return nil, err
	}
	var rules []host.PortAllow
	for _, d := range dbs {
		if d.PublicPort > 0 && d.PublicAllow != "" {
			rules = append(rules, host.PortAllow{Port: int(d.PublicPort), Sources: strings.Split(d.PublicAllow, ",")})
		}
	}
	return rules, nil
}

// applyAllowlist writes the rules to the host when they changed (or force).
func (s *Server) applyAllowlist(ctx context.Context, h *host.Service, force bool) error {
	s.sec.allowMu.Lock()
	defer s.sec.allowMu.Unlock()
	rules, err := s.allowRules(ctx)
	if err != nil {
		return err
	}
	key := ""
	for _, r := range rules {
		key += strconv.Itoa(r.Port) + "=" + strings.Join(r.Sources, ",") + " "
	}
	if !force && key == "" && s.sec.allowLast == "" {
		return nil // nothing to enforce, nothing to clean up
	}
	if err := h.ApplyPortAllowlist(ctx, rules); err != nil {
		return err
	}
	s.sec.allowLast = key
	return nil
}

// runAllowlist re-applies the allowlist periodically: a host reboot or a
// Docker restart can drop the iptables rules.
func (s *Server) runAllowlist(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	first := true
	for {
		if rules, err := s.allowRules(ctx); err == nil && (len(rules) > 0 || s.sec.allowLast != "") {
			if h, err := s.hostService(ctx); err == nil {
				if err := s.applyAllowlist(ctx, h, first); err != nil {
					s.log.Warn("database port allowlist", "err", err)
				}
				first = false
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
