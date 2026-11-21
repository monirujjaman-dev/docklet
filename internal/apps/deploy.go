package apps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/build"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

const maxDeployLog = 64 << 10

// deployLog collects human-readable progress, bounded in size.
type deployLog struct {
	mu  sync.Mutex
	b   strings.Builder
	now func() time.Time
}

func (l *deployLog) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.b.Len() > maxDeployLog {
		return
	}
	fmt.Fprintf(&l.b, "%s  %s\n", l.now().UTC().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func (l *deployLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// runDeploy executes one deployment: pull → create → start → health check →
// switch Caddy → retire the previous container. On any failure the new
// container is removed and the previous version keeps serving.
func (s *Service) runDeploy(ctx context.Context, depID int64) error {
	dep, err := s.st.Q.GetDeployment(ctx, depID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // app deleted meanwhile
	}
	if err != nil {
		return err
	}
	if dep.Status != DeployQueued && dep.Status != DeployRunning {
		return nil
	}
	unlock := s.lock(dep.AppID)
	defer unlock()

	app, err := s.st.Q.GetApp(ctx, dep.AppID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	project, err := s.st.Q.GetProject(ctx, app.ProjectID)
	if err != nil {
		return err
	}

	lg := &deployLog{now: s.now}
	if err := s.st.Q.StartDeployment(ctx, db.StartDeploymentParams{StartedAt: s.now().Unix(), ID: dep.ID}); err != nil {
		return err
	}
	lg.printf("Deployment #%d of %s/%s started by %s", dep.ID, project.Name, app.Name, dep.CreatedBy)

	// Save the log every 2 s so the dashboard can follow long builds.
	flushCtx, stopFlush := context.WithCancel(ctx)
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-flushCtx.Done():
				return
			case <-t.C:
				_ = s.st.Q.UpdateDeploymentLog(flushCtx, db.UpdateDeploymentLogParams{Log: lg.String(), ID: dep.ID})
			}
		}
	}()
	defer func() { stopFlush(); <-flushed }()

	containerID, derr := s.deploySteps(ctx, lg, project, app, dep)
	if derr != nil {
		if containerID != "" {
			_ = s.dk.ContainerRemove(context.WithoutCancel(ctx), containerID)
		}
		lg.printf("FAILED: %v", derr)
		if app.CurrentDeploymentID.Valid {
			lg.printf("The previous deployment keeps running.")
		}
		s.finish(ctx, app, dep, DeployFailed, derr.Error(), lg.String(), audit.ActionDeployFailed)
		return derr
	}
	lg.printf("Deployment succeeded.")
	stopFlush()
	<-flushed
	s.finish(ctx, app, dep, DeploySucceeded, "", lg.String(), audit.ActionDeploySucceeded)
	s.cleanupImages(context.WithoutCancel(ctx), app.ID)
	return nil
}

// cleanupImages removes built images beyond the newest KeepImages per app.
// Images still used by a container are skipped and retried next time.
func (s *Service) cleanupImages(ctx context.Context, appID int64) {
	rows, err := s.st.Q.ListPrunableBuilds(ctx, appID)
	if err != nil {
		s.log.Warn("list images for cleanup", "err", err)
		return
	}
	kept, seen := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		if seen[r.Image] { // rollbacks share their target's tag
			continue
		}
		seen[r.Image] = true
		if len(kept) < s.KeepImages {
			kept[r.Image] = true
			continue
		}
		if err := s.dk.ImageRemove(ctx, r.Image); err != nil {
			if !errors.Is(err, docker.ErrConflict) {
				s.log.Warn("remove old image", "image", r.Image, "err", err)
			}
			continue
		}
		_ = s.st.Q.MarkImagePruned(ctx, db.MarkImagePrunedParams{AppID: appID, Image: r.Image})
		s.log.Info("removed old build image", "image", r.Image)
	}
}

func (s *Service) finish(ctx context.Context, app db.App, dep db.Deployment, status, errMsg, log, action string) {
	ctx = context.WithoutCancel(ctx)
	now := s.now()
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.FinishDeployment(ctx, db.FinishDeploymentParams{
			Status: status, Error: errMsg, Log: log, FinishedAt: now.Unix(), ID: dep.ID,
		}); err != nil {
			return err
		}
		details := map[string]any{"deployment": dep.ID, "image": dep.Image}
		if errMsg != "" {
			details["error"] = errMsg
		}
		_, err := audit.Append(ctx, q, audit.Entry{
			Actor: "system", Action: action, Target: fmt.Sprintf("app:%d", app.ID), Details: details,
		}, now)
		return err
	})
	if err != nil {
		s.log.Error("finish deployment", "id", dep.ID, "err", err)
	}
}

