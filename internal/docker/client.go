// Package docker is a minimal Docker Engine API client over the Unix socket.
//
// It implements only what Docklet needs, using the standard library, instead
// of the official SDK whose dependency tree would grow the binary noticeably.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MinAPIVersion is the oldest Engine API we accept (Docker 25).
const MinAPIVersion = "1.44"

// Sentinel errors mapped from HTTP status codes.
var (
	ErrNotFound = errors.New("docker: not found")
	ErrConflict = errors.New("docker: conflict")
)

// Client talks to dockerd.
type Client struct {
	hc   *http.Client
	base string
	dial func(ctx context.Context) (net.Conn, error) // raw connection, for hijacked streams

	mu      sync.Mutex
	version string
}

// New returns a client for the daemon socket (e.g. /var/run/docker.sock).
func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{hc: &http.Client{Transport: tr}, base: "http://docker", dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
}

// newWithHTTP is used by tests to point the client at an httptest server.
func newWithHTTP(hc *http.Client, base string) *Client {
	return &Client{hc: hc, base: strings.TrimRight(base, "/")}
}

// Ping checks the daemon and negotiates the API version.
func (c *Client) Ping(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/_ping", nil)
	if err != nil {
		return "", err
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("docker: daemon unreachable: %w", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("docker: ping: %s", res.Status)
	}
	v := res.Header.Get("Api-Version")
	if v == "" {
		return "", errors.New("docker: ping: no API version")
	}
	if versionLess(v, MinAPIVersion) {
		return "", fmt.Errorf("docker: Engine API %s is too old; Docker 25+ (API %s) is required", v, MinAPIVersion)
	}
	c.mu.Lock()
	c.version = v
	c.mu.Unlock()
	return v, nil
}

func (c *Client) prefix(ctx context.Context) (string, error) {
	c.mu.Lock()
	v := c.version
	c.mu.Unlock()
	if v == "" {
		var err error
		if v, err = c.Ping(ctx); err != nil {
			return "", err
		}
	}
	return c.base + "/v" + v, nil
}

// request builds and sends a request, returning the open response on 2xx.
func (c *Client) request(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	p, err := c.prefix(ctx)
	if err != nil {
		return nil, err
	}
	u := p + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %s %s: %w", method, path, err)
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 || res.StatusCode == http.StatusNotModified {
		return res, nil
	}
	defer func() { _ = res.Body.Close() }()
	var e struct {
		Message string `json:"message"`
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	_ = json.Unmarshal(b, &e)
	msg := e.Message
	if msg == "" {
		msg = strings.TrimSpace(string(b))
	}
	switch res.StatusCode {
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, msg)
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: %s", ErrConflict, msg)
	}
	return nil, fmt.Errorf("docker: %s %s: %s: %s", method, path, res.Status, msg)
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	res, err := c.request(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if out == nil || res.StatusCode == http.StatusNoContent || res.StatusCode == http.StatusNotModified {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// versionLess compares dotted versions like "1.44" < "1.47".
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}
