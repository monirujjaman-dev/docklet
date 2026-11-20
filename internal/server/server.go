// Package server wires the HTTP API and the embedded dashboard.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/caddy"
	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
	"github.com/monirujjaman-dev/docklet/internal/version"
)

// Config controls how the HTTP server listens.
type Config struct {
	// Addr is the direct-access address (e.g. ":3000"). Once a panel domain
	// is verified, Docklet binds only 127.0.0.1 on the same port.
	Addr string
	// CaddyAdmin is the Caddy Admin API URL; empty disables Caddy integration.
	CaddyAdmin string
}

// Server is the Docklet control-plane HTTP server.
type Server struct {
	cfg           Config
	caddyInterval time.Duration
	log           *slog.Logger
	st            *store.Store
	box           *secrets.Box
	now           func() time.Time
	loginLimiter  *rateLimiter
	http          *http.Server
	lm            *listenerMgr

	caddy     *caddy.Reconciler
	caddyOpts caddy.Options

	docker   apps.Docker
	appsTune func(*apps.Service)
	apps     *apps.Service

	runCtx         context.Context
	prober         func(ctx context.Context, url string) (string, error)
	verifyTimeout  time.Duration
	verifyInterval time.Duration
	panelMu        sync.Mutex
	panelNonce     string
	verifyCancel   context.CancelFunc
}

// Option customises a Server (used by tests).
type Option func(*Server)

// WithClock overrides the time source.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithProber overrides the HTTPS self-check used to verify the panel domain.
func WithProber(p func(ctx context.Context, url string) (string, error)) Option {
	return func(s *Server) { s.prober = p }
}

// WithPanelVerify overrides how long and how often the panel domain is probed.
func WithPanelVerify(timeout, interval time.Duration) Option {
	return func(s *Server) { s.verifyTimeout, s.verifyInterval = timeout, interval }
}

// WithCaddyOptions sets extra Caddy options (ports), mainly for tests.
func WithCaddyOptions(o caddy.Options) Option { return func(s *Server) { s.caddyOpts = o } }

// WithDocker enables app deployments using the given Docker client.
func WithDocker(d apps.Docker) Option { return func(s *Server) { s.docker = d } }

// WithAppsTuning adjusts the apps service (timeouts etc.), mainly for tests.
func WithAppsTuning(fn func(*apps.Service)) Option { return func(s *Server) { s.appsTune = fn } }

// WithCaddyInterval sets the reconcile interval.
func WithCaddyInterval(d time.Duration) Option {
	return func(s *Server) { s.caddyInterval = d }
}

// New builds a Server with all routes registered.
func New(cfg Config, log *slog.Logger, st *store.Store, box *secrets.Box, opts ...Option) (*Server, error) {
	s := &Server{
		cfg: cfg, log: log, st: st, box: box, now: time.Now,
		prober: defaultProber, verifyTimeout: 90 * time.Second, verifyInterval: 3 * time.Second,
	}
	for _, o := range opts {
		o(s)
	}
	if cfg.CaddyAdmin != "" {
		client, err := caddy.NewClient(cfg.CaddyAdmin)
		if err != nil {
			return nil, err
		}
		s.caddy = caddy.NewReconciler(client, s, log, s.caddyInterval)
	}
	if s.docker != nil {
		var px apps.Proxy // stays a nil interface when Caddy is off
		if s.caddy != nil {
			px = s.caddy
		}
		s.apps = apps.New(st, s.docker, box, log, px)
		s.apps.SetClock(s.now)
		if s.appsTune != nil {
			s.appsTune(s.apps)
		}
	}
	// 10 attempts per minute per IP for login/setup, on top of per-account lockout.
	s.loginLimiter = newRateLimiter(10, time.Minute, func() time.Time { return s.now() })
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.lm = newListenerMgr(s.http)
	return s, nil
}

