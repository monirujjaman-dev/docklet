package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/databases"
	"github.com/monirujjaman-dev/docklet/internal/destinations"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

type destinationDTO struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Endpoint      string `json:"endpoint"`
	Region        string `json:"region"`
	Bucket        string `json:"bucket"`
	Prefix        string `json:"prefix"`
	AccessKey     string `json:"accessKey"`
	PathStyle     bool   `json:"pathStyle"`
	LastTestAt    int64  `json:"lastTestAt,omitempty"`
	LastTestError string `json:"lastTestError,omitempty"`
	UsedBy        int64  `json:"usedBy"`
}

func (s *Server) destinationDTO(ctx context.Context, d db.S3Destination) destinationDTO {
	used, _ := s.st.Q.CountDestinationUsers(ctx, sql.NullInt64{Int64: d.ID, Valid: true})
	return destinationDTO{ID: d.ID, Name: d.Name, Endpoint: d.Endpoint, Region: d.Region, Bucket: d.Bucket, Prefix: d.Prefix,
		AccessKey: d.AccessKey, PathStyle: d.PathStyle == 1, LastTestAt: d.LastTestAt, LastTestError: d.LastTestError, UsedBy: used}
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func (s *Server) audit(ctx context.Context, q *db.Queries, r *http.Request, action, target string, details map[string]any) error {
	a := authFrom(ctx)
	_, err := audit.Append(ctx, q, audit.Entry{
		ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: action, Target: target, IP: clientIP(r), Details: details,
	}, s.now())
	return err
}

// GET /api/destinations
func (s *Server) handleListDestinations(w http.ResponseWriter, r *http.Request) error {
	list, err := s.st.Q.ListDestinations(r.Context())
	if err != nil {
		return err
	}
	out := make([]destinationDTO, 0, len(list))
	for _, d := range list {
		out = append(out, s.destinationDTO(r.Context(), d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"destinations": out})
	return nil
}

// POST /api/destinations/test — tests unsaved settings.
func (s *Server) handleTestDestinationInput(w http.ResponseWriter, r *http.Request) error {
	var in destinations.Input
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := in.Normalize(); err != nil {
		return errf(http.StatusBadRequest, "%v", err)
	}
	c, err := s.dests.ClientFor(in)
	if err != nil {
		return errf(http.StatusBadRequest, "%v", err)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := c.Test(ctx); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

// POST /api/destinations — saves a destination, then tests it.
func (s *Server) handleCreateDestination(w http.ResponseWriter, r *http.Request) error {
	var in destinations.Input
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := in.Normalize(); err != nil {
		return errf(http.StatusBadRequest, "%v", err)
	}
	ctx := r.Context()
	now := s.now().Unix()
	var row db.S3Destination
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateDestination(ctx, db.CreateDestinationParams{Name: in.Name, Endpoint: in.Endpoint, Region: in.Region,
			Bucket: in.Bucket, Prefix: in.Prefix, AccessKey: in.AccessKey, PathStyle: b2i(in.PathStyle), CreatedAt: now, UpdatedAt: now})
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return errf(http.StatusConflict, "a destination named %q already exists", in.Name)
			}
			return err
		}
		enc, err := s.dests.SealSecret(row.ID, in.SecretKey)
		if err != nil {
			return err
		}
		if err := q.SetDestinationSecret(ctx, db.SetDestinationSecretParams{SecretEnc: enc, ID: row.ID}); err != nil {
			return err
		}
		return s.audit(ctx, q, r, audit.ActionS3Created, fmt.Sprintf("s3:%d", row.ID), map[string]any{
			"name": in.Name, "endpoint": in.Endpoint, "bucket": in.Bucket, "prefix": in.Prefix, "accessKey": in.AccessKey,
		})
	})
	if err != nil {
		return err
	}
	_ = s.dests.Test(ctx, row.ID)
	row, _ = s.st.Q.GetDestination(ctx, row.ID)
	writeJSON(w, http.StatusCreated, s.destinationDTO(ctx, row))
	return nil
}

