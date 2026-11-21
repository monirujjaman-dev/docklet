package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/databases"
	"github.com/monirujjaman-dev/docklet/internal/docker/dockertest"
)

func startDBServer(t *testing.T) (*panelEnv, *client, *dockertest.Fake) {
	t.Helper()
	dk := dockertest.New()
	for _, e := range databases.Engines {
		for _, v := range e.Versions {
			dk.AddImage(e.Image(v), e.Port)
		}
	}
	dk.AddImage("hello:v1", 8080)
	// Minimal tool emulation: probes succeed, pg_dump emits a header + data.
	dk.SetExec(func(f *dockertest.Fake, id string, cmd, _ []string, stdout io.Writer) (int, string) {
		switch cmd[0] {
		case "pg_dump":
			b, _ := f.ReadFile(id, "/var/lib/postgresql/data/data")
			_, _ = stdout.Write(append([]byte("PGDMP"), b...))
		case "pg_restore":
			b, _ := f.ReadFile(id, cmd[len(cmd)-1])
			_ = f.WriteFile(id, "/var/lib/postgresql/data/data", []byte(strings.TrimPrefix(string(b), "PGDMP")))
		}
		return 0, ""
	})
	env := startPanelServer(t, filepath.Join(t.TempDir(), "d.db"), true,
		WithDocker(dk),
		WithAppsTuning(func(s *apps.Service) {
			s.HealthTimeout, s.HealthInterval, s.DrainDelay = time.Second, 10*time.Millisecond, 0
			s.HealthCheck = func(context.Context, string, int) error { return nil }
		}),
		WithDatabasesTuning(func(s *databases.Service) {
			s.BackupDir = t.TempDir()
			s.ReadyTimeout, s.ReadyInterval = time.Second, 10*time.Millisecond
		}))
	return env, env.client(t), dk
}

func waitDB(t *testing.T, c *client, id any, cond func(map[string]any) bool) map[string]any {
	t.Helper()
	var body map[string]any
	waitFor(t, "database state", func() bool {
		_, body = c.do("GET", fmt.Sprintf("/api/databases/%v", id), nil)
		return cond(body)
	})
	return body
}

