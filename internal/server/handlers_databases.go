package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/databases"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

func (s *Server) requireDatabases() error {
	if s.dbs == nil {
		return errf(http.StatusServiceUnavailable, "Docker integration is disabled (-docker-socket off)")
	}
	return nil
}

type dbSummary struct {
	ID             int64  `json:"id"`
	ProjectID      int64  `json:"projectId"`
	Name           string `json:"name"`
	Engine         string `json:"engine"`
	EngineLabel    string `json:"engineLabel"`
	Version        string `json:"version"`
	RuntimeStatus  string `json:"runtimeStatus"`
	RuntimeMessage string `json:"runtimeMessage,omitempty"`
	PublicPort     int64  `json:"publicPort,omitempty"`
}

func toDBSummary(d db.Database) dbSummary {
	label := d.Engine
	if e := databases.Engines[d.Engine]; e != nil {
		label = e.Label
	}
	return dbSummary{
		ID: d.ID, ProjectID: d.ProjectID, Name: d.Name, Engine: d.Engine, EngineLabel: label, Version: d.Version,
		RuntimeStatus: d.RuntimeStatus, RuntimeMessage: d.RuntimeMessage, PublicPort: d.PublicPort,
	}
}

func (s *Server) auditDB(ctx context.Context, q *db.Queries, r *http.Request, id int64, action string, details map[string]any) error {
	a := authFrom(ctx)
	_, err := audit.Append(ctx, q, audit.Entry{
		ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: action,
		Target: fmt.Sprintf("database:%d", id), IP: clientIP(r), Details: details,
	}, s.now())
	return err
}

// GET /api/databases/engines
func (s *Server) handleEngines(w http.ResponseWriter, _ *http.Request) error {
	type engineDTO struct {
		Name     string   `json:"name"`
		Label    string   `json:"label"`
		Versions []string `json:"versions"`
		Port     int      `json:"port"`
		EnvKey   string   `json:"envKey"`
	}
	var out []engineDTO
	for _, name := range []string{"postgres", "mysql", "mariadb", "redis"} {
		e := databases.Engines[name]
		out = append(out, engineDTO{Name: e.Name, Label: e.Label, Versions: e.Versions, Port: e.Port, EnvKey: e.DefaultEnvKey})
	}
	writeJSON(w, http.StatusOK, map[string]any{"engines": out})
	return nil
}

