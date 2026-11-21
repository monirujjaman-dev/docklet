package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/apps"
	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/databases"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/host"
	"github.com/monirujjaman-dev/docklet/internal/release"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
	"github.com/monirujjaman-dev/docklet/internal/ws"
)

// The browser terminal: a WebSocket carries raw TTY bytes (binary frames)
// and resize messages (text frames). The host shell runs in a helper
// container that nsenters the host; app and database shells are docker exec.

const (
	recentAuth      = 10 * time.Minute // terminal and file writes need a login this recent
	terminalIdle    = 30 * time.Minute
	ticketLifetime  = time.Minute
	maxTerminalOpen = 8
)

type termTicket struct {
	target    termTarget
	userID    int64
	username  string
	sessionID string
	ip        string
	expires   time.Time
}

type termTarget struct {
	Kind  string `json:"kind"` // host, app, database
	ID    int64  `json:"id,omitempty"`
	Label string `json:"label"`
	ref   string // container id for app/database
}

type terminalState struct {
	mu      sync.Mutex
	tickets map[string]termTicket
	open    int
	reauth  map[string]time.Time // session id -> last password re-entry
}

// recentlyAuthed reports whether the session logged in or re-entered the
// password within recentAuth.
func (s *Server) recentlyAuthed(a *authInfo) bool {
	s.term.mu.Lock()
	defer s.term.mu.Unlock()
	last := time.Unix(a.Session.CreatedAt, 0)
	if t, ok := s.term.reauth[a.Session.ID]; ok && t.After(last) {
		last = t
	}
	return s.now().Sub(last) < recentAuth
}

// requireRecentAuth checks the password unless the login is recent.
func (s *Server) requireRecentAuth(a *authInfo, password string) error {
	if password == "" && s.recentlyAuthed(a) {
		return nil
	}
	if password == "" {
		return &apiError{Status: http.StatusUnauthorized, Msg: "enter your password to continue", Extra: map[string]any{"reauthRequired": true}}
	}
	if err := checkPassword(a.User, password); err != nil {
		return err
	}
	s.term.mu.Lock()
	if s.term.reauth == nil {
		s.term.reauth = map[string]time.Time{}
	}
	s.term.reauth[a.Session.ID] = s.now()
	s.term.mu.Unlock()
	return nil
}

// resolveTarget checks that the target exists and finds its container.
func (s *Server) resolveTarget(ctx context.Context, t termTarget) (termTarget, error) {
	switch t.Kind {
	case "host":
		t.Label = "server"
		return t, nil
	case "app":
		a, err := s.st.Q.GetApp(ctx, t.ID)
		if err != nil {
			return t, errf(http.StatusNotFound, "app not found")
		}
		t.Label = "app " + a.Name
		list, err := s.docker.ContainerList(ctx, map[string]string{apps.LabelApp: strconv.FormatInt(a.ID, 10), apps.LabelManaged: "true"})
		if err != nil {
			return t, err
		}
		for _, c := range list {
			if c.State == "running" {
				t.ref = c.ID
				return t, nil
			}
		}
		return t, errf(http.StatusConflict, "app %s has no running container", a.Name)
	case "database":
		d, err := s.st.Q.GetDatabase(ctx, t.ID)
		if err != nil {
			return t, errf(http.StatusNotFound, "database not found")
		}
		t.Label = "database " + d.Name
		list, err := s.docker.ContainerList(ctx, map[string]string{databases.LabelDatabase: strconv.FormatInt(d.ID, 10), databases.LabelManaged: "true"})
		if err != nil {
			return t, err
		}
		for _, c := range list {
			if c.State == "running" {
				t.ref = c.ID
				return t, nil
			}
		}
		return t, errf(http.StatusConflict, "database %s is not running", d.Name)
	}
	return t, errf(http.StatusBadRequest, "target must be host, app or database")
}

