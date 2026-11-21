// Package server wires the HTTP API and the embedded dashboard.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/caddy"
	"github.com/monirujjaman-dev/docklet/internal/cron"
	"github.com/monirujjaman-dev/docklet/internal/databases"
	"github.com/monirujjaman-dev/docklet/internal/destinations"
	"github.com/monirujjaman-dev/docklet/internal/host"
	"github.com/monirujjaman-dev/docklet/internal/metrics"
	"github.com/monirujjaman-dev/docklet/internal/release"
	"github.com/monirujjaman-dev/docklet/internal/s3"
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
	// PanelUpstream is the address Caddy uses to reach this dashboard
	// (default 127.0.0.1:<port>). Set it when Docklet runs in a container,
	// e.g. "172.30.0.3:3000". When set, Docklet never rebinds its listener:
	// direct access is controlled by which ports the container publishes.
	PanelUpstream string
	// TrustedProxies are peers (besides loopback) whose X-Forwarded-For and
	// X-Forwarded-Proto headers are honoured, e.g. the Caddy container.
	TrustedProxies []netip.Prefix
	// BackupDir stores database backups (default: a "backups" dir next to the DB).
	BackupDir string
	// DataDir holds docklet.db and master.key; enables self-backup downloads.
	DataDir string
	// DisableUpdateCheck turns off the dashboard's "check for updates".
	DisableUpdateCheck bool
	// ReleaseRepo is the GitHub repository checked for releases.
	ReleaseRepo string
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
	// Up to 30 webhook calls per minute per token.
	webhookLimiter *rateLimiter
	http           *http.Server
	lm             *listenerMgr

	caddy     *caddy.Reconciler
	caddyOpts caddy.Options

	docker   apps.Docker
	appsTune func(*apps.Service)
	dbs      *databases.Service
	dbsTune  func(*databases.Service)
	cron     *cron.Service
	updates  *updateChecker
	dests    *destinations.Service
	sbRunner selfBackupRunner
	host     hostState
	sec      securityState
	term     terminalState
	selfUpd  selfUpdateState
	// filesContent overrides how file contents move (tests).
	filesContent host.Content
	sbEvery      time.Duration
	cronTune     func(*cron.Service)
	apps         *apps.Service
	metrics      *metrics.Collector

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

// WithDatabasesTuning adjusts the databases service (tests).
func WithDatabasesTuning(fn func(*databases.Service)) Option {
	return func(s *Server) { s.dbsTune = fn }
}

// WithReleaseAPI points the update check at another GitHub API base (tests).
func WithReleaseAPI(base string) Option {
	return func(s *Server) { s.updates.rel.APIBase = base }
}

// WithCronTuning adjusts the cron service (tests).
func WithCronTuning(fn func(*cron.Service)) Option {
	return func(s *Server) { s.cronTune = fn }
}

// WithMetrics enables the monitoring endpoints and runs the collector.
func WithMetrics(c *metrics.Collector) Option { return func(s *Server) { s.metrics = c } }

// WithCaddyInterval sets the reconcile interval.
func WithCaddyInterval(d time.Duration) Option {
	return func(s *Server) { s.caddyInterval = d }
}

