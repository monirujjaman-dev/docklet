package databases

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/destinations"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/s3"
	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// TestRealUsersAndS3 checks the M9 acceptance criteria with real images
// (DOCKLET_TEST_DOCKER=1). With DOCKLET_TEST_S3_* set (see internal/s3), it
// also uploads backups, restores one after deleting the local file, and
// checks remote retention.
func TestRealUsersAndS3(t *testing.T) {
	if os.Getenv("DOCKLET_TEST_DOCKER") == "" {
		t.Skip("set DOCKLET_TEST_DOCKER=1 to run against a real Docker daemon")
	}
	ctx := context.Background()
	dk := docker.New("/var/run/docker.sock")
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _ := secrets.New(bytes.Repeat([]byte{7}, secrets.KeySize))
	svc := New(st, dk, box, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})), t.TempDir())
	const network = "docklet-e2e-users"
	svc.EnsureNetwork = func(ctx context.Context, _ int64) (string, error) {
		return network, dk.NetworkEnsure(ctx, network, nil)
	}
	t.Cleanup(func() { _ = dk.NetworkRemove(context.Background(), network) })
	p, _ := st.Q.CreateProject(ctx, db.CreateProjectParams{Name: "e2e", CreatedAt: 1})

	// Optional S3 destination.
	var dest sql.NullInt64
	var remote *s3.Client
	if ep := os.Getenv("DOCKLET_TEST_S3_ENDPOINT"); ep != "" {
		ds := destinations.New(st, box)
		in := destinations.Input{Name: "test", Endpoint: ep, Region: os.Getenv("DOCKLET_TEST_S3_REGION"), Bucket: os.Getenv("DOCKLET_TEST_S3_BUCKET"),
			Prefix: "e2e-" + time.Now().Format("150405"), AccessKey: os.Getenv("DOCKLET_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("DOCKLET_TEST_S3_SECRET_KEY"),
			PathStyle: os.Getenv("DOCKLET_TEST_S3_PATH_STYLE") == "1"}
		if err := in.Normalize(); err != nil {
			t.Fatal(err)
		}
		row, err := st.Q.CreateDestination(ctx, db.CreateDestinationParams{Name: in.Name, Endpoint: in.Endpoint, Region: in.Region, Bucket: in.Bucket,
			Prefix: in.Prefix, AccessKey: in.AccessKey, PathStyle: 1, CreatedAt: 1, UpdatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		enc, _ := ds.SealSecret(row.ID, in.SecretKey)
		_ = st.Q.SetDestinationSecret(ctx, db.SetDestinationSecretParams{SecretEnc: enc, ID: row.ID})
		if err := ds.Test(ctx, row.ID); err != nil {
			t.Fatalf("destination test: %v", err)
		}
		svc.Remote = func(ctx context.Context, id int64) (*s3.Client, error) {
			c, _, err := ds.Client(ctx, id)
			return c, err
		}
		dest = sql.NullInt64{Int64: row.ID, Valid: true}
		remote, _, _ = ds.Client(ctx, row.ID)
	}

	only := os.Getenv("DOCKLET_TEST_ENGINES")
	for _, engine := range []string{"postgres", "mysql", "mariadb", "redis"} {
		if only != "" && !strings.Contains(only, engine) {
			continue
		}
		t.Run(engine, func(t *testing.T) {
			eng := Engines[engine]
			name := engine + "u"
			pw, _ := GeneratePassword()
			root := ""
			if eng.HasUser && engine != "postgres" {
				root, _ = GeneratePassword()
			}
			pwEnc, rootEnc, _ := svc.SealCreds(p.ID, name, pw, root)
			d, err := st.Q.CreateDatabase(ctx, db.CreateDatabaseParams{ProjectID: p.ID, Name: name, Engine: engine, Version: eng.Versions[0],
				DbName: "app", Username: "app", PasswordEnc: pwEnc, RootPasswordEnc: rootEnc, CreatedAt: 1, UpdatedAt: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = svc.Delete(context.Background(), d.ID, true, func(*db.Queries) error { return nil }) })
			if err := svc.Provision(ctx, d.ID); err != nil {
				t.Fatal(err)
			}
			d, _ = st.Q.GetDatabase(ctx, d.ID)

			ins, _ := dk.ContainerInspect(ctx, d.ContainerID)
			dbIP := ins.IP(network)
			// as runs a statement as one of the users; ok reports success.
			as := func(user, pass, stmt string) (string, bool) {
				var cmd, env []string
				switch engine {
				case "postgres":
					// Not 127.0.0.1: the image trusts loopback connections without a password.
					cmd, env = []string{"psql", "-X", "-h", dbIP, "-U", user, "-d", "app", "-tAqc", stmt}, []string{"PGPASSWORD=" + pass}
				case "mysql", "mariadb":
					cmd, env = []string{mysqlClient(engine), "-h", "127.0.0.1", "-u" + user, "app", "-N", "-e", stmt}, []string{"MYSQL_PWD=" + pass}
				case "redis":
					cmd, env = append([]string{"redis-cli", "--no-auth-warning", "--user", user}, strings.Fields(stmt)...), []string{"REDISCLI_AUTH=" + pass}
				}
				var out bytes.Buffer
				res, err := dk.Exec(ctx, d.ContainerID, cmd, env, &out)
				o := strings.TrimSpace(out.String() + res.Stderr)
				return o, err == nil && res.ExitCode == 0 && !strings.HasPrefix(o, "NOPERM") && !strings.Contains(o, "ERR") && !strings.Contains(o, "WRONGPASS")
			}
			ownerUser := "app"
			if engine == "redis" {
				ownerUser = "default"
			}
			mk := func(n, role string) (string, string) {
				u, err := svc.CreateUser(ctx, d.ID, n, role)
				if err != nil {
					t.Fatalf("create %s: %v", n, err)
				}
				pw, _ := svc.UserPassword(u)
				return n, pw
			}

			var write, read, ddl, privEsc string
			if engine == "redis" {
				write, read, ddl, privEsc = "SET k 1", "GET k", "FLUSHALL", "CONFIG GET maxmemory"
			} else {
				write, read, ddl = "INSERT INTO orders VALUES (2)", "SELECT COUNT(*) FROM orders", "DROP TABLE orders"
				privEsc = map[string]string{"postgres": "CREATE ROLE evil", "mysql": "CREATE USER evil", "mariadb": "CREATE USER evil"}[engine]
			}
			if engine == "redis" {
				if _, ok := as(ownerUser, pw, "SET k 1"); !ok {
					t.Fatal("owner write failed")
				}
			} else if out, ok := as(ownerUser, pw, "CREATE TABLE orders (n int); INSERT INTO orders VALUES (1);"); !ok {
				t.Fatalf("owner create table: %s", out)
			}
			ro, roPw := mk("reader", RoleReadOnly)
			rw, rwPw := mk("writer", RoleReadWrite)
			ad, adPw := mk("boss", RoleAdmin)

			check := func(who, user, pass, stmt string, want bool) {
				t.Helper()
				out, ok := as(user, pass, stmt)
				if ok != want {
					t.Errorf("%s %q: allowed=%v want %v (%s)", who, stmt, ok, want, out)
				}
			}
			check("reader", ro, roPw, read, true)
			check("reader", ro, roPw, write, false)
			check("reader", ro, roPw, ddl, false)
			check("writer", rw, rwPw, read, true)
			check("writer", rw, rwPw, write, true)
			check("writer", rw, rwPw, ddl, false)
			check("writer", rw, rwPw, privEsc, false)
			check("admin", ad, adPw, privEsc, false)
			if engine != "redis" {
				check("writer", rw, rwPw, "CREATE TABLE w (n int)", false)
				check("admin", ad, adPw, "CREATE TABLE a (n int)", true)
				check("admin", ad, adPw, "INSERT INTO orders VALUES (3)", true)
				check("reader (table created by admin)", ro, roPw, "SELECT COUNT(*) FROM a", true)
				check("writer (table created by admin)", rw, rwPw, "INSERT INTO a VALUES (1)", true)
				if engine == "postgres" {
					check("admin", ad, adPw, "DROP DATABASE postgres", false)
				} else {
					check("admin", ad, adPw, "DROP DATABASE mysql", false)
					check("admin", ad, adPw, "SELECT COUNT(*) FROM mysql.user", false)
				}
			} else {
				check("admin", ad, adPw, ddl, true)
				_, _ = as(ownerUser, pw, "SET k 1")
			}

			// Role downgrade takes effect; password reset invalidates the old one.
			u, _ := st.Q.ListDatabaseUsers(ctx, d.ID)
			var writerID int64
			for _, x := range u {
				if x.Username == "writer" {
					writerID = x.ID
				}
			}
			if _, err := svc.UpdateUser(ctx, writerID, RoleReadOnly, true); err != nil {
				t.Fatal(err)
			}
			wu, _ := st.Q.GetDatabaseUser(ctx, writerID)
			newPw, _ := svc.UserPassword(wu)
			check("writer (old password)", rw, rwPw, read, false)
			check("writer (now read-only)", rw, newPw, read, true)
			check("writer (now read-only)", rw, newPw, write, false)

			// Users survive container recreation (Redis: re-applied).
			if err := svc.Recreate(ctx, d.ID); err != nil {
				t.Fatal(err)
			}
			d, _ = st.Q.GetDatabase(ctx, d.ID)
			ins, _ = dk.ContainerInspect(ctx, d.ContainerID)
			dbIP = ins.IP(network)
			check("reader after recreate", ro, roPw, read, true)
			if engine == "redis" {
				// A restart by Docker (crash, restart policy) loses ACL users;
				// the reconciler puts them back.
				if err := dk.ContainerRestart(ctx, d.ContainerID, 5); err != nil {
					t.Fatal(err)
				}
				time.Sleep(2 * time.Second)
				check("reader right after a docker restart", ro, roPw, read, false)
				if err := svc.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				check("reader after reconcile", ro, roPw, read, true)
			}

			// Backup → upload → delete local file → restore from S3.
			if remote != nil {
				if err := svc.SetDestination(ctx, d.ID, dest); err != nil {
					t.Fatal(err)
				}
				_ = st.Q.SetDatabaseBackupPolicy(ctx, db.SetDatabaseBackupPolicyParams{BackupSchedule: "off", BackupKeep: 2, UpdatedAt: 1, ID: d.ID})
				d, _ = st.Q.GetDatabase(ctx, d.ID)
				b, err := svc.Backup(ctx, d.ID, "manual")
				if err != nil {
					t.Fatal(err)
				}
				if b.RemoteStatus != RemoteUploaded {
					t.Fatalf("remote status = %q %s", b.RemoteStatus, b.RemoteError)
				}
				if err := os.Remove(svc.backupPath(b.File)); err != nil {
					t.Fatal(err)
				}
				_, _ = as(ownerUser, pw, ddl)
				if _, err := svc.Restore(ctx, d.ID, b.ID); err != nil {
					t.Fatalf("restore from S3: %v", err)
				}
				out, ok := as(ownerUser, pw, read)
				if !ok || (engine != "redis" && out != "3") || (engine == "redis" && out != "1") {
					t.Fatalf("after restore from S3: %q %v", out, ok)
				}
				for i := 0; i < 3; i++ {
					if _, err := svc.Backup(ctx, d.ID, "manual"); err != nil {
						t.Fatal(err)
					}
				}
				objs, err := remote.List(ctx, "databases/")
				if err != nil {
					t.Fatal(err)
				}
				n := 0
				for _, o := range objs {
					if strings.Contains(o.Key, "-"+d.Name+"/") {
						n++
					}
				}
				if n != 2 {
					t.Fatalf("remote retention: %d objects for %s, want 2: %+v", n, d.Name, objs)
				}
				t.Logf("%s: S3 upload, restore from S3 and retention ok", engine)
			}

			// Delete removes the user from the engine.
			for _, x := range u {
				if x.Username == "reader" {
					if _, err := svc.DeleteUser(ctx, x.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			check("reader after delete", ro, roPw, read, false)
		})
	}
}