// POST /api/projects/{id}/databases
func (s *Server) handleCreateDatabase(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	projectID, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Name     string `json:"name"`
		Engine   string `json:"engine"`
		Version  string `json:"version"`
		MemoryMB int64  `json:"memoryMb"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if !resourceNameRe.MatchString(name) {
		return errf(http.StatusBadRequest, "name must be 1-32 lowercase letters, digits or '-'")
	}
	e := databases.Engines[in.Engine]
	if e == nil {
		return errf(http.StatusBadRequest, "engine must be postgres, mysql, mariadb or redis")
	}
	if in.Version == "" {
		in.Version = e.Versions[0]
	}
	if !e.ValidVersion(in.Version) {
		return errf(http.StatusBadRequest, "version must be one of %s", strings.Join(e.Versions, ", "))
	}
	if in.MemoryMB != 0 && (in.MemoryMB < 64 || in.MemoryMB > 1<<20) {
		return errf(http.StatusBadRequest, "memoryMb must be 0 (no limit) or at least 64")
	}

	pw, err := databases.GeneratePassword()
	if err != nil {
		return err
	}
	root := ""
	if in.Engine == "mysql" || in.Engine == "mariadb" {
		if root, err = databases.GeneratePassword(); err != nil {
			return err
		}
	}
	pwEnc, rootEnc, err := s.dbs.SealCreds(projectID, name, pw, root)
	if err != nil {
		return err
	}
	ident := strings.ReplaceAll(name, "-", "_")
	if !e.HasUser {
		ident = ""
	}

	ctx := r.Context()
	var d db.Database
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if _, err := q.GetProject(ctx, projectID); err != nil {
			return notFoundAs(err, "project")
		}
		if err := nameFreeInProject(ctx, q, projectID, name); err != nil {
			return err
		}
		d, err = q.CreateDatabase(ctx, db.CreateDatabaseParams{
			ProjectID: projectID, Name: name, Engine: e.Name, Version: in.Version, DbName: ident, Username: ident,
			PasswordEnc: pwEnc, RootPasswordEnc: rootEnc, MemoryMb: in.MemoryMB,
			CreatedAt: s.now().Unix(), UpdatedAt: s.now().Unix(),
		})
		if err != nil {
			return err
		}
		return s.auditDB(ctx, q, r, d.ID, audit.ActionDatabaseCreated,
			map[string]any{"name": name, "engine": e.Name, "version": in.Version})
	})
	if err != nil {
		return err
	}
	s.dbs.ProvisionAsync(d.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"id": d.ID})
	return nil
}

// nameFreeInProject: apps and databases share the project network, where
// their names are DNS aliases, so a name can be used only once.
func nameFreeInProject(ctx context.Context, q *db.Queries, projectID int64, name string) error {
	apps, err := q.ListProjectApps(ctx, projectID)
	if err != nil {
		return err
	}
	for _, a := range apps {
		if strings.EqualFold(a.Name, name) {
			return errf(http.StatusConflict, "an app named %q already exists in this project", name)
		}
	}
	dbs, err := q.ListDatabases(ctx)
	if err != nil {
		return err
	}
	for _, d := range dbs {
		if d.ProjectID == projectID && strings.EqualFold(d.Name, name) {
			return errf(http.StatusConflict, "a database named %q already exists in this project", name)
		}
	}
	return nil
}

func (s *Server) dbFromPath(r *http.Request) (db.Database, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return db.Database{}, err
	}
	d, err := s.st.Q.GetDatabase(r.Context(), id)
	if err != nil {
		return d, notFoundAs(err, "database")
	}
	return d, nil
}

// GET /api/databases/{id}
func (s *Server) handleGetDatabase(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	d, err := s.dbFromPath(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	e := databases.Engines[d.Engine]
	project, err := s.st.Q.GetProject(ctx, d.ProjectID)
	if err != nil {
		return err
	}
	links, err := s.st.Q.ListDatabaseLinks(ctx, d.ID)
	if err != nil {
		return err
	}
	backups, err := s.st.Q.ListBackups(ctx, db.ListBackupsParams{DatabaseID: d.ID, Limit: 50})
	if err != nil {
		return err
	}
	apps, err := s.st.Q.ListProjectApps(ctx, d.ProjectID)
	if err != nil {
		return err
	}
	type linkDTO struct {
		AppID   int64  `json:"appId"`
		AppName string `json:"appName"`
		EnvKey  string `json:"envKey"`
	}
	type appRef struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	type backupDTO struct {
		ID         int64  `json:"id"`
		Trigger    string `json:"trigger"`
		Status     string `json:"status"`
		SizeBytes  int64  `json:"sizeBytes"`
		Error      string `json:"error,omitempty"`
		StartedAt  int64  `json:"startedAt"`
		FinishedAt int64  `json:"finishedAt,omitempty"`
		File       string `json:"file,omitempty"`
		Local      bool   `json:"local"`
		Remote     string `json:"remoteStatus,omitempty"`
		RemoteErr  string `json:"remoteError,omitempty"`
	}
	users, err := s.st.Q.ListDatabaseUsers(ctx, d.ID)
	if err != nil {
		return err
	}
	out := struct {
		dbSummary
		ProjectName    string      `json:"projectName"`
		Host           string      `json:"host"`
		Port           int         `json:"port"`
		DBName         string      `json:"dbName,omitempty"`
		Username       string      `json:"username,omitempty"`
		MemoryMB       int64       `json:"memoryMb"`
		DesiredStatus  string      `json:"desiredStatus"`
		BackupSchedule string      `json:"backupSchedule"`
		BackupKeep     int64       `json:"backupKeep"`
		DefaultEnvKey  string      `json:"defaultEnvKey"`
		Links          []linkDTO   `json:"links"`
		Apps           []appRef    `json:"apps"`
		Backups        []backupDTO `json:"backups"`
		CreatedAt      int64       `json:"createdAt"`
		DestinationID  *int64      `json:"backupDestinationId"`
		Users          []dbUserDTO `json:"users"`
		Roles          []string    `json:"roles"`
	}{
		dbSummary: toDBSummary(d), ProjectName: project.Name, Host: strings.ToLower(d.Name), Port: e.Port,
		DBName: d.DbName, Username: d.Username, MemoryMB: d.MemoryMb, DesiredStatus: d.DesiredStatus,
		BackupSchedule: d.BackupSchedule, BackupKeep: d.BackupKeep, DefaultEnvKey: e.DefaultEnvKey,
		Links: []linkDTO{}, Apps: []appRef{}, Backups: []backupDTO{}, CreatedAt: d.CreatedAt,
		Users: []dbUserDTO{}, Roles: databases.Roles,
	}
	if d.BackupDestinationID.Valid {
		out.DestinationID = &d.BackupDestinationID.Int64
	}
	for _, u := range users {
		out.Users = append(out.Users, toDBUserDTO(u))
	}
	for _, l := range links {
		out.Links = append(out.Links, linkDTO{AppID: l.AppID, AppName: l.AppName, EnvKey: l.EnvKey})
	}
	for _, a := range apps {
		out.Apps = append(out.Apps, appRef{ID: a.ID, Name: a.Name})
	}
	for _, b := range backups {
		out.Backups = append(out.Backups, backupDTO{
			ID: b.ID, Trigger: b.Trigger, Status: b.Status, SizeBytes: b.SizeBytes, Error: b.Error,
			StartedAt: b.StartedAt, FinishedAt: b.FinishedAt, File: filepath.Base(b.File),
			Local: s.dbs.HasLocalFile(b), Remote: b.RemoteStatus, RemoteErr: b.RemoteError,
		})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// GET /api/databases/{id}/credentials — reveals passwords; audited.
func (s *Server) handleDatabaseCredentials(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	d, err := s.dbFromPath(r)
	if err != nil {
		return err
	}
	c, err := s.dbs.Creds(d)
	if err != nil {
		return err
	}
	u, err := s.dbs.InternalURL(d)
	if err != nil {
		return err
	}
	ctx := r.Context()
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		return s.auditDB(ctx, q, r, d.ID, audit.ActionDatabaseRevealed, nil)
	}); err != nil {
		return err
	}
	resp := map[string]string{"password": c.Password, "url": u}
	if c.RootPassword != "" {
		resp["rootPassword"] = c.RootPassword
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// POST /api/databases/{id}/{action} for start, stop, restart, recreate.
func (s *Server) handleDatabaseLifecycle(action string) func(http.ResponseWriter, *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		if err := s.requireDatabases(); err != nil {
			return err
		}
		d, err := s.dbFromPath(r)
		if err != nil {
			return err
		}
		ctx := r.Context()
		var act string
		switch action {
		case "start":
			err, act = s.dbs.Start(ctx, d.ID), audit.ActionDatabaseStarted
		case "stop":
			err, act = s.dbs.Stop(ctx, d.ID), audit.ActionDatabaseStopped
		case "restart":
			err, act = s.dbs.Restart(ctx, d.ID), audit.ActionDatabaseRestarted
		case "recreate":
			err, act = s.dbs.Recreate(ctx, d.ID), audit.ActionDatabaseRecreated
		}
		if err != nil {
			return errf(http.StatusBadGateway, "%s failed: %v", action, err)
		}
		if err := s.st.Tx(ctx, func(q *db.Queries) error { return s.auditDB(ctx, q, r, d.ID, act, nil) }); err != nil {
			return err
		}
		return s.handleGetDatabase(w, r)
	}
}

// PUT /api/databases/{id}/public — {"port": 0} hides, {"port": N} publishes.
func (s *Server) handleDatabasePublic(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	d, err := s.dbFromPath(r)
	if err != nil {
		return err
	}
	var in struct {
		Port int64 `json:"port"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	if in.Port != 0 {
		if in.Port < 1024 || in.Port > 65535 {
			return errf(http.StatusBadRequest, "port must be 1024-65535, or 0 to make the database private")
		}
		if in.Port == int64(s.lm.port()) {
			return errf(http.StatusConflict, "port %d is used by the dashboard", in.Port)
		}
		if n, err := s.st.Q.PublicPortInUse(ctx, db.PublicPortInUseParams{PublicPort: in.Port, ID: d.ID}); err != nil {
			return err
		} else if n > 0 {
			return errf(http.StatusConflict, "port %d is already used by another database", in.Port)
		}
	}
	if err := s.dbs.SetPublicPort(ctx, d.ID, in.Port); err != nil {
		return errf(http.StatusBadGateway, "%s", err.Error())
	}
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		return s.auditDB(ctx, q, r, d.ID, audit.ActionDatabasePublicPort, map[string]any{"from": d.PublicPort, "to": in.Port})
	}); err != nil {
		return err
	}
	return s.handleGetDatabase(w, r)
}

