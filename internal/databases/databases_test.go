package databases

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker/dockertest"
	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// fakeEngine emulates each engine's CLI tools over a single "data" file in
// the engine's data directory (which lives on the database volume).
func fakeEngine(dataFile func(cmd []string) string) dockertest.ExecFunc {
	return func(f *dockertest.Fake, id string, cmd, env []string, stdout io.Writer) (int, string) {
		data := func() string {
			// Find which data path this container mounts.
			for _, p := range []string{"/var/lib/postgresql/data", "/var/lib/postgresql", "/var/lib/mysql", "/data"} {
				if b, ok := f.ReadFile(id, p+"/data"); ok {
					_ = b
					return p + "/data"
				}
			}
			return dataFile(cmd)
		}
		switch cmd[0] {
		case "pg_isready", "mysqladmin", "mariadb-admin":
			return 0, ""
		case "redis-cli":
			joined := strings.Join(cmd, " ")
			switch {
			case strings.HasSuffix(joined, "ping"):
				_, _ = stdout.Write([]byte("PONG\n"))
			case strings.Contains(joined, "--rdb"):
				b, _ := f.ReadFile(id, data())
				_ = f.WriteFile(id, cmd[len(cmd)-1], b)
			case strings.Contains(joined, "DEBUG RELOAD"):
				b, _ := f.ReadFile(id, "/data/dump.rdb")
				_ = f.WriteFile(id, "/data/data", b)
			}
			return 0, ""
		case "pg_dump": // custom format always starts with a header
			b, _ := f.ReadFile(id, data())
			_, _ = stdout.Write(append([]byte("PGDMP"), b...))
			return 0, ""
		case "mysqldump", "mariadb-dump":
			b, _ := f.ReadFile(id, data())
			_, _ = stdout.Write(b)
			return 0, ""
		case "psql", "rm":
			return 0, ""
		case "pg_restore":
			b, ok := f.ReadFile(id, cmd[len(cmd)-1])
			if !ok {
				return 1, "pg_restore: input file not found"
			}
			_ = f.WriteFile(id, data(), bytes.TrimPrefix(b, []byte("PGDMP")))
			return 0, ""
		case "sh": // sh -c "mysql ... < /tmp/docklet-restore.sql"
			path := cmd[2][strings.LastIndex(cmd[2], " ")+1:]
			b, _ := f.ReadFile(id, path)
			_ = f.WriteFile(id, data(), b)
			return 0, ""
		}
		return 127, cmd[0] + ": not found"
	}
}

