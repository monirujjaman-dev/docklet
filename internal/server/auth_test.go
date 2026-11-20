package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- generating TOTP codes in tests
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type testEnv struct {
	ts    *httptest.Server
	st    *store.Store
	clock *fakeClock
}

func newTestEnv(t *testing.T) (*httptest.Server, *testEnv) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "docklet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := secrets.New(bytes.Repeat([]byte{1}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	s, err := New(Config{Addr: "127.0.0.1:0"}, slog.New(slog.NewTextHandler(io.Discard, nil)), st, box, WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, &testEnv{ts: ts, st: st, clock: clock}
}

// client is a browser-like client with a cookie jar and CSRF token.
type client struct {
	t    *testing.T
	base string
	http *http.Client
	csrf string
}

func newClient(t *testing.T, env *testEnv) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: env.ts.URL, http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		req.Header.Set(csrfHeader, c.csrf)
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer closeBody(c.t, res)
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if tok, ok := out["csrfToken"].(string); ok {
		c.csrf = tok
	}
	return res.StatusCode, out
}

const adminPW = "correct horse battery"

func setupAdmin(t *testing.T, env *testEnv) *client {
	t.Helper()
	c := newClient(t, env)
	code, body := c.do("POST", "/api/setup", map[string]string{"username": "admin", "password": adminPW})
	if code != http.StatusCreated {
		t.Fatalf("setup: %d %v", code, body)
	}
	return c
}

func auditActions(t *testing.T, env *testEnv) []string {
	t.Helper()
	rows, err := env.st.Q.AuditEntriesFrom(context.Background(), db.AuditEntriesFromParams{AfterID: 0, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Action)
	}
	return out
}

func TestSetupOnlyOnce(t *testing.T) {
	_, env := newTestEnv(t)
	c := newClient(t, env)
	if code, body := c.do("GET", "/api/setup/status", nil); code != 200 || body["needsSetup"] != true {
		t.Fatalf("status: %d %v", code, body)
	}
	if code, _ := c.do("POST", "/api/setup", map[string]string{"username": "admin", "password": "short"}); code != 400 {
		t.Fatalf("weak password: %d", code)
	}
	c = setupAdmin(t, env)
	if code, body := c.do("GET", "/api/auth/me", nil); code != 200 || body["user"].(map[string]any)["username"] != "admin" {
		t.Fatalf("me after setup: %d %v", code, body)
	}
	other := newClient(t, env)
	if code, _ := other.do("POST", "/api/setup", map[string]string{"username": "evil", "password": "another password"}); code != http.StatusConflict {
		t.Fatalf("second setup: %d, want 409", code)
	}
	if _, body := other.do("GET", "/api/setup/status", nil); body["needsSetup"] != false {
		t.Fatal("needsSetup still true")
	}
}

func TestLoginLogout(t *testing.T) {
	_, env := newTestEnv(t)
	setupAdmin(t, env)
	c := newClient(t, env)
	if code, _ := c.do("GET", "/api/auth/me", nil); code != 401 {
		t.Fatalf("me without session: %d", code)
	}
	if code, body := c.do("POST", "/api/auth/login", map[string]string{"username": "ADMIN", "password": adminPW}); code != 200 {
		t.Fatalf("login: %d %v", code, body)
	}
	if code, _ := c.do("GET", "/api/auth/me", nil); code != 200 {
		t.Fatalf("me: %d", code)
	}
	if code, _ := c.do("POST", "/api/auth/logout", nil); code != 200 {
		t.Fatalf("logout: %d", code)
	}
	if code, _ := c.do("GET", "/api/auth/me", nil); code != 401 {
		t.Fatalf("me after logout: %d", code)
	}
	got := strings.Join(auditActions(t, env), ",")
	if got != "setup.completed,auth.login,auth.logout" {
		t.Fatalf("audit = %s", got)
	}
}

