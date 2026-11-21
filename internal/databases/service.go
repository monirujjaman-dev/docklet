package databases

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// Container labels.
const (
	LabelManaged  = "docklet.managed"
	LabelProject  = "docklet.project"
	LabelDatabase = "docklet.database"
)

// Runtime statuses.
const (
	StatusCreating = "creating"
	StatusRunning  = "running"
	StatusStopped  = "stopped"
	StatusFailed   = "failed"
	StatusMissing  = "missing"
	StatusExited   = "exited"
)

// Docker is the subset of the Docker client the service needs.
type Docker interface {
	ImagePull(ctx context.Context, ref string, progress func(string)) error
	ImageInspect(ctx context.Context, ref string) (docker.Image, error)
	VolumeCreate(ctx context.Context, name string, labels map[string]string) error
	VolumeRemove(ctx context.Context, name string) error
	ContainerCreate(ctx context.Context, s docker.ContainerSpec) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerStop(ctx context.Context, id string, timeoutSec int) error
	ContainerRestart(ctx context.Context, id string, timeoutSec int) error
	ContainerRemove(ctx context.Context, id string) error
	ContainerInspect(ctx context.Context, id string) (docker.ContainerInfo, error)
	Exec(ctx context.Context, id string, cmd, env []string, stdout io.Writer) (docker.ExecResult, error)
	CopyTo(ctx context.Context, id, dir, name string, content io.Reader, size, mode int64) error
	CopyFrom(ctx context.Context, id, path string, w io.Writer) (int64, error)
}

// Service manages databases.
type Service struct {
	st  *store.Store
	dk  Docker
	box *secrets.Box
	log *slog.Logger
	now func() time.Time

	// EnsureNetwork creates the project network (shared with apps).
	EnsureNetwork func(ctx context.Context, projectID int64) (string, error)
	// SealEnv encrypts an app env value (for links).
	SealEnv func(appID int64, key, value string) ([]byte, error)

	BackupDir         string
	ReadyTimeout      time.Duration
	ReadyInterval     time.Duration
	ReconcileInterval time.Duration

	locks sync.Map // database id -> *sync.Mutex
	wg    sync.WaitGroup
}

// New creates a service.
func New(st *store.Store, dk Docker, box *secrets.Box, log *slog.Logger, backupDir string) *Service {
	return &Service{
		st: st, dk: dk, box: box, log: log, now: time.Now, BackupDir: backupDir,
		ReadyTimeout: 2 * time.Minute, ReadyInterval: 500 * time.Millisecond, ReconcileInterval: 30 * time.Second,
	}
}

// SetClock overrides the time source.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func (s *Service) lock(id int64) func() {
	m, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// VolumeName is the data volume of a database.
func VolumeName(id int64) string { return fmt.Sprintf("docklet-db-%d", id) }

func containerName(project, name string) string {
	return "docklet-db-" + strings.ToLower(project) + "-" + strings.ToLower(name)
}

// GeneratePassword returns a random alphanumeric password (URL-safe).
func GeneratePassword() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 28)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b), nil
}

// SealCreds encrypts credentials for a new database row. The row id is not
// known yet, so the AAD uses the (project, name) pair instead.
func (s *Service) SealCreds(projectID int64, name, pw, rootPw string) (pwEnc, rootEnc []byte, err error) {
	aad := fmt.Sprintf("db:%d/%s", projectID, strings.ToLower(name))
	if pwEnc, err = s.box.Seal([]byte(pw), []byte(aad+":password")); err != nil {
		return nil, nil, err
	}
	if rootPw != "" {
		if rootEnc, err = s.box.Seal([]byte(rootPw), []byte(aad+":root")); err != nil {
			return nil, nil, err
		}
	}
	return pwEnc, rootEnc, nil
}

