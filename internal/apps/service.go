// Package apps runs application deployments and lifecycle actions on Docker.
package apps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/build"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// Container labels.
const (
	LabelManaged    = "docklet.managed"
	LabelProject    = "docklet.project"
	LabelApp        = "docklet.app"
	LabelDeployment = "docklet.deployment"
)

// Status values.
const (
	DesiredRunning = "running"
	DesiredStopped = "stopped"

	DeployQueued     = "queued"
	DeployRunning    = "running"
	DeploySucceeded  = "succeeded"
	DeployFailed     = "failed"
	DeploySuperseded = "superseded"

	JobDeploy = "deploy"

	SourceImage    = "image"
	SourceGit      = "git"
	SourceRollback = "rollback"
)

// ErrNoDeployment means the app has never been deployed successfully.
var ErrNoDeployment = errors.New("app has no running deployment yet; deploy it first")

// Docker is the subset of the Docker client the service uses.
type Docker interface {
	Ping(ctx context.Context) (string, error)
	ImagePull(ctx context.Context, ref string, progress func(string)) error
	ImageInspect(ctx context.Context, ref string) (docker.Image, error)
	ImageRemove(ctx context.Context, ref string) error
	NetworkEnsure(ctx context.Context, name string, labels map[string]string) error
	NetworkAttach(ctx context.Context, network, container string) error
	NetworkRemove(ctx context.Context, name string) error
	ContainerCreate(ctx context.Context, s docker.ContainerSpec) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerStop(ctx context.Context, id string, timeoutSec int) error
	ContainerRestart(ctx context.Context, id string, timeoutSec int) error
	ContainerRemove(ctx context.Context, id string) error
	ContainerInspect(ctx context.Context, id string) (docker.ContainerInfo, error)
	ContainerList(ctx context.Context, labels map[string]string) ([]docker.ContainerSummary, error)
	ContainerLogs(ctx context.Context, id string, o docker.LogOptions, fn func(docker.LogLine) error) error
}

// Builder clones and builds Git sources (internal/build).
type Builder interface {
	Build(ctx context.Context, spec build.Spec, logf func(string)) (build.Result, error)
}

// Proxy applies routing changes synchronously (Caddy reconciler).
type Proxy interface {
	Sync(ctx context.Context) error
}

// Service coordinates deployments and lifecycle actions.
type Service struct {
	st    *store.Store
	dk    Docker
	box   *secrets.Box
	log   *slog.Logger
	proxy Proxy
	now   func() time.Time

	// Tunables (tests shorten them).
	Workers           int
	HealthTimeout     time.Duration
	HealthInterval    time.Duration
	DrainDelay        time.Duration
	StopTimeoutSec    int
	ReconcileInterval time.Duration
	HealthCheck       func(ctx context.Context, ip string, port int) error

	// Builder builds Git sources; nil disables Git deployments.
	Builder Builder
	// KeepImages is how many built images per app are kept for rollback.
	KeepImages int

	// AttachContainers are joined to every project network. Set it when
	// Docklet and Caddy run in containers (e.g. Docker Desktop) so they can
	// reach app containers by IP.
	AttachContainers []string

	wake  chan struct{}
	locks sync.Map // app id -> *sync.Mutex
}

// New creates a service. proxy may be nil when Caddy integration is off.
func New(st *store.Store, dk Docker, box *secrets.Box, log *slog.Logger, proxy Proxy) *Service {
	return &Service{
		st: st, dk: dk, box: box, log: log, proxy: proxy, now: time.Now,
		Workers: 2, HealthTimeout: 60 * time.Second, HealthInterval: 500 * time.Millisecond,
		DrainDelay: 2 * time.Second, StopTimeoutSec: 10, ReconcileInterval: 30 * time.Second,
		HealthCheck: tcpCheck, KeepImages: 5,
		wake: make(chan struct{}, 1),
	}
}

// SetClock overrides the time source.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func tcpCheck(ctx context.Context, ip string, port int) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return c.Close()
}

// ensureNetwork creates the project network and attaches AttachContainers.
func (s *Service) ensureNetwork(ctx context.Context, projectID int64) (string, error) {
	network := NetworkName(projectID)
	if err := s.dk.NetworkEnsure(ctx, network, map[string]string{LabelManaged: "true", LabelProject: fmt.Sprint(projectID)}); err != nil {
		return "", err
	}
	if err := s.attach(ctx, network); err != nil {
		return "", err
	}
	return network, nil
}

func (s *Service) attach(ctx context.Context, network string) error {
	for _, c := range s.AttachContainers {
		if err := s.dk.NetworkAttach(ctx, network, c); err != nil {
			return fmt.Errorf("attach %s to %s: %w", c, network, err)
		}
	}
	return nil
}

// NetworkName is the per-project Docker network.
func NetworkName(projectID int64) string { return fmt.Sprintf("docklet-p%d", projectID) }

