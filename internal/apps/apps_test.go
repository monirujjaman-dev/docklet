package apps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/build"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/docker/dockertest"
	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

type countingProxy struct {
	mu    sync.Mutex
	syncs int
}

func (p *countingProxy) Sync(context.Context) error {
	p.mu.Lock()
	p.syncs++
	p.mu.Unlock()
	return nil
}

type env struct {
	svc   *Service
	st    *store.Store
	dk    *dockertest.Fake
	proxy *countingProxy
	app   db.App
}

func setup(t *testing.T, image string) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _ := secrets.New(bytes.Repeat([]byte{3}, secrets.KeySize))
	dk := dockertest.New()
	dk.AddImage("hello:v1", 8080)
	dk.AddImage("hello:v2", 8080)
	dk.AddImage("crash:v1", 8080)
	dk.AddImage("nopull:v1", 9000)
	px := &countingProxy{}
	svc := New(st, dk, box, slog.New(slog.NewTextHandler(io.Discard, nil)), px)
	svc.HealthTimeout, svc.HealthInterval, svc.DrainDelay = time.Second, 10*time.Millisecond, 0
	svc.HealthCheck = func(context.Context, string, int) error { return nil }

	p, err := st.Q.CreateProject(ctx, db.CreateProjectParams{Name: "web", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.Q.CreateApp(ctx, db.CreateAppParams{ProjectID: p.ID, Name: "site", Image: image, RestartPolicy: "unless-stopped", MemoryMb: 64, CreatedAt: 1, UpdatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := svc.SealEnv(app.ID, "SECRET", "s3cr3t")
	if err := st.Q.UpsertEnv(ctx, db.UpsertEnvParams{AppID: app.ID, Key: "SECRET", ValueEnc: enc, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Q.InsertDomain(ctx, db.InsertDomainParams{Domain: "site.example.com", AppID: app.ID, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	return &env{svc: svc, st: st, dk: dk, proxy: px, app: app}
}

// deploy enqueues and runs one deployment synchronously.
func (e *env) deploy(t *testing.T, image string) (db.Deployment, error) {
	t.Helper()
	return e.deployAs(t, image, KindWeb, "")
}

func (e *env) deployAs(t *testing.T, image, kind, command string) (db.Deployment, error) {
	t.Helper()
	ctx := context.Background()
	if err := e.st.Q.UpdateAppConfig(ctx, db.UpdateAppConfigParams{
		Image: image, RestartPolicy: "unless-stopped", MemoryMb: 64, Kind: kind, Command: command, UpdatedAt: 2, ID: e.app.ID,
	}); err != nil {
		t.Fatal(err)
	}
	app, _ := e.st.Q.GetApp(ctx, e.app.ID)
	var dep db.Deployment
	err := e.st.Tx(ctx, func(q *db.Queries) error {
		var err error
		dep, err = e.svc.EnqueueDeploy(ctx, q, app, "admin")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	derr := e.svc.runDeploy(ctx, dep.ID)
	dep, _ = e.st.Q.GetDeployment(ctx, dep.ID)
	return dep, derr
}

func (e *env) current(t *testing.T) db.App {
	app, err := e.st.Q.GetApp(context.Background(), e.app.ID)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestDeploySucceedsWithEnvAndRoute(t *testing.T) {
	e := setup(t, "hello:v1")
	dep, err := e.deploy(t, "hello:v1")
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, dep.Log)
	}
	if dep.Status != DeploySucceeded || dep.Port != 8080 || dep.ContainerIp == "" {
		t.Fatalf("dep = %+v", dep)
	}
	app := e.current(t)
	if app.CurrentDeploymentID.Int64 != dep.ID || app.RuntimeStatus != "running" {
		t.Fatalf("app = %+v", app)
	}
	run := e.dk.Running()
	if len(run) != 1 {
		t.Fatalf("running containers = %d", len(run))
	}
	spec := run[0].Spec
	if !contains(spec.Env, "SECRET=s3cr3t") || spec.MemoryBytes != 64<<20 || spec.Labels[LabelDeployment] != fmt.Sprint(dep.ID) {
		t.Fatalf("spec = %+v", spec)
	}
	routes, _ := e.st.Q.ListRoutableDomains(context.Background())
	if len(routes) != 1 || routes[0].Domain != "site.example.com" || routes[0].Port != 8080 {
		t.Fatalf("routes = %+v", routes)
	}
	if e.proxy.syncs == 0 {
		t.Fatal("proxy never synced")
	}
}

func TestRedeployRetiresPrevious(t *testing.T) {
	e := setup(t, "hello:v1")
	first, _ := e.deploy(t, "hello:v1")
	second, err := e.deploy(t, "hello:v2")
	if err != nil {
		t.Fatal(err)
	}
	if run := e.dk.Running(); len(run) != 1 || run[0].Spec.Image != "hello:v2" {
		t.Fatalf("running = %+v", run)
	}
	first, _ = e.st.Q.GetDeployment(context.Background(), first.ID)
	if first.Status != DeploySuperseded {
		t.Fatalf("first status = %s", first.Status)
	}
	if e.current(t).CurrentDeploymentID.Int64 != second.ID {
		t.Fatal("current deployment not switched")
	}
}

func TestFailedDeployKeepsPrevious(t *testing.T) {
	e := setup(t, "hello:v1")
	good, _ := e.deploy(t, "hello:v1")
	bad, err := e.deploy(t, "crash:v1")
	if err == nil || bad.Status != DeployFailed {
		t.Fatalf("expected failure, got %v %s", err, bad.Status)
	}
	if !strings.Contains(bad.Log, "FAILED") || !strings.Contains(bad.Log, "boom: config missing") {
		t.Fatalf("log lacks failure details:\n%s", bad.Log)
	}
	if e.current(t).CurrentDeploymentID.Int64 != good.ID {
		t.Fatal("current deployment changed after failure")
	}
	run := e.dk.Running()
	if len(run) != 1 || run[0].Spec.Image != "hello:v1" {
		t.Fatalf("previous not running: %+v", run)
	}
	if n := e.dk.Count(); n != 1 {
		t.Fatalf("failed container not removed: %d containers", n)
	}
}

func TestPullFailureUsesLocalImageOrFails(t *testing.T) {
	e := setup(t, "nopull:v1")
	dep, err := e.deploy(t, "nopull:v1")
	if err != nil || !strings.Contains(dep.Log, "using the image already on this host") || dep.Port != 9000 {
		t.Fatalf("local fallback: %v %d\n%s", err, dep.Port, dep.Log)
	}
	dep, err = e.deploy(t, "missing:v1")
	if err == nil || !strings.Contains(dep.Error, "pull missing:v1") {
		t.Fatalf("missing image: %v %q", err, dep.Error)
	}
}

func TestReconcileDetectsMissingAndOrphans(t *testing.T) {
	e := setup(t, "hello:v1")
	ctx := context.Background()
	dep, _ := e.deploy(t, "hello:v1")

	// An orphan from a failed deployment.
	failed, _ := e.st.Q.CreateDeployment(ctx, db.CreateDeploymentParams{AppID: e.app.ID, Image: "x", Source: SourceImage, CreatedBy: "t", CreatedAt: 1})
	_ = e.st.Q.SetDeploymentStatus(ctx, db.SetDeploymentStatusParams{Status: DeployFailed, ID: failed.ID})
	_, _ = e.dk.ContainerCreate(ctx, docker.ContainerSpec{Name: "orphan", Labels: map[string]string{LabelManaged: "true", LabelDeployment: fmt.Sprint(failed.ID)}})

	if err := e.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if e.dk.Has("orphan") {
		t.Fatal("orphan not removed")
	}

	_ = e.dk.ContainerRemove(ctx, dep.ContainerID) // deleted outside Docklet
	if err := e.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	app := e.current(t)
	if app.RuntimeStatus != "missing" || !strings.Contains(app.RuntimeMessage, "outside Docklet") {
		t.Fatalf("app = %s %q", app.RuntimeStatus, app.RuntimeMessage)
	}
	var n int
	_ = e.st.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'app.container_missing'").Scan(&n)
	if n != 1 {
		t.Fatalf("missing audited %d times", n)
	}
	_ = e.svc.Reconcile(ctx)
	_ = e.st.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'app.container_missing'").Scan(&n)
	if n != 1 {
		t.Fatal("missing audited again on unchanged state")
	}
}

func TestStopStartRestart(t *testing.T) {
	e := setup(t, "hello:v1")
	ctx := context.Background()
	if err := e.svc.Start(ctx, e.app.ID); !errors.Is(err, ErrNoDeployment) {
		t.Fatalf("start before deploy: %v", err)
	}
	_, _ = e.deploy(t, "hello:v1")
	if err := e.svc.Stop(ctx, e.app.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.dk.Running()) != 0 || e.current(t).RuntimeStatus != "stopped" {
		t.Fatal("not stopped")
	}
	if r, _ := e.st.Q.ListRoutableDomains(ctx); len(r) != 0 {
		t.Fatal("stopped app still routed")
	}
	if err := e.svc.Start(ctx, e.app.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.dk.Running()) != 1 || e.current(t).RuntimeStatus != "running" {
		t.Fatal("not started")
	}
	if err := e.svc.Restart(ctx, e.app.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteRemovesContainers(t *testing.T) {
	e := setup(t, "hello:v1")
	ctx := context.Background()
	_, _ = e.deploy(t, "hello:v1")
	if err := e.svc.Delete(ctx, e.app.ID, func(*db.Queries) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if e.dk.Count() != 0 {
		t.Fatal("containers left after delete")
	}
	if _, err := e.st.Q.GetApp(ctx, e.app.ID); err == nil {
		t.Fatal("app still exists")
	}
}

func TestWorkerRunsQueuedAndInterruptedJobs(t *testing.T) {
	e := setup(t, "hello:v1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app := e.current(t)
	var dep db.Deployment
	_ = e.st.Tx(ctx, func(q *db.Queries) error {
		var err error
		dep, err = e.svc.EnqueueDeploy(ctx, q, app, "admin")
		return err
	})
	// Simulate a crash mid-job: job is "running" with no worker.
	if _, err := e.st.Q.ClaimJob(ctx, 1); err != nil {
		t.Fatal(err)
	}
	e.svc.ReconcileInterval = time.Hour
	done := make(chan struct{})
	go func() { e.svc.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := e.st.Q.GetDeployment(ctx, dep.ID)
		if d.Status == DeploySucceeded {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("interrupted job was not resumed")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestAttachContainersJoinProjectNetwork(t *testing.T) {
	e := setup(t, "hello:v1")
	e.svc.AttachContainers = []string{"docklet", "docklet-caddy"}
	if _, err := e.deploy(t, "hello:v1"); err != nil {
		t.Fatal(err)
	}
	net := NetworkName(e.app.ProjectID)
	for _, c := range e.svc.AttachContainers {
		if !e.dk.Attached(net, c) {
			t.Errorf("%s not attached to %s", c, net)
		}
	}
}

// fakeBuilder "builds" by registering the tag in the fake Docker. Commits
// whose message contains "broken" fail like a bad Dockerfile would.
type fakeBuilder struct {
	dk     *dockertest.Fake
	mu     sync.Mutex
	builds int
	msg    string
}

func (b *fakeBuilder) Build(_ context.Context, spec build.Spec, logf func(string)) (build.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.builds++
	logf("Cloning " + spec.RepoURL)
	if strings.Contains(b.msg, "broken") {
		logf("  ERROR: failed to solve: process \"/bin/sh -c make\" did not complete successfully")
		return build.Result{CommitSHA: fmt.Sprintf("%040d", b.builds), CommitMessage: b.msg}, errors.New("docker build: exit status 1")
	}
	b.dk.AddImage(spec.Tag, 8080)
	return build.Result{CommitSHA: fmt.Sprintf("%040d", b.builds), CommitMessage: b.msg}, nil
}

func (e *env) makeGit(t *testing.T) *fakeBuilder {
	t.Helper()
	fb := &fakeBuilder{dk: e.dk, msg: "initial"}
	e.svc.Builder = fb
	if err := e.st.Q.SetAppGitSource(context.Background(), db.SetAppGitSourceParams{
		GitUrl: "https://example.com/org/site.git", GitBranch: "main", BuildContext: ".", Dockerfile: "Dockerfile",
		AutoDeploy: 1, UpdatedAt: 2, ID: e.app.ID,
	}); err != nil {
		t.Fatal(err)
	}
	return fb
}

func (e *env) deployCurrent(t *testing.T) (db.Deployment, error) {
	t.Helper()
	ctx := context.Background()
	app := e.current(t)
	var dep db.Deployment
	if err := e.st.Tx(ctx, func(q *db.Queries) error {
		var err error
		dep, err = e.svc.EnqueueDeploy(ctx, q, app, "webhook")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	derr := e.svc.runDeploy(ctx, dep.ID)
	dep, _ = e.st.Q.GetDeployment(ctx, dep.ID)
	return dep, derr
}

func TestGitDeployBuildsAndRecordsCommit(t *testing.T) {
	e := setup(t, "hello:v1")
	e.makeGit(t)
	dep, err := e.deployCurrent(t)
	if err != nil {
		t.Fatalf("%v\n%s", err, dep.Log)
	}
	if dep.Source != SourceGit || !strings.HasPrefix(dep.Image, "docklet/web-site:d") || dep.CommitMessage != "initial" || len(dep.CommitSha) != 40 {
		t.Fatalf("dep = %+v", dep)
	}
	if run := e.dk.Running(); len(run) != 1 || run[0].Spec.Image != dep.Image {
		t.Fatalf("running = %+v", run)
	}
}

func TestFailedBuildKeepsCurrent(t *testing.T) {
	e := setup(t, "hello:v1")
	fb := e.makeGit(t)
	good, _ := e.deployCurrent(t)
	fb.msg = "broken commit"
	bad, err := e.deployCurrent(t)
	if err == nil || bad.Status != DeployFailed || !strings.Contains(bad.Log, "failed to solve") || bad.CommitMessage != "broken commit" {
		t.Fatalf("bad = %s %v\n%s", bad.Status, err, bad.Log)
	}
	if e.current(t).CurrentDeploymentID.Int64 != good.ID {
		t.Fatal("current changed after failed build")
	}
	if run := e.dk.Running(); len(run) != 1 || run[0].Spec.Image != good.Image {
		t.Fatal("previous version not running")
	}
}

func TestRollbackReusesImageWithoutBuild(t *testing.T) {
	e := setup(t, "hello:v1")
	fb := e.makeGit(t)
	ctx := context.Background()
	first, _ := e.deployCurrent(t)
	fb.msg = "second"
	_, _ = e.deployCurrent(t)
	buildsBefore := fb.builds

	first, _ = e.st.Q.GetDeployment(ctx, first.ID)
	app := e.current(t)
	var rb db.Deployment
	if err := e.st.Tx(ctx, func(q *db.Queries) error {
		var err error
		rb, err = e.svc.EnqueueRollback(ctx, q, app, first, "admin")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.runDeploy(ctx, rb.ID); err != nil {
		t.Fatal(err)
	}
	rb, _ = e.st.Q.GetDeployment(ctx, rb.ID)
	if fb.builds != buildsBefore || rb.Status != DeploySucceeded || !strings.Contains(rb.Log, "no build or pull") {
		t.Fatalf("rollback rebuilt or failed: builds %d->%d, %s\n%s", buildsBefore, fb.builds, rb.Status, rb.Log)
	}
	if run := e.dk.Running(); len(run) != 1 || run[0].Spec.Image != first.ImageID {
		t.Fatalf("running = %+v, want image %s", run, first.ImageID)
	}

	// A failed deployment cannot be a rollback target.
	failed := first
	failed.Status = DeployFailed
	if _, err := e.svc.EnqueueRollback(ctx, e.st.Q, app, failed, "admin"); !errors.Is(err, ErrNotRollbackable) {
		t.Fatalf("err = %v", err)
	}
}

func TestCleanupKeepsNewestImages(t *testing.T) {
	e := setup(t, "hello:v1")
	fb := e.makeGit(t)
	e.svc.KeepImages = 2
	var deps []db.Deployment
	for i := 0; i < 5; i++ {
		fb.msg = fmt.Sprintf("commit %d", i)
		d, err := e.deployCurrent(t)
		if err != nil {
			t.Fatal(err)
		}
		deps = append(deps, d)
	}
	for i, d := range deps {
		want := i >= 3 // newest two kept
		if got := e.dk.HasImage(d.Image); got != want {
			t.Errorf("deployment %d image present=%v, want %v", i, got, want)
		}
		d, _ = e.st.Q.GetDeployment(context.Background(), d.ID)
		if (d.ImagePruned == 1) == want {
			t.Errorf("deployment %d image_pruned=%d", i, d.ImagePruned)
		}
	}
}

func TestGitDeployWithoutBuilder(t *testing.T) {
	e := setup(t, "hello:v1")
	e.makeGit(t)
	e.svc.Builder = nil
	dep, err := e.deployCurrent(t)
	if err == nil || !strings.Contains(dep.Error, "builds are not available") {
		t.Fatalf("%v %q", err, dep.Error)
	}
}

func TestWorkerDeploysWithoutPort(t *testing.T) {
	e := setup(t, "hello:v1")
	e.svc.WorkerSettle = 30 * time.Millisecond
	checks := 0
	e.svc.HealthCheck = func(context.Context, string, int) error { checks++; return nil }
	dep, err := e.deployAs(t, "hello:v1", KindWorker, `node worker.js --queue "emails high"`)
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, dep.Log)
	}
	if dep.Port != 0 || checks != 0 {
		t.Fatalf("worker got port %d / %d TCP checks", dep.Port, checks)
	}
	run := e.dk.Running()
	if len(run) != 1 || strings.Join(run[0].Spec.Cmd, "|") != "node|worker.js|--queue|emails high" || run[0].Spec.Port != 0 {
		t.Fatalf("spec = %+v", run[0].Spec)
	}
	if !strings.Contains(dep.Log, "Worker is running") {
		t.Fatalf("log:\n%s", dep.Log)
	}

	// A crash is handled by Docker's restart policy and shown in the status.
	e.dk.Crash(dep.ContainerID, 1)
	if err := e.svc.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	app := e.current(t)
	if app.RuntimeStatus != "running" || !strings.Contains(app.RuntimeMessage, "Restarted 1 time(s)") {
		t.Fatalf("status = %s %q", app.RuntimeStatus, app.RuntimeMessage)
	}
}

func TestWorkerThatExitsFailsDeploy(t *testing.T) {
	e := setup(t, "hello:v1")
	e.svc.WorkerSettle = time.Second
	e.dk.SetRun(func(s docker.ContainerSpec) (dockertest.RunResult, bool) {
		return dockertest.RunResult{Exit: 2, Duration: 20 * time.Millisecond, Logs: []string{"missing QUEUE_URL"}}, true
	})
	dep, err := e.deployAs(t, "hello:v1", KindWorker, "")
	if err == nil || !strings.Contains(err.Error(), "exited with code 2") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(dep.Log, "missing QUEUE_URL") {
		t.Fatalf("log lacks container output:\n%s", dep.Log)
	}
}

func TestSplitCommand(t *testing.T) {
	for in, want := range map[string]string{
		"":                            "",
		"  php artisan   queue:work ": "php|artisan|queue:work",
		`sh -c 'echo $HOME && date'`:  "sh|-c|echo $HOME && date",
		`echo "a \"b\" c" d\ e`:       `echo|a "b" c|d e`,
		`python -m app.jobs --n=""`:   "python|-m|app.jobs|--n=",
	} {
		got, err := SplitCommand(in)
		if err != nil || strings.Join(got, "|") != want {
			t.Errorf("SplitCommand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`echo 'x`, `echo "x`, `echo \`} {
		if _, err := SplitCommand(bad); err == nil {
			t.Errorf("SplitCommand(%q) accepted", bad)
		}
	}
}
