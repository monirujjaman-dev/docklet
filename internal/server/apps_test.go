package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/docker/dockertest"
)

func startAppsServer(t *testing.T) (*panelEnv, *client, *dockertest.Fake) {
	t.Helper()
	dk := dockertest.New()
	dk.AddImage("hello:v1", 8080)
	dk.AddImage("hello:v2", 8080)
	dk.AddImage("crash:v1", 8080)
	env := startPanelServer(t, filepath.Join(t.TempDir(), "d.db"), true,
		WithDocker(dk),
		WithAppsTuning(func(s *apps.Service) {
			s.HealthTimeout, s.HealthInterval, s.DrainDelay = time.Second, 10*time.Millisecond, 0
			s.HealthCheck = func(context.Context, string, int) error { return nil }
		}))
	return env, env.client(t), dk
}

func waitDeployment(t *testing.T, c *client, id any) map[string]any {
	t.Helper()
	var body map[string]any
	waitFor(t, "deployment to finish", func() bool {
		_, body = c.do("GET", fmt.Sprintf("/api/deployments/%v", id), nil)
		st := body["status"]
		return st == "succeeded" || st == "failed"
	})
	return body
}

func TestAppLifecycleAPI(t *testing.T) {
	env, c, dk := startAppsServer(t)

	code, body := c.do("POST", "/api/projects", map[string]string{"name": "web"})
	if code != http.StatusCreated {
		t.Fatalf("create project: %d %v", code, body)
	}
	pid := body["id"]
	if code, _ := c.do("POST", "/api/projects", map[string]string{"name": "web"}); code != http.StatusConflict {
		t.Fatalf("duplicate project: %d", code)
	}

	code, body = c.do("POST", fmt.Sprintf("/api/projects/%v/apps", pid), map[string]any{
		"name": "site", "image": "hello:v1", "domains": []string{"Site.Example.com"},
		"env": map[string]string{"SECRET": "hunter2", "MODE": "prod"}, "memoryMb": 128,
	})
	if code != http.StatusCreated {
		t.Fatalf("create app: %d %v", code, body)
	}
	appID, depID := body["id"], body["deploymentId"]
	dep := waitDeployment(t, c, depID)
	if dep["status"] != "succeeded" {
		t.Fatalf("deploy failed: %v", dep)
	}

	_, app := c.do("GET", fmt.Sprintf("/api/apps/%v", appID), nil)
	if app["runtimeStatus"] != "running" || fmt.Sprint(app["domains"]) != "[site.example.com]" {
		t.Fatalf("app = %v", app)
	}
	envList := fmt.Sprint(app["env"])
	if strings.Contains(envList, "hunter2") || !strings.Contains(envList, "SECRET") {
		t.Fatalf("env listing leaks or misses keys: %s", envList)
	}

	// Env values are encrypted at rest and never in the audit log.
	var raw []byte
	_ = env.st.DB.QueryRow("SELECT value_enc FROM env_vars WHERE key = 'SECRET'").Scan(&raw)
	if len(raw) == 0 || bytes.Contains(raw, []byte("hunter2")) {
		t.Fatal("env value not encrypted")
	}
	var leaks int
	_ = env.st.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE details LIKE '%hunter2%'").Scan(&leaks)
	if leaks != 0 {
		t.Fatal("secret value in audit log")
	}

	// Caddy got the app route pointing at the container.
	waitFor(t, "app route in caddy", func() bool {
		return strings.Contains(env.caddy.json(), "site.example.com") && strings.Contains(env.caddy.json(), ":8080")
	})

	// Env change + redeploy; a failing image keeps the previous version.
	if code, _ := c.do("PUT", fmt.Sprintf("/api/apps/%v/env", appID), map[string]any{"env": map[string]any{"MODE": nil, "NEW": "1"}}); code != 200 {
		t.Fatalf("env update: %d", code)
	}
	if code, _ := c.do("PATCH", fmt.Sprintf("/api/apps/%v", appID), map[string]any{"image": "crash:v1"}); code != 200 {
		t.Fatalf("patch: %d", code)
	}
	_, body = c.do("POST", fmt.Sprintf("/api/apps/%v/deploy", appID), nil)
	if dep := waitDeployment(t, c, body["deploymentId"]); dep["status"] != "failed" {
		t.Fatalf("crash deploy should fail: %v", dep)
	}
	if run := dk.Running(); len(run) != 1 || run[0].Spec.Image != "hello:v1" {
		t.Fatal("previous version not kept after failed deploy")
	}

	c.do("PATCH", fmt.Sprintf("/api/apps/%v", appID), map[string]any{"image": "hello:v2"})
	_, body = c.do("POST", fmt.Sprintf("/api/apps/%v/deploy", appID), nil)
	waitDeployment(t, c, body["deploymentId"])
	run := dk.Running()
	if len(run) != 1 || run[0].Spec.Image != "hello:v2" {
		t.Fatalf("redeploy: %+v", run)
	}
	envStr := strings.Join(run[0].Spec.Env, ",")
	if !strings.Contains(envStr, "NEW=1") || strings.Contains(envStr, "MODE=") || !strings.Contains(envStr, "SECRET=hunter2") {
		t.Fatalf("env not applied: %s", envStr)
	}

	// Stop removes the route; start brings it back.
	if code, _ := c.do("POST", fmt.Sprintf("/api/apps/%v/stop", appID), nil); code != 200 {
		t.Fatalf("stop: %d", code)
	}
	waitFor(t, "route removed on stop", func() bool { return !strings.Contains(env.caddy.json(), "site.example.com") })
	if code, _ := c.do("POST", fmt.Sprintf("/api/apps/%v/start", appID), nil); code != 200 {
		t.Fatalf("start: %d", code)
	}

	// Project with apps cannot be deleted; app delete removes containers.
	if code, _ := c.do("DELETE", fmt.Sprintf("/api/projects/%v", pid), nil); code != http.StatusConflict {
		t.Fatalf("delete non-empty project: %d", code)
	}
	if code, _ := c.do("DELETE", fmt.Sprintf("/api/apps/%v", appID), nil); code != 200 {
		t.Fatalf("delete app: %d", code)
	}
	if dk.Count() != 0 {
		t.Fatalf("%d containers left", dk.Count())
	}
	if code, _ := c.do("DELETE", fmt.Sprintf("/api/projects/%v", pid), nil); code != 200 {
		t.Fatalf("delete project: %d", code)
	}

	acts := strings.Join(actionsOf(t, env.st), ",")
	for _, want := range []string{"project.created", "app.created", "app.deploy_requested", "app.deploy_succeeded",
		"app.deploy_failed", "app.env_updated", "app.updated", "app.stopped", "app.started", "app.deleted", "project.deleted"} {
		if !strings.Contains(acts, want) {
			t.Errorf("audit missing %s", want)
		}
	}
}

