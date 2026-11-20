package server

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/metrics"
)

func TestMetricsEndpoints(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "d.db")
	var mc *metrics.Collector
	env := startPanelServer(t, dbPath, true, func(s *Server) {
		mc = metrics.New(s.st, nil, metrics.Config{DiskPath: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		mc.Interval = 50 * time.Millisecond
		s.metrics = mc
	})
	c := env.client(t)

	code, st := c.do("GET", "/api/metrics/status", nil)
	if code != 200 || st["containerSource"] != "none" {
		t.Fatalf("status: %d %v", code, st)
	}
	waitFor(t, "host samples", func() bool {
		_, body := c.do("GET", "/api/metrics/recent?scope=host", nil)
		s, _ := body["samples"].([]any)
		return len(s) >= 2
	})
	if code, _ := c.do("GET", "/api/metrics/history?scope=host&range=1h", nil); code != 200 {
		t.Fatalf("history: %d", code)
	}
	for _, q := range []string{"scope=nope", "scope=app:x", "scope=host&range=2y"} {
		if code, _ := c.do("GET", "/api/metrics/history?"+q, nil); code != 400 {
			t.Errorf("%s accepted (%d)", q, code)
		}
	}

	req, _ := http.NewRequest("GET", c.base+"/api/metrics/stream", nil)
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, res)
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, `data: {"ts":`) || !strings.Contains(line, `"host":`) {
		t.Fatalf("first event %q %v", line, err)
	}

	anon := &client{t: t, base: c.base, http: http.DefaultClient}
	if code, _ := anon.do("GET", "/api/metrics/recent?scope=host", nil); code != 401 {
		t.Fatalf("anonymous access: %d", code)
	}
}