// PUT /api/databases/{id}/backup-policy — {"schedule": "daily", "keep": 7}
func (s *Server) handleBackupPolicy(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	d, err := s.dbFromPath(r)
	if err != nil {
		return err
	}
	var in struct {
		Schedule string `json:"schedule"`
		Keep     int64  `json:"keep"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if _, ok := databases.Schedules[in.Schedule]; !ok {
		return errf(http.StatusBadRequest, "schedule must be off, hourly, daily or weekly")
	}
	if in.Keep < 1 || in.Keep > 365 {
		return errf(http.StatusBadRequest, "keep must be 1-365")
	}
	ctx := r.Context()
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.SetDatabaseBackupPolicy(ctx, db.SetDatabaseBackupPolicyParams{
			BackupSchedule: in.Schedule, BackupKeep: in.Keep, UpdatedAt: s.now().Unix(), ID: d.ID,
		}); err != nil {
			return err
		}
		return s.auditDB(ctx, q, r, d.ID, audit.ActionBackupPolicy, map[string]any{"schedule": in.Schedule, "keep": in.Keep})
	})
	if err != nil {
		return err
	}
	return s.handleGetDatabase(w, r)
}

// POST /api/databases/{id}/backups — start a backup now.
func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	d, err := s.dbFromPath(r)
	if err != nil {
		return err
	}
	if d.RuntimeStatus != databases.StatusRunning {
		return errf(http.StatusConflict, "the database must be running to back it up")
	}
	ctx := r.Context()
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		return s.auditDB(ctx, q, r, d.ID, audit.ActionBackupRequested, nil)
	}); err != nil {
		return err
	}
	s.dbs.BackupAsync(d.ID, databases.TriggerManual)
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	return nil
}

// POST /api/databases/{id}/restore — {"backupId": N}
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	d, err := s.dbFromPath(r)
	if err != nil {
		return err
	}
	var in struct {
		BackupID int64 `json:"backupId"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	b, err := s.st.Q.GetBackup(ctx, in.BackupID)
	if err != nil || b.DatabaseID != d.ID {
		return errf(http.StatusNotFound, "backup not found for this database")
	}
	if b.Status != "succeeded" {
		return errf(http.StatusConflict, "only successful backups can be restored")
	}
	if d.RuntimeStatus != databases.StatusRunning {
		return errf(http.StatusConflict, "the database must be running to restore into it")
	}
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		return s.auditDB(ctx, q, r, d.ID, audit.ActionRestoreRequested, map[string]any{"backup": b.ID})
	}); err != nil {
		return err
	}
	s.dbs.RestoreAsync(d.ID, b.ID)
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	return nil
}

