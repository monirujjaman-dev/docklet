package cron

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// Run statuses and triggers.
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusTimeout   = "timeout"
	StatusSkipped   = "skipped"

	TriggerSchedule = "schedule"
	TriggerManual   = "manual"

	ConcurrencySkip  = "skip"
	ConcurrencyAllow = "allow"

	LabelJob = "docklet.cron.job"
	LabelRun = "docklet.cron.run"
)

// Docker is the subset of the Docker client cron runs need.
type Docker interface {
	ContainerCreate(ctx context.Context, s docker.ContainerSpec) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerWait(ctx context.Context, id string) (int, error)
	ContainerStop(ctx context.Context, id string, timeoutSec int) error
	ContainerRemove(ctx context.Context, id string) error
	ContainerList(ctx context.Context, labels map[string]string) ([]docker.ContainerSummary, error)
	ContainerLogs(ctx context.Context, id string, o docker.LogOptions, fn func(docker.LogLine) error) error
	ImageInspect(ctx context.Context, ref string) (docker.Image, error)
}

// ErrNoImage means the job's app has never been deployed.
var ErrNoImage = errors.New("the app has no successful deployment yet, so there is no image to run")

// Service fires due jobs and runs them.
type Service struct {
	st  *store.Store
	dk  Docker
	log *slog.Logger
	now func() time.Time

	EnvFor        func(ctx context.Context, appID int64) ([]string, error)
	EnsureNetwork func(ctx context.Context, projectID int64) (string, error)

	Tick           time.Duration // how often due jobs are checked
	MissedAfter    time.Duration // a due time older than this (Docklet was down) is skipped
	StopTimeoutSec int
	KeepRuns       int
	MaxLogBytes    int

	mu      sync.Mutex
	running map[int64]context.CancelFunc // run id -> cancel
	wg      sync.WaitGroup
	base    context.Context
	wake    chan struct{}
}

// New creates a service.
func New(st *store.Store, dk Docker, log *slog.Logger) *Service {
	return &Service{
		st: st, dk: dk, log: log, now: time.Now,
		Tick: time.Second, MissedAfter: 2 * time.Minute, StopTimeoutSec: 10, KeepRuns: 100, MaxLogBytes: 64 << 10,
		running: map[int64]context.CancelFunc{}, base: context.Background(), wake: make(chan struct{}, 1),
	}
}

// SetClock overrides the time source.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// NextRun computes a job's next run time after now (0 when disabled).
func (s *Service) NextRun(schedule, tz string, enabled bool) (int64, error) {
	sch, err := Parse(schedule, tz)
	if err != nil {
		return 0, err
	}
	if !enabled {
		return 0, nil
	}
	return sch.Next(s.now()).Unix(), nil
}

// Wake makes the scheduler re-check due jobs (after edits).
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run recovers from a restart, then fires due jobs until ctx ends, and waits
// for in-flight runs to be stopped.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()
	s.recover(ctx)
	t := time.NewTicker(s.Tick)
	defer t.Stop()
	for {
		s.fireDue(ctx)
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// Wait blocks until in-flight runs finish (tests).
func (s *Service) Wait() { s.wg.Wait() }

// recover marks runs interrupted by a restart as failed, removes their
// containers, and reschedules every enabled job from now.
func (s *Service) recover(ctx context.Context) {
	if n, err := s.st.Q.FailRunningCronRuns(ctx, db.FailRunningCronRunsParams{
		Error: "interrupted: Docklet restarted while this run was in progress", FinishedAt: sql.NullInt64{Int64: s.now().Unix(), Valid: true},
	}); err == nil && n > 0 {
		s.log.Info("marked interrupted cron runs as failed", "count", n)
	}
	if list, err := s.dk.ContainerList(ctx, map[string]string{apps.LabelManaged: "true"}); err == nil {
		for _, c := range list {
			if c.Labels[LabelRun] != "" {
				_ = s.dk.ContainerRemove(ctx, c.ID)
			}
		}
	}
	jobs, err := s.st.Q.ListCronJobs(ctx)
	if err != nil {
		return
	}
	for _, j := range jobs {
		if j.Enabled == 0 {
			continue
		}
		// Runs missed while Docklet was down are not caught up.
		if j.NextRunAt != 0 && j.NextRunAt >= s.now().Add(-s.MissedAfter).Unix() {
			continue
		}
		if next, err := s.NextRun(j.Schedule, j.Timezone, true); err == nil {
			_ = s.st.Q.SetCronNextRun(ctx, db.SetCronNextRunParams{NextRunAt: next, ID: j.ID})
		}
	}
}

func (s *Service) fireDue(ctx context.Context) {
	now := s.now()
	jobs, err := s.st.Q.ListDueCronJobs(ctx, now.Unix())
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("list due cron jobs", "err", err)
		}
		return
	}
	for _, j := range jobs {
		next, err := s.NextRun(j.Schedule, j.Timezone, true)
		if err != nil {
			s.log.Warn("cron job has an invalid schedule; disabling its timer", "job", j.ID, "err", err)
			next = 0
		}
		if err := s.st.Q.SetCronNextRun(ctx, db.SetCronNextRunParams{NextRunAt: next, ID: j.ID}); err != nil {
			continue
		}
		if now.Unix()-j.NextRunAt > int64(s.MissedAfter/time.Second) {
			s.log.Info("skipping a missed cron run", "job", j.Name, "due", time.Unix(j.NextRunAt, 0).UTC())
			continue
		}
		if _, err := s.Start(ctx, j.ID, TriggerSchedule); err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.log.Warn("start cron run", "job", j.Name, "err", err)
		}
	}
}

