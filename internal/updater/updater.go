// Package updater replaces Docklet's own container with a new image and
// rolls back if the new version doesn't come up healthy.
//
// It runs in a separate, short-lived container (started by Docklet, from
// Docklet's current image) so it survives Docklet being stopped:
//
//  1. inspect the running container and keep its configuration;
//  2. stop it, rename it to docklet-old-…, snapshot the SQLite files;
//  3. create the new container with the same configuration and the new image;
//  4. wait until it answers /api/version with the new version;
//  5. success: remove the old container and pin the version in .env;
//     failure: remove the new one, restore the snapshot, start the old one.
package updater

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker"
)

// States of an update.
const (
	StateRunning    = "running"
	StateSucceeded  = "succeeded"
	StateRolledBack = "rolled_back"
	StateFailed     = "failed"
)

// Step is one line of progress.
type Step struct {
	At  time.Time `json:"at"`
	Msg string    `json:"msg"`
}

// Status is written to <data>/update/status.json, so whichever Docklet runs
// afterwards can show how the update went.
type Status struct {
	ID         string    `json:"id"`
	State      string    `json:"state"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Image      string    `json:"image"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	Error      string    `json:"error,omitempty"`
	Backup     string    `json:"backup,omitempty"`
	Steps      []Step    `json:"steps"`
	Auto       bool      `json:"auto,omitempty"`
}

// StatusPath is where the status lives in a data directory.
func StatusPath(dataDir string) string { return filepath.Join(dataDir, "update", "status.json") }

