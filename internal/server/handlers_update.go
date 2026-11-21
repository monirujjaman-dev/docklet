package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/release"
	"github.com/monirujjaman-dev/docklet/internal/selfbackup"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
	"github.com/monirujjaman-dev/docklet/internal/updater"
	"github.com/monirujjaman-dev/docklet/internal/version"
)

// Update now: Docklet backs itself up, pulls the new image and starts an
// updater container (from its current image) that replaces the Docklet
// container and rolls back on failure. See internal/updater.

var versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,63}$`)

type selfUpdateState struct {
	mu      sync.Mutex
	running bool
}

type updateDocker interface {
	ContainerInspectRaw(ctx context.Context, id string) (map[string]any, error)
	ContainerCreateRaw(ctx context.Context, name string, body map[string]any) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ImagePull(ctx context.Context, ref string, progress func(string)) error
	ImageInspect(ctx context.Context, ref string) (docker.Image, error)
}

// selfContainer describes the container Docklet runs in.
type selfContainer struct {
	Name      string
	Image     string // reference, e.g. ghcr.io/monirujjaman-dev/docklet:1.3.0
	ImageID   string
	Network   string
	DataMount string // volume name or host path holding the data dir
	Socket    string // host path of the Docker socket
	WorkDir   string // the installer's directory (compose working dir)
}

func (s *Server) inspectSelf(ctx context.Context, dk updateDocker) (selfContainer, error) {
	hn, err := os.Hostname()
	if err != nil {
		return selfContainer{}, err
	}
	raw, err := dk.ContainerInspectRaw(ctx, hn)
	if err != nil {
		return selfContainer{}, fmt.Errorf("find Docklet's own container (%s): %w", hn, err)
	}
	var sc selfContainer
	sc.Name = strings.TrimPrefix(str(raw["Name"]), "/")
	sc.ImageID = str(raw["Image"])
	cfg, _ := raw["Config"].(map[string]any)
	sc.Image = str(cfg["Image"])
	if labels, ok := cfg["Labels"].(map[string]any); ok {
		sc.WorkDir = str(labels["com.docker.compose.project.working_dir"])
	}
	hc, _ := raw["HostConfig"].(map[string]any)
	sc.Network = str(hc["NetworkMode"])
	mounts, _ := raw["Mounts"].([]any)
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		dst := str(mm["Destination"])
		src := str(mm["Source"])
		if str(mm["Type"]) == "volume" {
			src = str(mm["Name"])
		}
		switch dst {
		case s.cfg.DataDir:
			sc.DataMount = src
		case "/var/run/docker.sock":
			sc.Socket = src
		}
	}
	if sc.DataMount == "" {
		return sc, fmt.Errorf("the data directory %s is not a mounted volume", s.cfg.DataDir)
	}
	if sc.Socket == "" {
		sc.Socket = "/var/run/docker.sock"
	}
	return sc, nil
}

func str(v any) string { s, _ := v.(string); return s }

// newImageRef replaces the tag of the running image.
func newImageRef(current, v string) string {
	if repo := os.Getenv("DOCKLET_UPDATE_REPO"); repo != "" {
		return repo + ":" + v
	}
	img, _ := docker.SplitImageRef(current)
	if strings.HasPrefix(img, "sha256:") || img == "" {
		img = "ghcr.io/" + release.DefaultRepo
	}
	return img + ":" + v
}

// startUpdate kicks off an update to v ("" = latest release).
func (s *Server) startUpdate(ctx context.Context, v, actor string, actorID *int64, ip string, auto bool) (*updater.Status, error) {
	if !release.InContainer() {
		return nil, errf(http.StatusConflict, "Update now works for container installs; on a binary install run 'sudo docklet upgrade'")
	}
	dk, ok := s.docker.(updateDocker)
	if !ok {
		return nil, errf(http.StatusServiceUnavailable, "updating needs Docker")
	}
	s.selfUpd.mu.Lock()
	if s.selfUpd.running {
		s.selfUpd.mu.Unlock()
		return nil, errf(http.StatusConflict, "an update is already running")
	}
	if st, _ := updater.ReadStatus(s.cfg.DataDir); st != nil && st.State == updater.StateRunning && time.Since(st.StartedAt) < 15*time.Minute {
		s.selfUpd.mu.Unlock()
		return nil, errf(http.StatusConflict, "an update is already running")
	}
	s.selfUpd.running = true
	s.selfUpd.mu.Unlock()
	release_ := func() { s.selfUpd.mu.Lock(); s.selfUpd.running = false; s.selfUpd.mu.Unlock() }

	if v == "" {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		rel, err := s.updates.rel.Latest(rctx, "")
		cancel()
		if err != nil {
			release_()
			return nil, errf(http.StatusBadGateway, "find the latest release: %v", err)
		}
		v = rel.Tag
	}
	v = strings.TrimPrefix(v, "v")
	if !versionRe.MatchString(v) {
		release_()
		return nil, errf(http.StatusBadRequest, "invalid version %q", v)
	}
	if v == version.Version {
		release_()
		return nil, errf(http.StatusConflict, "Docklet %s is already running", v)
	}
	self, err := s.inspectSelf(ctx, dk)
	if err != nil {
		release_()
		return nil, errf(http.StatusServiceUnavailable, "%v", err)
	}
	st := updater.Status{
		ID: strconv.FormatInt(time.Now().UnixNano(), 36), State: updater.StateRunning, From: version.Version, To: v,
		Image: newImageRef(self.Image, v), StartedAt: time.Now().UTC(), Auto: auto,
	}
	rec := updater.NewRecorder(s.cfg.DataDir, st, func(m string) { s.log.Info("update", "step", m) })
	rec.Step("Updating Docklet %s → %s", st.From, v)
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		_, err := audit.Append(ctx, q, audit.Entry{ActorID: actorID, Actor: actor, Action: audit.ActionUpdateStarted, Target: "docklet", IP: ip,
			Details: map[string]any{"from": st.From, "to": v, "image": st.Image, "auto": auto}}, s.now())
		return err
	}); err != nil {
		release_()
		return nil, err
	}
	go func() {
		defer release_()
		bctx := context.WithoutCancel(ctx)
		if err := s.prepareUpdate(bctx, dk, self, rec, st); err != nil {
			rec.Finish(updater.StateFailed, err)
			s.log.Error("update failed before the switch", "err", err)
		}
	}()
	return &st, nil
}

// prepareUpdate does the steps that don't touch the running Docklet, then
// hands over to the updater container.
func (s *Server) prepareUpdate(ctx context.Context, dk updateDocker, self selfContainer, rec *updater.Recorder, st updater.Status) error {
	rec.Step("Backing up Docklet's database")
	dir := filepath.Join(s.cfg.DataDir, "self-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := filepath.Join(dir, "pre-upgrade-"+time.Now().UTC().Format("20060102-150405")+".tar.gz")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // fixed name in the data dir
	if err != nil {
		return err
	}
	_, err = selfbackup.Write(ctx, f, s.st, s.cfg.DataDir, selfbackup.Options{})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("pre-upgrade backup failed, nothing was changed: %w", err)
	}
	rec.Set(func(x *updater.Status) { x.Backup = name })
	rec.Step("Backup saved: %s", name)
	prunePreUpgrade(dir, 3)

	rec.Step("Pulling %s", st.Image)
	var lastProgress time.Time
	perr := dk.ImagePull(ctx, st.Image, func(line string) {
		if time.Since(lastProgress) > 5*time.Second {
			lastProgress = time.Now()
			rec.Step("Pull: %s", line)
		}
	})
	if perr != nil {
		if _, err := dk.ImageInspect(ctx, st.Image); err != nil {
			return fmt.Errorf("pull %s: %w", st.Image, perr)
		}
		rec.Step("Pull failed (%v); using the image already on this server", perr)
	}

	rec.Step("Starting the updater; Docklet restarts in a few seconds")
	binds := []string{self.Socket + ":/var/run/docker.sock", self.DataMount + ":/data"}
	args := []string{"self-update", "-container", self.Name, "-image", st.Image, "-to", st.To, "-data-dir", "/data"}
	if self.WorkDir != "" {
		binds = append(binds, self.WorkDir+":/install")
		args = append(args, "-env-file", "/install/.env")
	}
	id, err := dk.ContainerCreateRaw(ctx, self.Name+"-updater-"+st.ID, map[string]any{
		"Image": self.ImageID, "Entrypoint": []string{"docklet"}, "Cmd": args,
		"Env":    []string{"DOCKLET_DATA_DIR=/data"},
		"Labels": map[string]string{"docklet.managed": "true", "docklet.updater": "true"},
		"HostConfig": map[string]any{
			"Binds": binds, "NetworkMode": self.Network, "AutoRemove": true,
			"LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "1m"}},
		},
	})
	if err != nil {
		return fmt.Errorf("create the updater: %w", err)
	}
	if err := dk.ContainerStart(ctx, id); err != nil {
		return fmt.Errorf("start the updater: %w", err)
	}
	return nil
}

func prunePreUpgrade(dir string, keep int) {
	m, _ := filepath.Glob(filepath.Join(dir, "pre-upgrade-*.tar.gz"))
	sort.Strings(m)
	for len(m) > keep {
		_ = os.Remove(m[0])
		m = m[1:]
	}
}

// POST /api/system/update/start {version, password}
func (s *Server) handleUpdateStart(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Version  string `json:"version"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	a := authFrom(r.Context())
	if err := checkPassword(a.User, in.Password); err != nil {
		return err
	}
	st, err := s.startUpdate(r.Context(), strings.TrimSpace(in.Version), a.User.Username, ptr(a.User.ID), clientIP(r), false)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": st})
	return nil
}

