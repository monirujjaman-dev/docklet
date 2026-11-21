package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/build"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

var (
	resourceNameRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	imageRefRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,254}$`)
	envKeyRe        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	restartPolicies = map[string]bool{"unless-stopped": true, "always": true, "on-failure": true, "no": true}
)

const (
	maxEnvVars    = 200
	maxEnvValue   = 32 << 10
	maxAppDomains = 20
)

func (s *Server) requireDocker() error {
	if s.apps == nil {
		return errf(http.StatusServiceUnavailable, "Docker integration is disabled (-docker-socket off)")
	}
	return nil
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id < 1 {
		return 0, errf(http.StatusBadRequest, "invalid %s", name)
	}
	return id, nil
}

func notFoundAs(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return errf(http.StatusNotFound, "%s not found", what)
	}
	return err
}

// --- DTOs ---

type appSummary struct {
	ID             int64    `json:"id"`
	ProjectID      int64    `json:"projectId"`
	Name           string   `json:"name"`
	Image          string   `json:"image"`
	DesiredStatus  string   `json:"desiredStatus"`
	RuntimeStatus  string   `json:"runtimeStatus"`
	RuntimeMessage string   `json:"runtimeMessage,omitempty"`
	Domains        []string `json:"domains"`
}

type envEntry struct {
	Key       string `json:"key"`
	UpdatedAt int64  `json:"updatedAt"`
}

type deploymentDTO struct {
	ID          int64  `json:"id"`
	Image       string `json:"image"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	Port        int64  `json:"port"`
	ContainerID string `json:"containerId,omitempty"`
	CreatedBy   string `json:"createdBy"`
	CreatedAt   int64  `json:"createdAt"`
	StartedAt   int64  `json:"startedAt,omitempty"`
	FinishedAt  int64  `json:"finishedAt,omitempty"`
	Log         string `json:"log,omitempty"`
	Current     bool   `json:"current,omitempty"`

	Source        string `json:"source"`
	CommitSHA     string `json:"commitSha,omitempty"`
	CommitMessage string `json:"commitMessage,omitempty"`
	RollbackOf    int64  `json:"rollbackOf,omitempty"`
	Rollbackable  bool   `json:"rollbackable,omitempty"`
}

type appDetail struct {
	appSummary
	ProjectName   string          `json:"projectName"`
	Port          int64           `json:"port"`
	RestartPolicy string          `json:"restartPolicy"`
	CPUMillis     int64           `json:"cpuMillis"`
	MemoryMB      int64           `json:"memoryMb"`
	Env           []envEntry      `json:"env"`
	Deployments   []deploymentDTO `json:"deployments"`
	CreatedAt     int64           `json:"createdAt"`

	SourceType      string     `json:"sourceType"`
	Git             *gitConfig `json:"git,omitempty"`
	BuildsAvailable bool       `json:"buildsAvailable"`
}

func (s *Server) summarize(ctx context.Context, a db.App) (appSummary, error) {
	domains, err := s.st.Q.ListAppDomains(ctx, a.ID)
	if err != nil {
		return appSummary{}, err
	}
	return appSummary{
		ID: a.ID, ProjectID: a.ProjectID, Name: a.Name, Image: a.Image,
		DesiredStatus: a.DesiredStatus, RuntimeStatus: a.RuntimeStatus, RuntimeMessage: a.RuntimeMessage,
		Domains: domains,
	}, nil
}

// --- Projects ---

// GET /api/projects
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	projects, err := s.st.Q.ListProjects(ctx)
	if err != nil {
		return err
	}
	all, err := s.st.Q.ListApps(ctx)
	if err != nil {
		return err
	}
	type projectDTO struct {
		ID        int64        `json:"id"`
		Name      string       `json:"name"`
		CreatedAt int64        `json:"createdAt"`
		Apps      []appSummary `json:"apps"`
		Databases []dbSummary  `json:"databases"`
	}
	out := make([]projectDTO, 0, len(projects))
	idx := map[int64]int{}
	for _, p := range projects {
		idx[p.ID] = len(out)
		out = append(out, projectDTO{ID: p.ID, Name: p.Name, CreatedAt: p.CreatedAt, Apps: []appSummary{}, Databases: []dbSummary{}})
	}
	dbs, err := s.st.Q.ListDatabases(ctx)
	if err != nil {
		return err
	}
	for _, d := range dbs {
		if i, ok := idx[d.ProjectID]; ok {
			out[i].Databases = append(out[i].Databases, toDBSummary(d))
		}
	}
	for _, a := range all {
		sum, err := s.summarize(ctx, a)
		if err != nil {
			return err
		}
		if i, ok := idx[a.ProjectID]; ok {
			out[i].Apps = append(out[i].Apps, sum)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out, "docker": s.apps != nil})
	return nil
}

