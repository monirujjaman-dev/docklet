// Command docklet is the Docklet control plane: a lightweight self-hosted PaaS
// built on Docker and Caddy.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // cron time zones work without zoneinfo on the host

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/build"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/metrics"
	"github.com/monirujjaman-dev/docklet/internal/release"
	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/selfbackup"
	"github.com/monirujjaman-dev/docklet/internal/server"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/version"
)

const usage = `Usage: docklet <command> [flags]

Commands:
  serve                  Run the control plane
  backup                 Back up Docklet's database and master key
  restore                Restore a backup (Docklet must be stopped)
  upgrade                Upgrade a binary install to the latest release
  audit verify           Verify the audit log hash chain (exit 1 if broken)
  admin reset-password   Reset a user's password and unlock the account
  admin clear-panel-domain
                         Remove the panel domain and re-enable direct access
  version                Print version information

Run 'docklet <command> -h' for command flags.
`

const defaultDataDir = "/var/lib/docklet"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	switch args[0] {
	case "serve":
		return cmdServe(args[1:], stderr)
	case "backup":
		return cmdBackup(args[1:], stdout, stderr)
	case "restore":
		return cmdRestore(args[1:], stdout, stderr)
	case "upgrade":
		return cmdUpgrade(args[1:], stdout, stderr)
	case "self-update":
		return cmdSelfUpdate(args[1:], stderr)
	case "audit":
		if len(args) > 1 && args[1] == "verify" {
			return cmdAuditVerify(args[2:], stdout, stderr)
		}
	case "admin":
		if len(args) > 1 && args[1] == "reset-password" {
			return cmdResetPassword(args[2:], stdout, stderr)
		}
		if len(args) > 1 && args[1] == "clear-panel-domain" {
			return cmdClearPanelDomain(args[2:], stdout, stderr)
		}
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version.Get().String())
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
	return 2
}

// dataDirFlag registers the shared -data-dir flag.
func dataDirFlag(fs *flag.FlagSet) *string {
	return fs.String("data-dir", envOr("DOCKLET_DATA_DIR", defaultDataDir), "data directory (env DOCKLET_DATA_DIR)")
}

func dbPath(dataDir string) string  { return filepath.Join(dataDir, "docklet.db") }
func keyPath(dataDir string) string { return filepath.Join(dataDir, "master.key") }

