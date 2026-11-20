package caddy

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestRealCaddy runs against a live Caddy when DOCKLET_TEST_CADDY_ADMIN is set,
// e.g. DOCKLET_TEST_CADDY_ADMIN=http://localhost:2019. It checks that the
// config Caddy reports back equals what we loaded, so the reconciler does not
// reload on every pass.
func TestRealCaddy(t *testing.T) {
	admin := os.Getenv("DOCKLET_TEST_CADDY_ADMIN")
	if admin == "" {
		t.Skip("set DOCKLET_TEST_CADDY_ADMIN to run against a real Caddy")
	}
	c, err := NewClient(admin)
	if err != nil {
		t.Fatal(err)
	}
	src := &staticSource{
		routes: []Route{{ID: "panel", Domain: "panel.test.localhost", Upstream: "127.0.0.1:3999"}},
		opts:   Options{ACMEEmail: "ops@example.com", HTTPPort: 18080, HTTPSPort: 18443},
	}
	rec := NewReconciler(c, src, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	ctx := context.Background()
	if err := rec.Once(ctx); err != nil {
		t.Fatal(err)
	}

	routes, opts, _ := src.DesiredRoutes(ctx)
	opts.AdminListen = c.AdminListen()
	current, err := c.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(current, BuildConfig(routes, opts)) {
		t.Fatalf("Caddy normalised the config; reconciler would reload forever.\ngot: %v", current)
	}
	t.Cleanup(func() { _ = c.Load(ctx, BuildConfig(nil, Options{AdminListen: c.AdminListen()})) })
}