func (s *Service) lock(appID int64) func() {
	m, _ := s.locks.LoadOrStore(appID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Ping reports whether Docker is usable.
func (s *Service) Ping(ctx context.Context) error {
	_, err := s.dk.Ping(ctx)
	return err
}

// EnqueueDeploy records a deployment and a job inside the caller's transaction.
// Call Wake after the transaction commits.
func (s *Service) EnqueueDeploy(ctx context.Context, q *db.Queries, app db.App, actor string) (db.Deployment, error) {
	p := db.CreateDeploymentParams{AppID: app.ID, Image: app.Image, Source: SourceImage, CreatedBy: actor, CreatedAt: s.now().Unix()}
	if app.SourceType == SourceGit {
		p.Image, p.Source = "", SourceGit // set once the build finishes
	}
	return s.enqueue(ctx, q, p)
}

// ErrNotRollbackable explains why a deployment cannot be rolled back to.
var ErrNotRollbackable = errors.New("that deployment's image is no longer available")

// EnqueueRollback redeploys the exact image of an earlier deployment,
// without pulling or building.
func (s *Service) EnqueueRollback(ctx context.Context, q *db.Queries, app db.App, target db.Deployment, actor string) (db.Deployment, error) {
	if target.AppID != app.ID || target.ImageID == "" || target.ImagePruned == 1 ||
		(target.Status != DeploySucceeded && target.Status != DeploySuperseded) {
		return db.Deployment{}, ErrNotRollbackable
	}
	return s.enqueue(ctx, q, db.CreateDeploymentParams{
		AppID: app.ID, Image: target.Image, ImageID: target.ImageID, Source: SourceRollback,
		RollbackOf: sql.NullInt64{Int64: target.ID, Valid: true}, CreatedBy: actor, CreatedAt: s.now().Unix(),
	})
}

func (s *Service) enqueue(ctx context.Context, q *db.Queries, p db.CreateDeploymentParams) (db.Deployment, error) {
	dep, err := q.CreateDeployment(ctx, p)
	if err != nil {
		return dep, err
	}
	_, err = q.InsertJob(ctx, db.InsertJobParams{Kind: JobDeploy, RefID: dep.ID, CreatedAt: p.CreatedAt})
	return dep, err
}

// DeployKeyAAD binds an encrypted deploy key to its app.
func DeployKeyAAD(appID int64) []byte { return []byte(fmt.Sprintf("app:%d:deploykey", appID)) }

// WebhookSecretAAD binds an encrypted webhook secret to its app.
func WebhookSecretAAD(appID int64) []byte { return []byte(fmt.Sprintf("app:%d:webhook", appID)) }

// Seal encrypts a value with the master key (deploy keys, webhook secrets).
func (s *Service) Seal(value, aad []byte) ([]byte, error) { return s.box.Seal(value, aad) }

// Open decrypts a value sealed with Seal.
func (s *Service) Open(value, aad []byte) ([]byte, error) { return s.box.Open(value, aad) }

// Wake nudges the job workers.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run starts job workers and the Docker state reconciler.
func (s *Service) Run(ctx context.Context) {
	if n, err := s.st.Q.RequeueRunningJobs(ctx); err != nil {
		s.log.Error("requeue jobs", "err", err)
	} else if n > 0 {
		s.log.Info("resuming interrupted jobs", "count", n)
	}
	var wg sync.WaitGroup
	for i := 0; i < s.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.worker(ctx)
		}()
	}
	s.Wake()

	t := time.NewTicker(s.ReconcileInterval)
	defer t.Stop()
	s.reconcileLogged(ctx)
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
			s.reconcileLogged(ctx)
		}
	}
}

func (s *Service) reconcileLogged(ctx context.Context) {
	if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("docker reconcile failed", "err", err)
	}
}

func (s *Service) worker(ctx context.Context) {
	for {
		job, err := s.st.Q.ClaimJob(ctx, s.now().Unix())
		if errors.Is(err, sql.ErrNoRows) {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Error("claim job", "err", err)
			time.Sleep(time.Second)
			continue
		}
		// Another worker may be idle; let it check for more jobs.
		s.Wake()

		var jerr error
		switch job.Kind {
		case JobDeploy:
			jerr = s.runDeploy(ctx, job.RefID)
		default:
			jerr = fmt.Errorf("unknown job kind %q", job.Kind)
		}
		if ctx.Err() != nil {
			return // leave as running; requeued on next start
		}
		status, msg := "done", ""
		if jerr != nil {
			status, msg = "failed", jerr.Error()
		}
		if err := s.st.Q.FinishJob(ctx, db.FinishJobParams{Status: status, Error: msg, FinishedAt: s.now().Unix(), ID: job.ID}); err != nil {
			s.log.Error("finish job", "id", job.ID, "err", err)
		}
	}
}

func (s *Service) syncProxy(ctx context.Context) error {
	if s.proxy == nil {
		return nil
	}
	return s.proxy.Sync(ctx)
}

// envFor decrypts an app's environment as KEY=VALUE pairs.
func (s *Service) envFor(ctx context.Context, appID int64) ([]string, error) {
	rows, err := s.st.Q.ListEnv(ctx, appID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		v, err := s.box.Open(r.ValueEnc, EnvAAD(appID, r.Key))
		if err != nil {
			return nil, fmt.Errorf("decrypt env %s: %w", r.Key, err)
		}
		out = append(out, r.Key+"="+string(v))
	}
	return out, nil
}

// EnvAAD binds an encrypted env value to its app and key.
func EnvAAD(appID int64, key string) []byte {
	return []byte(fmt.Sprintf("app:%d:env:%s", appID, key))
}

// SealEnv encrypts an env value for storage.
func (s *Service) SealEnv(appID int64, key, value string) ([]byte, error) {
	return s.box.Seal([]byte(value), EnvAAD(appID, key))
}