func TestDatabaseAPI(t *testing.T) {
	env, c, dk := startDBServer(t)
	_, p := c.do("POST", "/api/projects", map[string]string{"name": "web"})
	pid := p["id"]

	for _, bad := range []map[string]any{
		{"name": "db", "engine": "oracle"},
		{"name": "db", "engine": "postgres", "version": "9"},
		{"name": "Bad", "engine": "postgres"},
		{"name": "db", "engine": "redis", "memoryMb": 10},
	} {
		if code, _ := c.do("POST", fmt.Sprintf("/api/projects/%v/databases", pid), bad); code != 400 {
			t.Errorf("accepted %v (%d)", bad, code)
		}
	}
	code, body := c.do("POST", fmt.Sprintf("/api/projects/%v/databases", pid), map[string]any{"name": "main", "engine": "postgres"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	id := body["id"]
	d := waitDB(t, c, id, func(b map[string]any) bool { return b["runtimeStatus"] == "running" })
	if d["host"] != "main" || d["port"] != float64(5432) || d["dbName"] != "main" || d["publicPort"] != nil {
		t.Fatalf("detail = %v", d)
	}
	if run := dk.Running(); len(run) != 1 || len(run[0].Spec.Publish) != 0 {
		t.Fatal("database should not be published by default")
	}

	// Names are unique across apps and databases in a project.
	if code, _ := c.do("POST", fmt.Sprintf("/api/projects/%v/apps", pid), map[string]any{"name": "main", "image": "hello:v1", "deploy": false}); code != http.StatusConflict {
		t.Fatalf("app with database's name: %d", code)
	}
	if code, _ := c.do("DELETE", fmt.Sprintf("/api/projects/%v", pid), nil); code != http.StatusConflict {
		t.Fatalf("project with database deleted: %d", code)
	}

	// Credentials reveal is audited.
	_, cred := c.do("GET", fmt.Sprintf("/api/databases/%v/credentials", id), nil)
	pw, _ := cred["password"].(string)
	if len(pw) < 20 || !strings.HasPrefix(fmt.Sprint(cred["url"]), "postgres://main:"+pw+"@main:5432/main") {
		t.Fatalf("credentials = %v", cred)
	}

	// Link to an app writes DATABASE_URL into its environment.
	_, a := c.do("POST", fmt.Sprintf("/api/projects/%v/apps", pid), map[string]any{"name": "api", "image": "hello:v1", "deploy": false})
	if code, d := c.do("POST", fmt.Sprintf("/api/databases/%v/links", id), map[string]any{"appId": a["id"]}); code != 200 || len(d["links"].([]any)) != 1 {
		t.Fatalf("link: %d %v", code, d)
	}
	_, app := c.do("GET", fmt.Sprintf("/api/apps/%v", a["id"]), nil)
	if !strings.Contains(fmt.Sprint(app["env"]), "DATABASE_URL") {
		t.Fatalf("app env = %v", app["env"])
	}

	// Public port toggle recreates the container with a binding.
	if code, _ := c.do("PUT", fmt.Sprintf("/api/databases/%v/public", id), map[string]any{"port": 80}); code != 400 {
		t.Fatalf("privileged port accepted: %d", code)
	}
	if code, d := c.do("PUT", fmt.Sprintf("/api/databases/%v/public", id), map[string]any{"port": 15432}); code != 200 || d["publicPort"] != float64(15432) {
		t.Fatalf("publish: %d %v", code, d)
	}
	if run := dk.Running(); run[0].Spec.Publish[5432] != 15432 {
		t.Fatal("port not published")
	}
	c.do("PUT", fmt.Sprintf("/api/databases/%v/public", id), map[string]any{"port": 0})

	// Backup -> download -> drop -> restore.
	waitDB(t, c, id, func(b map[string]any) bool { return b["runtimeStatus"] == "running" })
	var cid string
	for _, ct := range dk.Running() {
		cid = ct.Spec.Name
	}
	_ = dk.WriteFile(cid, "/var/lib/postgresql/data/data", []byte("orders: 42"))
	if code, _ := c.do("POST", fmt.Sprintf("/api/databases/%v/backups", id), nil); code != http.StatusAccepted {
		t.Fatalf("backup: %d", code)
	}
	d = waitDB(t, c, id, func(b map[string]any) bool {
		bs, _ := b["backups"].([]any)
		return len(bs) > 0 && bs[0].(map[string]any)["status"] == "succeeded"
	})
	backup := d["backups"].([]any)[0].(map[string]any)
	req, _ := http.NewRequest("GET", c.base+fmt.Sprintf("/api/backups/%v/download", backup["id"]), nil)
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	closeBody(t, res)
	if string(got) != "PGDMPorders: 42" || !strings.Contains(res.Header.Get("Content-Disposition"), ".dump") {
		t.Fatalf("download = %q %v", got, res.Header)
	}

	_ = dk.WriteFile(cid, "/var/lib/postgresql/data/data", []byte(""))
	if code, _ := c.do("POST", fmt.Sprintf("/api/databases/%v/restore", id), map[string]any{"backupId": backup["id"]}); code != http.StatusAccepted {
		t.Fatalf("restore: %d", code)
	}
	waitFor(t, "restored data", func() bool {
		b, _ := dk.ReadFile(cid, "/var/lib/postgresql/data/data")
		return string(b) == "orders: 42"
	})
	d = waitDB(t, c, id, func(b map[string]any) bool { return b["runtimeMessage"] == nil })
	triggers := ""
	for _, b := range d["backups"].([]any) {
		triggers += b.(map[string]any)["trigger"].(string) + ","
	}
	if !strings.Contains(triggers, "pre-restore") {
		t.Fatalf("no pre-restore backup: %s", triggers)
	}

	// Delete with data removes the volume and the linked env var.
	if code, _ := c.do("DELETE", fmt.Sprintf("/api/databases/%v?deleteData=1", id), nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if dk.HasVolume(databases.VolumeName(1)) {
		t.Fatal("volume kept despite deleteData")
	}
	_, app = c.do("GET", fmt.Sprintf("/api/apps/%v", a["id"]), nil)
	if strings.Contains(fmt.Sprint(app["env"]), "DATABASE_URL") {
		t.Fatal("linked env var survived database deletion")
	}

	acts := strings.Join(actionsOf(t, env.st), ",")
	for _, want := range []string{"database.created", "database.ready", "database.credentials_viewed", "database.linked",
		"database.public_port_changed", "database.backup_succeeded", "database.restore_succeeded", "database.deleted"} {
		if !strings.Contains(acts, want) {
			t.Errorf("audit missing %s", want)
		}
	}
	var leaks int
	_ = env.st.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE details LIKE ?", "%"+pw+"%").Scan(&leaks)
	if leaks != 0 {
		t.Fatal("password in audit log details")
	}
}