// New builds a Server with all routes registered.
func New(cfg Config, log *slog.Logger, st *store.Store, box *secrets.Box, opts ...Option) (*Server, error) {
	s := &Server{
		cfg: cfg, log: log, st: st, box: box, now: time.Now,
		prober: defaultProber, verifyTimeout: 90 * time.Second, verifyInterval: 3 * time.Second,
		updates: &updateChecker{rel: release.New(cfg.ReleaseRepo)},
		dests:   destinations.New(st, box),
		sbEvery: 10 * time.Minute,
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
		if dk, ok := s.docker.(databases.Docker); ok {
			dir := cfg.BackupDir
			if dir == "" {
				dir = filepath.Join(os.TempDir(), "docklet-backups")
			}
			s.dbs = databases.New(st, dk, box, log, dir)
			s.dbs.SetClock(s.now)
			s.dbs.EnsureNetwork = s.apps.EnsureNetwork
			s.dbs.SealEnv = s.apps.SealEnv
			s.dbs.Remote = func(ctx context.Context, id int64) (*s3.Client, error) {
				c, _, err := s.dests.Client(ctx, id)
				return c, err
			}
			if s.dbsTune != nil {
				s.dbsTune(s.dbs)
			}
		}
		if dk, ok := s.docker.(cron.Docker); ok {
			s.cron = cron.New(st, dk, log)
			s.cron.SetClock(s.now)
			s.cron.EnvFor = s.apps.EnvFor
			s.cron.EnsureNetwork = s.apps.EnsureNetwork
			if s.cronTune != nil {
				s.cronTune(s.cron)
			}
		}
	}
	// 10 attempts per minute per IP for login/setup, on top of per-account lockout.
	s.loginLimiter = newRateLimiter(10, time.Minute, func() time.Time { return s.now() })
	s.webhookLimiter = newRateLimiter(30, time.Minute, func() time.Time { return s.now() })
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
	mux.HandleFunc("PUT /api/apps/{id}/git", authed(s.handleUpdateGit))
	mux.HandleFunc("POST /api/apps/{id}/deploy-key", authed(s.handleRegenerateDeployKey))
	mux.HandleFunc("GET /api/apps/{id}/webhook", authed(s.handleGetWebhook))
	mux.HandleFunc("POST /api/apps/{id}/webhook/rotate", authed(s.handleRotateWebhook))
	mux.HandleFunc("POST /api/apps/{id}/rollback", authed(s.handleRollback))
	mux.HandleFunc("POST /api/webhooks/{token}", h(s.handleWebhook))

	mux.HandleFunc("GET /api/server/overview", authed(s.handleServerOverview))
	mux.HandleFunc("GET /api/server/timezones", authed(s.handleServerTimezones))
	mux.HandleFunc("PUT /api/server/hostname", authed(s.handleServerHostname))
	mux.HandleFunc("PUT /api/server/timezone", authed(s.handleServerTimezone))
	mux.HandleFunc("PUT /api/server/swap", authed(s.handleServerSwap))
	mux.HandleFunc("PUT /api/server/swappiness", authed(s.handleServerSwappiness))
	mux.HandleFunc("POST /api/server/reboot", authed(s.handleServerReboot))
	mux.HandleFunc("GET /api/server/ports", authed(s.handleServerPorts))
	mux.HandleFunc("GET /api/server/processes", authed(s.handleServerProcesses))
	mux.HandleFunc("POST /api/server/processes/{pid}/signal", authed(s.handleServerSignal))
	mux.HandleFunc("GET /api/server/services", authed(s.handleServerServices))
	mux.HandleFunc("POST /api/server/services/{unit}/{action}", authed(s.handleServerServiceAction))
	mux.HandleFunc("GET /api/server/services/{unit}/logs", authed(s.handleServerServiceLogs))
	mux.HandleFunc("GET /api/server/cleanup", authed(s.handleServerCleanupPreview))
	mux.HandleFunc("POST /api/server/cleanup", authed(s.handleServerCleanup))
	mux.HandleFunc("POST /api/auth/reauth", authed(s.handleReauth))
	mux.HandleFunc("POST /api/terminal/sessions", authed(s.handleTerminalTicket))
	mux.HandleFunc("GET /api/files/roots", authed(s.handleFileRoots))
	mux.HandleFunc("GET /api/files/list", authed(s.handleFileList))
	mux.HandleFunc("GET /api/files/read", authed(s.handleFileRead))
	mux.HandleFunc("PUT /api/files/write", authed(s.handleFileWrite))
	mux.HandleFunc("POST /api/files/upload", authed(s.handleFileUpload))
	mux.HandleFunc("GET /api/files/download", authed(s.handleFileDownload))
	mux.HandleFunc("POST /api/files/op/{op}", authed(s.handleFileOp))
	mux.HandleFunc("GET /api/terminal/ws", s.handleTerminalWS)
	mux.HandleFunc("GET /api/server/security/pending", authed(s.handleSecurityPending))
	mux.HandleFunc("POST /api/server/security/pending/confirm", authed(s.handleSecurityConfirm))
	mux.HandleFunc("POST /api/server/security/pending/undo", authed(s.handleSecurityUndo))
	mux.HandleFunc("GET /api/server/ssh", authed(s.handleSSHStatus))
	mux.HandleFunc("PUT /api/server/ssh", authed(s.handleSSHChange))
	mux.HandleFunc("POST /api/server/ssh/keys", authed(s.handleSSHAddKey))
	mux.HandleFunc("POST /api/server/ssh/keys/delete", authed(s.handleSSHRemoveKey))
	mux.HandleFunc("GET /api/server/firewall", authed(s.handleFirewall))
	mux.HandleFunc("POST /api/server/firewall/install", authed(s.handleFirewallInstall))
	mux.HandleFunc("POST /api/server/firewall/enable", authed(s.handleFirewallEnable))
	mux.HandleFunc("POST /api/server/firewall/disable", authed(s.handleFirewallDisable))
	mux.HandleFunc("POST /api/server/firewall/rules", authed(s.handleFirewallAddRule))
	mux.HandleFunc("POST /api/server/firewall/rules/delete", authed(s.handleFirewallDeleteRule))
	mux.HandleFunc("PUT /api/server/firewall/defaults", authed(s.handleFirewallDefaults))
	mux.HandleFunc("GET /api/server/fail2ban", authed(s.handleFail2Ban))
	mux.HandleFunc("POST /api/server/fail2ban/install", authed(s.handleFail2BanInstall))
	mux.HandleFunc("PUT /api/server/fail2ban", authed(s.handleFail2BanConfigure))
	mux.HandleFunc("POST /api/server/fail2ban/unban", authed(s.handleFail2BanUnban))
	mux.HandleFunc("PUT /api/databases/{id}/allowlist", authed(s.handleDatabaseAllowlist))
	mux.HandleFunc("POST /api/system/backup", authed(s.handleSystemBackup))
	mux.HandleFunc("GET /api/system/backup-schedule", authed(s.handleGetSelfBackupSchedule))
	mux.HandleFunc("PUT /api/system/backup-schedule", authed(s.handlePutSelfBackupSchedule))
	mux.HandleFunc("POST /api/system/backup-schedule/run", authed(s.handleRunSelfBackup))
	mux.HandleFunc("GET /api/system/remote-backups", authed(s.handleListRemoteSelfBackups))
	mux.HandleFunc("GET /api/destinations", authed(s.handleListDestinations))
	mux.HandleFunc("POST /api/destinations", authed(s.handleCreateDestination))
	mux.HandleFunc("POST /api/destinations/test", authed(s.handleTestDestinationInput))
	mux.HandleFunc("PUT /api/destinations/{id}", authed(s.handleUpdateDestination))
	mux.HandleFunc("POST /api/destinations/{id}/test", authed(s.handleTestDestination))
	mux.HandleFunc("DELETE /api/destinations/{id}", authed(s.handleDeleteDestination))
	mux.HandleFunc("PUT /api/databases/{id}/backup-destination", authed(s.handleDatabaseDestination))
	mux.HandleFunc("POST /api/backups/{id}/upload", authed(s.handleUploadBackup))
	mux.HandleFunc("POST /api/databases/{id}/users", authed(s.handleCreateDBUser))
	mux.HandleFunc("GET /api/database-users/{id}/credentials", authed(s.handleDBUserCredentials))
	mux.HandleFunc("PATCH /api/database-users/{id}", authed(s.handleUpdateDBUser))
	mux.HandleFunc("DELETE /api/database-users/{id}", authed(s.handleDeleteDBUser))
	mux.HandleFunc("GET /api/system/update", authed(s.handleUpdateCheck))
	mux.HandleFunc("GET /api/system/update/status", authed(s.handleUpdateStatus))
	mux.HandleFunc("POST /api/system/update/start", authed(s.handleUpdateStart))
	mux.HandleFunc("PUT /api/system/update/auto", authed(s.handleUpdateAuto))
	mux.HandleFunc("GET /api/apps/{id}/cron", authed(s.handleListCron))
	mux.HandleFunc("POST /api/apps/{id}/cron", authed(s.handleCreateCron))
	mux.HandleFunc("POST /api/cron/preview", authed(s.handleCronPreview))
	mux.HandleFunc("PATCH /api/cron/{id}", authed(s.handleUpdateCron))
	mux.HandleFunc("DELETE /api/cron/{id}", authed(s.handleDeleteCron))
	mux.HandleFunc("POST /api/cron/{id}/run", authed(s.handleRunCron))
	mux.HandleFunc("GET /api/cron/{id}/runs", authed(s.handleListCronRuns))
	mux.HandleFunc("GET /api/cron-runs/{id}", authed(s.handleGetCronRun))
	mux.HandleFunc("POST /api/cron-runs/{id}/cancel", authed(s.handleCancelCronRun))
	mux.HandleFunc("GET /api/databases/engines", authed(s.handleEngines))
	mux.HandleFunc("POST /api/projects/{id}/databases", authed(s.handleCreateDatabase))
	mux.HandleFunc("GET /api/databases/{id}", authed(s.handleGetDatabase))
	mux.HandleFunc("DELETE /api/databases/{id}", authed(s.handleDeleteDatabase))
	mux.HandleFunc("GET /api/databases/{id}/credentials", authed(s.handleDatabaseCredentials))
	mux.HandleFunc("POST /api/databases/{id}/start", authed(s.handleDatabaseLifecycle("start")))
	mux.HandleFunc("POST /api/databases/{id}/stop", authed(s.handleDatabaseLifecycle("stop")))
	mux.HandleFunc("POST /api/databases/{id}/restart", authed(s.handleDatabaseLifecycle("restart")))
	mux.HandleFunc("POST /api/databases/{id}/recreate", authed(s.handleDatabaseLifecycle("recreate")))
	mux.HandleFunc("PUT /api/databases/{id}/public", authed(s.handleDatabasePublic))
	mux.HandleFunc("PUT /api/databases/{id}/backup-policy", authed(s.handleBackupPolicy))
	mux.HandleFunc("POST /api/databases/{id}/backups", authed(s.handleCreateBackup))
	mux.HandleFunc("POST /api/databases/{id}/restore", authed(s.handleRestore))
	mux.HandleFunc("POST /api/databases/{id}/links", authed(s.handleLinkDatabase))
	mux.HandleFunc("DELETE /api/databases/{id}/links/{appId}", authed(s.handleUnlinkDatabase))
	mux.HandleFunc("GET /api/backups/{id}/download", authed(s.handleDownloadBackup))

	mux.HandleFunc("GET /api/metrics/status", authed(s.handleMetricsStatus))
	mux.HandleFunc("GET /api/metrics/recent", authed(s.handleMetricsRecent))
	mux.HandleFunc("GET /api/metrics/history", authed(s.handleMetricsHistory))
	mux.HandleFunc("GET /api/metrics/stream", authed(s.handleMetricsStream))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	mux.Handle("/", spaHandler())
	return s.withClientInfo(securityHeaders(sameOrigin(mux)))
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
	if panel.Status == panelActive && s.cfg.PanelUpstream == "" {
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
	if s.dbs != nil {
		go s.dbs.Run(ctx)
		go s.runAllowlist(ctx, 5*time.Minute)
	}
	if s.cfg.DataDir != "" {
		go s.runSelfBackupSchedule(ctx, s.sbEvery)
		if release.InContainer() && s.docker != nil {
			go s.runAutoUpdate(ctx)
		}
	}
	cronDone := make(chan struct{})
	if s.cron != nil {
		go func() { s.cron.Run(ctx); close(cronDone) }()
	} else {
		close(cronDone)
	}
	metricsDone := make(chan struct{})
	if s.metrics != nil {
		go func() { s.metrics.Run(ctx); close(metricsDone) }()
	} else {
		close(metricsDone)
	}
	// Let the collector write its partial minute before the DB closes.
	defer func() {
		select {
		case <-metricsDone:
		case <-time.After(5 * time.Second):
		}
	}()
	// Cron runs stop their containers and record the outcome.
	defer func() {
		select {
		case <-cronDone:
		case <-time.After(15 * time.Second):
		}
	}()

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
			s.webhookLimiter.prune()
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

// handleVersion is public (for monitoring), so it reports only the release,
// not the commit, build date or Go version.
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": version.Version})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		// Inline styles are needed by the charts; scripts are same-origin only.
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		if isHTTPS(r) {
			// Only over HTTPS (the panel domain): direct http://ip:3000 access
			// before a domain is set must keep working.
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}
