// Package host runs commands on the Docker host and implements the Server
// page: hostname, time zone, swap, disk cleanup, reboot, listening ports,
// processes and systemd services (ADR-002).
//
// Commands are argv lists or fixed scripts written in this package; values
// from the admin reach scripts only through environment variables, never by
// string interpolation into shell code.
package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker"
)

// Result is a finished command.
type Result struct {
	Stdout string
	Stderr string
	Exit   int
}

// Runner executes argv in the host's namespaces with extra env (KEY=VALUE).
type Runner interface {
	Run(ctx context.Context, argv []string, env []string) (Result, error)
}

// ErrCommand wraps a non-zero exit.
type ErrCommand struct {
	Argv []string
	Res  Result
}

func (e *ErrCommand) Error() string {
	msg := strings.TrimSpace(e.Res.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(e.Res.Stdout)
	}
	if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
		msg = msg[i+1:]
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Sprintf("%s exited with %d: %s", e.Argv[0], e.Res.Exit, msg)
}

// Check runs argv and fails on a non-zero exit.
func Check(ctx context.Context, r Runner, argv []string, env ...string) (string, error) {
	res, err := r.Run(ctx, argv, env)
	if err != nil {
		return "", err
	}
	if res.Exit != 0 {
		return res.Stdout, &ErrCommand{Argv: argv, Res: res}
	}
	return res.Stdout, nil
}

// Script runs a fixed shell script with values passed as environment.
func Script(ctx context.Context, r Runner, script string, env ...string) (string, error) {
	return Check(ctx, r, []string{"/bin/sh", "-c", script, "docklet"}, env...)
}

// Direct runs commands as child processes: Docklet itself is on the host
// (binary install).
type Direct struct{}

// Run implements Runner.
func (Direct) Run(ctx context.Context, argv []string, env []string) (Result, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv built by this package
	cmd.Env = append(os.Environ(), env...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &limited{b: &out}, &limited{b: &errb}
	err := cmd.Run()
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.Exit = ee.ExitCode()
		return res, nil
	}
	return res, err
}

type limited struct{ b *strings.Builder }

func (l *limited) Write(p []byte) (int, error) {
	if l.b.Len() < maxOutput {
		room := maxOutput - l.b.Len()
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}

const maxOutput = 4 << 20

// Docker is what the helper-container runner needs.
type Docker interface {
	ContainerCreate(ctx context.Context, s docker.ContainerSpec) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerWait(ctx context.Context, id string) (int, error)
	ContainerStop(ctx context.Context, id string, timeoutSec int) error
	ContainerRemove(ctx context.Context, id string) error
	ContainerLogs(ctx context.Context, id string, o docker.LogOptions, fn func(docker.LogLine) error) error
}

// Helper runs each command in a short-lived privileged container (from
// Docklet's own image) that enters the namespaces of TargetPID with nsenter.
type Helper struct {
	DK        Docker
	Image     string
	TargetPID int // 1 = the host; tests point it at a container's init
	Timeout   time.Duration
}

// LabelHelper marks helper containers so leftovers can be cleaned up.
const LabelHelper = "docklet.host-helper"

// Run implements Runner.
func (h *Helper) Run(ctx context.Context, argv []string, env []string) (Result, error) {
	pid := h.TargetPID
	if pid == 0 {
		pid = 1
	}
	timeout := h.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	cmd := append([]string{"-t", strconv.Itoa(pid), "-m", "-u", "-i", "-n", "-p", "--"}, argv...)
	id, err := h.DK.ContainerCreate(ctx, docker.ContainerSpec{
		Name: "docklet-host-" + hex.EncodeToString(rnd[:]), Image: h.Image, Entrypoint: []string{"nsenter"}, Cmd: cmd,
		Env: env, Privileged: true, PidMode: "host", Network: "host", RestartPolicy: "no",
		Labels: map[string]string{"docklet.managed": "true", LabelHelper: "true"},
	})
	if err != nil {
		return Result{}, fmt.Errorf("start host helper: %w", err)
	}
	rm := context.WithoutCancel(ctx)
	defer func() { _ = h.DK.ContainerRemove(rm, id) }()
	if err := h.DK.ContainerStart(ctx, id); err != nil {
		return Result{}, fmt.Errorf("start host helper: %w", err)
	}
	code, err := h.DK.ContainerWait(ctx, id)
	if err != nil {
		_ = h.DK.ContainerStop(rm, id, 1)
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("%s timed out after %s", argv[0], timeout)
		}
		return Result{}, err
	}
	var out, errb strings.Builder
	lo, le := &limited{b: &out}, &limited{b: &errb}
	_ = h.DK.ContainerLogs(rm, id, docker.LogOptions{}, func(l docker.LogLine) error {
		w := lo
		if l.Stream == "stderr" {
			w = le
		}
		_, _ = w.Write([]byte(l.Text + "\n"))
		return nil
	})
	return Result{Stdout: out.String(), Stderr: errb.String(), Exit: code}, nil
}

// SelfImage finds the image of the container Docklet runs in.
func SelfImage(ctx context.Context, inspect func(ctx context.Context, id string) (docker.ContainerInfo, error)) (string, error) {
	if img := os.Getenv("DOCKLET_HELPER_IMAGE"); img != "" {
		return img, nil
	}
	hn, err := os.Hostname()
	if err != nil {
		return "", err
	}
	info, err := inspect(ctx, hn)
	if err != nil {
		return "", fmt.Errorf("find Docklet's own container (%s): %w", hn, err)
	}
	return info.Image, nil
}