// POST /api/terminal/sessions {kind, id, password} → {ticket, label}
func (s *Server) handleTerminalTicket(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		termTarget
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	a := authFrom(r.Context())
	if err := s.requireRecentAuth(a, in.Password); err != nil {
		return err
	}
	if s.docker == nil {
		return errf(http.StatusServiceUnavailable, "the terminal needs Docker")
	}
	t, err := s.resolveTarget(r.Context(), in.termTarget)
	if err != nil {
		return err
	}
	var b [24]byte
	_, _ = rand.Read(b[:])
	tok := hex.EncodeToString(b[:])
	s.term.mu.Lock()
	if s.term.tickets == nil {
		s.term.tickets = map[string]termTicket{}
	}
	for k, v := range s.term.tickets {
		if s.now().After(v.expires) {
			delete(s.term.tickets, k)
		}
	}
	s.term.tickets[tok] = termTicket{target: t, userID: a.User.ID, username: a.User.Username, sessionID: a.Session.ID, ip: clientIP(r), expires: s.now().Add(ticketLifetime)}
	s.term.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ticket": tok, "label": t.Label})
	return nil
}

// ttySession is a running shell.
type ttySession interface {
	io.ReadWriter
	Resize(rows, cols int) error
	Close() error
}

type ttyDocker interface {
	ExecTTY(ctx context.Context, id string, cmd, env []string, workDir string) (string, *docker.Stream, error)
	ExecResize(ctx context.Context, execID string, rows, cols int) error
	ExecRunning(ctx context.Context, execID string) (bool, error)
	Exec(ctx context.Context, id string, cmd, env []string, stdout io.Writer) (docker.ExecResult, error)
	ContainerCreate(ctx context.Context, s docker.ContainerSpec) (string, error)
	ContainerAttach(ctx context.Context, id string) (*docker.Stream, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerResize(ctx context.Context, id string, rows, cols int) error
	ContainerStop(ctx context.Context, id string, timeoutSec int) error
	ContainerRemove(ctx context.Context, id string) error
	ContainerInspect(ctx context.Context, id string) (docker.ContainerInfo, error)
}

var shellEnv = []string{"TERM=xterm-256color", "COLORTERM=truecolor", "LANG=C.UTF-8",
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}

// The first line is the shell's PID (inside the container), so the session
// can be hung up when the browser goes away: closing a docker exec stream
// doesn't end the process.
const shellScript = `printf 'DOCKLETPID:%s\n' $$
if command -v bash >/dev/null 2>&1; then exec bash -l; else exec sh -l; fi`

type execTTY struct {
	dk     ttyDocker
	cid    string
	execID string
	pid    string
	*docker.Stream
	r io.Reader
}

func (e *execTTY) Read(p []byte) (int, error) { return e.r.Read(p) }
func (e *execTTY) Resize(rows, cols int) error {
	return e.dk.ExecResize(context.Background(), e.execID, rows, cols)
}
func (e *execTTY) Close() error {
	_ = e.Stream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e.pid != "" {
		_, _ = e.dk.Exec(ctx, e.cid, []string{"sh", "-c", "kill -HUP -- -$P $P 2>/dev/null; sleep 2; kill -KILL -- -$P $P 2>/dev/null; true"}, []string{"P=" + e.pid}, nil)
	}
	return nil
}

type hostTTY struct {
	dk  ttyDocker
	cid string
	*docker.Stream
	r io.Reader
}

func (h *hostTTY) Read(p []byte) (int, error) { return h.r.Read(p) }
func (h *hostTTY) Resize(rows, cols int) error {
	return h.dk.ContainerResize(context.Background(), h.cid, rows, cols)
}
func (h *hostTTY) Close() error {
	_ = h.Stream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = h.dk.ContainerStop(ctx, h.cid, 1) // ends every process the shell started
	return h.dk.ContainerRemove(ctx, h.cid)
}

// readPID consumes the DOCKLETPID line and returns a reader for the rest.
func readPID(st io.Reader) (string, io.Reader, error) {
	br := bufio.NewReader(st)
	line, err := br.ReadString('\n')
	if err != nil {
		return "", nil, err
	}
	pid := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "DOCKLETPID:"))
	if _, err := strconv.Atoi(pid); err != nil {
		return "", nil, fmt.Errorf("unexpected shell output %q", line)
	}
	return pid, br, nil
}

