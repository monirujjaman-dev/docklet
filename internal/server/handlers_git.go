package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/auth"
	"github.com/monirujjaman-dev/docklet/internal/build"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

const maxWebhookBody = 5 << 20

// gitConfig is the Git source of an app (input and output).
type gitConfig struct {
	URL         string `json:"url"`
	Branch      string `json:"branch"`
	Context     string `json:"context"`
	Dockerfile  string `json:"dockerfile"`
	AutoDeploy  bool   `json:"autoDeploy"`
	DeployKey   string `json:"deployKey,omitempty"`   // output: public key
	SSH         bool   `json:"ssh,omitempty"`         // output
	WebhookPath string `json:"webhookPath,omitempty"` // output
}

func (g *gitConfig) normalize() error {
	g.URL = strings.TrimSpace(g.URL)
	if g.Branch = strings.TrimSpace(g.Branch); g.Branch == "" {
		g.Branch = "main"
	}
	if g.Context = strings.TrimSpace(g.Context); g.Context == "" {
		g.Context = "."
	}
	if g.Dockerfile = strings.TrimSpace(g.Dockerfile); g.Dockerfile == "" {
		g.Dockerfile = "Dockerfile"
	}
	g.DeployKey, g.SSH, g.WebhookPath = "", false, ""
	if err := build.ValidateRepoURL(g.URL); err != nil {
		return errf(http.StatusBadRequest, "%s", err.Error())
	}
	if err := build.ValidateBranch(g.Branch); err != nil {
		return errf(http.StatusBadRequest, "%s", err.Error())
	}
	if err := build.ValidatePath(g.Context); err != nil {
		return errf(http.StatusBadRequest, "build context %s", err.Error())
	}
	if err := build.ValidatePath(g.Dockerfile); err != nil {
		return errf(http.StatusBadRequest, "Dockerfile %s", err.Error())
	}
	return nil
}

// setupGitApp stores the Git source and creates a deploy key and webhook.
func (s *Server) setupGitApp(ctx context.Context, q *db.Queries, app db.App, g gitConfig) (db.App, error) {
	if err := q.SetAppGitSource(ctx, db.SetAppGitSourceParams{
		GitUrl: g.URL, GitBranch: g.Branch, BuildContext: g.Context, Dockerfile: g.Dockerfile,
		AutoDeploy: boolInt(g.AutoDeploy), UpdatedAt: s.now().Unix(), ID: app.ID,
	}); err != nil {
		return app, err
	}
	if err := s.newDeployKey(ctx, q, app.ID); err != nil {
		return app, err
	}
	if _, err := s.newWebhook(ctx, q, app.ID); err != nil {
		return app, err
	}
	return q.GetApp(ctx, app.ID)
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func (s *Server) newDeployKey(ctx context.Context, q *db.Queries, appID int64) error {
	priv, pub, err := build.NewDeployKey(fmt.Sprintf("docklet-app-%d", appID))
	if err != nil {
		return err
	}
	enc, err := s.apps.Seal(priv, apps.DeployKeyAAD(appID))
	if err != nil {
		return err
	}
	return q.SetAppDeployKey(ctx, db.SetAppDeployKeyParams{DeployKeyEnc: enc, DeployKeyPub: pub, UpdatedAt: s.now().Unix(), ID: appID})
}

func (s *Server) newWebhook(ctx context.Context, q *db.Queries, appID int64) (string, error) {
	token, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	secret, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	enc, err := s.apps.Seal([]byte(secret), apps.WebhookSecretAAD(appID))
	if err != nil {
		return "", err
	}
	return secret, q.SetAppWebhook(ctx, db.SetAppWebhookParams{
		WebhookToken: sql.NullString{String: token, Valid: true}, WebhookSecretEnc: enc, UpdatedAt: s.now().Unix(), ID: appID,
	})
}

func (s *Server) gitApp(ctx context.Context, q *db.Queries, id int64) (db.App, error) {
	app, err := q.GetApp(ctx, id)
	if err != nil {
		return app, notFoundAs(err, "app")
	}
	if app.SourceType != apps.SourceGit {
		return app, errf(http.StatusConflict, "this app deploys a registry image, not a Git repository")
	}
	return app, nil
}

func (s *Server) auditApp(ctx context.Context, q *db.Queries, r *http.Request, appID int64, action string, details map[string]any) error {
	a := authFrom(ctx)
	_, err := audit.Append(ctx, q, audit.Entry{
		ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: action,
		Target: fmt.Sprintf("app:%d", appID), IP: clientIP(r), Details: details,
	}, s.now())
	return err
}

// PUT /api/apps/{id}/git — update the Git source of a Git app.
func (s *Server) handleUpdateGit(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDocker(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in gitConfig
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := in.normalize(); err != nil {
		return err
	}
	ctx := r.Context()
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if _, err := s.gitApp(ctx, q, id); err != nil {
			return err
		}
		if err := q.SetAppGitSource(ctx, db.SetAppGitSourceParams{
			GitUrl: in.URL, GitBranch: in.Branch, BuildContext: in.Context, Dockerfile: in.Dockerfile,
			AutoDeploy: boolInt(in.AutoDeploy), UpdatedAt: s.now().Unix(), ID: id,
		}); err != nil {
			return err
		}
		return s.auditApp(ctx, q, r, id, audit.ActionAppGitUpdated, map[string]any{"git": in})
	})
	if err != nil {
		return err
	}
	return s.handleGetApp(w, r)
}

