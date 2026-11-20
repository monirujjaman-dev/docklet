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
	"syscall"

	"github.com/monirujjaman-dev/docklet/internal/server"
	"github.com/monirujjaman-dev/docklet/internal/version"
)

const usage = `Usage: docklet <command> [flags]

Commands:
  serve     Run the control plane
  version   Print version information

Run 'docklet <command> -h' for command flags.
`

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
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version.Get().String())
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func cmdServe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", envOr("DOCKLET_ADDR", ":3000"), "listen address (env DOCKLET_ADDR)")
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
	log.Info("starting docklet", "version", version.Version, "commit", version.Commit)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(server.Config{Addr: *addr}, log)
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