// Creds decrypts a database's credentials.
func (s *Service) Creds(d db.Database) (Creds, error) {
	aad := fmt.Sprintf("db:%d/%s", d.ProjectID, strings.ToLower(d.Name))
	pw, err := s.box.Open(d.PasswordEnc, []byte(aad+":password"))
	if err != nil {
		return Creds{}, fmt.Errorf("decrypt password: %w", err)
	}
	c := Creds{DBName: d.DbName, User: d.Username, Password: string(pw)}
	if len(d.RootPasswordEnc) > 0 {
		root, err := s.box.Open(d.RootPasswordEnc, []byte(aad+":root"))
		if err != nil {
			return Creds{}, fmt.Errorf("decrypt root password: %w", err)
		}
		c.RootPassword = string(root)
	}
	return c, nil
}

// InternalURL is the connection string for apps on the project network.
func (s *Service) InternalURL(d db.Database) (string, error) {
	e := Engines[d.Engine]
	c, err := s.Creds(d)
	if err != nil {
		return "", err
	}
	return e.URL(strings.ToLower(d.Name), c), nil
}

func (s *Service) setRuntime(ctx context.Context, id int64, status, msg string) {
	if err := s.st.Q.SetDatabaseRuntime(context.WithoutCancel(ctx), db.SetDatabaseRuntimeParams{
		RuntimeStatus: status, RuntimeMessage: msg, ID: id,
	}); err != nil {
		s.log.Warn("save database status", "id", id, "err", err)
	}
}

// ProvisionAsync creates the container in the background.
func (s *Service) ProvisionAsync(id int64) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if err := s.Provision(ctx, id); err != nil {
			s.log.Warn("database provisioning failed", "id", id, "err", err)
		}
	}()
}

// Wait blocks until background work finishes (tests, shutdown).
func (s *Service) Wait() { s.wg.Wait() }

// Provision pulls the image, creates the volume and container, and waits
// until the database accepts connections.
func (s *Service) Provision(ctx context.Context, id int64) error {
	defer s.lock(id)()
	d, err := s.st.Q.GetDatabase(ctx, id)
	if err != nil {
		return err
	}
	s.setRuntime(ctx, id, StatusCreating, "Pulling image…")
	if err := s.create(ctx, d, true); err != nil {
		s.setRuntime(ctx, id, StatusFailed, err.Error())
		s.auditSystem(ctx, d, audit.ActionDatabaseFailed, map[string]any{"error": err.Error()})
		return err
	}
	s.setRuntime(ctx, id, StatusRunning, "")
	s.auditSystem(ctx, d, audit.ActionDatabaseReady, nil)
	return nil
}

func (s *Service) auditSystem(ctx context.Context, d db.Database, action string, details map[string]any) {
	ctx = context.WithoutCancel(ctx)
	if details == nil {
		details = map[string]any{}
	}
	details["name"] = d.Name
	_ = s.st.Tx(ctx, func(q *db.Queries) error {
		_, err := audit.Append(ctx, q, audit.Entry{
			Actor: "system", Action: action, Target: fmt.Sprintf("database:%d", d.ID), Details: details,
		}, s.now())
		return err
	})
}

