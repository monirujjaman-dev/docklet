package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

type fakeCaddy struct {
	mu  sync.Mutex
	cfg any
}

func (f *fakeCaddy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/config/":
		_ = json.NewEncoder(w).Encode(f.cfg)
	case r.Method == http.MethodPost && r.URL.Path == "/load":
		b, _ := io.ReadAll(r.Body)
		var v any
		_ = json.Unmarshal(b, &v)
		f.cfg = v
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCaddy) json() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(f.cfg)
	return string(b)
}

type panelEnv struct {
	srv     *Server
	st      *store.Store
	caddy   *fakeCaddy
	cancel  context.CancelFunc
	done    chan error
	runErr  error
	probeOK bool
	mu      sync.Mutex
}

// startPanelServer runs a real Server on 0.0.0.0:0 with a fake Caddy and a
// prober that, instead of going through Caddy+TLS, calls the probe endpoint
// directly on loopback (what Caddy would proxy to).
func startPanelServer(t *testing.T, dbPath string, probeOK bool, extra ...Option) *panelEnv {
	t.Helper()
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(bytes.Repeat([]byte{2}, secrets.KeySize))
	fc := &fakeCaddy{}
	cts := httptest.NewServer(fc)
	t.Cleanup(cts.Close)

	env := &panelEnv{st: st, caddy: fc, probeOK: probeOK, done: make(chan error, 1)}
	var srv *Server
	prober := func(ctx context.Context, url string) (string, error) {
		env.mu.Lock()
		ok := env.probeOK
		env.mu.Unlock()
		if !ok {
			return "", errors.New("dial tcp: connection refused")
		}
		_, rest, _ := strings.Cut(strings.TrimPrefix(url, "https://"), "/")
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+loopbackAddr(srv.lm.port())+"/"+rest, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return string(b), nil
	}
	srv, err = New(Config{Addr: "0.0.0.0:0", CaddyAdmin: cts.URL},
		slog.New(slog.NewTextHandler(io.Discard, nil)), st, box,
		append([]Option{WithProber(prober), WithPanelVerify(time.Second, 20*time.Millisecond)}, append([]Option{WithCaddyInterval(50 * time.Millisecond)}, extra...)...)...)
	if err != nil {
		t.Fatal(err)
	}
	env.srv = srv
	ctx, cancel := context.WithCancel(context.Background())
	env.cancel = cancel
	go func() { env.done <- srv.Run(ctx) }()
	waitFor(t, "listening", func() bool { return srv.Addr() != "" })
	t.Cleanup(env.stop)
	return env
}

func (e *panelEnv) stop() {
	if e.cancel != nil {
		e.cancel()
		e.runErr = <-e.done
		e.cancel = nil
		_ = e.st.Close()
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *panelEnv) client(t *testing.T) *client {
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: "http://" + loopbackAddr(e.srv.lm.port()), http: &http.Client{Jar: jar}}
	if code, body := c.do("POST", "/api/setup", map[string]string{"username": "admin", "password": adminPW}); code != http.StatusCreated {
		t.Fatalf("setup: %d %v", code, body)
	}
	return c
}

func (e *panelEnv) status(t *testing.T, c *client) map[string]any {
	_, body := c.do("GET", "/api/settings/panel", nil)
	return body
}

func TestPanelDomainVerifiedRestrictsToLoopback(t *testing.T) {
	env := startPanelServer(t, filepath.Join(t.TempDir(), "d.db"), true)
	c := env.client(t)
	if strings.HasPrefix(env.srv.Addr(), "127.0.0.1:") {
		t.Fatalf("should start public, got %s", env.srv.Addr())
	}

	code, body := c.do("PUT", "/api/settings/panel", map[string]string{"domain": "Panel.Example.com", "acmeEmail": "ops@example.com"})
	if code != http.StatusAccepted || body["domain"] != "panel.example.com" {
		t.Fatalf("put: %d %v", code, body)
	}
	waitFor(t, "active", func() bool { return env.status(t, c)["status"] == "active" })
	waitFor(t, "loopback listener", func() bool { return strings.HasPrefix(env.srv.Addr(), "127.0.0.1:") })

	// The reconciler applies the route asynchronously.
	for _, want := range []string{`"panel.example.com"`, `"dial":"` + loopbackAddr(env.srv.lm.port()) + `"`, `"email":"ops@example.com"`} {
		waitFor(t, "caddy config with "+want, func() bool { return strings.Contains(env.caddy.json(), want) })
	}
	acts := strings.Join(actionsOf(t, env.st), ",")
	if !strings.Contains(acts, "settings.panel_domain_set") || !strings.Contains(acts, "settings.panel_verified") {
		t.Fatalf("audit = %s", acts)
	}

	// Switching to a new domain re-opens direct access until it verifies.
	env.mu.Lock()
	env.probeOK = false
	env.mu.Unlock()
	if code, _ := c.do("PUT", "/api/settings/panel", map[string]string{"domain": "new.example.com"}); code != http.StatusAccepted {
		t.Fatalf("change domain: %d", code)
	}
	if strings.HasPrefix(env.srv.Addr(), "127.0.0.1:") {
		t.Fatal("direct access not restored while new domain is unverified")
	}
	waitFor(t, "failed", func() bool { return env.status(t, c)["status"] == "failed" })
	if strings.HasPrefix(env.srv.Addr(), "127.0.0.1:") {
		t.Fatal("locked to loopback after failed domain change")
	}

	// Changing only the e-mail of an active domain keeps it active.
	env.mu.Lock()
	env.probeOK = true
	env.mu.Unlock()
	c.do("PUT", "/api/settings/panel", map[string]string{"domain": "new.example.com"})
	waitFor(t, "active again", func() bool { return env.status(t, c)["status"] == "active" })
	if code, body := c.do("PUT", "/api/settings/panel", map[string]string{"domain": "new.example.com", "acmeEmail": "a@example.com"}); code != http.StatusAccepted || body["status"] != "active" {
		t.Fatalf("email-only change: %d %v", code, body)
	}

	// Removing the domain re-opens direct access and clears Caddy routes.
	if code, _ := c.do("DELETE", "/api/settings/panel", nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if strings.HasPrefix(env.srv.Addr(), "127.0.0.1:") {
		t.Fatalf("still loopback after delete: %s", env.srv.Addr())
	}
	waitFor(t, "routes removed", func() bool { return !strings.Contains(env.caddy.json(), "example.com") })
}

func TestPanelDomainFailedKeepsPublicAccess(t *testing.T) {
	env := startPanelServer(t, filepath.Join(t.TempDir(), "d.db"), false)
	c := env.client(t)
	c.do("PUT", "/api/settings/panel", map[string]string{"domain": "panel.example.com"})
	waitFor(t, "failed", func() bool { return env.status(t, c)["status"] == "failed" })
	st := env.status(t, c)
	if msg, _ := st["message"].(string); !strings.Contains(msg, "DNS") {
		t.Fatalf("message = %q", msg)
	}
	if strings.HasPrefix(env.srv.Addr(), "127.0.0.1:") {
		t.Fatal("listener restricted despite failed verification")
	}
	if !strings.Contains(strings.Join(actionsOf(t, env.st), ","), "settings.panel_verify_failed") {
		t.Fatal("failure not audited")
	}
}

func TestPanelActiveAtStartupBindsLoopback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	env := startPanelServer(t, path, true)
	c := env.client(t)
	c.do("PUT", "/api/settings/panel", map[string]string{"domain": "panel.example.com"})
	waitFor(t, "active", func() bool { return env.status(t, c)["status"] == "active" })
	env.stop()

	env2 := startPanelServer(t, path, true)
	if !strings.HasPrefix(env2.srv.Addr(), "127.0.0.1:") {
		t.Fatalf("restart should bind loopback, got %s", env2.srv.Addr())
	}
	waitFor(t, "routes restored on start", func() bool { return strings.Contains(env2.caddy.json(), "panel.example.com") })
}

