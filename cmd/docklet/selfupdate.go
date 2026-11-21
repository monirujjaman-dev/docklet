package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/updater"
)

// cmdSelfUpdate runs inside the updater container that Docklet starts for
// Settings → Updates → Update now. It isn't meant to be run by hand.
func cmdSelfUpdate(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("self-update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	container := fs.String("container", "docklet", "container to replace")
	image := fs.String("image", "", "new image")
	to := fs.String("to", "", "version the new image must report")
	data := fs.String("data-dir", "/data", "Docklet's data volume")
	envFile := fs.String("env-file", "", "installer .env to update")
	health := fs.Duration("health-timeout", 2*time.Minute, "how long to wait for the new version")
	socket := fs.String("docker-socket", "/var/run/docker.sock", "Docker socket")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *image == "" || *to == "" {
		fmt.Fprintln(stderr, "self-update: -image and -to are required")
		return 2
	}
	st, err := updater.ReadStatus(*data)
	if err != nil || st == nil {
		fmt.Fprintln(stderr, "self-update: no update in progress:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rec := updater.NewRecorder(*data, *st, func(m string) { fmt.Fprintln(stderr, m) })
	if *envFile != "" {
		if _, err := os.Stat(*envFile); err != nil {
			*envFile = ""
		}
	}
	err = updater.Run(ctx, docker.New(*socket), rec, updater.Options{
		Container: *container, Image: *image, To: *to, DataDir: *data, EnvFile: *envFile, Health: *health,
	})
	if err != nil {
		fmt.Fprintln(stderr, "self-update:", err)
		return 1
	}
	return 0
}
