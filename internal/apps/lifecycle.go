package apps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// currentContainer returns the container of the app's current deployment.
func (s *Service) currentContainer(ctx context.Context, app db.App) (db.Deployment, error) {
	if !app.CurrentDeploymentID.Valid {
		return db.Deployment{}, ErrNoDeployment
	}
	dep, err := s.st.Q.GetDeployment(ctx, app.CurrentDeploymentID.Int64)
	if err != nil {
		return dep, err
	}
	if dep.ContainerID == "" {
		return dep, ErrNoDeployment
	}
	return dep, nil
}

func (s *Service) setDesired(ctx context.Context, appID int64, desired string) error {
	return s.st.Q.SetAppDesiredStatus(ctx, db.SetAppDesiredStatusParams{DesiredStatus: desired, UpdatedAt: s.now().Unix(), ID: appID})
}

// Start starts the current container and routes traffic to it.
func (s *Service) Start(ctx context.Context, appID int64) error {
	defer s.lock(appID)()
	app, err := s.st.Q.GetApp(ctx, appID)
	if err != nil {
		return err
	}
	dep, err := s.currentContainer(ctx, app)
	if err != nil {
		return err
	}
	if err := s.dk.ContainerStart(ctx, dep.ContainerID); err != nil {
		if errors.Is(err, docker.ErrNotFound) {
			return errors.New("the container no longer exists; redeploy the app")
		}
		return err
	}
	if err := s.setDesired(ctx, appID, DesiredRunning); err != nil {
		return err
	}
	return s.refresh(ctx, app.ID)
}

// Stop removes the app's routes, then stops its container.
func (s *Service) Stop(ctx context.Context, appID int64) error {
	defer s.lock(appID)()
	app, err := s.st.Q.GetApp(ctx, appID)
	if err != nil {
		return err
	}
	if err := s.setDesired(ctx, appID, DesiredStopped); err != nil {
		return err
	}
	_ = s.syncProxy(ctx)
	dep, err := s.currentContainer(ctx, app)
	if errors.Is(err, ErrNoDeployment) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.dk.ContainerStop(ctx, dep.ContainerID, s.StopTimeoutSec); err != nil && !errors.Is(err, docker.ErrNotFound) {
		return err
	}
	return s.refresh(ctx, app.ID)
}

// Restart restarts the current container.
func (s *Service) Restart(ctx context.Context, appID int64) error {
	defer s.lock(appID)()
	app, err := s.st.Q.GetApp(ctx, appID)
	if err != nil {
		return err
	}
	dep, err := s.currentContainer(ctx, app)
	if err != nil {
		return err
	}
	if err := s.dk.ContainerRestart(ctx, dep.ContainerID, s.StopTimeoutSec); err != nil {
		if errors.Is(err, docker.ErrNotFound) {
			return errors.New("the container no longer exists; redeploy the app")
		}
		return err
	}
	if err := s.setDesired(ctx, appID, DesiredRunning); err != nil {
		return err
	}
	return s.refresh(ctx, app.ID)
}

// Delete removes every container of the app, then the app itself.
// The caller audits the action.
func (s *Service) Delete(ctx context.Context, appID int64, auditFn func(q *db.Queries) error) error {
	defer s.lock(appID)()
	containers, err := s.dk.ContainerList(ctx, map[string]string{LabelApp: strconv.FormatInt(appID, 10), LabelManaged: "true"})
	if err != nil {
		return err
	}
	for _, c := range containers {
		if err := s.dk.ContainerRemove(ctx, c.ID); err != nil {
			return err
		}
	}
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.DeleteApp(ctx, appID); err != nil {
			return err
		}
		return auditFn(q)
	}); err != nil {
		return err
	}
	_ = s.syncProxy(ctx)
	return nil
}

// RemoveProjectNetwork deletes a project's Docker network.
func (s *Service) RemoveProjectNetwork(ctx context.Context, projectID int64) error {
	return s.dk.NetworkRemove(ctx, NetworkName(projectID))
}