// POST /api/projects
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if !resourceNameRe.MatchString(name) {
		return errf(http.StatusBadRequest, "name must be 1-32 lowercase letters, digits or '-'")
	}
	ctx := r.Context()
	a := authFrom(ctx)
	var p db.Project
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		var err error
		if p, err = q.CreateProject(ctx, db.CreateProjectParams{Name: name, CreatedAt: s.now().Unix()}); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return errf(http.StatusConflict, "a project named %q already exists", name)
			}
			return err
		}
		_, err = audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionProjectCreated,
			Target: "project:" + name, IP: clientIP(r),
		}, s.now())
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": p.ID, "name": p.Name, "createdAt": p.CreatedAt})
	return nil
}

// DELETE /api/projects/{id}
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	a := authFrom(ctx)
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		p, err := q.GetProject(ctx, id)
		if err != nil {
			return notFoundAs(err, "project")
		}
		n, err := q.CountProjectApps(ctx, id)
		if err != nil {
			return err
		}
		nd, err := q.CountProjectDatabases(ctx, id)
		if err != nil {
			return err
		}
		if n+nd > 0 {
			return errf(http.StatusConflict, "delete the project's apps and databases first (%d app(s), %d database(s))", n, nd)
		}
		if err := q.DeleteProject(ctx, id); err != nil {
			return err
		}
		_, err = audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionProjectDeleted,
			Target: "project:" + p.Name, IP: clientIP(r),
		}, s.now())
		return err
	})
	if err != nil {
		return err
	}
	if s.apps != nil {
		if err := s.apps.RemoveProjectNetwork(ctx, id); err != nil {
			s.log.Warn("remove project network", "project", id, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// --- Apps ---

type appConfigInput struct {
	Image         *string `json:"image"`
	Port          *int64  `json:"port"`
	RestartPolicy *string `json:"restartPolicy"`
	CPUMillis     *int64  `json:"cpuMillis"`
	MemoryMB      *int64  `json:"memoryMb"`
}

// apply validates and merges the input into app.
func (in appConfigInput) apply(app *db.App) error {
	if in.Image != nil {
		img := strings.TrimSpace(*in.Image)
		if !imageRefRe.MatchString(img) {
			return errf(http.StatusBadRequest, "invalid image reference")
		}
		app.Image = img
	}
	if in.Port != nil {
		if *in.Port < 0 || *in.Port > 65535 {
			return errf(http.StatusBadRequest, "port must be 1-65535 (or 0 to use the image's exposed port)")
		}
		app.Port = *in.Port
	}
	if in.RestartPolicy != nil {
		if !restartPolicies[*in.RestartPolicy] {
			return errf(http.StatusBadRequest, "restartPolicy must be unless-stopped, always, on-failure or no")
		}
		app.RestartPolicy = *in.RestartPolicy
	}
	if in.CPUMillis != nil {
		if *in.CPUMillis < 0 || *in.CPUMillis > 256_000 {
			return errf(http.StatusBadRequest, "cpuMillis must be 0-256000 (1000 = one core)")
		}
		app.CpuMillis = *in.CPUMillis
	}
	if in.MemoryMB != nil {
		if *in.MemoryMB != 0 && (*in.MemoryMB < 6 || *in.MemoryMB > 1<<20) {
			return errf(http.StatusBadRequest, "memoryMb must be 0 (no limit) or at least 6")
		}
		app.MemoryMb = *in.MemoryMB
	}
	return nil
}

// validateEnv checks keys and values; nil values mean "unset".
func validateEnv(env map[string]*string) error {
	if len(env) > maxEnvVars {
		return errf(http.StatusBadRequest, "at most %d environment variables", maxEnvVars)
	}
	for k, v := range env {
		if !envKeyRe.MatchString(k) {
			return errf(http.StatusBadRequest, "invalid environment variable name %q", k)
		}
		if v != nil && len(*v) > maxEnvValue {
			return errf(http.StatusBadRequest, "value of %s is too long (max %d bytes)", k, maxEnvValue)
		}
	}
	return nil
}

func (s *Server) applyEnv(ctx context.Context, q *db.Queries, appID int64, env map[string]*string) ([]string, error) {
	keys := make([]string, 0, len(env))
	for k, v := range env {
		keys = append(keys, k)
		if v == nil {
			if err := q.DeleteEnv(ctx, db.DeleteEnvParams{AppID: appID, Key: k}); err != nil {
				return nil, err
			}
			continue
		}
		enc, err := s.apps.SealEnv(appID, k, *v)
		if err != nil {
			return nil, err
		}
		if err := q.UpsertEnv(ctx, db.UpsertEnvParams{AppID: appID, Key: k, ValueEnc: enc, UpdatedAt: s.now().Unix()}); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// normalizeDomains validates a domain list and checks it is free.
func (s *Server) normalizeDomains(ctx context.Context, q *db.Queries, appID int64, in []string) ([]string, error) {
	if len(in) > maxAppDomains {
		return nil, errf(http.StatusBadRequest, "at most %d domains per app", maxAppDomains)
	}
	panel, err := getSetting(ctx, q, SettingPanelDomain)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		nd, err := validateDomain(d)
		if err != nil {
			return nil, errf(http.StatusBadRequest, "%s: %s", d, err.Error())
		}
		if seen[nd] {
			continue
		}
		seen[nd] = true
		if nd == panel {
			return nil, errf(http.StatusConflict, "%s is the panel domain", nd)
		}
		owner, err := q.GetDomainOwner(ctx, nd)
		if err == nil && owner != appID {
			return nil, errf(http.StatusConflict, "%s is already used by another app", nd)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		out = append(out, nd)
	}
	return out, nil
}

func (s *Server) setDomains(ctx context.Context, q *db.Queries, appID int64, domains []string) error {
	if err := q.DeleteAppDomains(ctx, appID); err != nil {
		return err
	}
	for _, d := range domains {
		if err := q.InsertDomain(ctx, db.InsertDomainParams{Domain: d, AppID: appID, CreatedAt: s.now().Unix()}); err != nil {
			return err
		}
	}
	return nil
}

// POST /api/projects/{id}/apps
func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDocker(); err != nil {
		return err
	}
	projectID, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Name string `json:"name"`
		appConfigInput
		Domains []string           `json:"domains"`
		Env     map[string]*string `json:"env"`
		Deploy  *bool              `json:"deploy"`
		Git     *gitConfig         `json:"git"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if !resourceNameRe.MatchString(name) {
		return errf(http.StatusBadRequest, "name must be 1-32 lowercase letters, digits or '-'")
	}
	if in.Git != nil {
		if in.Image != nil {
			return errf(http.StatusBadRequest, "set either image or git, not both")
		}
		if err := in.Git.normalize(); err != nil {
			return err
		}
	} else if in.Image == nil {
		return errf(http.StatusBadRequest, "image (or git) is required")
	}
	app := db.App{RestartPolicy: "unless-stopped"}
	if err := in.apply(&app); err != nil {
		return err
	}
	if err := validateEnv(in.Env); err != nil {
		return err
	}
	for k, v := range in.Env {
		if v == nil {
			delete(in.Env, k)
		}
	}
	deploy := in.Deploy == nil || *in.Deploy
	// A private SSH repo can't be cloned until the deploy key is added.
	if in.Git != nil && build.IsSSH(in.Git.URL) && in.Deploy == nil {
		deploy = false
	}

	ctx := r.Context()
	a := authFrom(ctx)
	now := s.now()
	var created db.App
	var dep db.Deployment
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if _, err := q.GetProject(ctx, projectID); err != nil {
			return notFoundAs(err, "project")
		}
		if err := nameFreeInProject(ctx, q, projectID, name); err != nil {
			return err
		}
		created, err = q.CreateApp(ctx, db.CreateAppParams{
			ProjectID: projectID, Name: name, Image: app.Image, Port: app.Port, RestartPolicy: app.RestartPolicy,
			CpuMillis: app.CpuMillis, MemoryMb: app.MemoryMb, CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
		})
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return errf(http.StatusConflict, "an app named %q already exists in this project", name)
			}
			return err
		}
		domains, err := s.normalizeDomains(ctx, q, created.ID, in.Domains)
		if err != nil {
			return err
		}
		if err := s.setDomains(ctx, q, created.ID, domains); err != nil {
			return err
		}
		keys, err := s.applyEnv(ctx, q, created.ID, in.Env)
		if err != nil {
			return err
		}
		if in.Git != nil {
			if created, err = s.setupGitApp(ctx, q, created, *in.Git); err != nil {
				return err
			}
		}
		if _, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionAppCreated,
			Target: fmt.Sprintf("app:%d", created.ID), IP: clientIP(r),
			Details: map[string]any{"name": name, "image": app.Image, "git": in.Git, "domains": domains, "envKeys": keys},
		}, now); err != nil {
			return err
		}
		if deploy {
			if dep, err = s.apps.EnqueueDeploy(ctx, q, created, a.User.Username); err != nil {
				return err
			}
			_, err = audit.Append(ctx, q, audit.Entry{
				ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionDeployRequested,
				Target: fmt.Sprintf("app:%d", created.ID), IP: clientIP(r),
				Details: map[string]any{"deployment": dep.ID, "image": dep.Image},
			}, now)
		}
		return err
	})
	if err != nil {
		return err
	}
	s.apps.Wake()
	resp := map[string]any{"id": created.ID}
	if deploy {
		resp["deploymentId"] = dep.ID
	}
	writeJSON(w, http.StatusCreated, resp)
	return nil
}

// GET /api/apps/{id}
func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	app, err := s.st.Q.GetApp(ctx, id)
	if err != nil {
		return notFoundAs(err, "app")
	}
	project, err := s.st.Q.GetProject(ctx, app.ProjectID)
	if err != nil {
		return err
	}
	sum, err := s.summarize(ctx, app)
	if err != nil {
		return err
	}
	envRows, err := s.st.Q.ListEnv(ctx, id)
	if err != nil {
		return err
	}
	env := make([]envEntry, 0, len(envRows))
	for _, e := range envRows {
		env = append(env, envEntry{Key: e.Key, UpdatedAt: e.UpdatedAt})
	}
	deps, err := s.st.Q.ListAppDeployments(ctx, db.ListAppDeploymentsParams{AppID: id, Limit: 20})
	if err != nil {
		return err
	}
	dd := make([]deploymentDTO, 0, len(deps))
	for _, d := range deps {
		current := app.CurrentDeploymentID.Valid && app.CurrentDeploymentID.Int64 == d.ID
		dd = append(dd, deploymentDTO{
			ID: d.ID, Image: d.Image, Status: d.Status, Error: d.Error, Port: d.Port, ContainerID: shortID(d.ContainerID),
			CreatedBy: d.CreatedBy, CreatedAt: d.CreatedAt, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
			Current: current, Source: d.Source, CommitSHA: d.CommitSha, CommitMessage: d.CommitMessage,
			RollbackOf: d.RollbackOf.Int64,
			Rollbackable: !current && d.ImageID != "" && d.ImagePruned == 0 &&
				(d.Status == apps.DeploySucceeded || d.Status == apps.DeploySuperseded),
		})
	}
	detail := appDetail{
		appSummary: sum, ProjectName: project.Name, Port: app.Port, RestartPolicy: app.RestartPolicy,
		CPUMillis: app.CpuMillis, MemoryMB: app.MemoryMb, Env: env, Deployments: dd, CreatedAt: app.CreatedAt,
		SourceType: app.SourceType, BuildsAvailable: s.apps != nil && s.apps.Builder != nil,
	}
	if app.SourceType == apps.SourceGit {
		detail.Git = &gitConfig{
			URL: app.GitUrl, Branch: app.GitBranch, Context: app.BuildContext, Dockerfile: app.Dockerfile,
			AutoDeploy: app.AutoDeploy == 1, DeployKey: app.DeployKeyPub, SSH: build.IsSSH(app.GitUrl),
		}
		if app.WebhookToken.Valid {
			detail.Git.WebhookPath = "/api/webhooks/" + app.WebhookToken.String
		}
	}
	writeJSON(w, http.StatusOK, detail)
	return nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// PATCH /api/apps/{id}
func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in appConfigInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	a := authFrom(ctx)
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		app, err := q.GetApp(ctx, id)
		if err != nil {
			return notFoundAs(err, "app")
		}
		before := app
		if err := in.apply(&app); err != nil {
			return err
		}
		if err := q.UpdateAppConfig(ctx, db.UpdateAppConfigParams{
			Image: app.Image, Port: app.Port, RestartPolicy: app.RestartPolicy, CpuMillis: app.CpuMillis,
			MemoryMb: app.MemoryMb, UpdatedAt: s.now().Unix(), ID: id,
		}); err != nil {
			return err
		}
		changes := map[string]any{}
		if before.Image != app.Image {
			changes["image"] = []string{before.Image, app.Image}
		}
		if before.Port != app.Port {
			changes["port"] = []int64{before.Port, app.Port}
		}
		if before.RestartPolicy != app.RestartPolicy {
			changes["restartPolicy"] = []string{before.RestartPolicy, app.RestartPolicy}
		}
		if before.CpuMillis != app.CpuMillis {
			changes["cpuMillis"] = []int64{before.CpuMillis, app.CpuMillis}
		}
		if before.MemoryMb != app.MemoryMb {
			changes["memoryMb"] = []int64{before.MemoryMb, app.MemoryMb}
		}
		_, err = audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionAppUpdated,
			Target: fmt.Sprintf("app:%d", id), IP: clientIP(r), Details: changes,
		}, s.now())
		return err
	})
	if err != nil {
		return err
	}
	return s.handleGetApp(w, r)
}

// PUT /api/apps/{id}/env — {"env": {"KEY": "value", "OLD": null}}; other keys unchanged.
func (s *Server) handleUpdateEnv(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDocker(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Env map[string]*string `json:"env"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := validateEnv(in.Env); err != nil {
		return err
	}
	ctx := r.Context()
	a := authFrom(ctx)
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if _, err := q.GetApp(ctx, id); err != nil {
			return notFoundAs(err, "app")
		}
		var set, unset []string
		for k, v := range in.Env {
			if v == nil {
				unset = append(unset, k)
			} else {
				set = append(set, k)
			}
		}
		if _, err := s.applyEnv(ctx, q, id, in.Env); err != nil {
			return err
		}
		rows, err := q.ListEnv(ctx, id)
		if err != nil {
			return err
		}
		if len(rows) > maxEnvVars {
			return errf(http.StatusBadRequest, "at most %d environment variables", maxEnvVars)
		}
		// Only key names are audited, never values.
		_, err = audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionAppEnvUpdated,
			Target: fmt.Sprintf("app:%d", id), IP: clientIP(r),
			Details: map[string]any{"set": set, "unset": unset},
		}, s.now())
		return err
	})
	if err != nil {
		return err
	}
	return s.handleGetApp(w, r)
}

