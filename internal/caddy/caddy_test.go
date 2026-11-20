package caddy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAdmin emulates the parts of Caddy's admin API Docklet uses.
type fakeAdmin struct {
	mu    sync.Mutex
	cfg   any
	loads int
	down  bool
}

func (f *fakeAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/config/":
		_ = json.NewEncoder(w).Encode(f.cfg)
	case r.Method == http.MethodPost && r.URL.Path == "/load":
		b, _ := io.ReadAll(r.Body)
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.cfg = v
		f.loads++
	default:
		http.NotFound(w, r)
	}
}

type staticSource struct {
	mu     sync.Mutex
	routes []Route
	opts   Options
}

func (s *staticSource) DesiredRoutes(context.Context) ([]Route, Options, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.routes, s.opts, nil
}

func setup(t *testing.T) (*fakeAdmin, *Client, *staticSource, *Reconciler) {
	t.Helper()
	fa := &fakeAdmin{}
	ts := httptest.NewServer(fa)
	t.Cleanup(ts.Close)
	c, err := NewClient(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	src := &staticSource{}
	rec := NewReconciler(c, src, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	return fa, c, src, rec
}

func TestBuildConfigEmptyHasOnlyAdmin(t *testing.T) {
	cfg := BuildConfig(nil, Options{AdminListen: "localhost:2019"})
	if _, ok := cfg["apps"]; ok {
		t.Fatal("empty config should not include apps")
	}
}

func TestBuildConfigRoutesAndEmail(t *testing.T) {
	cfg := BuildConfig([]Route{{ID: "panel", Domain: "panel.example.com", Upstream: "127.0.0.1:3000"}},
		Options{AdminListen: "localhost:2019", ACMEEmail: "ops@example.com"})
	b, _ := json.Marshal(cfg)
	s := string(b)
	for _, want := range []string{
		`"@id":"docklet-panel"`, `"host":["panel.example.com"]`, `"dial":"127.0.0.1:3000"`,
		`"listen":[":443"]`, `"email":"ops@example.com"`, `"listen":"localhost:2019"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("config missing %s\n%s", want, s)
		}
	}
}

func TestEqualIgnoresKeyOrderAndNumberTypes(t *testing.T) {
	a := map[string]any{"x": 1, "y": []any{"a"}}
	var b any
	_ = json.Unmarshal([]byte(`{"y":["a"],"x":1.0}`), &b)
	if !Equal(a, b) {
		t.Fatal("expected equal")
	}
	if Equal(a, map[string]any{"x": 2}) {
		t.Fatal("expected different")
	}
}

func TestReconcileLoadsOnlyOnDrift(t *testing.T) {
	fa, _, src, rec := setup(t)
	ctx := context.Background()
	src.routes = []Route{{ID: "panel", Domain: "p.example.com", Upstream: "127.0.0.1:3000"}}

	if err := rec.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rec.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if fa.loads != 1 {
		t.Fatalf("loads = %d, want 1 (no reload when unchanged)", fa.loads)
	}

	// Simulate a Caddy restart with an empty config.
	fa.mu.Lock()
	fa.cfg = nil
	fa.mu.Unlock()
	if err := rec.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if fa.loads != 2 {
		t.Fatalf("loads = %d, want 2 after drift", fa.loads)
	}
	b, _ := json.Marshal(fa.cfg)
	if !strings.Contains(string(b), "p.example.com") {
		t.Fatalf("route not restored: %s", b)
	}
	if st := rec.Status(); !st.Reachable || st.LastError != "" || st.LastSync.IsZero() {
		t.Fatalf("status = %+v", st)
	}
}

func TestReconcileReportsUnreachable(t *testing.T) {
	fa, _, _, rec := setup(t)
	fa.down = true
	if err := rec.Once(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if st := rec.Status(); st.Reachable || st.LastError == "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestNewClientValidatesURL(t *testing.T) {
	for _, u := range []string{"", "localhost:2019", "https://x", "ftp://h"} {
		if _, err := NewClient(u); err == nil {
			t.Errorf("accepted %q", u)
		}
	}
}
