// Package caddy builds Caddy JSON configuration and talks to Caddy's Admin API.
//
// Docklet owns Caddy's configuration: the database is the source of truth and
// the full config is (re)loaded with POST /load whenever it drifts.
package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
)

// Route maps a domain to an upstream dial address (host:port).
type Route struct {
	ID       string // stable identifier, exposed as Caddy "@id"
	Domain   string
	Upstream string
}

// Options are global settings for the generated config.
type Options struct {
	AdminListen string // must match the address the client talks to
	ACMEEmail   string
	HTTPPort    int // 0 = Caddy default (80)
	HTTPSPort   int // 0 = Caddy default (443)
}

// BuildConfig returns the complete Caddy config for routes. With no routes the
// HTTP app is omitted so Caddy does not bind ports 80/443 needlessly.
func BuildConfig(routes []Route, o Options) map[string]any {
	cfg := map[string]any{
		"admin": map[string]any{"listen": o.AdminListen},
	}
	if len(routes) == 0 {
		return cfg
	}

	httpsPort := o.HTTPSPort
	if httpsPort == 0 {
		httpsPort = 443
	}
	caddyRoutes := make([]any, 0, len(routes))
	for _, r := range routes {
		caddyRoutes = append(caddyRoutes, map[string]any{
			"@id":   "docklet-" + r.ID,
			"match": []any{map[string]any{"host": []any{r.Domain}}},
			"handle": []any{map[string]any{
				"handler":   "reverse_proxy",
				"upstreams": []any{map[string]any{"dial": r.Upstream}},
			}},
			"terminal": true,
		})
	}

	httpApp := map[string]any{
		"servers": map[string]any{
			"docklet": map[string]any{
				"listen": []any{fmt.Sprintf(":%d", httpsPort)},
				"routes": caddyRoutes,
			},
		},
	}
	if o.HTTPPort != 0 {
		httpApp["http_port"] = o.HTTPPort
	}
	if o.HTTPSPort != 0 {
		httpApp["https_port"] = o.HTTPSPort
	}
	apps := map[string]any{"http": httpApp}

	if o.ACMEEmail != "" {
		apps["tls"] = map[string]any{
			"automation": map[string]any{
				"policies": []any{map[string]any{
					"issuers": []any{
						map[string]any{"module": "acme", "email": o.ACMEEmail},
					},
				}},
			},
		}
	}
	cfg["apps"] = apps
	return cfg
}

// Client talks to the Caddy Admin API.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient returns a client for the admin endpoint, e.g. http://localhost:2019.
func NewClient(base string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("caddy: admin URL must look like http://localhost:2019, got %q", base)
	}
	return &Client{
		base: strings.TrimRight(base, "/"),
		hc:   &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// AdminListen is the host:port to keep in the "admin" block.
func (c *Client) AdminListen() string {
	u, _ := url.Parse(c.base)
	return u.Host
}

// Config returns Caddy's current config (nil if empty).
func (c *Client) Config(ctx context.Context) (any, error) {
	// #nosec G704 -- base is the operator-configured Caddy admin URL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/config/", nil)
	if err != nil {
		return nil, err
	}
	res, err := c.hc.Do(req) // #nosec G704 -- operator-configured admin URL
	if err != nil {
		return nil, fmt.Errorf("caddy: unreachable: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("caddy: GET /config/: %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("caddy: decode config: %w", err)
	}
	return v, nil
}

// Load replaces Caddy's entire config.
func (c *Client) Load(ctx context.Context, cfg any) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	// #nosec G704 -- base is the operator-configured Caddy admin URL
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/load", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.hc.Do(req) // #nosec G704 -- operator-configured admin URL
	if err != nil {
		return fmt.Errorf("caddy: unreachable: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		return fmt.Errorf("caddy: load rejected: %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// Equal compares two configs semantically (key order and number types ignored).
func Equal(a, b any) bool {
	return reflect.DeepEqual(normalize(a), normalize(b))
}

func normalize(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