// GET /api/system/update/status
func (s *Server) handleUpdateStatus(w http.ResponseWriter, _ *http.Request) error {
	st, err := updater.ReadStatus(s.cfg.DataDir)
	if err != nil {
		return err
	}
	auto, _ := s.autoUpdateSettings(context.Background())
	writeJSON(w, http.StatusOK, map[string]any{"current": version.Version, "inContainer": release.InContainer(), "last": st, "auto": auto})
	return nil
}

type autoUpdate struct {
	Patch bool `json:"patch"`
	Hour  int  `json:"hourUtc"` // maintenance window start (one hour)
}

func (s *Server) autoUpdateSettings(ctx context.Context) (autoUpdate, error) {
	a := autoUpdate{Hour: 3}
	v, err := getSetting(ctx, s.st.Q, "update.auto_patch")
	if err != nil {
		return a, err
	}
	a.Patch = v == "1"
	if h, err := getSetting(ctx, s.st.Q, "update.window_hour"); err == nil && h != "" {
		a.Hour, _ = strconv.Atoi(h)
	}
	return a, nil
}

// PUT /api/system/update/auto {patch, hourUtc}
func (s *Server) handleUpdateAuto(w http.ResponseWriter, r *http.Request) error {
	var in autoUpdate
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.Hour < 0 || in.Hour > 23 {
		return errf(http.StatusBadRequest, "hour must be 0-23")
	}
	ctx := r.Context()
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		if err := putSetting(ctx, q, "update.auto_patch", map[bool]string{true: "1", false: "0"}[in.Patch], s.now()); err != nil {
			return err
		}
		if err := putSetting(ctx, q, "update.window_hour", strconv.Itoa(in.Hour), s.now()); err != nil {
			return err
		}
		return s.audit(ctx, q, r, audit.ActionUpdateAuto, "docklet", map[string]any{"patch": in.Patch, "hourUtc": in.Hour})
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, in)
	return nil
}