// POST /api/apps/{id}/deploy-key — replace the SSH deploy key.
func (s *Server) handleRegenerateDeployKey(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDocker(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if _, err := s.gitApp(ctx, q, id); err != nil {
			return err
		}
		if err := s.newDeployKey(ctx, q, id); err != nil {
			return err
		}
		return s.auditApp(ctx, q, r, id, audit.ActionAppDeployKey, nil)
	})
	if err != nil {
		return err
	}
	return s.handleGetApp(w, r)
}

// GET /api/apps/{id}/webhook — reveals the webhook secret to paste into the Git host.
func (s *Server) handleGetWebhook(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDocker(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	app, err := s.gitApp(ctx, s.st.Q, id)
	if err != nil {
		return err
	}
	secret, err := s.apps.Open(app.WebhookSecretEnc, apps.WebhookSecretAAD(id))
	if err != nil {
		return fmt.Errorf("decrypt webhook secret: %w", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": "/api/webhooks/" + app.WebhookToken.String, "secret": string(secret)})
	return nil
}

// POST /api/apps/{id}/webhook/rotate — new URL and secret.
func (s *Server) handleRotateWebhook(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDocker(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	var secret string
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if _, err := s.gitApp(ctx, q, id); err != nil {
			return err
		}
		if secret, err = s.newWebhook(ctx, q, id); err != nil {
			return err
		}
		return s.auditApp(ctx, q, r, id, audit.ActionAppWebhookRotated, nil)
	})
	if err != nil {
		return err
	}
	app, err := s.st.Q.GetApp(ctx, id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": "/api/webhooks/" + app.WebhookToken.String, "secret": secret})
	return nil
}

// POST /api/apps/{id}/rollback — {"deploymentId": N}: redeploy that image.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDocker(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		DeploymentID int64 `json:"deploymentId"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	a := authFrom(ctx)
	var dep db.Deployment
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		app, err := q.GetApp(ctx, id)
		if err != nil {
			return notFoundAs(err, "app")
		}
		target, err := q.GetDeployment(ctx, in.DeploymentID)
		if err != nil || target.AppID != id {
			return errf(http.StatusNotFound, "deployment not found for this app")
		}
		if app.CurrentDeploymentID.Valid && app.CurrentDeploymentID.Int64 == target.ID {
			return errf(http.StatusConflict, "deployment #%d is already running", target.ID)
		}
		dep, err = s.apps.EnqueueRollback(ctx, q, app, target, a.User.Username)
		if errors.Is(err, apps.ErrNotRollbackable) {
			return errf(http.StatusConflict, "%s", err.Error())
		}
		if err != nil {
			return err
		}
		return s.auditApp(ctx, q, r, id, audit.ActionAppRollback, map[string]any{"deployment": dep.ID, "rollbackOf": target.ID, "image": target.Image})
	})
	if err != nil {
		return err
	}
	s.apps.Wake()
	writeJSON(w, http.StatusAccepted, map[string]any{"deploymentId": dep.ID})
	return nil
}