// Handler exposes the root handler (used by tests).
func (s *Server) Handler() http.Handler { return s.http.Handler }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	h := s.handler
	authed := func(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
		return s.requireAuth(h(fn))
	}

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/version", s.handleVersion)

	mux.HandleFunc("GET /api/setup/status", h(s.handleSetupStatus))
	mux.HandleFunc("POST /api/setup", h(s.handleSetup))
	mux.HandleFunc("POST /api/auth/login", h(s.handleLogin))
	mux.HandleFunc("POST /api/auth/logout", authed(s.handleLogout))
	mux.HandleFunc("GET /api/auth/me", authed(s.handleMe))

	mux.HandleFunc("POST /api/account/password", authed(s.handleChangePassword))
	mux.HandleFunc("POST /api/account/totp/setup", authed(s.handleTOTPSetup))
	mux.HandleFunc("POST /api/account/totp/enable", authed(s.handleTOTPEnable))
	mux.HandleFunc("POST /api/account/totp/disable", authed(s.handleTOTPDisable))

	mux.HandleFunc("GET /api/audit", authed(s.handleAuditList))
	mux.HandleFunc("GET /api/audit/verify", authed(s.handleAuditVerify))

	mux.HandleFunc("GET /api/settings/panel", authed(s.handleGetPanel))
	mux.HandleFunc("PUT /api/settings/panel", authed(s.handlePutPanel))
	mux.HandleFunc("DELETE /api/settings/panel", authed(s.handleDeletePanel))
	mux.HandleFunc("GET /api/panel/probe", s.handlePanelProbe)

	mux.HandleFunc("GET /api/projects", authed(s.handleListProjects))
	mux.HandleFunc("POST /api/projects", authed(s.handleCreateProject))
	mux.HandleFunc("DELETE /api/projects/{id}", authed(s.handleDeleteProject))
	mux.HandleFunc("POST /api/projects/{id}/apps", authed(s.handleCreateApp))
	mux.HandleFunc("GET /api/apps/{id}", authed(s.handleGetApp))
	mux.HandleFunc("PATCH /api/apps/{id}", authed(s.handleUpdateApp))
	mux.HandleFunc("DELETE /api/apps/{id}", authed(s.handleDeleteApp))
	mux.HandleFunc("PUT /api/apps/{id}/env", authed(s.handleUpdateEnv))
	mux.HandleFunc("PUT /api/apps/{id}/domains", authed(s.handleUpdateDomains))
	mux.HandleFunc("POST /api/apps/{id}/deploy", authed(s.handleDeploy))
	mux.HandleFunc("POST /api/apps/{id}/start", authed(s.handleLifecycle("start")))
	mux.HandleFunc("POST /api/apps/{id}/stop", authed(s.handleLifecycle("stop")))
	mux.HandleFunc("POST /api/apps/{id}/restart", authed(s.handleLifecycle("restart")))
	mux.HandleFunc("GET /api/apps/{id}/logs", authed(s.handleAppLogs))
	mux.HandleFunc("GET /api/deployments/{id}", authed(s.handleGetDeployment))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	mux.Handle("/", spaHandler())
	return securityHeaders(sameOrigin(mux))
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	s.panelMu.Lock()
	s.runCtx = ctx
	s.panelMu.Unlock()

	panel, err := s.panelState(ctx)
	if err != nil {
		return err
	}
	addr := s.cfg.Addr
	if panel.Status == panelActive {
		port, err := portOf(addr)
		if err != nil {
			return err
		}
		addr = loopbackAddr(port)
		s.log.Info("panel domain active; direct access limited to localhost", "domain", panel.Domain)
	}
	if err := s.lm.listen(addr); err != nil {
		return err
	}
	s.log.Info("listening", "addr", s.lm.addr())

	go s.janitor(ctx)
	if s.caddy != nil {
		go s.caddy.Run(ctx)
		if panel.Status == panelPending {
			s.startPanelVerify(panel.Domain)
		}
	}
	if s.apps != nil {
		go s.apps.Run(ctx)
	}

	select {
	case err := <-s.lm.errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.http.Shutdown(shutdownCtx)
	}
}

// janitor periodically removes expired sessions and idle rate-limit buckets.
func (s *Server) janitor(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := s.now()
			n, err := s.st.Q.DeleteExpiredSessions(ctx, db.DeleteExpiredSessionsParams{
				Now: now.Unix(), IdleBefore: now.Add(-sessionIdleTimeout).Unix(),
			})
			if err != nil {
				s.log.Warn("session cleanup failed", "err", err)
			} else if n > 0 {
				s.log.Debug("expired sessions removed", "count", n)
			}
			s.loginLimiter.prune()
		}
	}
}

// Addr returns the current listen address.
func (s *Server) Addr() string { return s.lm.addr() }

func (s *Server) baseCtx() context.Context {
	s.panelMu.Lock()
	defer s.panelMu.Unlock()
	if s.runCtx != nil {
		return s.runCtx
	}
	return context.Background()
}

func portOf(addr string) (int, error) {
	_, p, err := splitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	return p, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, version.Get())
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