// ReadStatus reads the last update's status (nil if none).
func ReadStatus(dataDir string) (*Status, error) {
	b, err := os.ReadFile(StatusPath(dataDir)) //nolint:gosec // fixed path under the data dir
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Recorder writes the status file as steps happen.
type Recorder struct {
	mu   sync.Mutex
	dir  string
	st   Status
	logf func(string)
}

// NewRecorder starts a status file.
func NewRecorder(dataDir string, st Status, logf func(string)) *Recorder {
	if st.Steps == nil {
		st.Steps = []Step{}
	}
	r := &Recorder{dir: dataDir, st: st, logf: logf}
	_ = r.save()
	return r
}

// Step records progress.
func (r *Recorder) Step(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.mu.Lock()
	r.st.Steps = append(r.st.Steps, Step{At: time.Now().UTC(), Msg: msg})
	r.mu.Unlock()
	if r.logf != nil {
		r.logf(msg)
	}
	_ = r.save()
}

// Finish sets the final state.
func (r *Recorder) Finish(state string, err error) {
	r.mu.Lock()
	r.st.State, r.st.FinishedAt = state, time.Now().UTC()
	if err != nil {
		r.st.Error = err.Error()
	}
	r.mu.Unlock()
	_ = r.save()
}

// Set changes fields under the lock.
func (r *Recorder) Set(fn func(*Status)) {
	r.mu.Lock()
	fn(&r.st)
	r.mu.Unlock()
	_ = r.save()
}

func (r *Recorder) save() error {
	r.mu.Lock()
	b, err := json.MarshalIndent(r.st, "", "  ")
	r.mu.Unlock()
	if err != nil {
		return err
	}
	p := StatusPath(r.dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Docker is what the updater needs.
type Docker interface {
	ContainerInspectRaw(ctx context.Context, id string) (map[string]any, error)
	ImageInspectRaw(ctx context.Context, ref string) (map[string]any, error)
	ContainerInspect(ctx context.Context, id string) (docker.ContainerInfo, error)
	ContainerCreateRaw(ctx context.Context, name string, body map[string]any) (string, error)
	ContainerRename(ctx context.Context, id, name string) error
	ContainerStart(ctx context.Context, id string) error
	ContainerStop(ctx context.Context, id string, timeoutSec int) error
	ContainerRemove(ctx context.Context, id string) error
	ContainerLogs(ctx context.Context, id string, o docker.LogOptions, fn func(docker.LogLine) error) error
	NetworkConnect(ctx context.Context, network, container string, endpoint map[string]any) error
}

// Options for Run.
type Options struct {
	Container string // the container to replace, e.g. "docklet"
	Image     string // new image reference
	To        string // version the new container must report
	DataDir   string // Docklet's data volume, mounted in the updater
	EnvFile   string // the installer's .env (optional)
	Health    time.Duration
	// Probe fetches /api/version from addr (tests replace it).
	Probe func(ctx context.Context, addr string) (string, error)
}

// Run performs the update. The status file says what happened; the error is
// for the updater's own log.
func Run(ctx context.Context, dk Docker, rec *Recorder, o Options) error {
	if o.Health == 0 {
		o.Health = 2 * time.Minute
	}
	if o.Probe == nil {
		o.Probe = probeVersion
	}
	raw, err := dk.ContainerInspectRaw(ctx, o.Container)
	if err != nil {
		rec.Finish(StateFailed, fmt.Errorf("inspect %s: %w", o.Container, err))
		return err
	}
	oldID, _ := raw["Id"].(string)
	var imageCfg map[string]any
	if img, _ := raw["Image"].(string); img != "" {
		if ir, err := dk.ImageInspectRaw(ctx, img); err == nil {
			imageCfg, _ = ir["Config"].(map[string]any)
		}
	}
	body, primary, extra, addr, err := createBody(raw, imageCfg, o.Image)
	if err != nil {
		rec.Finish(StateFailed, err)
		return err
	}
	from := ""
	rec.Set(func(s *Status) { from = s.From })

	rec.Step("Stopping Docklet %s", from)
	oldName := fmt.Sprintf("%s-old-%d", o.Container, time.Now().Unix())
	if err := dk.ContainerStop(ctx, oldID, 30); err != nil {
		rec.Finish(StateFailed, fmt.Errorf("stop the running Docklet: %w", err))
		_ = dk.ContainerStart(ctx, oldID)
		return err
	}
	if err := dk.ContainerRename(ctx, oldID, oldName); err != nil {
		_ = dk.ContainerStart(ctx, oldID)
		rec.Finish(StateFailed, fmt.Errorf("rename the running Docklet: %w", err))
		return err
	}
	snap, err := snapshot(o.DataDir)
	if err != nil {
		rec.Step("Warning: couldn't snapshot the database: %v", err)
	} else {
		rec.Step("Database snapshot saved")
	}

	rollback := func(cause error, newID string) error {
		rec.Step("The new version failed: %v", cause)
		if newID != "" {
			if tail := logTail(ctx, dk, newID); tail != "" {
				rec.Step("Last log lines of the new version:\n%s", tail)
			}
			_ = dk.ContainerStop(ctx, newID, 5)
			_ = dk.ContainerRemove(ctx, newID)
		}
		if snap != "" {
			if err := restore(o.DataDir, snap); err != nil {
				rec.Step("Warning: couldn't restore the database snapshot: %v", err)
			} else {
				rec.Step("Database restored from the snapshot")
			}
		}
		rec.Step("Starting the previous version again")
		if err := dk.ContainerRename(ctx, oldID, o.Container); err != nil {
			rec.Finish(StateFailed, fmt.Errorf("%w; and renaming the previous container back failed: %w", cause, err))
			return err
		}
		if err := dk.ContainerStart(ctx, oldID); err != nil {
			rec.Finish(StateFailed, fmt.Errorf("%w; and starting the previous version failed: %w", cause, err))
			return err
		}
		if v, err := waitHealthy(ctx, dk, o, oldID, addr, from); err != nil {
			rec.Step("Warning: the previous version is slow to come back: %v", err)
		} else {
			rec.Step("Docklet %s is running again", v)
		}
		rec.Finish(StateRolledBack, cause)
		return cause
	}

	rec.Step("Starting Docklet %s", o.To)
	newID, err := dk.ContainerCreateRaw(ctx, o.Container, body)
	if err != nil {
		return rollback(fmt.Errorf("create the new container: %w", err), "")
	}
	for name, ep := range extra {
		if err := dk.NetworkConnect(ctx, name, newID, ep); err != nil {
			rec.Step("Warning: couldn't join network %s: %v", name, err)
		}
	}
	_ = primary
	if err := dk.ContainerStart(ctx, newID); err != nil {
		return rollback(fmt.Errorf("start the new container: %w", err), newID)
	}
	rec.Step("Waiting for the new version to answer")
	if _, err := waitHealthy(ctx, dk, o, newID, addr, o.To); err != nil {
		return rollback(err, newID)
	}
	rec.Step("Docklet %s is healthy", o.To)
	if err := dk.ContainerRemove(ctx, oldID); err != nil {
		rec.Step("Warning: couldn't remove the previous container %s: %v", oldName, err)
	}
	if o.EnvFile != "" {
		if err := pinVersion(o.EnvFile, o.To); err != nil {
			rec.Step("Warning: couldn't update DOCKLET_VERSION in .env: %v", err)
		} else {
			rec.Step("DOCKLET_VERSION=%s saved in .env", o.To)
		}
	}
	rec.Finish(StateSucceeded, nil)
	return nil
}

// createBody turns an inspect document into a create body for image. It
// returns the primary network, other networks to join after creation, and
// the address Docklet listens on.
func createBody(raw, imageCfg map[string]any, image string) (body map[string]any, primary string, extra map[string]map[string]any, addr string, err error) {
	cfg, _ := raw["Config"].(map[string]any)
	hc, _ := raw["HostConfig"].(map[string]any)
	ns, _ := raw["NetworkSettings"].(map[string]any)
	if cfg == nil || hc == nil {
		return nil, "", nil, "", errors.New("unexpected inspect output")
	}
	oldID, _ := raw["Id"].(string)
	body = map[string]any{}
	for k, v := range cfg {
		body[k] = v
	}
	body["Image"] = image
	stripImageDefaults(body, imageCfg)
	// The hostname defaults to the container id; Docklet finds its own
	// container through it, so the new one must get its own.
	if h, _ := cfg["Hostname"].(string); oldID != "" && strings.HasPrefix(oldID, h) {
		delete(body, "Hostname")
	}
	delete(body, "Domainname")
	body["HostConfig"] = hc
	primary, _ = hc["NetworkMode"].(string)
	extra = map[string]map[string]any{}
	nets, _ := ns["Networks"].(map[string]any)
	for name, v := range nets {
		n, _ := v.(map[string]any)
		ep := map[string]any{}
		if ipam, ok := n["IPAMConfig"].(map[string]any); ok && len(ipam) > 0 {
			ep["IPAMConfig"] = ipam
		}
		var aliases []any
		if al, ok := n["Aliases"].([]any); ok {
			for _, a := range al {
				if s, _ := a.(string); s != "" && !strings.HasPrefix(oldID, s) {
					aliases = append(aliases, s)
				}
			}
		}
		if len(aliases) > 0 {
			ep["Aliases"] = aliases
		}
		if name == primary {
			body["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{name: ep}}
		} else {
			extra[name] = ep
		}
	}
	// DOCKLET_ADDR tells where the health probe should look.
	if env, ok := cfg["Env"].([]any); ok {
		for _, e := range env {
			if s, _ := e.(string); strings.HasPrefix(s, "DOCKLET_ADDR=") {
				addr = strings.TrimPrefix(s, "DOCKLET_ADDR=")
			}
		}
	}
	if addr == "" {
		addr = ":3000"
	}
	return body, primary, extra, addr, nil
}

// stripImageDefaults removes what the container only inherited from the old
// image (entrypoint, command, env, labels, ...), so the new image's own
// values apply. What compose set explicitly stays.
func stripImageDefaults(body, img map[string]any) {
	if img == nil {
		return
	}
	same := func(a, b any) bool {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return string(x) == string(y)
	}
	for _, k := range []string{"Entrypoint", "Cmd", "WorkingDir", "User", "StopSignal", "Healthcheck", "Shell", "OnBuild"} {
		if v, ok := body[k]; ok && same(v, img[k]) {
			delete(body, k)
		}
	}
	if env, ok := body["Env"].([]any); ok {
		inImage := map[string]bool{}
		if ie, ok := img["Env"].([]any); ok {
			for _, e := range ie {
				if s, ok := e.(string); ok {
					inImage[s] = true
				}
			}
		}
		var keep []any
		for _, e := range env {
			if s, _ := e.(string); !inImage[s] {
				keep = append(keep, e)
			}
		}
		body["Env"] = keep
	}
	for _, k := range []string{"Labels", "ExposedPorts", "Volumes"} {
		m, ok := body[k].(map[string]any)
		im, _ := img[k].(map[string]any)
		if !ok || im == nil {
			continue
		}
		out := map[string]any{}
		for key, v := range m {
			if iv, found := im[key]; !found || !same(iv, v) {
				out[key] = v
			}
		}
		body[k] = out
	}
}

// waitHealthy waits until the container answers with version want.
func waitHealthy(ctx context.Context, dk Docker, o Options, id, addr, want string) (string, error) {
	deadline := time.Now().Add(o.Health)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("DOCKLET_ADDR %q: %w", addr, err)
	}
	var last string
	for time.Now().Before(deadline) {
		info, err := dk.ContainerInspect(ctx, id)
		if err != nil {
			return "", err
		}
		if info.RestartCount > 2 || (!info.State.Running && !info.State.Restarting && info.State.Status != "created") {
			return "", fmt.Errorf("the container stopped (exit code %d%s)", info.State.ExitCode, errSuffix(info.State.Error))
		}
		h := host
		if h == "" || h == "0.0.0.0" {
			for _, n := range info.NetworkSettings.Networks {
				if n.IPAddress != "" {
					h = n.IPAddress
					break
				}
			}
		}
		if h != "" && info.State.Running {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			v, err := o.Probe(pctx, net.JoinHostPort(h, port))
			cancel()
			switch {
			case err != nil:
				last = err.Error()
			case want == "" || v == want:
				return v, nil
			default:
				return "", fmt.Errorf("it reports version %s, expected %s", v, want)
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return "", fmt.Errorf("no healthy answer within %s (%s)", o.Health, last)
}

func errSuffix(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

func probeVersion(ctx context.Context, addr string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/version", nil)
	if err != nil {
		return "", err
	}
	res, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("/api/version: %s", res.Status)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&v); err != nil {
		return "", err
	}
	return v.Version, nil
}

func logTail(ctx context.Context, dk Docker, id string) string {
	var lines []string
	_ = dk.ContainerLogs(ctx, id, docker.LogOptions{Tail: 15}, func(l docker.LogLine) error {
		lines = append(lines, l.Text)
		return nil
	})
	return strings.Join(lines, "\n")
}

var dbFiles = []string{"docklet.db", "docklet.db-wal", "docklet.db-shm"}

// snapshot copies the SQLite files (Docklet is stopped, so they are
// consistent) to <data>/update/snapshot.
func snapshot(dataDir string) (string, error) {
	dst := filepath.Join(dataDir, "update", "snapshot")
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return "", err
	}
	found := false
	for _, f := range dbFiles {
		src := filepath.Join(dataDir, f)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := copyFile(src, filepath.Join(dst, f)); err != nil {
			return "", err
		}
		found = true
	}
	if !found {
		return "", errors.New("docklet.db not found")
	}
	return dst, nil
}

func restore(dataDir, snap string) error {
	for _, f := range dbFiles {
		_ = os.Remove(filepath.Join(dataDir, f))
		src := filepath.Join(snap, f)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := copyFile(src, filepath.Join(dataDir, f)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // fixed names in the data dir
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm()) //nolint:gosec // fixed names
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

var versionLine = regexp.MustCompile(`^DOCKLET_VERSION=.*$`)

// pinVersion sets DOCKLET_VERSION in the installer's .env.
func pinVersion(envFile, v string) error {
	b, err := os.ReadFile(envFile) //nolint:gosec // the installer's .env, mounted by Docklet
	if err != nil {
		return err
	}
	var out []string
	found := false
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		l := sc.Text()
		if versionLine.MatchString(l) {
			l, found = "DOCKLET_VERSION="+v, true
		}
		out = append(out, l)
	}
	if !found {
		out = append(out, "DOCKLET_VERSION="+v)
	}
	fi, err := os.Stat(envFile)
	if err != nil {
		return err
	}
	return os.WriteFile(envFile, []byte(strings.Join(out, "\n")+"\n"), fi.Mode().Perm())
}