// GET /api/backups/{id}/download
func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	b, err := s.st.Q.GetBackup(r.Context(), id)
	if err != nil {
		return notFoundAs(err, "backup")
	}
	f, size, err := s.dbs.OpenBackupReader(r.Context(), b)
	if err != nil {
		return errf(http.StatusNotFound, "%s", err.Error())
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(b.File)))
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	_, _ = io.Copy(w, f)
	return nil
}

// POST /api/databases/{id}/links — {"appId": N, "envKey": "DATABASE_URL"}
func (s *Server) handleLinkDatabase(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	d, err := s.dbFromPath(r)
	if err != nil {
		return err
	}
	var in struct {
		AppID  int64  `json:"appId"`
		EnvKey string `json:"envKey"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.EnvKey == "" {
		in.EnvKey = databases.Engines[d.Engine].DefaultEnvKey
	}
	if !envKeyRe.MatchString(in.EnvKey) {
		return errf(http.StatusBadRequest, "invalid environment variable name")
	}
	ctx := r.Context()
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if err := s.dbs.Link(ctx, q, d, in.AppID, in.EnvKey); err != nil {
			if errors.Is(err, databases.ErrNotFound) {
				return errf(http.StatusNotFound, "app not found")
			}
			return errf(http.StatusBadRequest, "%s", err.Error())
		}
		return s.auditDB(ctx, q, r, d.ID, audit.ActionDatabaseLinked, map[string]any{"app": in.AppID, "envKey": in.EnvKey})
	})
	if err != nil {
		return err
	}
	return s.handleGetDatabase(w, r)
}

// DELETE /api/databases/{id}/links/{appId}
func (s *Server) handleUnlinkDatabase(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	d, err := s.dbFromPath(r)
	if err != nil {
		return err
	}
	appID, err := pathID(r, "appId")
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if err := s.dbs.Unlink(ctx, q, d.ID, appID); err != nil {
			return err
		}
		return s.auditDB(ctx, q, r, d.ID, audit.ActionDatabaseUnlinked, map[string]any{"app": appID})
	})
	if err != nil {
		return err
	}
	return s.handleGetDatabase(w, r)
}

// DELETE /api/databases/{id}?deleteData=1
func (s *Server) handleDeleteDatabase(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	d, err := s.dbFromPath(r)
	if err != nil {
		return err
	}
	deleteData := r.URL.Query().Get("deleteData") == "1"
	ctx := r.Context()
	err = s.dbs.Delete(ctx, d.ID, deleteData, func(q *db.Queries) error {
		return s.auditDB(ctx, q, r, d.ID, audit.ActionDatabaseDeleted, map[string]any{"name": d.Name, "deleteData": deleteData})
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}
