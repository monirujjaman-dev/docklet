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
	s.finish(ctx, app, dep, DeploySucceeded, "", lg.String(), audit.ActionDeploySucceeded)
	return nil
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

func (s *Service) deploySteps(ctx context.Context, lg *deployLog, project db.Project, app db.App, dep db.Deployment) (string, error) {
	image := dep.Image

	// 1. Pull. A local image is accepted if the registry is unreachable.
	lg.printf("Pulling %s", image)
	if err := s.dk.ImagePull(ctx, image, func(line string) { lg.printf("  %s", line) }); err != nil {
		if _, ierr := s.dk.ImageInspect(ctx, image); ierr != nil {
			return "", fmt.Errorf("pull %s: %w", image, err)
		}
		lg.printf("Pull failed (%v); using the image already on this host", err)
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
	network := NetworkName(project.ID)
	if err := s.dk.NetworkEnsure(ctx, network, map[string]string{LabelManaged: "true", LabelProject: fmt.Sprint(project.ID)}); err != nil {
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