// samePatchLine reports whether to is a newer patch release of from (1.4.0 → 1.4.2).
func samePatchLine(from, to string) bool {
	f, t := strings.Split(from, "."), strings.Split(to, ".")
	if len(f) != 3 || len(t) != 3 || f[0] != t[0] || f[1] != t[1] {
		return false
	}
	a, err1 := strconv.Atoi(f[2])
	b, err2 := strconv.Atoi(t[2])
	return err1 == nil && err2 == nil && b > a
}

// runAutoUpdate installs patch releases in the maintenance window.
func (s *Server) runAutoUpdate(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	tried := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a, err := s.autoUpdateSettings(ctx)
		if err != nil || !a.Patch || time.Now().UTC().Hour() != a.Hour || s.cfg.DisableUpdateCheck {
			continue
		}
		day := time.Now().UTC().Format("2006-01-02")
		if tried == day {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		rel, err := s.updates.rel.Latest(rctx, "")
		cancel()
		if err != nil {
			continue
		}
		tried = day
		to := strings.TrimPrefix(rel.Tag, "v")
		if !samePatchLine(version.Version, to) {
			continue
		}
		// Don't retry a version that already rolled back.
		if last, _ := updater.ReadStatus(s.cfg.DataDir); last != nil && last.To == to && last.State != updater.StateSucceeded {
			continue
		}
		if _, err := s.startUpdate(ctx, to, "system", nil, "", true); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("automatic update", "to", to, "err", err)
		}
	}
}
