package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/cron"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

const maxCronJobsPerApp = 50

type cronRunDTO struct {
	ID         int64  `json:"id"`
	Trigger    string `json:"trigger"`
	Status     string `json:"status"`
	ExitCode   *int64 `json:"exitCode,omitempty"`
	Error      string `json:"error,omitempty"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	DurationMs int64  `json:"durationMs"`
	Logs       string `json:"logs,omitempty"`
}

type cronJobDTO struct {
	ID          int64       `json:"id"`
	AppID       int64       `json:"appId"`
	Name        string      `json:"name"`
	Schedule    string      `json:"schedule"`
	Timezone    string      `json:"timezone"`
	Command     string      `json:"command"`
	TimeoutSec  int64       `json:"timeoutSec"`
	Concurrency string      `json:"concurrency"`
	Enabled     bool        `json:"enabled"`
	NextRunAt   int64       `json:"nextRunAt,omitempty"`
	LastRun     *cronRunDTO `json:"lastRun,omitempty"`
}

func runDTO(id, startedAt, durationMs int64, trigger, status, errMsg string, exit, finished nullInt) cronRunDTO {
	d := cronRunDTO{ID: id, Trigger: trigger, Status: status, Error: errMsg, StartedAt: startedAt, DurationMs: durationMs}
	if exit.Valid {
		v := exit.Int64
		d.ExitCode = &v
	}
	if finished.Valid {
		d.FinishedAt = finished.Int64
	}
	return d
}

type nullInt = struct {
	Int64 int64
	Valid bool
}

func listRowDTO(r db.ListCronRunsRow) cronRunDTO {
	return runDTO(r.ID, r.StartedAt, r.DurationMs, r.Trigger, r.Status, r.Error,
		nullInt{r.ExitCode.Int64, r.ExitCode.Valid}, nullInt{r.FinishedAt.Int64, r.FinishedAt.Valid})
}

func (s *Server) jobDTO(ctx context.Context, j db.CronJob) cronJobDTO {
	d := cronJobDTO{
		ID: j.ID, AppID: j.AppID, Name: j.Name, Schedule: j.Schedule, Timezone: j.Timezone, Command: j.Command,
		TimeoutSec: j.TimeoutSec, Concurrency: j.Concurrency, Enabled: j.Enabled == 1, NextRunAt: j.NextRunAt,
	}
	if r, err := s.st.Q.LastCronRun(ctx, j.ID); err == nil {
		v := listRowDTO(db.ListCronRunsRow(r))
		d.LastRun = &v
	}
	return d
}

func (s *Server) requireCron() error {
	if err := s.requireDocker(); err != nil {
		return err
	}
	if s.cron == nil {
		return errf(http.StatusServiceUnavailable, "cron jobs are not available on this server")
	}
	return nil
}

type cronInput struct {
	Name        *string `json:"name"`
	Schedule    *string `json:"schedule"`
	Timezone    *string `json:"timezone"`
	Command     *string `json:"command"`
	TimeoutSec  *int64  `json:"timeoutSec"`
	Concurrency *string `json:"concurrency"`
	Enabled     *bool   `json:"enabled"`
}

// apply validates the input and merges it into j.
func (in cronInput) apply(j *db.CronJob) error {
	if in.Schedule != nil {
		j.Schedule = strings.Join(strings.Fields(*in.Schedule), " ")
	}
	if in.Timezone != nil {
		j.Timezone = strings.TrimSpace(*in.Timezone)
		if j.Timezone == "" {
			j.Timezone = "UTC"
		}
	}
	if _, err := cron.Parse(j.Schedule, j.Timezone); err != nil {
		return errf(http.StatusBadRequest, "schedule: %v", err)
	}
	if in.Command != nil {
		j.Command = strings.TrimSpace(*in.Command)
	}
	if len(j.Command) > 4096 {
		return errf(http.StatusBadRequest, "command is too long (max 4096 characters)")
	}
	if args, err := apps.SplitCommand(j.Command); err != nil {
		return errf(http.StatusBadRequest, "command: %v", err)
	} else if len(args) == 0 {
		return errf(http.StatusBadRequest, "command is required (e.g. php artisan schedule:run)")
	}
	if in.TimeoutSec != nil {
		if *in.TimeoutSec < 1 || *in.TimeoutSec > 86400 {
			return errf(http.StatusBadRequest, "timeoutSec must be 1-86400")
		}
		j.TimeoutSec = *in.TimeoutSec
	}
	if in.Concurrency != nil {
		if *in.Concurrency != cron.ConcurrencySkip && *in.Concurrency != cron.ConcurrencyAllow {
			return errf(http.StatusBadRequest, "concurrency must be skip or allow")
		}
		j.Concurrency = *in.Concurrency
	}
	if in.Enabled != nil {
		j.Enabled = 0
		if *in.Enabled {
			j.Enabled = 1
		}
	}
	return nil
}

// GET /api/apps/{id}/cron
func (s *Server) handleListCron(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	if _, err := s.st.Q.GetApp(ctx, id); err != nil {
		return notFoundAs(err, "app")
	}
	jobs, err := s.st.Q.ListAppCronJobs(ctx, id)
	if err != nil {
		return err
	}
	out := make([]cronJobDTO, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, s.jobDTO(ctx, j))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out, "available": s.cron != nil})
	return nil
}

// POST /api/apps/{id}/cron
func (s *Server) handleCreateCron(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireCron(); err != nil {
		return err
	}
	appID, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in cronInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	name := ""
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	if !resourceNameRe.MatchString(name) {
		return errf(http.StatusBadRequest, "name must be 1-32 lowercase letters, digits or '-'")
	}
	if in.Schedule == nil {
		return errf(http.StatusBadRequest, "schedule is required (e.g. */5 * * * *)")
	}
	j := db.CronJob{Timezone: "UTC", TimeoutSec: 3600, Concurrency: cron.ConcurrencySkip, Enabled: 1}
	if err := in.apply(&j); err != nil {
		return err
	}
	next, err := s.cron.NextRun(j.Schedule, j.Timezone, j.Enabled == 1)
	if err != nil {
		return errf(http.StatusBadRequest, "schedule: %v", err)
	}
	ctx := r.Context()
	a := authFrom(ctx)
	now := s.now()
	var created db.CronJob
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if _, err := q.GetApp(ctx, appID); err != nil {
			return notFoundAs(err, "app")
		}
		if n, err := q.CountAppCronJobs(ctx, appID); err != nil {
			return err
		} else if n >= maxCronJobsPerApp {
			return errf(http.StatusBadRequest, "an app can have at most %d cron jobs", maxCronJobsPerApp)
		}
		created, err = q.CreateCronJob(ctx, db.CreateCronJobParams{
			AppID: appID, Name: name, Schedule: j.Schedule, Timezone: j.Timezone, Command: j.Command,
			TimeoutSec: j.TimeoutSec, Concurrency: j.Concurrency, Enabled: j.Enabled, NextRunAt: next,
			CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
		})
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return errf(http.StatusConflict, "this app already has a cron job named %q", name)
			}
			return err
		}
		_, err = audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionCronCreated,
			Target: fmt.Sprintf("cron:%d", created.ID), IP: clientIP(r),
			Details: map[string]any{"app": appID, "name": name, "schedule": j.Schedule, "timezone": j.Timezone,
				"command": j.Command, "timeoutSec": j.TimeoutSec, "concurrency": j.Concurrency, "enabled": j.Enabled == 1},
		}, now)
		return err
	})
	if err != nil {
		return err
	}
	s.cron.Wake()
	writeJSON(w, http.StatusCreated, s.jobDTO(ctx, created))
	return nil
}

// PATCH /api/cron/{id}
func (s *Server) handleUpdateCron(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireCron(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in cronInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.Name != nil {
		return errf(http.StatusBadRequest, "a cron job can't be renamed; create a new one")
	}
	ctx := r.Context()
	a := authFrom(ctx)
	now := s.now()
	var updated db.CronJob
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		j, err := q.GetCronJob(ctx, id)
		if err != nil {
			return notFoundAs(err, "cron job")
		}
		before := j
		if err := in.apply(&j); err != nil {
			return err
		}
		next := j.NextRunAt
		if j.Schedule != before.Schedule || j.Timezone != before.Timezone || j.Enabled != before.Enabled {
			if next, err = s.cron.NextRun(j.Schedule, j.Timezone, j.Enabled == 1); err != nil {
				return errf(http.StatusBadRequest, "schedule: %v", err)
			}
		}
		if err := q.UpdateCronJob(ctx, db.UpdateCronJobParams{
			Schedule: j.Schedule, Timezone: j.Timezone, Command: j.Command, TimeoutSec: j.TimeoutSec,
			Concurrency: j.Concurrency, Enabled: j.Enabled, NextRunAt: next, UpdatedAt: now.Unix(), ID: id,
		}); err != nil {
			return err
		}
		changes := map[string]any{}
		diff := func(k string, a, b any) {
			if a != b {
				changes[k] = []any{a, b}
			}
		}
		diff("schedule", before.Schedule, j.Schedule)
		diff("timezone", before.Timezone, j.Timezone)
		diff("command", before.Command, j.Command)
		diff("timeoutSec", before.TimeoutSec, j.TimeoutSec)
		diff("concurrency", before.Concurrency, j.Concurrency)
		diff("enabled", before.Enabled == 1, j.Enabled == 1)
		if updated, err = q.GetCronJob(ctx, id); err != nil {
			return err
		}
		_, err = audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionCronUpdated,
			Target: fmt.Sprintf("cron:%d", id), IP: clientIP(r), Details: changes,
		}, now)
		return err
	})
	if err != nil {
		return err
	}
	s.cron.Wake()
	writeJSON(w, http.StatusOK, s.jobDTO(ctx, updated))
	return nil
}

// DELETE /api/cron/{id}
func (s *Server) handleDeleteCron(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	j, err := s.st.Q.GetCronJob(ctx, id)
	if err != nil {
		return notFoundAs(err, "cron job")
	}
	if s.cron != nil {
		s.cron.StopJobRuns(ctx, id)
	}
	a := authFrom(ctx)
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.DeleteCronJob(ctx, id); err != nil {
			return err
		}
		_, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionCronDeleted,
			Target: fmt.Sprintf("cron:%d", id), IP: clientIP(r),
			Details: map[string]any{"app": j.AppID, "name": j.Name},
		}, s.now())
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// POST /api/cron/{id}/run — run now, outside the schedule.
func (s *Server) handleRunCron(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireCron(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	j, err := s.st.Q.GetCronJob(ctx, id)
	if err != nil {
		return notFoundAs(err, "cron job")
	}
	run, err := s.cron.Start(ctx, id, cron.TriggerManual)
	if err != nil {
		return err
	}
	a := authFrom(ctx)
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		_, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionCronRunRequested,
			Target: fmt.Sprintf("cron:%d", id), IP: clientIP(r),
			Details: map[string]any{"app": j.AppID, "name": j.Name, "run": run.ID, "status": run.Status},
		}, s.now())
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"runId": run.ID, "status": run.Status, "error": run.Error})
	return nil
}

// GET /api/cron/{id}/runs?limit=50
func (s *Server) handleListCronRuns(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	limit := int64(50)
	if v, err := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	ctx := r.Context()
	j, err := s.st.Q.GetCronJob(ctx, id)
	if err != nil {
		return notFoundAs(err, "cron job")
	}
	rows, err := s.st.Q.ListCronRuns(ctx, db.ListCronRunsParams{JobID: id, Limit: limit})
	if err != nil {
		return err
	}
	out := make([]cronRunDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, listRowDTO(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": s.jobDTO(ctx, j), "runs": out})
	return nil
}

// GET /api/cron-runs/{id} — includes the captured output.
func (s *Server) handleGetCronRun(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	run, err := s.st.Q.GetCronRun(r.Context(), id)
	if err != nil {
		return notFoundAs(err, "run")
	}
	d := runDTO(run.ID, run.StartedAt, run.DurationMs, run.Trigger, run.Status, run.Error,
		nullInt{run.ExitCode.Int64, run.ExitCode.Valid}, nullInt{run.FinishedAt.Int64, run.FinishedAt.Valid})
	d.Logs = run.Logs
	writeJSON(w, http.StatusOK, map[string]any{"run": d, "jobId": run.JobID})
	return nil
}

// POST /api/cron-runs/{id}/cancel
func (s *Server) handleCancelCronRun(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireCron(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	run, err := s.st.Q.GetCronRun(ctx, id)
	if err != nil {
		return notFoundAs(err, "run")
	}
	if run.Status != cron.StatusRunning || !s.cron.Cancel(id) {
		return errf(http.StatusConflict, "this run is not in progress")
	}
	a := authFrom(ctx)
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		_, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionCronRunCancelled,
			Target: fmt.Sprintf("cron:%d", run.JobID), IP: clientIP(r), Details: map[string]any{"run": id},
		}, s.now())
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// POST /api/cron/preview {"schedule": "...", "timezone": "..."} — the next run times.
func (s *Server) handleCronPreview(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Schedule string `json:"schedule"`
		Timezone string `json:"timezone"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	sch, err := cron.Parse(in.Schedule, in.Timezone)
	if err != nil {
		return errf(http.StatusBadRequest, "%v", err)
	}
	t := s.now()
	next := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		if t = sch.Next(t); t.IsZero() {
			break
		}
		next = append(next, t.Unix())
	}
	writeJSON(w, http.StatusOK, map[string]any{"next": next, "timezone": sch.Location().String(), "now": s.now().Truncate(time.Second).Unix()})
	return nil
}