func cmdServe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", envOr("DOCKLET_ADDR", ":3000"), "listen address (env DOCKLET_ADDR)")
	dataDir := dataDirFlag(fs)
	caddyAdmin := fs.String("caddy-admin", envOr("DOCKLET_CADDY_ADMIN", "http://localhost:2019"),
		"Caddy Admin API URL, or 'off' to disable Caddy integration (env DOCKLET_CADDY_ADMIN)")
	dockerSocket := fs.String("docker-socket", envOr("DOCKLET_DOCKER_SOCKET", "/var/run/docker.sock"),
		"Docker Engine socket, or 'off' to disable app deployments (env DOCKLET_DOCKER_SOCKET)")
	panelUpstream := fs.String("panel-upstream", envOr("DOCKLET_PANEL_UPSTREAM", ""),
		"address Caddy uses to reach the dashboard, e.g. 172.30.0.3:3000 when running in a container (env DOCKLET_PANEL_UPSTREAM)")
	trusted := fs.String("trusted-proxies", envOr("DOCKLET_TRUSTED_PROXIES", ""),
		"comma-separated IPs/CIDRs of reverse proxies besides loopback (env DOCKLET_TRUSTED_PROXIES)")
	attach := fs.String("attach-containers", envOr("DOCKLET_ATTACH_CONTAINERS", ""),
		"comma-separated containers to join to every project network, e.g. docklet,docklet-caddy (env DOCKLET_ATTACH_CONTAINERS)")
	hostProc := fs.String("host-proc", envOr("DOCKLET_HOST_PROC", "/proc"),
		"host /proc for metrics; /host/proc when the host's /proc is mounted into the container (env DOCKLET_HOST_PROC)")
	hostCgroup := fs.String("host-cgroup", envOr("DOCKLET_HOST_CGROUP", "/sys/fs/cgroup"),
		"host cgroup v2 root for container metrics (env DOCKLET_HOST_CGROUP)")
	retentionDays := fs.Int("metrics-retention-days", envInt("DOCKLET_METRICS_RETENTION_DAYS", 7),
		"days of one-minute metrics to keep; 0 disables metrics (env DOCKLET_METRICS_RETENTION_DAYS)")
	buildTimeout := fs.Duration("build-timeout", envDuration("DOCKLET_BUILD_TIMEOUT", 20*time.Minute),
		"maximum time for one Git build (env DOCKLET_BUILD_TIMEOUT)")
	noUpdateCheck := fs.Bool("no-update-check", envOr("DOCKLET_UPDATE_CHECK", "on") == "off",
		"never contact GitHub to check for new releases (env DOCKLET_UPDATE_CHECK=off)")
	logLevel := fs.String("log-level", envOr("DOCKLET_LOG_LEVEL", "info"), "debug|info|warn|error (env DOCKLET_LOG_LEVEL)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(stderr, "invalid -log-level %q\n", *logLevel)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	log.Info("starting docklet", "version", version.Version, "commit", version.Commit, "data_dir", *dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	unlock, err := selfbackup.Lock(*dataDir)
	if err != nil {
		log.Error("data directory", "dir", *dataDir, "err", err)
		return 1
	}
	defer unlock()

	key, err := secrets.LoadOrCreateKey(keyPath(*dataDir))
	if err != nil {
		log.Error("master key", "err", err)
		return 1
	}
	box, err := secrets.New(key)
	if err != nil {
		log.Error("master key", "err", err)
		return 1
	}
	st, err := store.Open(ctx, dbPath(*dataDir))
	if err != nil {
		log.Error("database", "err", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	proxies, err := parsePrefixes(*trusted)
	if err != nil {
		fmt.Fprintf(stderr, "invalid -trusted-proxies: %v\n", err)
		return 2
	}
	cfg := server.Config{
		Addr: *addr, CaddyAdmin: *caddyAdmin, PanelUpstream: *panelUpstream, TrustedProxies: proxies,
		BackupDir: filepath.Join(*dataDir, "backups"), DataDir: *dataDir,
		DisableUpdateCheck: *noUpdateCheck, ReleaseRepo: envOr("DOCKLET_RELEASE_REPO", release.DefaultRepo),
	}
	if *caddyAdmin == "off" {
		cfg.CaddyAdmin = ""
	}
	var opts []server.Option
	var statsAPI metrics.StatsAPI
	if *dockerSocket != "off" {
		dc := docker.New(*dockerSocket)
		statsAPI = dc
		opts = append(opts, server.WithDocker(dc))
		names := splitList(*attach)
		builder := build.New("unix://"+*dockerSocket, filepath.Join(*dataDir, "builds"), 1, *buildTimeout)
		if err := builder.Available(); err != nil {
			log.Warn("Git builds disabled", "reason", err)
			builder = nil
		}
		opts = append(opts, server.WithAppsTuning(func(s *apps.Service) {
			s.AttachContainers = names
			if builder != nil {
				s.Builder = builder
			}
		}))
	}
	if *retentionDays > 0 {
		mc := metrics.New(st, statsAPI, metrics.Config{Proc: *hostProc, CgroupRoot: *hostCgroup, DiskPath: *dataDir}, log)
		mc.Retention = time.Duration(*retentionDays) * 24 * time.Hour
		opts = append(opts, server.WithMetrics(mc))
	}
	srv, err := server.New(cfg, log, st, box, opts...)
	if err != nil {
		log.Error("config", "err", err)
		return 1
	}
	if err := srv.Run(ctx); err != nil {
		log.Error("server stopped", "err", err)
		return 1
	}
	return 0
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parsePrefixes accepts "10.0.0.0/8, 172.30.0.2" (bare IPs become /32 or /128).
func parsePrefixes(v string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, p := range splitList(v) {
		if !strings.Contains(p, "/") {
			ip, err := netip.ParseAddr(p)
			if err != nil {
				return nil, err
			}
			out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
			continue
		}
		pf, err := netip.ParsePrefix(p)
		if err != nil {
			return nil, err
		}
		out = append(out, pf.Masked())
	}
	return out, nil
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