// create (re)creates the container from the row. pull=false reuses the
// local image (recreations).
func (s *Service) create(ctx context.Context, d db.Database, pull bool) error {
	e, ok := Engines[d.Engine]
	if !ok {
		return fmt.Errorf("unknown engine %q", d.Engine)
	}
	image := e.Image(d.Version)
	if pull {
		if err := s.dk.ImagePull(ctx, image, nil); err != nil {
			if _, ierr := s.dk.ImageInspect(ctx, image); ierr != nil {
				return fmt.Errorf("pull %s: %w", image, err)
			}
		}
	}
	creds, err := s.Creds(d)
	if err != nil {
		return err
	}
	project, err := s.st.Q.GetProject(ctx, d.ProjectID)
	if err != nil {
		return err
	}
	network := fmt.Sprintf("docklet-p%d", d.ProjectID)
	if s.EnsureNetwork != nil {
		if network, err = s.EnsureNetwork(ctx, d.ProjectID); err != nil {
			return fmt.Errorf("network: %w", err)
		}
	}
	labels := map[string]string{LabelManaged: "true", LabelProject: fmt.Sprint(d.ProjectID), LabelDatabase: fmt.Sprint(d.ID)}
	if err := s.dk.VolumeCreate(ctx, VolumeName(d.ID), labels); err != nil {
		return fmt.Errorf("volume: %w", err)
	}

	name := containerName(project.Name, d.Name)
	_ = s.dk.ContainerRemove(ctx, name) // leftover from an interrupted attempt
	spec := docker.ContainerSpec{
		Name: name, Image: image, Env: e.containerEnv(creds), Labels: labels,
		Hostname: strings.ToLower(d.Name), Port: e.Port, Network: network, Aliases: []string{strings.ToLower(d.Name)},
		RestartPolicy: "unless-stopped", MemoryBytes: d.MemoryMb << 20,
		Volumes: map[string]string{VolumeName(d.ID): e.dataPath(d.Version)},
	}
	if e.cmd != nil {
		spec.Cmd = e.cmd(creds)
	}
	if d.PublicPort > 0 {
		spec.Publish = map[int]int{e.Port: int(d.PublicPort)}
	}
	s.setRuntime(ctx, d.ID, StatusCreating, "Starting…")
	id, err := s.dk.ContainerCreate(ctx, spec)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := s.dk.ContainerStart(ctx, id); err != nil {
		_ = s.dk.ContainerRemove(ctx, id)
		if strings.Contains(err.Error(), "port is already allocated") || strings.Contains(err.Error(), "address already in use") {
			return fmt.Errorf("public port %d is already in use on this server", d.PublicPort)
		}
		return fmt.Errorf("start container: %w", err)
	}
	if err := s.st.Q.SetDatabaseContainer(ctx, db.SetDatabaseContainerParams{ContainerID: id, UpdatedAt: s.now().Unix(), ID: d.ID}); err != nil {
		return err
	}
	s.setRuntime(ctx, d.ID, StatusCreating, "Waiting for the database to accept connections…")
	return s.waitReady(ctx, e, creds, id)
}

func (s *Service) waitReady(ctx context.Context, e *Engine, c Creds, id string) error {
	ctx, cancel := context.WithTimeout(ctx, s.ReadyTimeout)
	defer cancel()
	cmd, env := e.readyCmd(c)
	var last string
	for {
		info, err := s.dk.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		if !info.State.Running && !info.State.Restarting {
			return fmt.Errorf("the database container exited with code %d; check its logs", info.State.ExitCode)
		}
		var out bytes.Buffer
		res, err := s.dk.Exec(ctx, id, cmd, env, &out)
		if err == nil && res.ExitCode == 0 && (e.readyStdout == "" || strings.Contains(out.String(), e.readyStdout)) {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = strings.TrimSpace(res.Stderr + " " + out.String())
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not ready after %s: %s", s.ReadyTimeout, last)
		case <-time.After(s.ReadyInterval):
		}
	}
}

// Start, Stop and Restart act on the container.
func (s *Service) Start(ctx context.Context, id int64) error {
	return s.lifecycle(ctx, id, "start")
}

// Stop stops the container (data stays in the volume).
func (s *Service) Stop(ctx context.Context, id int64) error { return s.lifecycle(ctx, id, "stop") }

// Restart restarts the container.
func (s *Service) Restart(ctx context.Context, id int64) error {
	return s.lifecycle(ctx, id, "restart")
}

func (s *Service) lifecycle(ctx context.Context, id int64, action string) error {
	defer s.lock(id)()
	d, err := s.st.Q.GetDatabase(ctx, id)
	if err != nil {
		return err
	}
	if d.ContainerID == "" {
		return errors.New("the database has no container yet")
	}
	switch action {
	case "start":
		err = s.dk.ContainerStart(ctx, d.ContainerID)
	case "stop":
		err = s.dk.ContainerStop(ctx, d.ContainerID, 30)
	case "restart":
		err = s.dk.ContainerRestart(ctx, d.ContainerID, 30)
	}
	if errors.Is(err, docker.ErrNotFound) {
		return errors.New("the container no longer exists; use Recreate")
	}
	if err != nil {
		return err
	}
	desired := StatusRunning
	if action == "stop" {
		desired = StatusStopped
	}
	if err := s.st.Q.SetDatabaseDesired(ctx, db.SetDatabaseDesiredParams{DesiredStatus: desired, UpdatedAt: s.now().Unix(), ID: id}); err != nil {
		return err
	}
	if action != "stop" {
		e := Engines[d.Engine]
		c, err := s.Creds(d)
		if err != nil {
			return err
		}
		if err := s.waitReady(ctx, e, c, d.ContainerID); err != nil {
			s.setRuntime(ctx, id, StatusFailed, err.Error())
			return err
		}
	}
	s.setRuntime(ctx, id, desired, "")
	return nil
}