func TestPanelValidationAndProbe(t *testing.T) {
	env := startPanelServer(t, filepath.Join(t.TempDir(), "d.db"), false)
	c := env.client(t)
	for _, d := range []string{"", "localhost", "1.2.3.4", "bad_domain.com", "-x.example.com", "a..b.com", "https://x.com"} {
		if code, _ := c.do("PUT", "/api/settings/panel", map[string]string{"domain": d}); code != 400 {
			t.Errorf("domain %q accepted (%d)", d, code)
		}
	}
	if code, _ := c.do("PUT", "/api/settings/panel", map[string]string{"domain": "ok.example.com", "acmeEmail": "not-an-email"}); code != 400 {
		t.Error("bad email accepted")
	}
	anon := &client{t: t, base: c.base, http: http.DefaultClient}
	if code, _ := anon.do("GET", "/api/panel/probe?nonce=guess", nil); code != 404 {
		t.Errorf("probe with wrong nonce: %d", code)
	}
	if code, _ := anon.do("PUT", "/api/settings/panel", map[string]string{"domain": "x.example.com"}); code != 401 {
		t.Errorf("anonymous put: %d", code)
	}
}

func TestValidateDomain(t *testing.T) {
	got, err := validateDomain("  Panel.Example.COM. ")
	if err != nil || got != "panel.example.com" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func actionsOf(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB.Query("SELECT action FROM audit_log ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		out = append(out, a)
	}
	return out
}

func TestResolveClientTrustedProxies(t *testing.T) {
	caddyNet := []netip.Prefix{netip.MustParsePrefix("172.30.0.2/32")}
	mk := func(remote, xff, proto string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		return r
	}
	cases := []struct {
		remote, xff, proto string
		trusted            []netip.Prefix
		ip                 string
		https              bool
	}{
		{"172.30.0.2:5000", "1.2.3.4", "https", caddyNet, "1.2.3.4", true},
		{"172.30.0.9:5000", "1.2.3.4", "https", caddyNet, "172.30.0.9", false}, // not the proxy: headers ignored
		{"127.0.0.1:5000", "9.9.9.9, 5.6.7.8", "https", nil, "5.6.7.8", true},
		{"10.0.0.5:5000", "1.2.3.4", "https", nil, "10.0.0.5", false},
	}
	for _, c := range cases {
		ci := resolveClient(mk(c.remote, c.xff, c.proto), c.trusted)
		if ci.ip != c.ip || ci.https != c.https {
			t.Errorf("%s with %v: got %+v, want ip=%s https=%v", c.remote, c.trusted, ci, c.ip, c.https)
		}
	}
}

func TestPanelUpstreamOverride(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	box, _ := secrets.New(bytes.Repeat([]byte{4}, secrets.KeySize))
	s, err := New(Config{Addr: "0.0.0.0:0", PanelUpstream: "172.30.0.3:3000"}, slog.New(slog.NewTextHandler(io.Discard, nil)), st, box)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = st.Q.PutSetting(ctx, db.PutSettingParams{Key: SettingPanelDomain, Value: "panel.example.com", UpdatedAt: 1})
	routes, _, err := s.DesiredRoutes(ctx)
	if err != nil || len(routes) != 1 || routes[0].Upstream != "172.30.0.3:3000" {
		t.Fatalf("routes = %+v, %v", routes, err)
	}
	if err := s.restrictToLoopback(); err != nil {
		t.Fatal(err)
	}
}