// resolveImage pulls, builds or reuses the deployment's image and returns
// the reference to run.
func (s *Service) resolveImage(ctx context.Context, lg *deployLog, project db.Project, app db.App, dep db.Deployment) (string, error) {
	switch dep.Source {
	case SourceRollback:
		lg.printf("Rolling back to the image of deployment #%d (%s), no build or pull", dep.RollbackOf.Int64, shortImageID(dep.ImageID))
		if _, err := s.dk.ImageInspect(ctx, dep.ImageID); err != nil {
			return "", fmt.Errorf("%w (%s)", ErrNotRollbackable, shortImageID(dep.ImageID))
		}
		return dep.ImageID, nil

	case SourceGit:
		if s.Builder == nil {
			return "", errors.New("builds are not available on this server (git and the docker CLI are required)")
		}
		var key []byte
		if len(app.DeployKeyEnc) > 0 {
			k, err := s.box.Open(app.DeployKeyEnc, DeployKeyAAD(app.ID))
			if err != nil {
				return "", fmt.Errorf("decrypt deploy key: %w", err)
			}
			key = k
		}
		tag := fmt.Sprintf("docklet/%s-%s:d%d", strings.ToLower(project.Name), strings.ToLower(app.Name), dep.ID)
		res, err := s.Builder.Build(ctx, build.Spec{
			RepoURL: app.GitUrl, Branch: app.GitBranch, Context: app.BuildContext, Dockerfile: app.Dockerfile,
			PrivateKey: key, Tag: tag,
			Labels: map[string]string{LabelManaged: "true", LabelApp: fmt.Sprint(app.ID), LabelDeployment: fmt.Sprint(dep.ID)},
		}, func(line string) { lg.printf("%s", line) })
		if err != nil {
			if res.CommitSHA != "" { // record which commit broke the build
				_ = s.st.Q.SetDeploymentBuild(ctx, db.SetDeploymentBuildParams{
					Image: "", CommitSha: res.CommitSHA, CommitMessage: res.CommitMessage, ID: dep.ID,
				})
			}
			return "", err
		}
		if err := s.st.Q.SetDeploymentBuild(ctx, db.SetDeploymentBuildParams{
			Image: tag, CommitSha: res.CommitSHA, CommitMessage: res.CommitMessage, ID: dep.ID,
		}); err != nil {
			return "", err
		}
		return tag, nil

	default:
		// A local image is accepted if the registry is unreachable.
		image := dep.Image
		lg.printf("Pulling %s", image)
		if err := s.dk.ImagePull(ctx, image, func(line string) { lg.printf("  %s", line) }); err != nil {
			if _, ierr := s.dk.ImageInspect(ctx, image); ierr != nil {
				return "", fmt.Errorf("pull %s: %w", image, err)
			}
			lg.printf("Pull failed (%v); using the image already on this host", err)
		}
		return image, nil
	}
}

