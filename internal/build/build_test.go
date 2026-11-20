package build

import (
	"context"
	"net/http/cgi" // #nosec G504 -- test-only git server; httpoxy was fixed in Go 1.6.3
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestValidateRepoURL(t *testing.T) {
	ok := []string{
		"https://github.com/org/repo.git", "http://git.local/repo", "ssh://git@github.com/org/repo.git",
		"git@github.com:org/repo.git", "git@gitlab.example.com:group/sub/repo",
	}
	bad := []string{
		"", "file:///etc", "-uhttps://x", "https://user:token@github.com/org/repo", "https://user@github.com/o/r",
		"ftp://x/y", "github.com/org/repo", "https://github.com/org/re po", "ext::sh -c touch",
	}
	for _, u := range ok {
		if err := ValidateRepoURL(u); err != nil {
			t.Errorf("%s rejected: %v", u, err)
		}
	}
	for _, u := range bad {
		if ValidateRepoURL(u) == nil {
			t.Errorf("%s accepted", u)
		}
	}
	if !IsSSH("git@github.com:o/r.git") || !IsSSH("ssh://git@h/o/r") || IsSSH("https://h/o/r") {
		t.Fatal("IsSSH")
	}
}

func TestValidateBranchAndPath(t *testing.T) {
	for _, b := range []string{"main", "release/1.2", "feature_x-y"} {
		if ValidateBranch(b) != nil {
			t.Errorf("branch %s rejected", b)
		}
	}
	for _, b := range []string{"", "-x", "a..b", "a b", "$(x)"} {
		if ValidateBranch(b) == nil {
			t.Errorf("branch %q accepted", b)
		}
	}
	for _, p := range []string{".", "Dockerfile", "svc/api", "docker/Dockerfile.prod"} {
		if ValidatePath(p) != nil {
			t.Errorf("path %s rejected", p)
		}
	}
	for _, p := range []string{"", "/etc", "..", "../x", "a/../../b", "-f"} {
		if ValidatePath(p) == nil {
			t.Errorf("path %q accepted", p)
		}
	}
}

func TestNewDeployKey(t *testing.T) {
	priv, pub, err := NewDeployKey("docklet-app-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 ") || !strings.HasSuffix(pub, " docklet-app-1") {
		t.Fatalf("pub = %q", pub)
	}
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pub, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))) {
		t.Fatal("public key does not match private key")
	}
}

// gitRepo creates a repository served over Git's dumb HTTP protocol.
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	work := t.TempDir()
	git := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(work, "init", "-q", "-b", "main")
	for name, body := range files {
		p := filepath.Join(work, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o750)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "first commit")
	root := t.TempDir()
	git(work, "clone", "-q", "--bare", work, filepath.Join(root, "repo.git"))
	return serveGit(t, root) + "/repo.git"
}

// serveGit serves bare repositories under root over Git's smart HTTP
// protocol (git http-backend via CGI), like GitHub or Gitea do.
func serveGit(t *testing.T, root string) string {
	t.Helper()
	backend, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git exec path unknown")
	}
	h := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(backend)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestBuildCloneAndMissingDockerfile(t *testing.T) {
	url := gitRepo(t, map[string]string{"README.md": "hi"})
	b := New("unix:///nonexistent.sock", t.TempDir(), 1, time.Minute)
	if b.Available() != nil {
		t.Skip("docker CLI not installed")
	}
	var lines []string
	_, err := b.Build(context.Background(), Spec{RepoURL: url, Branch: "main", Context: ".", Dockerfile: "Dockerfile", Tag: "x:y"},
		func(s string) { lines = append(lines, s) })
	if err == nil || !strings.Contains(err.Error(), "Dockerfile not found") {
		t.Fatalf("err = %v\n%s", err, strings.Join(lines, "\n"))
	}
	if !strings.Contains(strings.Join(lines, "\n"), "first commit") {
		t.Fatalf("commit not logged:\n%s", strings.Join(lines, "\n"))
	}
	entries, _ := os.ReadDir(b.WorkDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "build-") {
			t.Fatalf("checkout %s not cleaned up", e.Name())
		}
	}
}

func TestBuildWrongBranch(t *testing.T) {
	url := gitRepo(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	b := New("unix:///nonexistent.sock", t.TempDir(), 1, time.Minute)
	if b.Available() != nil {
		t.Skip("docker CLI not installed")
	}
	_, err := b.Build(context.Background(), Spec{RepoURL: url, Branch: "nope", Context: ".", Dockerfile: "Dockerfile", Tag: "x:y"}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "git clone") {
		t.Fatalf("err = %v", err)
	}
}

// TestRealBuild needs a Docker daemon: DOCKLET_TEST_DOCKER=1.
func TestRealBuild(t *testing.T) {
	if os.Getenv("DOCKLET_TEST_DOCKER") == "" {
		t.Skip("set DOCKLET_TEST_DOCKER=1")
	}
	url := gitRepo(t, map[string]string{"app/Dockerfile": "FROM scratch\nCOPY hello.txt /hello.txt\n", "app/hello.txt": "hi\n"})
	b := New("unix:///var/run/docker.sock", t.TempDir(), 1, 2*time.Minute)
	var log strings.Builder
	res, err := b.Build(context.Background(), Spec{
		RepoURL: url, Branch: "main", Context: "app", Dockerfile: "app/Dockerfile",
		Tag: "docklet-test/built:1", Labels: map[string]string{"docklet.managed": "true"},
	}, func(s string) { log.WriteString(s + "\n") })
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if len(res.CommitSHA) != 40 || res.CommitMessage != "first commit" {
		t.Fatalf("res = %+v", res)
	}
	_ = exec.Command("docker", "rmi", "docklet-test/built:1").Run()
}
