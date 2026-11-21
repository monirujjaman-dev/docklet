package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/release"
	"github.com/monirujjaman-dev/docklet/internal/version"
)

func cmdUpgrade(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tag := fs.String("version", "", "release tag to install, e.g. v1.0.1 (default: latest)")
	check := fs.Bool("check", false, "only report whether an update is available")
	force := fs.Bool("force", false, "install even if it isn't newer")
	restart := fs.Bool("restart", false, "run 'systemctl restart docklet' afterwards")
	repo := fs.String("repo", envOr("DOCKLET_RELEASE_REPO", release.DefaultRepo), "GitHub repository with the releases")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c := release.New(*repo)
	r, err := c.Latest(ctx, *tag)
	if err != nil {
		fmt.Fprintf(stderr, "look up release: %v\n", err)
		return 1
	}
	cur := version.Version
	cmp, comparable := release.Compare(cur, r.Tag)
	switch {
	case comparable && cmp >= 0 && !*force:
		fmt.Fprintf(stdout, "Docklet %s is up to date (latest release: %s).\n", cur, r.Tag)
		return 0
	case !comparable:
		fmt.Fprintf(stdout, "This build is %q; the latest release is %s.\n", cur, r.Tag)
	default:
		fmt.Fprintf(stdout, "Update available: %s → %s (%s)\n", cur, r.Tag, r.URL)
	}
	if *check {
		return 0
	}
	if release.InContainer() {
		fmt.Fprintln(stderr, `Docklet runs in a container here: upgrade the image instead. On the host run
    curl -sSL https://raw.githubusercontent.com/`+*repo+`/main/install.sh | sudo sh
or, in the install directory (/opt/docklet):
    docker compose pull && docker compose up -d`)
		return 2
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintf(stderr, "find this executable: %v\n", err)
		return 1
	}
	out, err := c.Install(ctx, r, exe)
	if err != nil {
		fmt.Fprintf(stderr, "upgrade failed (nothing was changed): %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Installed %s at %s (checksum verified; previous binary kept as %s.previous).\n", out, exe, exe)
	if *restart {
		if b, err := exec.CommandContext(ctx, "systemctl", "restart", "docklet").CombinedOutput(); err != nil {
			fmt.Fprintf(stderr, "systemctl restart docklet: %v %s\n", err, b)
			return 1
		}
		fmt.Fprintln(stdout, "Restarted docklet.service.")
	} else {
		fmt.Fprintln(stdout, "Restart Docklet to run the new version (e.g. systemctl restart docklet).")
	}
	return 0
}
