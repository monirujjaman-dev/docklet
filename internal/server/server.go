// Package server wires the HTTP API and the embedded dashboard.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
	"github.com/monirujjaman-dev/docklet/internal/version"
)

// Config controls how the HTTP server listens.
type Config struct {
	Addr string
}

// Server is the Docklet control-plane HTTP server.
type Server struct {
	cfg          Config
	log          *slog.Logger
	st           *store.Store
	box          *secrets.Box
	now          func() time.Time
	loginLimiter *rateLimiter
	http         *http.Server
}

// Option customises a Server (used by tests).
type Option func(*Server)

// WithClock overrides the time source.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// New builds a Server with all routes registered.
func New(cfg Config, log *slog.Logger, st *store.Store, box *secrets.Box, opts ...Option) *Server {
	s := &Server{cfg: cfg, log: log, st: st, box: box, now: time.Now}
	for _, o := range opts {
		o(s)
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
	return s
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

	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	mux.Handle("/", spaHandler())
	return securityHeaders(sameOrigin(mux))
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	s.log.Info("listening", "addr", ln.Addr().String())

	go s.janitor(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- s.http.Serve(ln) }()

	select {
	case err := <-errCh:
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
