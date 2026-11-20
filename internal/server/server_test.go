package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := New(Config{Addr: "127.0.0.1:0"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestHealth(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
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
	defer res.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["version"] == "" {
		t.Fatal("version is empty")
	}
}

func TestUnknownAPIRouteIs404JSON(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
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
		res.Body.Close()
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
	res.Body.Close()
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy"} {
		if res.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
}

func TestSPARejectsPost(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Post(ts.URL+"/", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.StatusCode)
	}
}