// PUT /api/destinations/{id} — an empty secretKey keeps the stored one.
func (s *Server) handleUpdateDestination(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in destinations.Input
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	cur, err := s.st.Q.GetDestination(ctx, id)
	if err != nil {
		return notFoundAs(err, "destination")
	}
	keepSecret := in.SecretKey == ""
	if keepSecret {
		in.SecretKey = "unchanged" // satisfies validation; not stored
	}
	if err := in.Normalize(); err != nil {
		return errf(http.StatusBadRequest, "%v", err)
	}
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.UpdateDestination(ctx, db.UpdateDestinationParams{Name: in.Name, Endpoint: in.Endpoint, Region: in.Region, Bucket: in.Bucket,
			Prefix: in.Prefix, AccessKey: in.AccessKey, PathStyle: b2i(in.PathStyle), UpdatedAt: s.now().Unix(), ID: id}); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return errf(http.StatusConflict, "a destination named %q already exists", in.Name)
			}
			return err
		}
		if !keepSecret {
			enc, err := s.dests.SealSecret(id, in.SecretKey)
			if err != nil {
				return err
			}
			if err := q.SetDestinationSecret(ctx, db.SetDestinationSecretParams{SecretEnc: enc, ID: id}); err != nil {
				return err
			}
		}
		changes := map[string]any{"secretChanged": !keepSecret}
		for k, v := range map[string][2]string{"name": {cur.Name, in.Name}, "endpoint": {cur.Endpoint, in.Endpoint}, "region": {cur.Region, in.Region},
			"bucket": {cur.Bucket, in.Bucket}, "prefix": {cur.Prefix, in.Prefix}, "accessKey": {cur.AccessKey, in.AccessKey}} {
			if v[0] != v[1] {
				changes[k] = v
			}
		}
		return s.audit(ctx, q, r, audit.ActionS3Updated, fmt.Sprintf("s3:%d", id), changes)
	})
	if err != nil {
		return err
	}
	_ = s.dests.Test(ctx, id)
	row, _ := s.st.Q.GetDestination(ctx, id)
	writeJSON(w, http.StatusOK, s.destinationDTO(ctx, row))
	return nil
}

// POST /api/destinations/{id}/test
func (s *Server) handleTestDestination(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	if _, err := s.st.Q.GetDestination(ctx, id); err != nil {
		return notFoundAs(err, "destination")
	}
	terr := s.dests.Test(ctx, id)
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		d := map[string]any{"ok": terr == nil}
		if terr != nil {
			d["error"] = terr.Error()
		}
		return s.audit(ctx, q, r, audit.ActionS3Tested, fmt.Sprintf("s3:%d", id), d)
	}); err != nil {
		return err
	}
	row, _ := s.st.Q.GetDestination(ctx, id)
	writeJSON(w, http.StatusOK, s.destinationDTO(ctx, row))
	return nil
}