func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func (s *Service) deploySteps(ctx context.Context, lg *deployLog, project db.Project, app db.App, dep db.Deployment) (string, error) {
	// 1. Get the image: pull, build from Git, or reuse for a rollback.
	image, err := s.resolveImage(ctx, lg, project, app, dep)
	if err != nil {
		return "", err
	}
	img, err := s.dk.ImageInspect(ctx, image)
	if err != nil {
		return "", fmt.Errorf("inspect image: %w", err)
	}
	port := int(app.Port)
	if port == 0 {
		port = img.FirstTCPPort()
		if port == 0 {
			return "", errors.New("the image exposes no TCP port; set the app's port")
		}
		lg.printf("Using port %d exposed by the image", port)
	}

	// 2. Network and environment.
	network, err := s.ensureNetwork(ctx, project.ID)
	if err != nil {
		return "", fmt.Errorf("network: %w", err)
	}
	env, err := s.envFor(ctx, app.ID)
	if err != nil {
		return "", err
	}

	// 3. Create and start. A leftover from an interrupted attempt is removed.
	name := fmt.Sprintf("docklet-%s-%s-d%d", strings.ToLower(project.Name), strings.ToLower(app.Name), dep.ID)
	_ = s.dk.ContainerRemove(ctx, name)
	spec := docker.ContainerSpec{
		Name: name, Image: image, Env: env, Hostname: strings.ToLower(app.Name), Port: port,
		Network: network, Aliases: []string{strings.ToLower(app.Name)},
		RestartPolicy: app.RestartPolicy,
		NanoCPUs:      app.CpuMillis * 1_000_000,
		MemoryBytes:   app.MemoryMb << 20,
		Labels: map[string]string{
			LabelManaged: "true", LabelProject: fmt.Sprint(project.ID),
			LabelApp: fmt.Sprint(app.ID), LabelDeployment: fmt.Sprint(dep.ID),
		},
	}
	lg.printf("Creating container %s (port %d, %d env vars)", name, port, len(env))
	id, err := s.dk.ContainerCreate(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	if err := s.dk.ContainerStart(ctx, id); err != nil {
		return id, fmt.Errorf("start container: %w", err)
	}

	// 4. Health check.
	lg.printf("Waiting for port %d to accept connections (timeout %s)", port, s.HealthTimeout)
	ip, err := s.waitHealthy(ctx, id, network, port)
	if err != nil {
		s.appendContainerLogs(ctx, lg, id)
		return id, err
	}
	lg.printf("Healthy at %s:%d", ip, port)

	// 5. Switch traffic.
	prev := app.CurrentDeploymentID
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.SetDeploymentContainer(ctx, db.SetDeploymentContainerParams{
			ContainerID: id, ContainerIp: ip, ImageID: img.ID, Port: int64(port), ID: dep.ID,
		}); err != nil {
			return err
		}
		now := s.now().Unix()
		if err := q.SetAppCurrentDeployment(ctx, db.SetAppCurrentDeploymentParams{
			CurrentDeploymentID: sql.NullInt64{Int64: dep.ID, Valid: true}, UpdatedAt: now, ID: app.ID,
		}); err != nil {
			return err
		}
		if err := q.SetAppDesiredStatus(ctx, db.SetAppDesiredStatusParams{DesiredStatus: DesiredRunning, UpdatedAt: now, ID: app.ID}); err != nil {
			return err
		}
		return q.SetAppRuntime(ctx, db.SetAppRuntimeParams{RuntimeStatus: "running", RuntimeMessage: "", ID: app.ID})
	})
	if err != nil {
		return id, err
	}
	if err := s.syncProxy(ctx); err != nil {
		lg.printf("Warning: could not update Caddy: %v (routes will be retried automatically)", err)
	} else {
		lg.printf("Traffic switched to the new container")
	}

	// 6. Retire the previous container after in-flight requests drain.
	if prev.Valid && prev.Int64 != dep.ID {
		s.retire(ctx, lg, prev.Int64)
	}
	return id, nil
}

func (s *Service) retire(ctx context.Context, lg *deployLog, depID int64) {
	old, err := s.st.Q.GetDeployment(ctx, depID)
	if err != nil {
		return
	}
	if old.ContainerID != "" {
		select {
		case <-time.After(s.DrainDelay):
		case <-ctx.Done():
		}
		lg.printf("Stopping previous container (deployment #%d)", old.ID)
		stopCtx := context.WithoutCancel(ctx)
		if err := s.dk.ContainerStop(stopCtx, old.ContainerID, s.StopTimeoutSec); err != nil && !errors.Is(err, docker.ErrNotFound) {
			lg.printf("Warning: stop previous container: %v", err)
		}
		if err := s.dk.ContainerRemove(stopCtx, old.ContainerID); err != nil {
			lg.printf("Warning: remove previous container: %v", err)
		}
	}
	_ = s.st.Q.SetDeploymentStatus(context.WithoutCancel(ctx), db.SetDeploymentStatusParams{Status: DeploySuperseded, ID: old.ID})
}

func (s *Service) waitHealthy(ctx context.Context, id, network string, port int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.HealthTimeout)
	defer cancel()
	var lastErr error
	for {
		info, err := s.dk.ContainerInspect(ctx, id)
		if err != nil {
			return "", fmt.Errorf("inspect container: %w", err)
		}
		if !info.State.Running && !info.State.Restarting {
			return "", fmt.Errorf("container exited with code %d %s", info.State.ExitCode, strings.TrimSpace(info.State.Error))
		}
		if info.RestartCount > 0 {
			return "", fmt.Errorf("container crashed and restarted %d time(s)", info.RestartCount)
		}
		if ip := info.IP(network); ip != "" {
			if lastErr = s.HealthCheck(ctx, ip, port); lastErr == nil {
				return ip, nil
			}
		}
		select {
		case <-ctx.Done():
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return "", fmt.Errorf("health check timed out: port %d never accepted connections (%w)", port, lastErr)
		case <-time.After(s.HealthInterval):
		}
	}
}

func (s *Service) appendContainerLogs(ctx context.Context, lg *deployLog, id string) {
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	lg.printf("Last container output:")
	_ = s.dk.ContainerLogs(lctx, id, docker.LogOptions{Tail: 30}, func(l docker.LogLine) error {
		lg.printf("  | %s", l.Text)
		return nil
	})
}