func TestLockoutAfterFiveFailures(t *testing.T) {
	_, env := newTestEnv(t)
	setupAdmin(t, env)
	c := newClient(t, env)
	for i := 1; i <= 4; i++ {
		if code, _ := c.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "wrong password"}); code != 401 {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	if code, body := c.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "wrong password"}); code != http.StatusLocked || body["locked"] != true {
		t.Fatalf("5th attempt: %d %v", code, body)
	}
	// Correct password is still rejected while locked.
	if code, _ := c.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPW}); code != http.StatusLocked {
		t.Fatalf("locked login: %d", code)
	}
	env.clock.Advance(lockoutDuration + time.Second)
	// One typo after the lock expires must not re-lock immediately.
	if code, _ := c.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "wrong password"}); code != 401 {
		t.Fatalf("first failure after lockout: %d, want 401", code)
	}
	if code, _ := c.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPW}); code != 200 {
		t.Fatalf("after lockout: %d", code)
	}
	acts := strings.Join(auditActions(t, env), ",")
	if !strings.Contains(acts, "auth.locked") || strings.Count(acts, "auth.login_failed") != 6 {
		t.Fatalf("audit = %s", acts)
	}
}

func TestUnknownUserSameError(t *testing.T) {
	_, env := newTestEnv(t)
	setupAdmin(t, env)
	c := newClient(t, env)
	code1, b1 := c.do("POST", "/api/auth/login", map[string]string{"username": "nobody", "password": "whatever pw"})
	code2, b2 := c.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "whatever pw"})
	if code1 != 401 || code2 != 401 || b1["error"] != b2["error"] {
		t.Fatalf("responses differ: %d %v / %d %v", code1, b1, code2, b2)
	}
}

func TestIPRateLimit(t *testing.T) {
	_, env := newTestEnv(t)
	setupAdmin(t, env) // uses 1 token
	c := newClient(t, env)
	limited := false
	for i := 0; i < 12; i++ {
		code, _ := c.do("POST", "/api/auth/login", map[string]string{"username": fmt.Sprintf("u%d", i), "password": "whatever pw"})
		if code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("rate limit never triggered")
	}
}

func TestCSRFRequired(t *testing.T) {
	_, env := newTestEnv(t)
	c := setupAdmin(t, env)
	tok := c.csrf
	c.csrf = ""
	if code, _ := c.do("POST", "/api/auth/logout", nil); code != http.StatusForbidden {
		t.Fatalf("logout without csrf: %d", code)
	}
	c.csrf = "bogus"
	if code, _ := c.do("POST", "/api/auth/logout", nil); code != http.StatusForbidden {
		t.Fatalf("logout with bad csrf: %d", code)
	}
	c.csrf = tok
	if code, _ := c.do("POST", "/api/auth/logout", nil); code != 200 {
		t.Fatalf("logout with csrf: %d", code)
	}
}

