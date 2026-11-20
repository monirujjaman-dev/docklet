package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/auth"
	"github.com/monirujjaman-dev/docklet/internal/caddy"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// Settings keys for the panel domain.
const (
	SettingPanelDomain  = "panel_domain"
	SettingACMEEmail    = "acme_email"
	SettingPanelStatus  = "panel_status"
	SettingPanelMessage = "panel_message"
)

// Panel status values.
const (
	panelNone    = "none"
	panelPending = "pending"
	panelActive  = "active"
	panelFailed  = "failed"
)

type panelState struct {
	Domain    string        `json:"domain"`
	ACMEEmail string        `json:"acmeEmail"`
	Status    string        `json:"status"`
	Message   string        `json:"message,omitempty"`
	Listen    string        `json:"listen"`
	Caddy     *caddy.Status `json:"caddy"`
}

func getSetting(ctx context.Context, q *db.Queries, key string) (string, error) {
	v, err := q.GetSetting(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func putSetting(ctx context.Context, q *db.Queries, key, value string, now time.Time) error {
	return q.PutSetting(ctx, db.PutSettingParams{Key: key, Value: value, UpdatedAt: now.Unix()})
}

func (s *Server) panelState(ctx context.Context) (panelState, error) {
	var st panelState
	var err error
	if st.Domain, err = getSetting(ctx, s.st.Q, SettingPanelDomain); err != nil {
		return st, err
	}
	if st.ACMEEmail, err = getSetting(ctx, s.st.Q, SettingACMEEmail); err != nil {
		return st, err
	}
	if st.Status, err = getSetting(ctx, s.st.Q, SettingPanelStatus); err != nil {
		return st, err
	}
	if st.Message, err = getSetting(ctx, s.st.Q, SettingPanelMessage); err != nil {
		return st, err
	}
	if st.Domain == "" || st.Status == "" {
		st.Status = panelNone
	}
	st.Listen = s.lm.addr()
	if s.caddy != nil {
		cs := s.caddy.Status()
		st.Caddy = &cs
	}
	return st, nil
}

// DesiredRoutes implements caddy.Source.
func (s *Server) DesiredRoutes(ctx context.Context) ([]caddy.Route, caddy.Options, error) {
	domain, err := getSetting(ctx, s.st.Q, SettingPanelDomain)
	if err != nil {
		return nil, caddy.Options{}, err
	}
	email, err := getSetting(ctx, s.st.Q, SettingACMEEmail)
	if err != nil {
		return nil, caddy.Options{}, err
	}
	opts := s.caddyOpts
	opts.ACMEEmail = email
	var routes []caddy.Route
	if up := s.panelUpstream(); domain != "" && up != "" {
		routes = append(routes, caddy.Route{ID: "panel", Domain: domain, Upstream: up})
	}
	appRoutes, err := s.st.Q.ListRoutableDomains(ctx)
	if err != nil {
		return nil, opts, err
	}
	for _, r := range appRoutes {
		if r.Domain == domain {
			continue // the panel wins if a domain was reused
		}
		routes = append(routes, caddy.Route{
			ID:       fmt.Sprintf("app%d-%s", r.AppID, r.Domain),
			Domain:   r.Domain,
			Upstream: net.JoinHostPort(r.ContainerIp, strconv.FormatInt(r.Port, 10)),
		})
	}
	return routes, opts, nil
}

// validateDomain accepts a bare DNS hostname such as panel.example.com.
func validateDomain(d string) (string, error) {
	d = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
	if d == "" || len(d) > 253 {
		return "", errors.New("domain is required (max 253 characters)")
	}
	if net.ParseIP(d) != nil {
		return "", errors.New("use a domain name, not an IP address")
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return "", errors.New("use a fully qualified domain like panel.example.com")
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", fmt.Errorf("invalid domain label %q", l)
		}
		for _, c := range l {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", fmt.Errorf("invalid character %q in domain", c)
			}
		}
	}
	return d, nil
}

