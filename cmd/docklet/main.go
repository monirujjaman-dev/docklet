// Command docklet is the Docklet control plane: a lightweight self-hosted PaaS
// built on Docker and Caddy.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/server"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/version"
)

const usage = `Usage: docklet <command> [flags]

Commands:
  serve                  Run the control plane
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

	cfg := server.Config{Addr: *addr, CaddyAdmin: *caddyAdmin}
	if *caddyAdmin == "off" {
		cfg.CaddyAdmin = ""
	}
	var opts []server.Option
	if *dockerSocket != "off" {
		opts = append(opts, server.WithDocker(docker.New(*dockerSocket)))
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
