package server

import (
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker/dockertest"
)

func TestReauthAndTerminalTicket(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	env := startPanelServer(t, filepath.Join(t.TempDir(), "d.db"), true, WithDocker(dockertest.New()), WithClock(clock))
	c := env.client(t)

	// A fresh login counts as recent.
	if code, o := c.do("POST", "/api/terminal/sessions", map[string]any{"kind": "host"}); code != 200 || o["ticket"] == "" {
		t.Fatalf("ticket: %d %v", code, o)
	}
	if code, _ := c.do("POST", "/api/terminal/sessions", map[string]any{"kind": "nope"}); code != 400 {
		t.Fatalf("bad target: %d", code)
	}
	if code, _ := c.do("POST", "/api/terminal/sessions", map[string]any{"kind": "app", "id": 99}); code != 404 {
		t.Fatalf("missing app: %d", code)
	}
	mu.Lock()
	now = now.Add(11 * time.Minute)
	mu.Unlock()
	code, o := c.do("POST", "/api/terminal/sessions", map[string]any{"kind": "host"})
	if code != http.StatusUnauthorized || o["reauthRequired"] != true {
		t.Fatalf("stale login: %d %v", code, o)
	}
	if code, _ := c.do("PUT", "/api/files/write", map[string]any{"root": "host", "path": "/tmp/x", "text": "x", "sha256": ""}); code != http.StatusUnauthorized {
		t.Fatalf("file write with stale login: %d", code)
	}
	if code, _ := c.do("POST", "/api/auth/reauth", map[string]any{"password": "wrong"}); code != http.StatusForbidden {
		t.Fatalf("wrong password: %d", code)
	}
	if code, _ := c.do("POST", "/api/auth/reauth", map[string]any{"password": adminPW}); code != 200 {
		t.Fatalf("reauth: %d", code)
	}
	if code, _ := c.do("POST", "/api/terminal/sessions", map[string]any{"kind": "host"}); code != 200 {
		t.Fatalf("after reauth: %d", code)
	}

	// The WebSocket refuses other origins and unknown tickets.
	req, _ := http.NewRequest("GET", c.base+"/api/terminal/ws?ticket=x", nil)
	req.Header.Set("Origin", "http://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin: %v %v", res.StatusCode, err)
	}
	_ = res.Body.Close()
	req.Header.Set("Origin", c.base)
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown ticket: %v %v", res.StatusCode, err)
	}
	_ = res.Body.Close()
}