// GET /api/settings/panel
func (s *Server) handleGetPanel(w http.ResponseWriter, r *http.Request) error {
	st, err := s.panelState(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

// PUT /api/settings/panel
func (s *Server) handlePutPanel(w http.ResponseWriter, r *http.Request) error {
	if s.caddy == nil {
		return errf(http.StatusConflict, "Caddy integration is disabled (-caddy-admin off)")
	}
	var in struct {
		Domain    string `json:"domain"`
		ACMEEmail string `json:"acmeEmail"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	domain, err := validateDomain(in.Domain)
	if err != nil {
		return errf(http.StatusBadRequest, "%s", err.Error())
	}
	email := strings.TrimSpace(in.ACMEEmail)
	if email != "" {
		if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
			return errf(http.StatusBadRequest, "invalid ACME email address")
		}
	}

	ctx := r.Context()
	a := authFrom(ctx)
	now := s.now()
	reverify := true
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		prevDomain, err := getSetting(ctx, q, SettingPanelDomain)
		if err != nil {
			return err
		}
		prevStatus, err := getSetting(ctx, q, SettingPanelStatus)
		if err != nil {
			return err
		}
		// Changing only the e-mail keeps an already verified domain active.
		reverify = prevDomain != domain || prevStatus != panelActive
		for k, v := range map[string]string{SettingPanelDomain: domain, SettingACMEEmail: email} {
			if err := putSetting(ctx, q, k, v, now); err != nil {
				return err
			}
		}
		if reverify {
			if err := putSetting(ctx, q, SettingPanelStatus, panelPending, now); err != nil {
				return err
			}
			if err := putSetting(ctx, q, SettingPanelMessage, "Waiting for Caddy to obtain a certificate…", now); err != nil {
				return err
			}
		}
		_, err = audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionPanelDomainSet,
			Target: "panel:" + domain, IP: clientIP(r),
			Details: map[string]any{"domain": domain, "acmeEmail": email, "previous": prevDomain},
		}, now)
		return err
	})
	if err != nil {
		return err
	}
	s.caddy.Trigger()
	if reverify {
		// Caddy now routes only the new domain; keep direct access open until
		// it is verified so a bad domain cannot lock the admin out.
		if err := s.restorePublicListener(); err != nil {
			return err
		}
		s.startPanelVerify(domain)
	}
	st, err := s.panelState(ctx)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, st)
	return nil
}

// DELETE /api/settings/panel
func (s *Server) handleDeletePanel(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	a := authFrom(ctx)
	if err := clearPanel(ctx, s.st.Tx, audit.Entry{
		ActorID: ptr(a.User.ID), Actor: a.User.Username, IP: clientIP(r),
	}, s.now()); err != nil {
		return err
	}
	s.stopPanelVerify()
	if s.caddy != nil {
		s.caddy.Trigger()
	}
	if err := s.restorePublicListener(); err != nil {
		return err
	}
	st, err := s.panelState(ctx)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

// ClearPanel removes the panel domain (shared with the CLI).
func ClearPanel(ctx context.Context, tx func(context.Context, func(*db.Queries) error) error, actor string, now time.Time) error {
	return clearPanel(ctx, tx, audit.Entry{Actor: actor}, now)
}

func clearPanel(ctx context.Context, tx func(context.Context, func(*db.Queries) error) error, e audit.Entry, now time.Time) error {
	return tx(ctx, func(q *db.Queries) error {
		prev, err := getSetting(ctx, q, SettingPanelDomain)
		if err != nil {
			return err
		}
		for _, k := range []string{SettingPanelDomain, SettingPanelStatus, SettingPanelMessage} {
			if err := q.DeleteSetting(ctx, k); err != nil {
				return err
			}
		}
		e.Action = audit.ActionPanelDomainCleared
		e.Target = "panel:" + prev
		_, err = audit.Append(ctx, q, e, now)
		return err
	})
}

// GET /api/panel/probe?nonce=… — answers the HTTPS self-check. Unauthenticated
// by necessity; it only echoes the nonce of an in-flight verification.
func (s *Server) handlePanelProbe(w http.ResponseWriter, r *http.Request) {
	s.panelMu.Lock()
	nonce := s.panelNonce
	s.panelMu.Unlock()
	got := r.URL.Query().Get("nonce")
	if nonce == "" || got != nonce {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"nonce": nonce})
}

// defaultProber fetches url with normal certificate verification.
func defaultProber(ctx context.Context, url string) (string, error) {
	// #nosec G704 -- url is https://<admin-validated hostname>/api/panel/probe
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	res, err := hc.Do(req) // #nosec G704 -- see above
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTPS check returned %s", res.Status)
	}
	return string(b), nil
}

func (s *Server) stopPanelVerify() {
	s.panelMu.Lock()
	defer s.panelMu.Unlock()
	if s.verifyCancel != nil {
		s.verifyCancel()
		s.verifyCancel = nil
	}
	s.panelNonce = ""
}

// startPanelVerify checks in the background that https://domain reaches this
// Docklet through Caddy with a valid certificate. Only then is direct access
// restricted to localhost, so a DNS or firewall mistake cannot lock the admin out.
func (s *Server) startPanelVerify(domain string) {
	nonce, err := auth.NewToken()
	if err != nil {
		s.log.Error("panel verify: nonce", "err", err)
		return
	}
	s.stopPanelVerify()
	ctx, cancel := context.WithTimeout(s.baseCtx(), s.verifyTimeout)
	s.panelMu.Lock()
	s.panelNonce, s.verifyCancel = nonce, cancel
	s.panelMu.Unlock()

	go func() {
		defer cancel()
		url := "https://" + domain + "/api/panel/probe?nonce=" + nonce
		var lastErr error
		t := time.NewTicker(s.verifyInterval)
		defer t.Stop()
		for {
			body, err := s.prober(ctx, url)
			if err == nil && strings.Contains(body, nonce) {
				s.finishPanelVerify(domain, nonce, nil)
				return
			}
			if err == nil {
				err = errors.New("HTTPS check reached a different server")
			}
			lastErr = err
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					s.finishPanelVerify(domain, nonce, lastErr)
				}
				return
			case <-t.C:
			}
		}
	}()
}

func (s *Server) finishPanelVerify(domain, nonce string, verr error) {
	s.panelMu.Lock()
	if s.panelNonce != nonce { // superseded by a newer request
		s.panelMu.Unlock()
		return
	}
	s.panelNonce, s.verifyCancel = "", nil
	s.panelMu.Unlock()

	ctx := context.Background()
	now := s.now()
	status, msg, action := panelActive, "", audit.ActionPanelVerified
	if verr != nil {
		status, action = panelFailed, audit.ActionPanelVerifyFailed
		msg = fmt.Sprintf("Could not reach https://%s: %v. Check that the DNS A/AAAA record points to this server and ports 80/443 are open, then save again.", domain, verr)
	}
	applied := false
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		cur, err := getSetting(ctx, q, SettingPanelDomain)
		if err != nil || cur != domain {
			return err
		}
		applied = true
		if err := putSetting(ctx, q, SettingPanelStatus, status, now); err != nil {
			return err
		}
		if err := putSetting(ctx, q, SettingPanelMessage, msg, now); err != nil {
			return err
		}
		details := map[string]any{"domain": domain}
		if verr != nil {
			details["error"] = verr.Error()
		}
		_, err = audit.Append(ctx, q, audit.Entry{Actor: "system", Action: action, Target: "panel:" + domain, Details: details}, now)
		return err
	})
	if err != nil {
		s.log.Error("panel verify: save result", "err", err)
		return
	}
	if !applied {
		return
	}
	if verr != nil {
		s.log.Warn("panel domain verification failed", "domain", domain, "err", verr)
		return
	}
	s.log.Info("panel domain verified", "domain", domain)
	if err := s.restrictToLoopback(); err != nil {
		s.log.Error("could not restrict listener to localhost", "err", err)
	}
}

func (s *Server) panelUpstream() string {
	if s.cfg.PanelUpstream != "" {
		return s.cfg.PanelUpstream
	}
	if port := s.lm.port(); port != 0 {
		return loopbackAddr(port)
	}
	return ""
}

// restrictToLoopback rebinds to 127.0.0.1 on the same port, if not already.
// With an explicit PanelUpstream (container deployments) Caddy reaches us over
// a Docker network, so the listener is left alone.
func (s *Server) restrictToLoopback() error {
	if s.cfg.PanelUpstream != "" {
		return nil
	}
	cur := s.lm.addr()
	if cur == "" || isLoopbackAddr(cur) {
		return nil
	}
	addr := loopbackAddr(s.lm.port())
	if err := s.lm.listen(addr); err != nil {
		return err
	}
	s.log.Info("direct access disabled; dashboard now served only through Caddy", "listen", addr)
	return nil
}

// restorePublicListener returns to the configured address after the panel
// domain is removed.
func (s *Server) restorePublicListener() error {
	cur := s.lm.addr()
	if cur == "" || !isLoopbackAddr(cur) || isLoopbackAddr(s.publicAddr()) {
		return nil
	}
	if err := s.lm.listen(s.publicAddr()); err != nil {
		return err
	}
	s.log.Info("direct access re-enabled", "listen", s.lm.addr())
	return nil
}

// publicAddr is the configured address, with port 0 pinned to the port in
// use so tests and restores keep the same port.
func (s *Server) publicAddr() string {
	host, port, err := net.SplitHostPort(s.cfg.Addr)
	if err != nil {
		return s.cfg.Addr
	}
	if port == "0" && s.lm.port() != 0 {
		return net.JoinHostPort(host, fmt.Sprint(s.lm.port()))
	}
	return s.cfg.Addr
}
