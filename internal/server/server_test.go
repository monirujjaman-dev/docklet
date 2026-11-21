package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts, _ := newTestEnv(t)
	return ts
}

func closeBody(t *testing.T, res *http.Response) {
	t.Helper()
	if err := res.Body.Close(); err != nil {
		t.Errorf("closing body: %v", err)
	}
}

func TestHealth(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status field = %q, want ok", body["status"])
	}
}

func TestVersion(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, res)
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["version"] == "" || len(body) != 1 {
		t.Fatalf("public version endpoint = %v", body)
	}
}

func TestUnknownAPIRouteIs404JSON(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, res)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
}

func TestSPAFallbackServesHTML(t *testing.T) {
	ts := newTestServer(t)
	for _, p := range []string{"/", "/apps/some-client-route"} {
		res, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		closeBody(t, res)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", p, res.StatusCode)
		}
		if !strings.Contains(strings.ToLower(string(b)), "<!doctype html>") {
			t.Fatalf("%s: body is not HTML", p)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, res)
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy", "Cross-Origin-Opener-Policy", "Permissions-Policy"} {
		if res.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	csp := res.Header.Get("Content-Security-Policy")
	for _, d := range []string{"script-src 'self'", "object-src 'none'", "base-uri 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, d) {
			t.Errorf("CSP lacks %q: %s", d, csp)
		}
	}
	if res.Header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS sent over plain HTTP")
	}
	// Behind Caddy (a trusted proxy on loopback) over HTTPS: HSTS is sent.
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, res)
	if !strings.HasPrefix(res.Header.Get("Strict-Transport-Security"), "max-age=") {
		t.Error("HSTS missing over HTTPS")
	}
}

func TestSPARejectsPost(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Post(ts.URL+"/", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, res)
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.StatusCode)
	}
}