// PUT /api/apps/{id}/domains — {"domains": [...]} replaces the list.
func (s *Server) handleUpdateDomains(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Domains []string `json:"domains"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	a := authFrom(ctx)
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if _, err := q.GetApp(ctx, id); err != nil {
			return notFoundAs(err, "app")
		}
		before, err := q.ListAppDomains(ctx, id)
		if err != nil {
			return err
		}
		domains, err := s.normalizeDomains(ctx, q, id, in.Domains)
		if err != nil {
			return err
		}
		if err := s.setDomains(ctx, q, id, domains); err != nil {
			return err
		}
		_, err = audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionAppDomains,
			Target: fmt.Sprintf("app:%d", id), IP: clientIP(r),
			Details: map[string]any{"before": before, "after": domains},
		}, s.now())
		return err
	})
	if err != nil {
		return err
	}
	if s.caddy != nil {
		s.caddy.Trigger()
	}
	return s.handleGetApp(w, r)
}

// POST /api/apps/{id}/deploy
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDocker(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	a := authFrom(ctx)
	var dep db.Deployment
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		app, err := q.GetApp(ctx, id)
		if err != nil {
			return notFoundAs(err, "app")
		}
		if dep, err = s.apps.EnqueueDeploy(ctx, q, app, a.User.Username); err != nil {
			return err
		}
		_, err = audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionDeployRequested,
			Target: fmt.Sprintf("app:%d", id), IP: clientIP(r),
			Details: map[string]any{"deployment": dep.ID, "image": dep.Image},
		}, s.now())
		return err
	})
	if err != nil {
		return err
	}
	s.apps.Wake()
	writeJSON(w, http.StatusAccepted, map[string]any{"deploymentId": dep.ID})
	return nil
}

// POST /api/apps/{id}/{action} for start, stop, restart.
func (s *Server) handleLifecycle(action string) func(http.ResponseWriter, *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		if err := s.requireDocker(); err != nil {
			return err
		}
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		ctx := r.Context()
		if _, err := s.st.Q.GetApp(ctx, id); err != nil {
			return notFoundAs(err, "app")
		}
		var auditAction string
		switch action {
		case "start":
			err, auditAction = s.apps.Start(ctx, id), audit.ActionAppStarted
		case "stop":
			err, auditAction = s.apps.Stop(ctx, id), audit.ActionAppStopped
		case "restart":
			err, auditAction = s.apps.Restart(ctx, id), audit.ActionAppRestarted
		}
		if errors.Is(err, apps.ErrNoDeployment) {
			return errf(http.StatusConflict, "%s", err.Error())
		}
		if err != nil {
			return errf(http.StatusBadGateway, "%s failed: %v", action, err)
		}
		a := authFrom(ctx)
		if err := s.st.Tx(ctx, func(q *db.Queries) error {
			_, err := audit.Append(ctx, q, audit.Entry{
				ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: auditAction,
				Target: fmt.Sprintf("app:%d", id), IP: clientIP(r),
			}, s.now())
			return err
		}); err != nil {
			return err
		}
		return s.handleGetApp(w, r)
	}
}

// DELETE /api/apps/{id}
func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDocker(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	app, err := s.st.Q.GetApp(ctx, id)
	if err != nil {
		return notFoundAs(err, "app")
	}
	a := authFrom(ctx)
	err = s.apps.Delete(ctx, id, func(q *db.Queries) error {
		_, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionAppDeleted,
			Target: fmt.Sprintf("app:%d", id), IP: clientIP(r), Details: map[string]any{"name": app.Name},
		}, s.now())
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// GET /api/deployments/{id}
func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	d, err := s.st.Q.GetDeployment(r.Context(), id)
	if err != nil {
		return notFoundAs(err, "deployment")
	}
	writeJSON(w, http.StatusOK, deploymentDTO{
		ID: d.ID, Image: d.Image, Status: d.Status, Error: d.Error, Port: d.Port, ContainerID: shortID(d.ContainerID),
		CreatedBy: d.CreatedBy, CreatedAt: d.CreatedAt, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt, Log: d.Log,
		Source: d.Source, CommitSHA: d.CommitSha, CommitMessage: d.CommitMessage, RollbackOf: d.RollbackOf.Int64,
	})
	return nil
}

// GET /api/apps/{id}/logs?tail=200&follow=1 — Server-Sent Events.
func (s *Server) handleAppLogs(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDocker(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	tail := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		if tail, err = strconv.Atoi(v); err != nil || tail < 1 || tail > 5000 {
			return errf(http.StatusBadRequest, "tail must be 1-5000")
		}
	}
	follow := r.URL.Query().Get("follow") == "1"
	ctx := r.Context()
	app, err := s.st.Q.GetApp(ctx, id)
	if err != nil {
		return notFoundAs(err, "app")
	}
	if !app.CurrentDeploymentID.Valid {
		return errf(http.StatusConflict, "%s", apps.ErrNoDeployment.Error())
	}

	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{}) // long-lived stream; don't inherit ReadTimeout
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	lines := make(chan docker.LogLine, 256)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.apps.Logs(ctx, id, docker.LogOptions{Follow: follow, Tail: tail, Timestamps: true}, func(l docker.LogLine) error {
			select {
			case lines <- l:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case l := <-lines:
			b, _ := json.Marshal(l)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return nil
			}
			// Batch lines that are already queued before flushing.
			for len(lines) > 0 {
				b, _ := json.Marshal(<-lines)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			}
			_ = rc.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			_ = rc.Flush()
		case err := <-errCh:
			for len(lines) > 0 {
				b, _ := json.Marshal(<-lines)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			}
			msg := "end"
			if err != nil && ctx.Err() == nil {
				msg = err.Error()
			}
			b, _ := json.Marshal(map[string]string{"message": msg})
			_, _ = fmt.Fprintf(w, "event: end\ndata: %s\n\n", b)
			_ = rc.Flush()
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}