// Logs streams the current container's logs.
func (s *Service) Logs(ctx context.Context, appID int64, o docker.LogOptions, fn func(docker.LogLine) error) error {
	app, err := s.st.Q.GetApp(ctx, appID)
	if err != nil {
		return err
	}
	dep, err := s.currentContainer(ctx, app)
	if err != nil {
		return err
	}
	return s.dk.ContainerLogs(ctx, dep.ContainerID, o, fn)
}

// Reconcile compares Docker with the database: it records each app's real
// state (including containers removed outside Docklet), follows IP changes
// after a Docker restart, and removes containers left by failed or
// superseded deployments.
func (s *Service) Reconcile(ctx context.Context) error {
	apps, err := s.st.Q.ListApps(ctx)
	if err != nil {
		return err
	}
	routesChanged := false
	for _, app := range apps {
		changed, err := s.observe(ctx, app)
		if err != nil {
			return err
		}
		routesChanged = routesChanged || changed
	}
	if routesChanged {
		_ = s.syncProxy(ctx)
	}
	return s.removeOrphans(ctx)
}

func (s *Service) refresh(ctx context.Context, appID int64) error {
	app, err := s.st.Q.GetApp(ctx, appID)
	if err != nil {
		return err
	}
	if _, err := s.observe(ctx, app); err != nil {
		return err
	}
	return s.syncProxy(ctx)
}

// observe updates runtime status for one app; it reports whether routing
// inputs (container IP) changed.
func (s *Service) observe(ctx context.Context, app db.App) (bool, error) {
	status, msg := "not_deployed", ""
	routesChanged := false
	if app.CurrentDeploymentID.Valid {
		dep, err := s.st.Q.GetDeployment(ctx, app.CurrentDeploymentID.Int64)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if err == nil && dep.ContainerID != "" {
			info, err := s.dk.ContainerInspect(ctx, dep.ContainerID)
			switch {
			case errors.Is(err, docker.ErrNotFound):
				status, msg = "missing", "The container was removed outside Docklet. Redeploy to recreate it."
			case err != nil:
				return false, err
			case info.State.Running:
				status = "running"
				if ip := info.IP(NetworkName(app.ProjectID)); ip != "" && ip != dep.ContainerIp {
					if err := s.st.Q.SetDeploymentIP(ctx, db.SetDeploymentIPParams{ContainerIp: ip, ID: dep.ID}); err != nil {
						return false, err
					}
					routesChanged = true
				}
			case info.State.Restarting:
				status, msg = "restarting", fmt.Sprintf("Restarting (exit code %d, %d restarts)", info.State.ExitCode, info.RestartCount)
			default:
				status = "stopped"
				if app.DesiredStatus == DesiredRunning {
					status, msg = "exited", fmt.Sprintf("Exited with code %d", info.State.ExitCode)
				}
			}
		}
	}
	if status != app.RuntimeStatus || msg != app.RuntimeMessage {
		if err := s.st.Q.SetAppRuntime(ctx, db.SetAppRuntimeParams{RuntimeStatus: status, RuntimeMessage: msg, ID: app.ID}); err != nil {
			return false, err
		}
		if status == "missing" {
			s.log.Warn("app container missing", "app", app.Name)
			_ = s.st.Tx(ctx, func(q *db.Queries) error {
				_, err := audit.Append(ctx, q, audit.Entry{
					Actor: "system", Action: audit.ActionContainerMissing, Target: fmt.Sprintf("app:%d", app.ID),
					Details: map[string]any{"app": app.Name},
				}, s.now())
				return err
			})
		}
	}
	return routesChanged, nil
}

func (s *Service) removeOrphans(ctx context.Context) error {
	containers, err := s.dk.ContainerList(ctx, map[string]string{LabelManaged: "true"})
	if err != nil {
		return err
	}
	for _, c := range containers {
		depID, err := strconv.ParseInt(c.Labels[LabelDeployment], 10, 64)
		if err != nil {
			continue
		}
		dep, err := s.st.Q.GetDeployment(ctx, depID)
		orphan := errors.Is(err, sql.ErrNoRows) ||
			(err == nil && (dep.Status == DeployFailed || dep.Status == DeploySuperseded))
		if orphan {
			s.log.Info("removing orphaned container", "id", c.ID[:min(12, len(c.ID))], "deployment", depID)
			_ = s.dk.ContainerRemove(ctx, c.ID)
		}
	}
	return nil
}
