package cron

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/docker/dockertest"
	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	svc *Service
	st  *store.Store
	dk  *dockertest.Fake
	clk *clock
	app db.App
}

func setup(t *testing.T, deployed bool) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, _ = secrets.New(bytes.Repeat([]byte{1}, secrets.KeySize))
	dk := dockertest.New()
	dk.AddImage("worker:v1", 8080)
	clk := &clock{t: time.Date(2026, 11, 21, 10, 0, 30, 0, time.UTC)}
	svc := New(st, dk, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.SetClock(clk.now)
	svc.EnvFor = func(context.Context, int64) ([]string, error) { return []string{"QUEUE=emails"}, nil }
	svc.EnsureNetwork = func(ctx context.Context, _ int64) (string, error) {
		return "docklet-p1", dk.NetworkEnsure(ctx, "docklet-p1", nil)
	}
	p, _ := st.Q.CreateProject(ctx, db.CreateProjectParams{Name: "shop", CreatedAt: 1})
	app, err := st.Q.CreateApp(ctx, db.CreateAppParams{ProjectID: p.ID, Name: "api", Image: "worker:v1", RestartPolicy: "unless-stopped", MemoryMb: 128, Kind: "web", CreatedAt: 1, UpdatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if deployed {
		dep, err := st.Q.CreateDeployment(ctx, db.CreateDeploymentParams{AppID: app.ID, Image: "worker:v1", ImageID: "sha256:worker:v1", Source: "image", CreatedBy: "admin", CreatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		_ = st.Q.SetAppCurrentDeployment(ctx, db.SetAppCurrentDeploymentParams{CurrentDeploymentID: sql.NullInt64{Int64: dep.ID, Valid: true}, UpdatedAt: 1, ID: app.ID})
	}
	return &env{svc: svc, st: st, dk: dk, clk: clk, app: app}
}

func (e *env) job(t *testing.T, name, schedule, command, concurrency string, timeout int64) db.CronJob {
	t.Helper()
	next, err := e.svc.NextRun(schedule, "UTC", true)
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.st.Q.CreateCronJob(context.Background(), db.CreateCronJobParams{
		AppID: e.app.ID, Name: name, Schedule: schedule, Timezone: "UTC", Command: command,
		TimeoutSec: timeout, Concurrency: concurrency, Enabled: 1, NextRunAt: next, CreatedAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (e *env) runs(t *testing.T, jobID int64) []db.ListCronRunsRow {
	t.Helper()
	r, err := e.st.Q.ListCronRuns(context.Background(), db.ListCronRunsParams{JobID: jobID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestManualRunSucceedsWithLogs(t *testing.T) {
	e := setup(t, true)
	var spec docker.ContainerSpec
	e.dk.SetRun(func(s docker.ContainerSpec) (dockertest.RunResult, bool) {
		spec = s
		return dockertest.RunResult{Logs: []string{"sent 3 emails", "done"}, Duration: 10 * time.Millisecond}, true
	})
	j := e.job(t, "digest", "0 * * * *", `php artisan "emails:send" --force`, ConcurrencySkip, 60)
	run, err := e.svc.Start(context.Background(), j.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	r, _ := e.st.Q.GetCronRun(context.Background(), run.ID)
	if r.Status != StatusSucceeded || !r.ExitCode.Valid || r.ExitCode.Int64 != 0 || r.Logs != "sent 3 emails\ndone" || !r.FinishedAt.Valid {
		t.Fatalf("run = %+v", r)
	}
	if strings.Join(spec.Cmd, "|") != "php|artisan|emails:send|--force" || spec.Image != "sha256:worker:v1" ||
		spec.RestartPolicy != "no" || spec.Network != "docklet-p1" || spec.MemoryBytes != 128<<20 ||
		spec.Env[0] != "QUEUE=emails" || spec.Labels[LabelRun] == "" || spec.Labels["docklet.deployment"] != "" {
		t.Fatalf("spec = %+v", spec)
	}
	if e.dk.Count() != 0 {
		t.Fatal("run container not removed")
	}
}

func TestRunFailureAndTimeout(t *testing.T) {
	e := setup(t, true)
	e.dk.SetRun(func(s docker.ContainerSpec) (dockertest.RunResult, bool) {
		if s.Cmd[0] == "slow" {
			return dockertest.RunResult{Forever: true, Logs: []string{"working…"}}, true
		}
		return dockertest.RunResult{Exit: 3, Logs: []string{"db unreachable"}, Duration: 5 * time.Millisecond}, true
	})
	fail := e.job(t, "fail", "@daily", "fail", ConcurrencySkip, 60)
	slow := e.job(t, "slow", "@daily", "slow", ConcurrencySkip, 1)
	r1, _ := e.svc.Start(context.Background(), fail.ID, TriggerManual)
	r2, _ := e.svc.Start(context.Background(), slow.ID, TriggerManual)
	e.svc.Wait()
	a, _ := e.st.Q.GetCronRun(context.Background(), r1.ID)
	if a.Status != StatusFailed || a.ExitCode.Int64 != 3 || a.Error != "exited with code 3" || a.Logs != "db unreachable" {
		t.Fatalf("failed run = %+v", a)
	}
	b, _ := e.st.Q.GetCronRun(context.Background(), r2.ID)
	if b.Status != StatusTimeout || b.ExitCode.Valid || !strings.Contains(b.Error, "timed out after 1s") || b.Logs != "working…" {
		t.Fatalf("timed-out run = %+v", b)
	}
	if e.dk.Count() != 0 {
		t.Fatal("containers left behind")
	}
}

func TestConcurrencySkipAndAllow(t *testing.T) {
	e := setup(t, true)
	release := make(chan struct{})
	e.dk.SetRun(func(docker.ContainerSpec) (dockertest.RunResult, bool) {
		return dockertest.RunResult{Forever: true}, true
	})
	skip := e.job(t, "skip", "* * * * *", "sleep 100", ConcurrencySkip, 3600)
	allow := e.job(t, "allow", "* * * * *", "sleep 100", ConcurrencyAllow, 3600)
	ctx := context.Background()
	first, _ := e.svc.Start(ctx, skip.ID, TriggerSchedule)
	second, _ := e.svc.Start(ctx, skip.ID, TriggerSchedule)
	if second.Status != StatusSkipped || !strings.Contains(second.Error, "previous run is still in progress") {
		t.Fatalf("overlap = %+v", second)
	}
	a1, _ := e.svc.Start(ctx, allow.ID, TriggerSchedule)
	a2, _ := e.svc.Start(ctx, allow.ID, TriggerSchedule)
	if a1.Status != StatusRunning || a2.Status != StatusRunning {
		t.Fatalf("allow: %s %s", a1.Status, a2.Status)
	}
	close(release)
	for _, id := range []int64{first.ID, a1.ID, a2.ID} {
		e.svc.Cancel(id)
	}
	e.svc.Wait()
	if n := len(e.runs(t, skip.ID)); n != 2 {
		t.Fatalf("skip job has %d runs", n)
	}
}

func TestSchedulerFiresEveryMinute(t *testing.T) {
	e := setup(t, true)
	e.dk.SetRun(func(docker.ContainerSpec) (dockertest.RunResult, bool) {
		return dockertest.RunResult{Logs: []string{"tick"}}, true
	})
	j := e.job(t, "tick", "*/1 * * * *", "echo tick", ConcurrencySkip, 60)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		e.svc.fireDue(ctx) // nothing due mid-minute
		e.clk.add(time.Minute)
		e.svc.fireDue(ctx)
		e.svc.Wait()
	}
	runs := e.runs(t, j.ID)
	if len(runs) != 5 {
		t.Fatalf("runs = %d, want 5", len(runs))
	}
	for _, r := range runs {
		if r.Status != StatusSucceeded || r.Trigger != TriggerSchedule {
			t.Fatalf("run = %+v", r)
		}
	}

	// After downtime, missed runs are not caught up.
	e.clk.add(time.Hour)
	e.svc.recover(ctx)
	e.svc.fireDue(ctx)
	e.svc.Wait()
	if n := len(e.runs(t, j.ID)); n != 5 {
		t.Fatalf("missed runs were replayed: %d", n)
	}
	got, _ := e.st.Q.GetCronJob(ctx, j.ID)
	if got.NextRunAt != e.clk.now().Truncate(time.Minute).Add(time.Minute).Unix() {
		t.Fatalf("next run = %s", time.Unix(got.NextRunAt, 0).UTC())
	}
}

func TestRunWithoutDeploymentFails(t *testing.T) {
	e := setup(t, false)
	j := e.job(t, "x", "@daily", "echo hi", ConcurrencySkip, 60)
	run, _ := e.svc.Start(context.Background(), j.ID, TriggerManual)
	e.svc.Wait()
	r, _ := e.st.Q.GetCronRun(context.Background(), run.ID)
	if r.Status != StatusFailed || !strings.Contains(r.Error, "no successful deployment") {
		t.Fatalf("run = %+v", r)
	}
}

func TestRecoverAndRetention(t *testing.T) {
	e := setup(t, true)
	e.svc.KeepRuns = 3
	e.dk.SetRun(func(docker.ContainerSpec) (dockertest.RunResult, bool) { return dockertest.RunResult{}, true })
	j := e.job(t, "x", "@daily", "true", ConcurrencyAllow, 60)
	ctx := context.Background()
	stale, _ := e.st.Q.CreateCronRun(ctx, db.CreateCronRunParams{JobID: j.ID, Trigger: TriggerSchedule, Status: StatusRunning, StartedAt: 1})
	_, _ = e.dk.ContainerCreate(ctx, docker.ContainerSpec{Name: "leftover", Labels: map[string]string{"docklet.managed": "true", LabelRun: "1"}})
	e.svc.recover(ctx)
	r, _ := e.st.Q.GetCronRun(ctx, stale.ID)
	if r.Status != StatusFailed || !strings.Contains(r.Error, "Docklet restarted") || e.dk.Has("leftover") {
		t.Fatalf("recover: %+v leftover=%v", r, e.dk.Has("leftover"))
	}
	for i := 0; i < 5; i++ {
		_, _ = e.svc.Start(ctx, j.ID, TriggerManual)
		e.svc.Wait()
	}
	if n := len(e.runs(t, j.ID)); n != 3 {
		t.Fatalf("kept %d runs, want 3", n)
	}
}