// SetPublicPort publishes (port > 0) or hides (0) the database on the host.
// The container is recreated on the same volume; on failure the previous
// configuration is restored.
func (s *Service) SetPublicPort(ctx context.Context, id int64, port int64) error {
	defer s.lock(id)()
	d, err := s.st.Q.GetDatabase(ctx, id)
	if err != nil {
		return err
	}
	prev := d.PublicPort
	if prev == port {
		return nil
	}
	if err := s.st.Q.SetDatabasePublicPort(ctx, db.SetDatabasePublicPortParams{PublicPort: port, UpdatedAt: s.now().Unix(), ID: id}); err != nil {
		return err
	}
	d.PublicPort = port
	if err := s.recreate(ctx, d); err != nil {
		s.log.Warn("public port change failed; restoring", "db", d.Name, "err", err)
		_ = s.st.Q.SetDatabasePublicPort(context.WithoutCancel(ctx), db.SetDatabasePublicPortParams{PublicPort: prev, UpdatedAt: s.now().Unix(), ID: id})
		d.PublicPort = prev
		if rerr := s.recreate(context.WithoutCancel(ctx), d); rerr != nil {
			s.setRuntime(ctx, id, StatusFailed, rerr.Error())
		}
		return err
	}
	return nil
}

// Recreate rebuilds the container on the same volume (e.g. after it was
// deleted outside Docklet).
func (s *Service) Recreate(ctx context.Context, id int64) error {
	defer s.lock(id)()
	d, err := s.st.Q.GetDatabase(ctx, id)
	if err != nil {
		return err
	}
	return s.recreate(ctx, d)
}

func (s *Service) recreate(ctx context.Context, d db.Database) error {
	if d.ContainerID != "" {
		_ = s.dk.ContainerStop(ctx, d.ContainerID, 30)
		if err := s.dk.ContainerRemove(ctx, d.ContainerID); err != nil {
			return err
		}
	}
	if err := s.create(ctx, d, false); err != nil {
		s.setRuntime(ctx, d.ID, StatusFailed, err.Error())
		return err
	}
	_ = s.st.Q.SetDatabaseDesired(ctx, db.SetDatabaseDesiredParams{DesiredStatus: StatusRunning, UpdatedAt: s.now().Unix(), ID: d.ID})
	s.setRuntime(ctx, d.ID, StatusRunning, "")
	return nil
}

// Delete removes the container and, with deleteData, the volume and backups.
// Linked apps lose their env var. auditFn runs in the deleting transaction.
func (s *Service) Delete(ctx context.Context, id int64, deleteData bool, auditFn func(q *db.Queries) error) error {
	defer s.lock(id)()
	d, err := s.st.Q.GetDatabase(ctx, id)
	if err != nil {
		return err
	}
	if d.ContainerID != "" {
		if err := s.dk.ContainerRemove(ctx, d.ContainerID); err != nil {
			return err
		}
	}
	links, err := s.st.Q.ListDatabaseLinks(ctx, id)
	if err != nil {
		return err
	}
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		for _, l := range links {
			if err := q.DeleteEnv(ctx, db.DeleteEnvParams{AppID: l.AppID, Key: l.EnvKey}); err != nil {
				return err
			}
		}
		if err := q.DeleteDatabase(ctx, id); err != nil {
			return err
		}
		return auditFn(q)
	})
	if err != nil {
		return err
	}
	if deleteData {
		if err := s.dk.VolumeRemove(ctx, VolumeName(id)); err != nil {
			s.log.Warn("remove database volume", "volume", VolumeName(id), "err", err)
		}
		s.removeBackupDir(id)
	}
	return nil
}

