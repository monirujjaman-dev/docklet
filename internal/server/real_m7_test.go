package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/docker"
)

func waitLong(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// TestRealWorkersAndCron checks the M7 acceptance criteria against a live
// Docker daemon with alpine:3.20 (DOCKLET_TEST_DOCKER=1; takes ~3 minutes):
// a crashing worker is restarted by its restart policy, a */1 cron job
// produces a run with logs every minute, and an overlapping run is skipped.
func TestRealWorkersAndCron(t *testing.T) {
	if os.Getenv("DOCKLET_TEST_DOCKER") == "" {
		t.Skip("set DOCKLET_TEST_DOCKER=1 to run against a real Docker daemon")
	}
	env := startPanelServer(t, filepath.Join(t.TempDir(), "d.db"), true,
		WithDocker(docker.New("/var/run/docker.sock")),
		WithAppsTuning(func(s *apps.Service) { s.ReconcileInterval = time.Second; s.WorkerSettle = 2 * time.Second }))
	c := env.client(t)
	_, p := c.do("POST", "/api/projects", map[string]string{"name": "m7e2e"})
	pid := p["id"]
	t.Cleanup(func() {
		out, _ := exec.Command("sh", "-c", "docker ps -aq --filter label=docklet.project="+fmt.Sprint(pid)+" | xargs -r docker rm -f; docker network rm docklet-p"+fmt.Sprint(pid)).CombinedOutput() //nolint:gosec // test cleanup
		t.Logf("cleanup: %s", strings.TrimSpace(string(out)))
	})

	// 1. A worker that crashes every 6 s is restarted by Docker.
	code, w := c.do("POST", fmt.Sprintf("/api/projects/%v/apps", pid), map[string]any{
		"name": "crashy", "kind": "worker", "image": "alpine:3.20",
		"command": `sh -c "echo worker up; sleep 6; echo crashing; exit 1"`,
	})
	if code != 201 {
		t.Fatalf("create worker: %d %v", code, w)
	}
	var app map[string]any
	waitLong(t, "worker deployed", 60*time.Second, func() bool {
		_, app = c.do("GET", fmt.Sprintf("/api/apps/%v", w["id"]), nil)
		deps, _ := app["deployments"].([]any)
		return len(deps) > 0 && deps[0].(map[string]any)["status"] == "succeeded"
	})
	start := time.Now()
	waitLong(t, "restart after crash", 40*time.Second, func() bool {
		_, app = c.do("GET", fmt.Sprintf("/api/apps/%v", w["id"]), nil)
		return strings.Contains(fmt.Sprint(app["runtimeMessage"]), "Restarted")
	})
	t.Logf("worker: %s %q after %s (restart policy %v)", app["runtimeStatus"], app["runtimeMessage"], time.Since(start).Round(time.Second), app["restartPolicy"])
	if app["kind"] != "worker" || app["restartPolicy"] != "unless-stopped" {
		t.Fatalf("worker = %v", app)
	}

	// 2. A web app to host the cron jobs (cron runs use its image).
	code, a := c.do("POST", fmt.Sprintf("/api/projects/%v/apps", pid), map[string]any{
		"name": "tasks", "kind": "worker", "image": "alpine:3.20", "command": "sleep 3600",
	})
	if code != 201 {
		t.Fatalf("create app: %d %v", code, a)
	}
	waitLong(t, "app deployed", 60*time.Second, func() bool {
		_, d := c.do("GET", fmt.Sprintf("/api/apps/%v", a["id"]), nil)
		deps, _ := d["deployments"].([]any)
		return len(deps) > 0 && deps[0].(map[string]any)["status"] == "succeeded"
	})
	_, _ = c.do("PUT", fmt.Sprintf("/api/apps/%v/env", a["id"]), map[string]any{"env": map[string]string{"GREETING": "hello from env"}})
	base := fmt.Sprintf("/api/apps/%v/cron", a["id"])
	_, tick := c.do("POST", base, map[string]any{"name": "tick", "schedule": "*/1 * * * *", "command": `sh -c 'echo "$GREETING"; date -u +%H:%M:%S'`})
	_, slow := c.do("POST", base, map[string]any{"name": "slow", "schedule": "* * * * *", "command": "sleep 75", "concurrency": "skip"})
	if tick["id"] == nil || slow["id"] == nil {
		t.Fatalf("create jobs: %v %v", tick, slow)
	}

	runsOf := func(id any) []map[string]any {
		_, body := c.do("GET", fmt.Sprintf("/api/cron/%v/runs", id), nil)
		var out []map[string]any
		for _, r := range body["runs"].([]any) {
			out = append(out, r.(map[string]any))
		}
		return out
	}
	// Two minute boundaries plus the run time.
	waitLong(t, "two scheduled tick runs and a skipped slow run", 150*time.Second, func() bool {
		ok := 0
		for _, r := range runsOf(tick["id"]) {
			if r["status"] == "succeeded" && r["trigger"] == "schedule" {
				ok++
			}
		}
		skipped := false
		for _, r := range runsOf(slow["id"]) {
			skipped = skipped || r["status"] == "skipped"
		}
		return ok >= 2 && skipped
	})
	ticks := runsOf(tick["id"])
	for _, r := range ticks {
		_, body := c.do("GET", fmt.Sprintf("/api/cron-runs/%v", r["id"]), nil)
		run := body["run"].(map[string]any)
		t.Logf("tick run #%v %s at %s in %vms: %q", run["id"], run["status"],
			time.Unix(int64(run["startedAt"].(float64)), 0).UTC().Format("15:04:05"), run["durationMs"], run["logs"])
		if run["status"] == "succeeded" && !strings.HasPrefix(fmt.Sprint(run["logs"]), "hello from env\n") {
			t.Fatalf("logs = %q", run["logs"])
		}
	}
	for _, r := range runsOf(slow["id"]) {
		t.Logf("slow run #%v %s %v", r["id"], r["status"], r["error"])
	}
	if len(ticks) >= 2 {
		gap := ticks[0]["startedAt"].(float64) - ticks[1]["startedAt"].(float64)
		if gap < 55 || gap > 65 {
			t.Fatalf("runs are %vs apart, want ~60s", gap)
		}
	}
	// No cron containers are left behind once runs finish (slow may still run).
	out, _ := exec.Command("docker", "ps", "-a", "--filter", "label=docklet.cron.job="+fmt.Sprint(tick["id"]), "-q").Output() //nolint:gosec // test-only
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("tick containers left: %s", out)
	}
}
