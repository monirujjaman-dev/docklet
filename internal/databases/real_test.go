package databases

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// TestRealEngines runs every engine against a live Docker daemon with the
// real images (DOCKLET_TEST_DOCKER=1). It checks the M6 acceptance criteria:
// ready time, a client connecting with the link URL over the project network,
// no published port by default, backup -> drop -> restore, and data surviving
// container recreation.
func TestRealEngines(t *testing.T) {
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
	const network = "docklet-e2e"
	svc.EnsureNetwork = func(ctx context.Context, _ int64) (string, error) {
		return network, dk.NetworkEnsure(ctx, network, nil)
	}
	t.Cleanup(func() { _ = dk.NetworkRemove(context.Background(), network) })
	p, err := st.Q.CreateProject(ctx, db.CreateProjectParams{Name: "e2e", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}

	only := os.Getenv("DOCKLET_TEST_ENGINES")
	for _, engine := range []string{"postgres", "mysql", "mariadb", "redis"} {
		if only != "" && !strings.Contains(only, engine) {
			continue
		}
		t.Run(engine, func(t *testing.T) {
			eng := Engines[engine]
			name := engine + "db"
			pw, _ := GeneratePassword()
			root := ""
			if eng.HasUser && engine != "postgres" {
				root, _ = GeneratePassword()
			}
			pwEnc, rootEnc, err := svc.SealCreds(p.ID, name, pw, root)
			if err != nil {
				t.Fatal(err)
			}
			d, err := st.Q.CreateDatabase(ctx, db.CreateDatabaseParams{
				ProjectID: p.ID, Name: name, Engine: engine, Version: eng.Versions[0],
				DbName: "app", Username: "app", PasswordEnc: pwEnc, RootPasswordEnc: rootEnc,
				CreatedAt: 1, UpdatedAt: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = svc.Delete(context.Background(), d.ID, true, func(*db.Queries) error { return nil }) })

			if _, err := dk.ImageInspect(ctx, eng.Image(d.Version)); err != nil {
				t.Skipf("image %s not present locally", eng.Image(d.Version))
			}
			start := time.Now()
			if err := svc.Provision(ctx, d.ID); err != nil {
				t.Fatal(err)
			}
			ready := time.Since(start)
			t.Logf("%s ready in %s", engine, ready.Round(100*time.Millisecond))
			if engine == "postgres" && ready > 20*time.Second {
				t.Errorf("postgres took %s to become ready (want < 20s)", ready)
			}
			d, _ = st.Q.GetDatabase(ctx, d.ID)

			bindings := func() string {
				out, err := exec.Command("docker", "inspect", "-f", "{{json .HostConfig.PortBindings}}", d.ContainerID).Output() //nolint:gosec // test-only
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(out))
			}
			if b := bindings(); b != "{}" && b != "null" {
				t.Fatalf("ports published by default: %s", b)
			}

			q := func(sql string) string {
				t.Helper()
				var cmd []string
				var env []string
				switch engine {
				case "postgres":
					cmd, env = []string{"psql", "-h", "127.0.0.1", "-U", "app", "-d", "app", "-tAqc", sql}, []string{"PGPASSWORD=" + pw}
				case "mysql", "mariadb":
					bin := map[string]string{"mysql": "mysql", "mariadb": "mariadb"}[engine]
					cmd, env = []string{bin, "-h", "127.0.0.1", "-uapp", "app", "-N", "-e", sql}, []string{"MYSQL_PWD=" + pw}
				case "redis":
					cmd, env = append([]string{"redis-cli", "-h", "127.0.0.1"}, strings.Fields(sql)...), []string{"REDISCLI_AUTH=" + pw}
				}
				var out bytes.Buffer
				res, err := dk.Exec(ctx, d.ContainerID, cmd, env, &out)
				if err != nil || res.ExitCode != 0 {
					t.Fatalf("%q: %v exit=%d %s", sql, err, res.ExitCode, res.Stderr)
				}
				return strings.TrimSpace(out.String())
			}
			var write, read, drop string
			if engine == "redis" {
				write, read, drop = "SET orders 42", "GET orders", "FLUSHALL"
			} else {
				write = "CREATE TABLE orders (n int); INSERT INTO orders VALUES (42);"
				read, drop = "SELECT n FROM orders", "DROP TABLE orders"
			}
			q(write)

			// A client container on the project network uses the link URL.
			url, err := svc.InternalURL(d)
			if err != nil {
				t.Fatal(err)
			}
			client := map[string][]string{
				"postgres": {"psql", url, "-tAc", "SELECT n FROM orders"},
				"mysql":    {"mysql", "-h", d.Name, "-uapp", "-p" + pw, "app", "-N", "-e", "SELECT n FROM orders"},
				"mariadb":  {"mariadb", "-h", d.Name, "-uapp", "-p" + pw, "app", "-N", "-e", "SELECT n FROM orders"},
				"redis":    {"redis-cli", "-u", url, "GET", "orders"},
			}[engine]
			args := append([]string{"run", "--rm", "--network", network, "-e", "DATABASE_URL=" + url, eng.Image(d.Version)}, client...)
			out, err := exec.Command("docker", args...).CombinedOutput() //nolint:gosec // test-only
			if err != nil || !strings.Contains(string(out), "42") {
				t.Fatalf("client via %s: %v %s", url, err, out)
			}

			b, err := svc.Backup(ctx, d.ID, "manual")
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s backup %d bytes", engine, b.SizeBytes)
			q(drop)
			if _, err := svc.Restore(ctx, d.ID, b.ID); err != nil {
				t.Fatal(err)
			}
			if got := q(read); got != "42" {
				t.Fatalf("after restore = %q", got)
			}

			if err := svc.Recreate(ctx, d.ID); err != nil {
				t.Fatal(err)
			}
			d, _ = st.Q.GetDatabase(ctx, d.ID)
			if got := q(read); got != "42" {
				t.Fatalf("after recreate = %q", got)
			}

			// Public toggle publishes on the host and keeps data.
			port := int64(25000 + len(engine))
			if err := svc.SetPublicPort(ctx, d.ID, port); err != nil {
				t.Fatal(err)
			}
			d, _ = st.Q.GetDatabase(ctx, d.ID)
			if b := bindings(); !strings.Contains(b, fmt.Sprintf(`"HostPort":"%d"`, port)) {
				t.Fatalf("port not published after toggle: %s", b)
			}
			if got := q(read); got != "42" {
				t.Fatalf("after publish = %q", got)
			}
		})
	}
}
