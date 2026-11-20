package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/build"
	"github.com/monirujjaman-dev/docklet/internal/docker/dockertest"
)

type stubBuilder struct {
	dk *dockertest.Fake
	mu sync.Mutex
	n  int
}

func (b *stubBuilder) Build(_ context.Context, spec build.Spec, logf func(string)) (build.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	logf("built " + spec.Tag)
	b.dk.AddImage(spec.Tag, 8080)
	return build.Result{CommitSHA: fmt.Sprintf("%040d", b.n), CommitMessage: fmt.Sprintf("commit %d", b.n)}, nil
}

func startGitServer(t *testing.T) (*panelEnv, *client, *stubBuilder) {
	t.Helper()
	dk := dockertest.New()
	sb := &stubBuilder{dk: dk}
	env := startPanelServer(t, filepath.Join(t.TempDir(), "d.db"), true,
		WithDocker(dk),
		WithAppsTuning(func(s *apps.Service) {
			s.HealthTimeout, s.HealthInterval, s.DrainDelay = time.Second, 10*time.Millisecond, 0
			s.HealthCheck = func(context.Context, string, int) error { return nil }
			s.Builder = sb
		}))
	return env, env.client(t), sb
}

func sendWebhook(t *testing.T, base, path string, headers map[string]string, payload any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", base+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, res)
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func sign(secret string, payload any) string {
	body, _ := json.Marshal(payload)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func TestGitAppWebhookAndRollback(t *testing.T) {
	_, c, sb := startGitServer(t)
	_, p := c.do("POST", "/api/projects", map[string]string{"name": "web"})
	code, body := c.do("POST", fmt.Sprintf("/api/projects/%v/apps", p["id"]), map[string]any{
		"name": "site", "git": map[string]any{"url": "https://github.com/org/site.git", "branch": "main", "autoDeploy": true},
	})
	if code != http.StatusCreated || body["deploymentId"] == nil {
		t.Fatalf("create: %d %v", code, body)
	}
	appID := body["id"]
	first := waitDeployment(t, c, body["deploymentId"])
	if first["status"] != "succeeded" || first["source"] != "git" || first["commitMessage"] != "commit 1" {
		t.Fatalf("first deploy: %v", first)
	}

	_, app := c.do("GET", fmt.Sprintf("/api/apps/%v", appID), nil)
	git, _ := app["git"].(map[string]any)
	if app["sourceType"] != "git" || app["buildsAvailable"] != true || git == nil ||
		!strings.HasPrefix(fmt.Sprint(git["deployKey"]), "ssh-ed25519 ") || !strings.HasPrefix(fmt.Sprint(git["webhookPath"]), "/api/webhooks/") {
		t.Fatalf("app = %v", app)
	}
	_, wh := c.do("GET", fmt.Sprintf("/api/apps/%v/webhook", appID), nil)
	path, secret := wh["path"].(string), wh["secret"].(string)

	push := map[string]any{"ref": "refs/heads/main", "after": "abc123", "head_commit": map[string]string{"message": "x"}}
	// Bad signature, wrong branch, ping, non-push event.
	if code, _ := sendWebhook(t, c.base, path, map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=" + sign("wrong", push)}, push); code != 401 {
		t.Fatalf("bad signature: %d", code)
	}
	if code, _ := sendWebhook(t, c.base, path, map[string]string{"X-GitHub-Event": "push"}, push); code != 401 {
		t.Fatalf("unsigned: %d", code)
	}
	other := map[string]any{"ref": "refs/heads/dev", "after": "abc"}
	if code, b := sendWebhook(t, c.base, path, map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=" + sign(secret, other)}, other); code != 200 || b["status"] != "ignored" {
		t.Fatalf("other branch: %d %v", code, b)
	}
	ping := map[string]any{"zen": "hi"}
	if code, b := sendWebhook(t, c.base, path, map[string]string{"X-GitHub-Event": "ping", "X-Hub-Signature-256": "sha256=" + sign(secret, ping)}, ping); code != 200 || b["status"] != "pong" {
		t.Fatalf("ping: %d %v", code, b)
	}
	if code, _ := sendWebhook(t, c.base, "/api/webhooks/unknown-token", map[string]string{"X-GitHub-Event": "push"}, push); code != 404 {
		t.Fatalf("unknown token: %d", code)
	}

	// GitHub push -> deploy.
	code, b := sendWebhook(t, c.base, path, map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=" + sign(secret, push)}, push)
	if code != http.StatusAccepted {
		t.Fatalf("github push: %d %v", code, b)
	}
	if d := waitDeployment(t, c, b["deploymentId"]); d["status"] != "succeeded" || d["createdBy"] != "webhook:github" {
		t.Fatalf("webhook deploy: %v", d)
	}
	// Gitea and GitLab signatures.
	code, b = sendWebhook(t, c.base, path, map[string]string{"X-Gitea-Event": "push", "X-Gitea-Signature": sign(secret, push)}, push)
	if code != http.StatusAccepted {
		t.Fatalf("gitea: %d %v", code, b)
	}
	waitDeployment(t, c, b["deploymentId"])
	gl := map[string]any{"ref": "refs/heads/main", "checkout_sha": "def"}
	code, b = sendWebhook(t, c.base, path, map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": secret}, gl)
	if code != http.StatusAccepted {
		t.Fatalf("gitlab: %d %v", code, b)
	}
	waitDeployment(t, c, b["deploymentId"])
	if code, _ := sendWebhook(t, c.base, path, map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "nope"}, gl); code != 401 {
		t.Fatalf("gitlab wrong token: %d", code)
	}
	if sb.n != 4 {
		t.Fatalf("builds = %d, want 4", sb.n)
	}

	// Rollback to the first deployment without building.
	if code, _ := c.do("POST", fmt.Sprintf("/api/apps/%v/rollback", appID), map[string]any{"deploymentId": first["id"]}); code != http.StatusAccepted {
		t.Fatalf("rollback: %d", code)
	}
	_, app = c.do("GET", fmt.Sprintf("/api/apps/%v", appID), nil)
	deps := app["deployments"].([]any)
	rb := deps[0].(map[string]any)
	if d := waitDeployment(t, c, rb["id"]); d["status"] != "succeeded" || d["source"] != "rollback" {
		t.Fatalf("rollback deploy: %v", d)
	}
	if sb.n != 4 {
		t.Fatal("rollback triggered a build")
	}
	_, app = c.do("GET", fmt.Sprintf("/api/apps/%v", appID), nil)
	cur := app["deployments"].([]any)[0].(map[string]any)
	if code, _ := c.do("POST", fmt.Sprintf("/api/apps/%v/rollback", appID), map[string]any{"deploymentId": cur["id"]}); code != http.StatusConflict {
		t.Fatalf("rollback to current: %d", code)
	}

	// Rotating the webhook invalidates the old URL.
	oldKey := git["deployKey"]
	c.do("POST", fmt.Sprintf("/api/apps/%v/webhook/rotate", appID), nil)
	if code, _ := sendWebhook(t, c.base, path, map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=" + sign(secret, push)}, push); code != 404 {
		t.Fatalf("old webhook still works: %d", code)
	}
	_, app = c.do("POST", fmt.Sprintf("/api/apps/%v/deploy-key", appID), nil)
	if app["git"].(map[string]any)["deployKey"] == oldKey {
		t.Fatal("deploy key not regenerated")
	}
}

func TestGitAppValidationAndSSHDefault(t *testing.T) {
	_, c, _ := startGitServer(t)
	_, p := c.do("POST", "/api/projects", map[string]string{"name": "web"})
	url := fmt.Sprintf("/api/projects/%v/apps", p["id"])
	for _, g := range []map[string]any{
		{"url": "https://user:tok@github.com/o/r.git"}, //nolint:gosec // deliberately invalid URL: must be rejected
		{"url": "file:///etc"},
		{"url": "https://github.com/o/r.git", "branch": "-x"},
		{"url": "https://github.com/o/r.git", "context": "../.."},
	} {
		if code, _ := c.do("POST", url, map[string]any{"name": "a", "git": g}); code != 400 {
			t.Errorf("accepted %v (%d)", g, code)
		}
	}
	if code, _ := c.do("POST", url, map[string]any{"name": "a", "image": "x", "git": map[string]any{"url": "https://github.com/o/r"}}); code != 400 {
		t.Error("image+git accepted")
	}
	code, body := c.do("POST", url, map[string]any{"name": "priv", "git": map[string]any{"url": "git@github.com:org/private.git"}})
	if code != http.StatusCreated || body["deploymentId"] != nil {
		t.Fatalf("ssh app should not deploy before the key is added: %d %v", code, body)
	}
	_, app := c.do("GET", fmt.Sprintf("/api/apps/%v", body["id"]), nil)
	if app["git"].(map[string]any)["ssh"] != true {
		t.Fatalf("ssh flag missing: %v", app["git"])
	}
	// Image apps reject git-only endpoints.
	code, body = c.do("POST", url, map[string]any{"name": "img", "image": "hello:v1", "deploy": false})
	if code != 201 {
		t.Fatal("create image app")
	}
	if code, _ := c.do("GET", fmt.Sprintf("/api/apps/%v/webhook", body["id"]), nil); code != http.StatusConflict {
		t.Fatalf("webhook on image app: %d", code)
	}
}