// pushEvent is the part of GitHub/Gitea/GitLab push payloads we read.
type pushEvent struct {
	Ref         string `json:"ref"`
	After       string `json:"after"`        // GitHub, Gitea
	CheckoutSHA string `json:"checkout_sha"` // GitLab
}

// POST /api/webhooks/{token} — called by GitHub, Gitea/Forgejo or GitLab on push.
// Unauthenticated by design; the payload must be signed with the app's secret.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) error {
	if s.apps == nil {
		return errf(http.StatusServiceUnavailable, "deployments are disabled")
	}
	token := r.PathValue("token")
	if !s.webhookLimiter.allow(token) {
		return errf(http.StatusTooManyRequests, "too many webhook calls")
	}
	ctx := r.Context()
	app, err := s.st.Q.GetAppByWebhookToken(ctx, sql.NullString{String: token, Valid: true})
	if err != nil {
		return notFoundAs(err, "webhook")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		return errf(http.StatusRequestEntityTooLarge, "payload too large")
	}
	secret, err := s.apps.Open(app.WebhookSecretEnc, apps.WebhookSecretAAD(app.ID))
	if err != nil {
		return fmt.Errorf("decrypt webhook secret: %w", err)
	}
	provider, ok := verifyWebhook(r, body, secret)
	if !ok {
		return errf(http.StatusUnauthorized, "invalid signature or token")
	}

	event := firstHeader(r, "X-GitHub-Event", "X-Gitea-Event", "X-Gogs-Event", "X-Gitlab-Event")
	switch strings.ToLower(event) {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return nil
	case "push", "push hook":
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "event " + event + " is not a push"})
		return nil
	}

	var ev pushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return errf(http.StatusBadRequest, "payload is not JSON (set the webhook content type to application/json)")
	}
	sha := ev.After
	if sha == "" {
		sha = ev.CheckoutSHA
	}
	branch := strings.TrimPrefix(ev.Ref, "refs/heads/")
	switch {
	case branch != app.GitBranch:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "push to " + ev.Ref + ", app follows " + app.GitBranch})
		return nil
	case strings.Trim(sha, "0") == "":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "branch deleted"})
		return nil
	case app.AutoDeploy == 0:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "auto deploy is off"})
		return nil
	}

	var dep db.Deployment
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		var err error
		if dep, err = s.apps.EnqueueDeploy(ctx, q, app, "webhook:"+provider); err != nil {
			return err
		}
		_, err = audit.Append(ctx, q, audit.Entry{
			Actor: "webhook:" + provider, Action: audit.ActionDeployRequested,
			Target: fmt.Sprintf("app:%d", app.ID), IP: clientIP(r),
			Details: map[string]any{"deployment": dep.ID, "ref": ev.Ref, "commit": sha},
		}, s.now())
		return err
	})
	if err != nil {
		return err
	}
	s.apps.Wake()
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "deploymentId": dep.ID})
	return nil
}

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := r.Header.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// verifyWebhook checks the provider-specific signature and names the provider.
func verifyWebhook(r *http.Request, body, secret []byte) (string, bool) {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	want := mac.Sum(nil)
	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" { // GitHub (and newer Gitea)
		got, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
		provider := "github"
		if r.Header.Get("X-Gitea-Event") != "" || r.Header.Get("X-Gogs-Event") != "" {
			provider = "gitea"
		}
		return provider, err == nil && hmac.Equal(got, want)
	}
	if sig := firstHeader(r, "X-Gitea-Signature", "X-Gogs-Signature"); sig != "" {
		got, err := hex.DecodeString(sig)
		return "gitea", err == nil && hmac.Equal(got, want)
	}
	if tok := r.Header.Get("X-Gitlab-Token"); tok != "" { // GitLab sends the secret itself
		return "gitlab", subtle.ConstantTimeCompare([]byte(tok), secret) == 1
	}
	return "", false
}