// Link writes the connection URL into an app's environment (same project).
func (s *Service) Link(ctx context.Context, q *db.Queries, d db.Database, appID int64, envKey string) error {
	app, err := q.GetApp(ctx, appID)
	if err != nil {
		return err
	}
	if app.ProjectID != d.ProjectID {
		return errors.New("the app must be in the same project as the database")
	}
	u, err := s.InternalURL(d)
	if err != nil {
		return err
	}
	enc, err := s.SealEnv(appID, envKey, u)
	if err != nil {
		return err
	}
	if err := q.UpsertEnv(ctx, db.UpsertEnvParams{AppID: appID, Key: envKey, ValueEnc: enc, UpdatedAt: s.now().Unix()}); err != nil {
		return err
	}
	return q.UpsertDatabaseLink(ctx, db.UpsertDatabaseLinkParams{DatabaseID: d.ID, AppID: appID, EnvKey: envKey, CreatedAt: s.now().Unix()})
}

// Unlink removes the env var a link created.
func (s *Service) Unlink(ctx context.Context, q *db.Queries, databaseID, appID int64) error {
	links, err := q.ListDatabaseLinks(ctx, databaseID)
	if err != nil {
		return err
	}
	for _, l := range links {
		if l.AppID == appID {
			if err := q.DeleteEnv(ctx, db.DeleteEnvParams{AppID: appID, Key: l.EnvKey}); err != nil {
				return err
			}
		}
	}
	return q.DeleteDatabaseLink(ctx, db.DeleteDatabaseLinkParams{DatabaseID: databaseID, AppID: appID})
}

// Run reconciles container state and runs scheduled backups.
func (s *Service) Run(ctx context.Context) {
	if n, err := s.st.Q.FailRunningBackups(ctx, s.now().Unix()); err == nil && n > 0 {
		s.log.Info("marked interrupted backups as failed", "count", n)
	}
	// Resume provisioning interrupted by a restart.
	if list, err := s.st.Q.ListDatabases(ctx); err == nil {
		for _, d := range list {
			if d.RuntimeStatus == StatusCreating {
				s.ProvisionAsync(d.ID)
			}
		}
	}
	rec := time.NewTicker(s.ReconcileInterval)
	defer rec.Stop()
	sched := time.NewTicker(time.Minute)
	defer sched.Stop()
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-rec.C:
			if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("database reconcile", "err", err)
			}
		case <-sched.C:
			s.RunScheduled(ctx)
		}
	}
}

// Reconcile records each database's real container state.
func (s *Service) Reconcile(ctx context.Context) error {
	list, err := s.st.Q.ListDatabases(ctx)
	if err != nil {
		return err
	}
	for _, d := range list {
		if d.RuntimeStatus == StatusCreating || d.ContainerID == "" {
			continue
		}
		status, msg := StatusRunning, ""
		info, err := s.dk.ContainerInspect(ctx, d.ContainerID)
		switch {
		case errors.Is(err, docker.ErrNotFound):
			status, msg = StatusMissing, "The container was removed outside Docklet. Recreate it; the data volume is kept."
		case err != nil:
			return err
		case info.State.Running:
		case d.DesiredStatus == StatusStopped:
			status = StatusStopped
		default:
			status, msg = StatusExited, "Exited with code "+strconv.Itoa(info.State.ExitCode)
		}
		if d.RuntimeStatus == StatusFailed && status == StatusRunning {
			continue // keep the failure message until an action succeeds
		}
		if status != d.RuntimeStatus || msg != d.RuntimeMessage {
			s.setRuntime(ctx, d.ID, status, msg)
		}
	}
	return nil
}

// ErrNotFound is returned for unknown databases.
var ErrNotFound = sql.ErrNoRows