// DELETE /api/destinations/{id}
func (s *Server) handleDeleteDestination(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		d, err := q.GetDestination(ctx, id)
		if err != nil {
			return notFoundAs(err, "destination")
		}
		if n, err := q.CountDestinationUsers(ctx, sql.NullInt64{Int64: id, Valid: true}); err != nil {
			return err
		} else if n > 0 {
			return errf(http.StatusConflict, "%q is used by %d backup configuration(s); change them first", d.Name, n)
		}
		if err := q.DeleteDestination(ctx, id); err != nil {
			return err
		}
		return s.audit(ctx, q, r, audit.ActionS3Deleted, fmt.Sprintf("s3:%d", id), map[string]any{"name": d.Name})
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// PUT /api/databases/{id}/backup-destination {"destinationId": N | null}
func (s *Server) handleDatabaseDestination(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	d, err := s.dbFromPath(r)
	if err != nil {
		return err
	}
	var in struct {
		DestinationID *int64 `json:"destinationId"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	dest := sql.NullInt64{}
	name := ""
	if in.DestinationID != nil {
		row, err := s.st.Q.GetDestination(ctx, *in.DestinationID)
		if err != nil {
			return errf(http.StatusBadRequest, "unknown destination")
		}
		dest, name = sql.NullInt64{Int64: row.ID, Valid: true}, row.Name
	}
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.SetDatabaseDestination(ctx, db.SetDatabaseDestinationParams{BackupDestinationID: dest, UpdatedAt: s.now().Unix(), ID: d.ID}); err != nil {
			return err
		}
		return s.auditDB(ctx, q, r, d.ID, audit.ActionDestinationChanged, map[string]any{"destination": name})
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// POST /api/backups/{id}/upload — retry an upload.
func (s *Server) handleUploadBackup(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if _, err := s.st.Q.GetBackup(r.Context(), id); err != nil {
		return notFoundAs(err, "backup")
	}
	if err := s.dbs.UploadBackup(r.Context(), id); err != nil {
		return errf(http.StatusBadRequest, "%v", err)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// --- Database users ------------------------------------------------------

type dbUserDTO struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

func toDBUserDTO(u db.DatabaseUser) dbUserDTO {
	return dbUserDTO{ID: u.ID, Username: u.Username, Role: u.Role, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt}
}

func dbUserError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return errf(http.StatusNotFound, "user not found")
	}
	return errf(http.StatusBadRequest, "%v", err)
}

// POST /api/databases/{id}/users {"username": "...", "role": "readonly"}
func (s *Server) handleCreateDBUser(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	d, err := s.dbFromPath(r)
	if err != nil {
		return err
	}
	var in struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	u, err := s.dbs.CreateUser(ctx, d.ID, strings.TrimSpace(in.Username), in.Role)
	if err != nil {
		return dbUserError(err)
	}
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		return s.auditDB(ctx, q, r, d.ID, audit.ActionDBUserCreated, map[string]any{"user": u.Username, "role": u.Role})
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, toDBUserDTO(u))
	return nil
}

func (s *Server) dbUserFromPath(r *http.Request) (db.DatabaseUser, db.Database, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return db.DatabaseUser{}, db.Database{}, err
	}
	u, err := s.st.Q.GetDatabaseUser(r.Context(), id)
	if err != nil {
		return u, db.Database{}, notFoundAs(err, "user")
	}
	d, err := s.st.Q.GetDatabase(r.Context(), u.DatabaseID)
	return u, d, err
}

// GET /api/database-users/{id}/credentials — audited.
func (s *Server) handleDBUserCredentials(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	u, d, err := s.dbUserFromPath(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	pw, err := s.dbs.UserPassword(u)
	if err != nil {
		return err
	}
	url, err := s.dbs.UserURL(d, u)
	if err != nil {
		return err
	}
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		return s.auditDB(ctx, q, r, d.ID, audit.ActionDBUserRevealed, map[string]any{"user": u.Username})
	}); err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"username": u.Username, "password": pw, "url": url})
	return nil
}

// PATCH /api/database-users/{id} {"role": "...", "resetPassword": true}
func (s *Server) handleUpdateDBUser(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	u, d, err := s.dbUserFromPath(r)
	if err != nil {
		return err
	}
	var in struct {
		Role          string `json:"role"`
		ResetPassword bool   `json:"resetPassword"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	nu, err := s.dbs.UpdateUser(ctx, u.ID, in.Role, in.ResetPassword)
	if err != nil {
		return dbUserError(err)
	}
	details := map[string]any{"user": u.Username, "passwordReset": in.ResetPassword}
	if nu.Role != u.Role {
		details["role"] = []string{u.Role, nu.Role}
	}
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		return s.auditDB(ctx, q, r, d.ID, audit.ActionDBUserUpdated, details)
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, toDBUserDTO(nu))
	return nil
}

// DELETE /api/database-users/{id}
func (s *Server) handleDeleteDBUser(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDatabases(); err != nil {
		return err
	}
	u, d, err := s.dbUserFromPath(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	if _, err := s.dbs.DeleteUser(ctx, u.ID); err != nil {
		return dbUserError(err)
	}
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		return s.auditDB(ctx, q, r, d.ID, audit.ActionDBUserDeleted, map[string]any{"user": u.Username})
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

var _ = databases.RoleAdmin