// Start records a run and executes it in the background. With concurrency
// "skip" and a run still in progress, the new run is recorded as skipped.
func (s *Service) Start(ctx context.Context, jobID int64, trigger string) (db.CronRun, error) {
	j, err := s.st.Q.GetCronJob(ctx, jobID)
	if err != nil {
		return db.CronRun{}, err
	}
	now := s.now()
	if j.Concurrency == ConcurrencySkip {
		n, err := s.st.Q.CountRunningCronRuns(ctx, j.ID)
		if err != nil {
			return db.CronRun{}, err
		}
		if n > 0 {
			return s.st.Q.CreateCronRun(ctx, db.CreateCronRunParams{
				JobID: j.ID, Trigger: trigger, Status: StatusSkipped,
				Error:     "skipped: the previous run is still in progress (concurrency policy: skip)",
				StartedAt: now.Unix(), FinishedAt: sql.NullInt64{Int64: now.Unix(), Valid: true},
			})
		}
	}
	run, err := s.st.Q.CreateCronRun(ctx, db.CreateCronRunParams{JobID: j.ID, Trigger: trigger, Status: StatusRunning, StartedAt: now.Unix()})
	if err != nil {
		return run, err
	}
	s.mu.Lock()
	base := s.base
	rctx, cancel := context.WithCancel(base)
	s.running[run.ID] = cancel
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.running, run.ID)
			s.mu.Unlock()
			cancel()
		}()
		s.execute(rctx, j, run, now)
	}()
	return run, nil
}

// Cancel stops a run in progress.
func (s *Service) Cancel(runID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.running[runID]; ok {
		c()
		return true
	}
	return false
}

type outcome struct {
	status string
	exit   sql.NullInt64
	err    string
	logs   string
}

func (s *Service) execute(ctx context.Context, j db.CronJob, run db.CronRun, started time.Time) {
	o := s.runContainer(ctx, j, run)
	fctx := context.WithoutCancel(ctx)
	fin := s.now()
	if err := s.st.Q.FinishCronRun(fctx, db.FinishCronRunParams{
		Status: o.status, ExitCode: o.exit, Error: o.err, Logs: o.logs,
		FinishedAt: sql.NullInt64{Int64: fin.Unix(), Valid: true}, DurationMs: fin.Sub(started).Milliseconds(), ID: run.ID,
	}); err != nil {
		s.log.Warn("finish cron run", "run", run.ID, "err", err)
	}
	_ = s.st.Q.PruneCronRuns(fctx, db.PruneCronRunsParams{JobID: j.ID, Keep: int64(s.KeepRuns)})
}