func (s *Server) openTTY(ctx context.Context, t termTarget, rows, cols int) (ttySession, error) {
	dk, ok := s.docker.(ttyDocker)
	if !ok {
		return nil, errors.New("the terminal needs a Docker connection")
	}
	if t.Kind != "host" {
		execID, st, err := dk.ExecTTY(ctx, t.ref, []string{"/bin/sh", "-c", shellScript}, append(shellEnv, "HOME=/root"), "")
		if err != nil {
			return nil, err
		}
		_ = dk.ExecResize(ctx, execID, rows, cols)
		pid, rd, err := readPID(st)
		if err != nil {
			_ = st.Close()
			return nil, err
		}
		return &execTTY{dk: dk, cid: t.ref, execID: execID, pid: pid, Stream: st, r: rd}, nil
	}
	img, pid, err := s.helperImage(ctx)
	if err != nil {
		return nil, err
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	cid, err := dk.ContainerCreate(ctx, docker.ContainerSpec{
		Name: "docklet-term-" + hex.EncodeToString(rnd[:]), Image: img, Entrypoint: []string{"nsenter"},
		Cmd: []string{"-t", strconv.Itoa(pid), "-m", "-u", "-i", "-n", "-p", "--", "/bin/sh", "-c", "cd ~ 2>/dev/null || cd /; " + shellScript},
		Env: append(shellEnv, "HOME=/root", "USER=root"), Privileged: true, PidMode: "host", Network: "host", RestartPolicy: "no",
		Tty: true, OpenStdin: true,
		Labels: map[string]string{"docklet.managed": "true", host.LabelHelper: "true", "docklet.terminal": "true"},
	})
	if err != nil {
		return nil, fmt.Errorf("start host shell: %w", err)
	}
	st, err := dk.ContainerAttach(ctx, cid)
	if err == nil {
		err = dk.ContainerStart(ctx, cid)
	}
	if err != nil {
		if st != nil {
			_ = st.Close()
		}
		_ = dk.ContainerRemove(context.WithoutCancel(ctx), cid)
		return nil, fmt.Errorf("start host shell: %w", err)
	}
	_ = dk.ContainerResize(ctx, cid, rows, cols)
	_, rd, err := readPID(st)
	if err != nil {
		h := &hostTTY{dk: dk, cid: cid, Stream: st}
		_ = h.Close()
		return nil, err
	}
	return &hostTTY{dk: dk, cid: cid, Stream: st, r: rd}, nil
}

// helperImage is the image and target PID for host helpers.
func (s *Server) helperImage(ctx context.Context) (string, int, error) {
	pid := 1
	if v, err := strconv.Atoi(os.Getenv("DOCKLET_HOST_PID")); err == nil && v > 0 {
		pid = v
	}
	if !release.InContainer() {
		return "", 0, errors.New("the host terminal needs Docklet's container install (use SSH otherwise)")
	}
	dk, ok := s.docker.(*docker.Client)
	if !ok {
		return "", 0, errors.New("the host terminal needs a Docker connection")
	}
	img, err := host.SelfImage(ctx, dk.ContainerInspect)
	return img, pid, err
}

func sameOriginWS(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

// GET /api/terminal/ws?ticket=…&rows=&cols=
func (s *Server) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	if !sameOriginWS(r) {
		http.Error(w, "cross-origin terminal blocked", http.StatusForbidden)
		return
	}
	tok := r.URL.Query().Get("ticket")
	s.term.mu.Lock()
	tk, ok := s.term.tickets[tok]
	delete(s.term.tickets, tok)
	full := s.term.open >= maxTerminalOpen
	if ok && !full {
		s.term.open++
	}
	s.term.mu.Unlock()
	if !ok || s.now().After(tk.expires) {
		http.Error(w, "terminal ticket expired; reopen the terminal", http.StatusUnauthorized)
		return
	}
	if full {
		http.Error(w, "too many open terminals", http.StatusTooManyRequests)
		return
	}
	defer func() { s.term.mu.Lock(); s.term.open--; s.term.mu.Unlock() }()
	// The session that asked for the ticket must still be valid.
	if sess, err := s.st.Q.GetSession(r.Context(), tk.sessionID); err != nil || sess.ExpiresAt < s.now().Unix() {
		http.Error(w, "session expired", http.StatusUnauthorized)
		return
	}
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	if rows < 2 || rows > 500 || cols < 10 || cols > 1000 {
		rows, cols = 24, 80
	}
	conn, err := ws.Upgrade(w, r)
	if err != nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	octx, cancel := context.WithTimeout(ctx, time.Minute)
	tty, err := s.openTTY(octx, tk.target, rows, cols)
	cancel()
	if err != nil {
		_ = conn.Write(ws.Text, mustJSON(map[string]string{"type": "error", "error": err.Error()}))
		_ = conn.Close(1011, "shell failed")
		return
	}
	started := s.now()
	s.auditTerminal(ctx, tk, audit.ActionTerminalStarted, nil)
	reason := s.pumpTerminal(conn, tty)
	_ = tty.Close()
	_ = conn.Close(1000, reason)
	s.auditTerminal(ctx, tk, audit.ActionTerminalEnded, map[string]any{"durationSec": int(s.now().Sub(started).Seconds()), "reason": reason})
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// pumpTerminal copies both ways until either side ends; it returns why.
func (s *Server) pumpTerminal(conn *ws.Conn, tty ttySession) string {
	done := make(chan string, 3)
	var lastInput sync.Mutex
	last := time.Now()
	go func() { // shell → browser
		buf := make([]byte, 32<<10)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				if werr := conn.Write(ws.Binary, buf[:n]); werr != nil {
					done <- "browser gone"
					return
				}
			}
			if err != nil {
				done <- "shell exited"
				return
			}
		}
	}()
	go func() { // browser → shell
		for {
			typ, data, err := conn.Read()
			if err != nil {
				done <- "browser closed"
				return
			}
			lastInput.Lock()
			last = time.Now()
			lastInput.Unlock()
			if typ == ws.Binary {
				if _, err := tty.Write(data); err != nil {
					done <- "shell exited"
					return
				}
				continue
			}
			var m struct {
				Type       string `json:"type"`
				Rows, Cols int
				Data       string `json:"data"`
			}
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.Type {
			case "resize":
				if m.Rows > 1 && m.Rows <= 500 && m.Cols > 1 && m.Cols <= 1000 {
					_ = tty.Resize(m.Rows, m.Cols)
				}
			case "input":
				if _, err := tty.Write([]byte(m.Data)); err != nil {
					done <- "shell exited"
					return
				}
			}
		}
	}()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case why := <-done:
			return why
		case <-t.C:
			lastInput.Lock()
			idle := time.Since(last)
			lastInput.Unlock()
			if idle > terminalIdle {
				_ = conn.Write(ws.Text, mustJSON(map[string]string{"type": "error", "error": "closed after 30 minutes without input"}))
				return "idle timeout"
			}
			_ = conn.Ping()
		}
	}
}

func (s *Server) auditTerminal(ctx context.Context, tk termTicket, action string, extra map[string]any) {
	d := map[string]any{"target": tk.target.Label}
	for k, v := range extra {
		d[k] = v
	}
	target := tk.target.Kind
	if tk.target.ID != 0 {
		target = fmt.Sprintf("%s:%d", tk.target.Kind, tk.target.ID)
	}
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		_, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(tk.userID), Actor: tk.username, Action: action, Target: target, IP: tk.ip, Details: d,
		}, s.now())
		return err
	}); err != nil {
		s.log.Warn("audit terminal", "err", err)
	}
}