func TestCrossOriginBlocked(t *testing.T) {
	ts, _ := newTestEnv(t)
	req, _ := http.NewRequest("POST", ts.URL+"/api/auth/login", strings.NewReader(`{"username":"a","password":"b"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, res)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestRejectsNonJSON(t *testing.T) {
	ts, _ := newTestEnv(t)
	res, err := http.Post(ts.URL+"/api/auth/login", "application/x-www-form-urlencoded", strings.NewReader("username=a"))
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, res)
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestSessionIdleExpiry(t *testing.T) {
	_, env := newTestEnv(t)
	c := setupAdmin(t, env)
	env.clock.Advance(sessionIdleTimeout + time.Minute)
	if code, _ := c.do("GET", "/api/auth/me", nil); code != 401 {
		t.Fatalf("idle session still valid: %d", code)
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	_, env := newTestEnv(t)
	a := setupAdmin(t, env)
	b := newClient(t, env)
	if code, _ := b.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPW}); code != 200 {
		t.Fatal("login b")
	}
	if code, _ := a.do("POST", "/api/account/password", map[string]string{"currentPassword": "wrong", "newPassword": "brand new password"}); code != 403 {
		t.Fatalf("wrong current: %d", code)
	}
	if code, body := a.do("POST", "/api/account/password", map[string]string{"currentPassword": adminPW, "newPassword": "brand new password"}); code != 200 {
		t.Fatalf("change: %d %v", code, body)
	}
	if code, _ := a.do("GET", "/api/auth/me", nil); code != 200 {
		t.Fatal("current session revoked")
	}
	if code, _ := b.do("GET", "/api/auth/me", nil); code != 401 {
		t.Fatal("other session survived password change")
	}
}

func totpCode(t *testing.T, secret string, now time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(now.Unix()/30)) // #nosec G115 -- positive test timestamps
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[off:off+4])&0x7fffffff)%1_000_000)
}

func TestTOTPFlow(t *testing.T) {
	_, env := newTestEnv(t)
	c := setupAdmin(t, env)

	code, body := c.do("POST", "/api/account/totp/setup", map[string]string{})
	if code != 200 {
		t.Fatalf("totp setup: %d %v", code, body)
	}
	secret := body["secret"].(string)
	if code, _ := c.do("POST", "/api/account/totp/enable", map[string]string{"code": "000000"}); code != 400 {
		t.Fatalf("bad code accepted: %d", code)
	}
	if code, body := c.do("POST", "/api/account/totp/enable", map[string]string{"code": totpCode(t, secret, env.clock.Now())}); code != 200 {
		t.Fatalf("enable: %d %v", code, body)
	}

	// Secret must not be stored in plaintext.
	var raw []byte
	if err := env.st.DB.QueryRow("SELECT totp_secret_enc FROM users WHERE id = 1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("totp secret stored in plaintext")
	}

	env.clock.Advance(time.Minute) // move past the step used to enable
	l := newClient(t, env)
	code, body = l.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPW})
	if code != 401 || body["totpRequired"] != true {
		t.Fatalf("expected totp challenge: %d %v", code, body)
	}
	good := totpCode(t, secret, env.clock.Now())
	if code, _ := l.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPW, "totp": good}); code != 200 {
		t.Fatalf("totp login: %d", code)
	}
	l2 := newClient(t, env)
	if code, _ := l2.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPW, "totp": good}); code != 401 {
		t.Fatalf("replayed code accepted: %d", code)
	}

	if code, _ := c.do("POST", "/api/account/totp/disable", map[string]string{"password": "nope"}); code != 403 {
		t.Fatalf("disable with wrong pw: %d", code)
	}
	if code, _ := c.do("POST", "/api/account/totp/disable", map[string]string{"password": adminPW}); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	acts := strings.Join(auditActions(t, env), ",")
	for _, want := range []string{audit.ActionTOTPEnabled, audit.ActionTOTPDisabled} {
		if !strings.Contains(acts, want) {
			t.Fatalf("audit missing %s: %s", want, acts)
		}
	}
}

func TestAuditEndpoints(t *testing.T) {
	_, env := newTestEnv(t)
	c := setupAdmin(t, env)
	for i := 0; i < 3; i++ {
		c.do("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPW})
	}
	code, body := c.do("GET", "/api/audit?limit=2", nil)
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	entries := body["entries"].([]any)
	if len(entries) != 2 || body["nextBefore"] == nil {
		t.Fatalf("pagination: %v", body)
	}
	first := entries[0].(map[string]any)
	if first["action"] != "auth.login" {
		t.Fatalf("newest first expected: %v", first)
	}
	if code, body := c.do("GET", "/api/audit/verify", nil); code != 200 || body["ok"] != true {
		t.Fatalf("verify: %d %v", code, body)
	}
	anon := newClient(t, env)
	if code, _ := anon.do("GET", "/api/audit", nil); code != 401 {
		t.Fatalf("anonymous audit access: %d", code)
	}
}