func (s *Service) runContainer(ctx context.Context, j db.CronJob, run db.CronRun) outcome {
	fail := func(format string, args ...any) outcome {
		return outcome{status: StatusFailed, err: fmt.Sprintf(format, args...)}
	}
	app, err := s.st.Q.GetApp(ctx, j.AppID)
	if err != nil {
		return fail("load app: %v", err)
	}
	if !app.CurrentDeploymentID.Valid {
		return fail("%v", ErrNoImage)
	}
	dep, err := s.st.Q.GetDeployment(ctx, app.CurrentDeploymentID.Int64)
	if err != nil || dep.ImageID == "" {
		return fail("%v", ErrNoImage)
	}
	project, err := s.st.Q.GetProject(ctx, app.ProjectID)
	if err != nil {
		return fail("load project: %v", err)
	}
	cmd, err := apps.SplitCommand(j.Command)
	if err != nil || len(cmd) == 0 {
		return fail("invalid command: %v", err)
	}
	env, err := s.EnvFor(ctx, app.ID)
	if err != nil {
		return fail("environment: %v", err)
	}
	network, err := s.EnsureNetwork(ctx, app.ProjectID)
	if err != nil {
		return fail("network: %v", err)
	}
	name := fmt.Sprintf("docklet-cron-%s-%s-%s-r%d", strings.ToLower(project.Name), strings.ToLower(app.Name), strings.ToLower(j.Name), run.ID)
	_ = s.dk.ContainerRemove(ctx, name)
	id, err := s.dk.ContainerCreate(ctx, docker.ContainerSpec{
		Name: name, Image: dep.ImageID, Env: env, Cmd: cmd, Network: network,
		Hostname:      strings.ToLower(app.Name) + "-cron",
		RestartPolicy: "no",
		NanoCPUs:      app.CpuMillis * 1_000_000,
		MemoryBytes:   app.MemoryMb << 20,
		Labels: map[string]string{
			apps.LabelManaged: "true", apps.LabelProject: strconv.FormatInt(app.ProjectID, 10),
			apps.LabelApp: strconv.FormatInt(app.ID, 10), LabelJob: strconv.FormatInt(j.ID, 10), LabelRun: strconv.FormatInt(run.ID, 10),
		},
	})
	if err != nil {
		return fail("create container: %v", err)
	}
	rm := context.WithoutCancel(ctx)
	defer func() { _ = s.dk.ContainerRemove(rm, id) }()
	_ = s.st.Q.SetCronRunContainer(ctx, db.SetCronRunContainerParams{ContainerID: id, ID: run.ID})
	if err := s.dk.ContainerStart(ctx, id); err != nil {
		return fail("start container: %v", err)
	}

	wctx, cancel := context.WithTimeout(ctx, time.Duration(j.TimeoutSec)*time.Second)
	code, werr := s.dk.ContainerWait(wctx, id)
	timedOut := errors.Is(wctx.Err(), context.DeadlineExceeded)
	cancelled := ctx.Err() != nil
	cancel()
	o := outcome{}
	switch {
	case timedOut || cancelled:
		_ = s.dk.ContainerStop(rm, id, s.StopTimeoutSec)
		o.status = StatusTimeout
		o.err = fmt.Sprintf("timed out after %s and was stopped", time.Duration(j.TimeoutSec)*time.Second)
		if cancelled && !timedOut {
			o.status, o.err = StatusFailed, "cancelled (the job was deleted or Docklet is stopping)"
		}
	case werr != nil:
		o.status, o.err = StatusFailed, fmt.Sprintf("wait: %v", werr)
	default:
		o.exit = sql.NullInt64{Int64: int64(code), Valid: true}
		o.status = StatusSucceeded
		if code != 0 {
			o.status, o.err = StatusFailed, fmt.Sprintf("exited with code %d", code)
		}
	}
	o.logs = s.collectLogs(rm, id)
	return o
}

// collectLogs keeps the tail of the container output within MaxLogBytes.
func (s *Service) collectLogs(ctx context.Context, id string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var lines []string
	size := 0
	_ = s.dk.ContainerLogs(ctx, id, docker.LogOptions{Tail: 2000}, func(l docker.LogLine) error {
		lines = append(lines, l.Text)
		size += len(l.Text) + 1
		for size > s.MaxLogBytes && len(lines) > 1 {
			size -= len(lines[0]) + 1
			lines = lines[1:]
		}
		return nil
	})
	out := strings.Join(lines, "\n")
	if len(out) > s.MaxLogBytes {
		out = out[len(out)-s.MaxLogBytes:]
	}
	return out
}

// StopJobRuns cancels the job's runs in progress (before deleting it).
func (s *Service) StopJobRuns(ctx context.Context, jobID int64) {
	runs, err := s.st.Q.ListCronRuns(ctx, db.ListCronRunsParams{JobID: jobID, Limit: 1000})
	if err != nil {
		return
	}
	for _, r := range runs {
		if r.Status == StatusRunning {
			s.Cancel(r.ID)
		}
	}
	if list, err := s.dk.ContainerList(ctx, map[string]string{LabelJob: strconv.FormatInt(jobID, 10)}); err == nil {
		for _, c := range list {
			_ = s.dk.ContainerRemove(ctx, c.ID)
		}
	}
}
