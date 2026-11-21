package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/cron"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/docker/dockertest"
)

func TestWorkersAndCronAPI(t *testing.T) {
	dk := dockertest.New()
	dk.AddImage("hello:v1", 8080)
	dk.SetRun(func(s docker.ContainerSpec) (dockertest.RunResult, bool) {
		switch {
		case len(s.Cmd) > 0 && s.Cmd[0] == "sleep":
			return dockertest.RunResult{Forever: true}, true
		case len(s.Cmd) > 0 && s.Cmd[0] == "report":
			return dockertest.RunResult{Logs: []string{"report ok"}, Duration: 5 * time.Millisecond}, true
		}
		return dockertest.RunResult{}, false
	})
	env := startPanelServer(t, filepath.Join(t.TempDir(), "d.db"), true,
		WithDocker(dk),
		WithAppsTuning(func(s *apps.Service) {
			s.HealthTimeout, s.HealthInterval, s.DrainDelay, s.WorkerSettle = time.Second, 10*time.Millisecond, 0, 20*time.Millisecond
			s.HealthCheck = func(context.Context, string, int) error { return nil }
		}),
		WithCronTuning(func(s *cron.Service) { s.Tick = 20 * time.Millisecond }))
	c := env.client(t)
	_, p := c.do("POST", "/api/projects", map[string]string{"name": "shop"})
	pid := p["id"]

	// Workers: no domains, command override, deployed without a port.
	if code, _ := c.do("POST", fmt.Sprintf("/api/projects/%v/apps", pid), map[string]any{
		"name": "jobs", "kind": "worker", "image": "hello:v1", "domains": []string{"jobs.example.com"},
	}); code != 400 {
		t.Fatalf("worker with domain: %d", code)
	}
	if code, _ := c.do("POST", fmt.Sprintf("/api/projects/%v/apps", pid), map[string]any{
		"name": "jobs", "kind": "worker", "image": "hello:v1", "command": `node "worker.js`,
	}); code != 400 {
		t.Fatalf("bad command accepted: %d", code)
	}
	code, w := c.do("POST", fmt.Sprintf("/api/projects/%v/apps", pid), map[string]any{
		"name": "jobs", "kind": "worker", "image": "hello:v1", "command": "node worker.js",
	})
	if code != http.StatusCreated {
		t.Fatalf("create worker: %d %v", code, w)
	}
	var app map[string]any
	waitFor(t, "worker running", func() bool {
		_, app = c.do("GET", fmt.Sprintf("/api/apps/%v", w["id"]), nil)
		return app["runtimeStatus"] == "running"
	})
	if app["kind"] != "worker" || app["command"] != "node worker.js" || app["restartPolicy"] != "unless-stopped" {
		t.Fatalf("worker detail = %v", app)
	}
	if code, _ := c.do("PUT", fmt.Sprintf("/api/apps/%v/domains", w["id"]), map[string]any{"domains": []string{"x.example.com"}}); code != 400 {
		t.Fatalf("domain on worker: %d", code)
	}

	// Cron jobs.
	base := fmt.Sprintf("/api/apps/%v/cron", w["id"])
	for _, bad := range []map[string]any{
		{"name": "x", "schedule": "* * *", "command": "report"},
		{"name": "x", "schedule": "* * * * *", "command": ""},
		{"name": "x", "schedule": "* * * * *", "command": "report", "timezone": "Nowhere/City"},
		{"name": "x", "schedule": "* * * * *", "command": "report", "concurrency": "queue"},
		{"name": "Bad Name", "schedule": "* * * * *", "command": "report"},
	} {
		if code, _ := c.do("POST", base, bad); code != 400 {
			t.Errorf("accepted %v (%d)", bad, code)
		}
	}
	code, job := c.do("POST", base, map[string]any{"name": "report", "schedule": "*/1 * * * *", "command": "report --daily", "timezone": "Asia/Dhaka"})
	if code != http.StatusCreated || job["nextRunAt"] == nil || job["concurrency"] != "skip" || job["timeoutSec"] != float64(3600) {
		t.Fatalf("create cron: %d %v", code, job)
	}
	if code, _ := c.do("POST", base, map[string]any{"name": "report", "schedule": "@daily", "command": "x"}); code != http.StatusConflict {
		t.Fatalf("duplicate name: %d", code)
	}
	_, prev := c.do("POST", "/api/cron/preview", map[string]any{"schedule": "0 3 * * *", "timezone": "Asia/Dhaka"})
	if n, _ := prev["next"].([]any); len(n) != 3 {
		t.Fatalf("preview = %v", prev)
	}

	// Run now → output captured.
	code, rn := c.do("POST", fmt.Sprintf("/api/cron/%v/run", job["id"]), nil)
	if code != http.StatusAccepted {
		t.Fatalf("run: %d %v", code, rn)
	}
	var run map[string]any
	waitFor(t, "run finished", func() bool {
		_, body := c.do("GET", fmt.Sprintf("/api/cron-runs/%v", rn["runId"]), nil)
		run, _ = body["run"].(map[string]any)
		return run != nil && run["status"] != "running"
	})
	if run["status"] != "succeeded" || run["logs"] != "report ok" || run["exitCode"] != float64(0) || run["trigger"] != "manual" {
		t.Fatalf("run = %v", run)
	}

	// The schedule fires on its own: move the job's next run into the past.
	if _, err := env.st.DB.Exec("UPDATE cron_jobs SET next_run_at = ? WHERE id = ?", time.Now().Unix()-1, job["id"]); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "scheduled run", func() bool {
		_, body := c.do("GET", fmt.Sprintf("/api/cron/%v/runs", job["id"]), nil)
		runs, _ := body["runs"].([]any)
		for _, r := range runs {
			if r.(map[string]any)["trigger"] == "schedule" && r.(map[string]any)["status"] == "succeeded" {
				return true
			}
		}
		return false
	})

	// Overlap with concurrency=skip, then cancel.
	code, slow := c.do("POST", base, map[string]any{"name": "slow", "schedule": "@hourly", "command": "sleep 600", "enabled": false})
	if code != http.StatusCreated || slow["nextRunAt"] != nil {
		t.Fatalf("disabled job: %d %v", code, slow)
	}
	_, r1 := c.do("POST", fmt.Sprintf("/api/cron/%v/run", slow["id"]), nil)
	_, r2 := c.do("POST", fmt.Sprintf("/api/cron/%v/run", slow["id"]), nil)
	if r1["status"] != "running" || r2["status"] != "skipped" {
		t.Fatalf("overlap: %v %v", r1, r2)
	}
	if code, _ := c.do("POST", fmt.Sprintf("/api/cron-runs/%v/cancel", r1["runId"]), nil); code != 200 {
		t.Fatalf("cancel: %d", code)
	}
	waitFor(t, "cancelled", func() bool {
		_, body := c.do("GET", fmt.Sprintf("/api/cron-runs/%v", r1["runId"]), nil)
		r, _ := body["run"].(map[string]any)
		return r != nil && r["status"] == "failed"
	})

	// Update and delete.
	if code, j := c.do("PATCH", fmt.Sprintf("/api/cron/%v", job["id"]), map[string]any{"enabled": false, "concurrency": "allow"}); code != 200 || j["enabled"] != false || j["nextRunAt"] != nil {
		t.Fatalf("patch: %d %v", code, j)
	}
	_, list := c.do("GET", base, nil)
	if jobs := list["jobs"].([]any); len(jobs) != 2 || jobs[0].(map[string]any)["lastRun"] == nil {
		t.Fatalf("list = %v", list)
	}
	if code, _ := c.do("DELETE", fmt.Sprintf("/api/cron/%v", slow["id"]), nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	acts := strings.Join(actionsOf(t, env.st), ",")
	for _, want := range []string{"cron.created", "cron.run_requested", "cron.run_cancelled", "cron.updated", "cron.deleted"} {
		if !strings.Contains(acts, want) {
			t.Errorf("audit missing %s", want)
		}
	}
	if strings.Count(acts, "cron.run_requested") != 3 {
		t.Errorf("scheduled runs should not be audited: %s", acts)
	}
}