type env struct {
	svc     *Service
	st      *store.Store
	dk      *dockertest.Fake
	project db.Project
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _ := secrets.New(bytes.Repeat([]byte{5}, secrets.KeySize))
	dk := dockertest.New()
	for _, e := range Engines {
		for _, v := range e.Versions {
			dk.AddImage(e.Image(v), e.Port)
		}
	}
	svc := New(st, dk, box, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	svc.ReadyTimeout, svc.ReadyInterval = time.Second, 10*time.Millisecond
	svc.EnsureNetwork = func(_ context.Context, id int64) (string, error) {
		n := "docklet-p1"
		return n, dk.NetworkEnsure(ctx, n, nil)
	}
	svc.SealEnv = func(appID int64, key, value string) ([]byte, error) { return []byte(value), nil }
	p, err := st.Q.CreateProject(ctx, db.CreateProjectParams{Name: "web", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	dk.SetExec(fakeEngine(func([]string) string { return "/data/data" }))
	return &env{svc: svc, st: st, dk: dk, project: p}
}

func (e *env) create(t *testing.T, engine, name string) db.Database {
	t.Helper()
	ctx := context.Background()
	eng := Engines[engine]
	pw, _ := GeneratePassword()
	root := ""
	if engine == "mysql" || engine == "mariadb" {
		root, _ = GeneratePassword()
	}
	pwEnc, rootEnc, err := e.svc.SealCreds(e.project.ID, name, pw, root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.st.Q.CreateDatabase(ctx, db.CreateDatabaseParams{
		ProjectID: e.project.ID, Name: name, Engine: engine, Version: eng.Versions[0],
		DbName: "app", Username: "app", PasswordEnc: pwEnc, RootPasswordEnc: rootEnc, CreatedAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Provision(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	d, _ = e.st.Q.GetDatabase(ctx, d.ID)
	return d
}

func (e *env) setData(t *testing.T, d db.Database, s string) {
	t.Helper()
	path := Engines[d.Engine].dataPath(d.Version) + "/data"
	if err := e.dk.WriteFile(d.ContainerID, path, []byte(s)); err != nil {
		t.Fatal(err)
	}
}

func (e *env) data(t *testing.T, d db.Database) string {
	t.Helper()
	d, _ = e.st.Q.GetDatabase(context.Background(), d.ID)
	b, _ := e.dk.ReadFile(d.ContainerID, Engines[d.Engine].dataPath(d.Version)+"/data")
	return string(b)
}

func TestProvisionPostgres(t *testing.T) {
	e := setup(t)
	d := e.create(t, "postgres", "main")
	if d.RuntimeStatus != StatusRunning || d.ContainerID == "" {
		t.Fatalf("db = %+v", d)
	}
	run := e.dk.Running()
	if len(run) != 1 {
		t.Fatalf("running = %d", len(run))
	}
	spec := run[0].Spec
	if spec.Volumes[VolumeName(d.ID)] != "/var/lib/postgresql/data" || len(spec.Publish) != 0 ||
		spec.Aliases[0] != "main" || !strings.Contains(strings.Join(spec.Env, ","), "POSTGRES_DB=app") {
		t.Fatalf("spec = %+v", spec)
	}
	u, err := e.svc.InternalURL(d)
	if err != nil || !strings.HasPrefix(u, "postgres://app:") || !strings.HasSuffix(u, "@main:5432/app?sslmode=disable") {
		t.Fatalf("url = %s %v", u, err)
	}
	// Credentials are not stored in plaintext.
	c, _ := e.svc.Creds(d)
	if bytes.Contains(d.PasswordEnc, []byte(c.Password)) {
		t.Fatal("password stored in plaintext")
	}
}

func TestBackupDropRestoreAllEngines(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql", "mariadb", "redis"} {
		t.Run(engine, func(t *testing.T) {
			e := setup(t)
			ctx := context.Background()
			d := e.create(t, engine, "db1")
			e.setData(t, d, "rows: alice, bob")
			b, err := e.svc.Backup(ctx, d.ID, TriggerManual)
			if err != nil || b.Status != "succeeded" || b.SizeBytes == 0 || !strings.HasSuffix(b.File, Engines[engine].BackupExt) {
				t.Fatalf("backup = %+v, %v", b, err)
			}
			if st, err := os.Stat(e.svc.backupPath(b.File)); err != nil || st.Mode().Perm() != 0o600 {
				t.Fatalf("backup file: %v", err)
			}

			e.setData(t, d, "") // "DROP"
			pre, err := e.svc.Restore(ctx, d.ID, b.ID)
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			if got := e.data(t, d); got != "rows: alice, bob" {
				t.Fatalf("data after restore = %q", got)
			}
			if pre.Trigger != TriggerPreRestore || pre.Status != "succeeded" {
				t.Fatalf("pre-restore backup = %+v", pre)
			}
		})
	}
}

func TestPublicPortRecreatesOnSameVolume(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	d := e.create(t, "postgres", "main")
	e.setData(t, d, "keep me")
	if err := e.svc.SetPublicPort(ctx, d.ID, 15432); err != nil {
		t.Fatal(err)
	}
	run := e.dk.Running()
	if len(run) != 1 || run[0].Spec.Publish[5432] != 15432 {
		t.Fatalf("not published: %+v", run)
	}
	if got := e.data(t, d); got != "keep me" {
		t.Fatalf("data lost on recreate: %q", got)
	}
	if err := e.svc.SetPublicPort(ctx, d.ID, 0); err != nil {
		t.Fatal(err)
	}
	if run := e.dk.Running(); len(run[0].Spec.Publish) != 0 {
		t.Fatal("still published")
	}
}

func TestReconcileMissingAndRecreate(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	d := e.create(t, "redis", "cache")
	e.setData(t, d, "k=v")
	_ = e.dk.ContainerRemove(ctx, d.ContainerID)
	if err := e.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ = e.st.Q.GetDatabase(ctx, d.ID)
	if d.RuntimeStatus != StatusMissing {
		t.Fatalf("status = %s", d.RuntimeStatus)
	}
	if err := e.svc.Recreate(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.data(t, d); got != "k=v" {
		t.Fatalf("data after recreate = %q", got)
	}
}

func TestScheduledBackupsAndRetention(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	d := e.create(t, "postgres", "main")
	e.setData(t, d, "x")
	_ = e.st.Q.SetDatabaseBackupPolicy(ctx, db.SetDatabaseBackupPolicyParams{BackupSchedule: "hourly", BackupKeep: 2, UpdatedAt: 1, ID: d.ID})
	clock := time.Unix(1_800_000_000, 0)
	e.svc.SetClock(func() time.Time { return clock })
	for i := 0; i < 4; i++ {
		e.svc.RunScheduled(ctx)
		e.svc.RunScheduled(ctx) // not due again within the hour
		clock = clock.Add(61 * time.Minute)
	}
	list, _ := e.st.Q.ListBackups(ctx, db.ListBackupsParams{DatabaseID: d.ID, Limit: 50})
	if len(list) != 2 {
		t.Fatalf("kept %d backups, want 2", len(list))
	}
	files, _ := os.ReadDir(filepath.Join(e.svc.BackupDir, "1"))
	if len(files) != 2 {
		t.Fatalf("%d files on disk, want 2", len(files))
	}
}

func TestLinkAndDelete(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	d := e.create(t, "postgres", "main")
	app, _ := e.st.Q.CreateApp(ctx, db.CreateAppParams{ProjectID: e.project.ID, Name: "api", Image: "x", RestartPolicy: "no", CreatedAt: 1, UpdatedAt: 1})
	other, _ := e.st.Q.CreateProject(ctx, db.CreateProjectParams{Name: "other", CreatedAt: 1})
	foreign, _ := e.st.Q.CreateApp(ctx, db.CreateAppParams{ProjectID: other.ID, Name: "x", Image: "x", RestartPolicy: "no", CreatedAt: 1, UpdatedAt: 1})

	if err := e.svc.Link(ctx, e.st.Q, d, foreign.ID, "DATABASE_URL"); err == nil {
		t.Fatal("linked across projects")
	}
	if err := e.svc.Link(ctx, e.st.Q, d, app.ID, "DATABASE_URL"); err != nil {
		t.Fatal(err)
	}
	envs, _ := e.st.Q.ListEnv(ctx, app.ID)
	if len(envs) != 1 || !strings.HasPrefix(string(envs[0].ValueEnc), "postgres://app:") {
		t.Fatalf("env = %+v", envs)
	}
	if err := e.svc.Delete(ctx, d.ID, true, func(*db.Queries) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if envs, _ := e.st.Q.ListEnv(ctx, app.ID); len(envs) != 0 {
		t.Fatal("linked env var not removed")
	}
	if e.dk.HasVolume(VolumeName(d.ID)) || e.dk.Count() != 0 {
		t.Fatal("container or volume left behind")
	}
}

func TestReadyTimeoutMarksFailed(t *testing.T) {
	e := setup(t)
	e.dk.SetExec(func(*dockertest.Fake, string, []string, []string, io.Writer) (int, string) {
		return 1, "could not connect to server"
	})
	ctx := context.Background()
	pwEnc, _, _ := e.svc.SealCreds(e.project.ID, "slow", "pw", "")
	d, _ := e.st.Q.CreateDatabase(ctx, db.CreateDatabaseParams{
		ProjectID: e.project.ID, Name: "slow", Engine: "postgres", Version: "17-alpine",
		DbName: "a", Username: "a", PasswordEnc: pwEnc, CreatedAt: 1, UpdatedAt: 1,
	})
	if err := e.svc.Provision(ctx, d.ID); err == nil || !strings.Contains(err.Error(), "could not connect") {
		t.Fatalf("err = %v", err)
	}
	d, _ = e.st.Q.GetDatabase(ctx, d.ID)
	if d.RuntimeStatus != StatusFailed {
		t.Fatalf("status = %s", d.RuntimeStatus)
	}
}

func TestBackupFailureKeepsNoFile(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	d := e.create(t, "postgres", "main")
	e.dk.SetExec(func(_ *dockertest.Fake, _ string, cmd []string, _ []string, _ io.Writer) (int, string) {
		if cmd[0] == "pg_dump" {
			return 1, "pg_dump: error: connection refused"
		}
		return 0, ""
	})
	b, err := e.svc.Backup(ctx, d.ID, TriggerManual)
	if err == nil || b.Status != "failed" || !strings.Contains(b.Error, "connection refused") || b.File != "" {
		t.Fatalf("backup = %+v, %v", b, err)
	}
	// Restore refuses failed backups and aborts if the safety backup fails.
	if _, err := e.svc.Restore(ctx, d.ID, b.ID); err == nil {
		t.Fatal("restored a failed backup")
	}
}
