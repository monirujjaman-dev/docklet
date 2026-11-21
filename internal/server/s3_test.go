package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/databases"
	"github.com/monirujjaman-dev/docklet/internal/docker/dockertest"
	"github.com/monirujjaman-dev/docklet/internal/s3"
	"github.com/monirujjaman-dev/docklet/internal/s3/s3test"
	"github.com/monirujjaman-dev/docklet/internal/selfbackup"
)

func TestS3DestinationsBackupsAndUsersAPI(t *testing.T) {
	fake := s3test.New("backups")
	defer fake.Close()

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "master.key"), bytes.Repeat([]byte{5}, 32), 0o600)
	backupDir := filepath.Join(dir, "backups")
	dk := dockertest.New()
	for _, e := range databases.Engines {
		for _, v := range e.Versions {
			dk.AddImage(e.Image(v), e.Port)
		}
	}
	var execMu sync.Mutex
	var execList []string
	execs := func() []string { execMu.Lock(); defer execMu.Unlock(); return append([]string(nil), execList...) }
	dk.SetExec(func(f *dockertest.Fake, id string, cmd, _ []string, stdout io.Writer) (int, string) {
		execMu.Lock()
		execList = append(execList, strings.Join(cmd, " "))
		execMu.Unlock()
		if cmd[0] == "pg_dump" {
			b, _ := f.ReadFile(id, "/var/lib/postgresql/data/data")
			_, _ = stdout.Write(append([]byte("PGDMP"), b...))
		}
		return 0, ""
	})
	env := startPanelServer(t, filepath.Join(dir, "docklet.db"), true,
		func(s *Server) {
			s.cfg.DataDir, s.cfg.BackupDir, s.sbEvery = dir, backupDir, 20*time.Millisecond
			s.dests.Configure = func(c *s3.Client) { c.RetryWait = time.Millisecond }
		},
		WithDocker(dk),
		WithAppsTuning(func(s *apps.Service) {
			s.HealthTimeout, s.HealthInterval, s.DrainDelay = time.Second, 10*time.Millisecond, 0
			s.HealthCheck = func(context.Context, string, int) error { return nil }
		}),
		WithDatabasesTuning(func(s *databases.Service) {
			s.BackupDir = backupDir
			s.ReadyTimeout, s.ReadyInterval = time.Second, 10*time.Millisecond
		}))
	c := env.client(t)

	// Destinations: validation, test before save, save, test, usage.
	good := map[string]any{"name": "r2", "endpoint": fake.URL, "bucket": "backups", "prefix": "/prod/", "accessKey": "AK", "secretKey": "s3cret-key", "pathStyle": true}
	for _, bad := range []map[string]any{
		{"name": "x", "endpoint": "ftp://x", "bucket": "backups", "accessKey": "a", "secretKey": "b"},
		{"name": "x", "endpoint": fake.URL, "bucket": "No_Caps", "accessKey": "a", "secretKey": "b"},
		{"name": "", "endpoint": fake.URL, "bucket": "backups", "accessKey": "a", "secretKey": "b"},
	} {
		if code, _ := c.do("POST", "/api/destinations", bad); code != 400 {
			t.Errorf("accepted %v (%d)", bad, code)
		}
	}
	missing := map[string]any{"name": "x", "endpoint": fake.URL, "bucket": "nope", "accessKey": "a", "secretKey": "b", "pathStyle": true}
	if _, res := c.do("POST", "/api/destinations/test", missing); res["ok"] != false || !strings.Contains(fmt.Sprint(res["error"]), "does not exist") {
		t.Fatalf("test missing bucket = %v", res)
	}
	code, dest := c.do("POST", "/api/destinations", good)
	if code != 201 || dest["lastTestError"] != nil || dest["lastTestAt"] == nil || dest["prefix"] != "prod" {
		t.Fatalf("create destination: %d %v", code, dest)
	}
	var secretInDB int
	_ = env.st.DB.QueryRow("SELECT COUNT(*) FROM s3_destinations WHERE CAST(secret_enc AS TEXT) LIKE '%s3cret-key%'").Scan(&secretInDB)
	if secretInDB != 0 {
		t.Fatal("secret key stored in plaintext")
	}
	if _, list := c.do("GET", "/api/destinations", nil); strings.Contains(fmt.Sprint(list), "s3cret") {
		t.Fatal("secret returned by the API")
	}
	upd := map[string]any{"name": "r2", "endpoint": fake.URL, "bucket": "backups", "prefix": "prod", "accessKey": "AK2", "secretKey": "", "pathStyle": true}
	if code, d := c.do("PUT", fmt.Sprintf("/api/destinations/%v", dest["id"]), upd); code != 200 || d["accessKey"] != "AK2" || d["lastTestError"] != nil {
		t.Fatalf("update keeping secret: %d %v", code, d)
	}

	// A database backs up to it.
	_, p := c.do("POST", "/api/projects", map[string]string{"name": "shop"})
	_, d := c.do("POST", fmt.Sprintf("/api/projects/%v/databases", p["id"]), map[string]any{"name": "main", "engine": "postgres"})
	waitDB(t, c, d["id"], func(b map[string]any) bool { return b["runtimeStatus"] == "running" })
	if code, _ := c.do("PUT", fmt.Sprintf("/api/databases/%v/backup-destination", d["id"]), map[string]any{"destinationId": dest["id"]}); code != 200 {
		t.Fatalf("set destination: %d", code)
	}
	if code, _ := c.do("DELETE", fmt.Sprintf("/api/destinations/%v", dest["id"]), nil); code != http.StatusConflict {
		t.Fatalf("delete destination in use: %d", code)
	}
	var cid string
	for _, ct := range dk.Running() {
		cid = ct.Spec.Name
	}
	_ = dk.WriteFile(cid, "/var/lib/postgresql/data/data", []byte("orders: 7"))
	c.do("POST", fmt.Sprintf("/api/databases/%v/backups", d["id"]), nil)
	det := waitDB(t, c, d["id"], func(b map[string]any) bool {
		bs, _ := b["backups"].([]any)
		return len(bs) > 0 && bs[0].(map[string]any)["remoteStatus"] == "uploaded"
	})
	bk := det["backups"].([]any)[0].(map[string]any)
	keys := fake.Keys("backups", "prod/databases/")
	if len(keys) != 1 || !strings.HasSuffix(keys[0], fmt.Sprint(bk["file"])) || det["backupDestinationId"] != dest["id"] {
		t.Fatalf("bucket keys = %v, backup = %v", keys, bk)
	}
	if obj, _ := fake.Object("backups", keys[0]); string(obj) != "PGDMPorders: 7" {
		t.Fatalf("uploaded = %q", obj)
	}

	// Local copy gone: download and restore still work, from the bucket.
	files, _ := filepath.Glob(filepath.Join(backupDir, "*", "*.dump"))
	for _, f := range files {
		_ = os.Remove(f)
	}
	waitDB(t, c, d["id"], func(b map[string]any) bool { return b["backups"].([]any)[0].(map[string]any)["local"] == false })
	req, _ := http.NewRequest("GET", c.base+fmt.Sprintf("/api/backups/%v/download", bk["id"]), nil)
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	closeBody(t, res)
	if string(got) != "PGDMPorders: 7" {
		t.Fatalf("download from bucket = %d %q", res.StatusCode, got)
	}
	if code, _ := c.do("POST", fmt.Sprintf("/api/databases/%v/restore", d["id"]), map[string]any{"backupId": bk["id"]}); code != 202 {
		t.Fatalf("restore: %d", code)
	}
	waitFor(t, "restore from the bucket", func() bool {
		for _, e := range execs() {
			if strings.HasPrefix(e, "pg_restore") {
				return true
			}
		}
		return false
	})
	waitDB(t, c, d["id"], func(b map[string]any) bool { return b["runtimeMessage"] == nil })

	// A failed upload is shown and can be retried.
	fake.FailPuts(true)
	c.do("POST", fmt.Sprintf("/api/databases/%v/backups", d["id"]), nil)
	det = waitDB(t, c, d["id"], func(b map[string]any) bool {
		return b["backups"].([]any)[0].(map[string]any)["remoteStatus"] == "failed"
	})
	failed := det["backups"].([]any)[0].(map[string]any)
	if failed["status"] != "succeeded" || failed["remoteError"] == nil {
		t.Fatalf("failed upload = %v", failed)
	}
	fake.FailPuts(false)
	if code, body := c.do("POST", fmt.Sprintf("/api/backups/%v/upload", failed["id"]), nil); code != 200 {
		t.Fatalf("retry upload: %d %v", code, body)
	}

	// Database users.
	if code, _ := c.do("POST", fmt.Sprintf("/api/databases/%v/users", d["id"]), map[string]any{"username": "main", "role": "readonly"}); code != 400 {
		t.Fatalf("owner name accepted: %d", code)
	}
	if code, _ := c.do("POST", fmt.Sprintf("/api/databases/%v/users", d["id"]), map[string]any{"username": "bi", "role": "superuser"}); code != 400 {
		t.Fatalf("bad role accepted: %d", code)
	}
	code, u := c.do("POST", fmt.Sprintf("/api/databases/%v/users", d["id"]), map[string]any{"username": "bi", "role": "readonly"})
	if code != 201 || u["role"] != "readonly" {
		t.Fatalf("create user: %d %v", code, u)
	}
	_, cred := c.do("GET", fmt.Sprintf("/api/database-users/%v/credentials", u["id"]), nil)
	pw := fmt.Sprint(cred["password"])
	if len(pw) < 20 || !strings.HasPrefix(fmt.Sprint(cred["url"]), "postgres://bi:"+pw+"@main:5432/main") {
		t.Fatalf("credentials = %v", cred)
	}
	if !strings.Contains(strings.Join(execs(), "\n"), `GRANT SELECT ON ALL TABLES IN SCHEMA public TO "bi"`) {
		t.Fatal("grants not applied")
	}
	if code, nu := c.do("PATCH", fmt.Sprintf("/api/database-users/%v", u["id"]), map[string]any{"role": "readwrite", "resetPassword": true}); code != 200 || nu["role"] != "readwrite" {
		t.Fatalf("update user: %d %v", code, nu)
	}
	_, cred2 := c.do("GET", fmt.Sprintf("/api/database-users/%v/credentials", u["id"]), nil)
	if cred2["password"] == pw {
		t.Fatal("password not reset")
	}
	if code, _ := c.do("DELETE", fmt.Sprintf("/api/database-users/%v", u["id"]), nil); code != 200 {
		t.Fatalf("delete user: %d", code)
	}
	var leaks int
	_ = env.st.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE details LIKE ? OR details LIKE ?", "%"+pw+"%", "%s3cret-key%").Scan(&leaks)
	if leaks != 0 {
		t.Fatal("secret in audit log")
	}

	// Scheduled Docklet backups to the bucket, with retention.
	if code, _ := c.do("PUT", "/api/system/backup-schedule", map[string]any{"destinationId": dest["id"], "schedule": "daily", "keep": 2, "password": "wrong"}); code < 400 {
		t.Fatalf("schedule without password: %d", code)
	}
	if code, _ := c.do("PUT", "/api/system/backup-schedule", map[string]any{"destinationId": dest["id"], "schedule": "daily", "keep": 2, "password": adminPW}); code != 400 {
		t.Fatalf("schedule without passphrase: %d", code)
	}
	code, pol := c.do("PUT", "/api/system/backup-schedule", map[string]any{
		"destinationId": dest["id"], "schedule": "daily", "keep": 2, "password": adminPW, "passphrase": "long enough phrase", "includeDatabaseBackups": true,
	})
	if code != 200 || pol["hasPassphrase"] != true {
		t.Fatalf("schedule: %d %v", code, pol)
	}
	waitFor(t, "scheduled self-backup", func() bool { return len(fake.Keys("backups", "prod/docklet/")) == 1 })
	for i := 0; i < 2; i++ {
		time.Sleep(1100 * time.Millisecond) // names have second resolution
		c.do("POST", "/api/system/backup-schedule/run", nil)
		want := i + 2
		if want > 2 {
			want = 2
		}
		waitFor(t, "manual self-backup", func() bool {
			_, p := c.do("GET", "/api/system/backup-schedule", nil)
			return p["running"] == false && len(fake.Keys("backups", "prod/docklet/")) == want && p["lastKey"] != nil
		})
	}
	keys = fake.Keys("backups", "prod/docklet/")
	if len(keys) != 2 {
		t.Fatalf("self-backup retention: %v", keys)
	}
	obj, _ := fake.Object("backups", keys[1])
	m, err := selfbackup.Inspect(bytes.NewReader(obj), "long enough phrase")
	if err != nil || !m.Encrypted || m.DatabaseBackups == 0 {
		t.Fatalf("self-backup in bucket: %+v %v", m, err)
	}
	if _, list := c.do("GET", "/api/system/remote-backups", nil); len(list["backups"].([]any)) != 2 {
		t.Fatalf("remote backups = %v", list)
	}

	acts := strings.Join(actionsOf(t, env.st), ",")
	for _, want := range []string{"s3_destination.created", "s3_destination.updated", "database.backup_destination_changed", "database.backup_uploaded",
		"database.backup_upload_failed", "database.user_created", "database.user_credentials_viewed", "database.user_updated", "database.user_deleted",
		"system.backup_schedule_changed", "system.backup_uploaded"} {
		if !strings.Contains(acts, want) {
			t.Errorf("audit missing %s", want)
		}
	}
}
