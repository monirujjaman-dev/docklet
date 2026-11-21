package docker

import (
	"bufio"
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
)

// Stream is a hijacked, raw TTY stream to a container or exec.
type Stream struct {
	conn net.Conn
	br   *bufio.Reader
}

func (s *Stream) Read(p []byte) (int, error)  { return s.br.Read(p) }
func (s *Stream) Write(p []byte) (int, error) { return s.conn.Write(p) }

// Close closes the stream.
func (s *Stream) Close() error { return s.conn.Close() }

// CloseWrite signals end of input.
func (s *Stream) CloseWrite() error {
	if cw, ok := s.conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// hijack sends a request asking Docker to upgrade the connection to a raw
// stream (as the docker CLI does for attach and exec).
func (c *Client) hijack(ctx context.Context, path string, q url.Values, body any) (*Stream, error) {
	if c.dial == nil {
		return nil, errors.New("docker: interactive streams need a socket connection")
	}
	p, err := c.prefix(ctx)
	if err != nil {
		return nil, err
	}
	u := strings.TrimPrefix(p, c.base) + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://docker"+u, strings.NewReader(string(b))) //nolint:noctx // written to a raw conn
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, req) //nolint:bodyclose // the body is the hijacked stream; closing conn closes it
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols && res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		_ = conn.Close()
		if res.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("docker: %s: %s: %s", path, res.Status, strings.TrimSpace(string(msg)))
	}
	return &Stream{conn: conn, br: br}, nil
}

// ExecTTY creates an interactive exec with a terminal and starts it.
func (c *Client) ExecTTY(ctx context.Context, id string, cmd, env []string, workDir string) (execID string, s *Stream, err error) {
	var created struct {
		ID string `json:"Id"`
	}
	spec := map[string]any{"Cmd": cmd, "Env": env, "Tty": true, "AttachStdin": true, "AttachStdout": true, "AttachStderr": true}
	if workDir != "" {
		spec["WorkingDir"] = workDir
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", nil, spec, &created); err != nil {
		return "", nil, err
	}
	s, err = c.hijack(ctx, "/exec/"+created.ID+"/start", nil, map[string]any{"Detach": false, "Tty": true})
	return created.ID, s, err
}

// ExecResize sets the terminal size of an exec.
func (c *Client) ExecResize(ctx context.Context, execID string, rows, cols int) error {
	return c.do(ctx, http.MethodPost, "/exec/"+url.PathEscape(execID)+"/resize",
		url.Values{"h": {strconv.Itoa(rows)}, "w": {strconv.Itoa(cols)}}, nil, nil)
}

// ExecRunning reports whether an exec's process is still running.
func (c *Client) ExecRunning(ctx context.Context, execID string) (bool, error) {
	var info struct {
		Running bool `json:"Running"`
	}
	err := c.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(execID)+"/json", nil, nil, &info)
	return info.Running, err
}

// ContainerAttach attaches to a created container's TTY (before starting it).
func (c *Client) ContainerAttach(ctx context.Context, id string) (*Stream, error) {
	return c.hijack(ctx, "/containers/"+url.PathEscape(id)+"/attach",
		url.Values{"stream": {"1"}, "stdin": {"1"}, "stdout": {"1"}, "stderr": {"1"}}, nil)
}

// ContainerResize sets a container's TTY size.
func (c *Client) ContainerResize(ctx context.Context, id string, rows, cols int) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/resize",
		url.Values{"h": {strconv.Itoa(rows)}, "w": {strconv.Itoa(cols)}}, nil, nil)
}