func TestAppValidationAndDomainConflicts(t *testing.T) {
	_, c, _ := startAppsServer(t)
	_, body := c.do("POST", "/api/projects", map[string]string{"name": "p"})
	pid := body["id"]
	url := fmt.Sprintf("/api/projects/%v/apps", pid)
	bad := []map[string]any{
		{"name": "Bad_Name", "image": "hello:v1"},
		{"name": "ok"},
		{"name": "ok", "image": "has space"},
		{"name": "ok", "image": "hello:v1", "port": 70000},
		{"name": "ok", "image": "hello:v1", "memoryMb": 2},
		{"name": "ok", "image": "hello:v1", "restartPolicy": "sometimes"},
		{"name": "ok", "image": "hello:v1", "env": map[string]string{"1BAD": "x"}},
		{"name": "ok", "image": "hello:v1", "domains": []string{"not a domain"}},
	}
	for _, b := range bad {
		if code, _ := c.do("POST", url, b); code != 400 {
			t.Errorf("accepted %v (%d)", b, code)
		}
	}
	if code, _ := c.do("POST", url, map[string]any{"name": "a", "image": "hello:v1", "domains": []string{"x.example.com"}, "deploy": false}); code != 201 {
		t.Fatal("create a")
	}
	if code, _ := c.do("POST", url, map[string]any{"name": "b", "image": "hello:v1", "domains": []string{"X.example.com"}, "deploy": false}); code != http.StatusConflict {
		t.Fatalf("duplicate domain across apps: %d", code)
	}
	c.do("PUT", "/api/settings/panel", map[string]string{"domain": "panel.example.com"})
	if code, _ := c.do("POST", url, map[string]any{"name": "c", "image": "hello:v1", "domains": []string{"panel.example.com"}, "deploy": false}); code != http.StatusConflict {
		t.Fatalf("panel domain reuse: %d", code)
	}
	if code, _ := c.do("POST", "/api/projects/999/apps", map[string]any{"name": "z", "image": "hello:v1"}); code != 404 {
		t.Fatalf("missing project: %d", code)
	}
}

func TestDockerDisabled(t *testing.T) {
	env := startPanelServer(t, filepath.Join(t.TempDir(), "d.db"), true)
	c := env.client(t)
	_, body := c.do("POST", "/api/projects", map[string]string{"name": "p"})
	code, resp := c.do("POST", fmt.Sprintf("/api/projects/%v/apps", body["id"]), map[string]any{"name": "a", "image": "x"})
	if code != http.StatusServiceUnavailable || !strings.Contains(fmt.Sprint(resp["error"]), "docker-socket") {
		t.Fatalf("got %d %v", code, resp)
	}
}
