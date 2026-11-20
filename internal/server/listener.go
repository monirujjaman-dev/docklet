package server

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
)

// listenerMgr lets the server move between listen addresses at runtime, e.g.
// from :3000 (public) to 127.0.0.1:3000 once Caddy serves the panel domain.
type listenerMgr struct {
	srv   *http.Server
	errCh chan error

	mu sync.Mutex
	ln net.Listener
}

func newListenerMgr(srv *http.Server) *listenerMgr {
	return &listenerMgr{srv: srv, errCh: make(chan error, 1)}
}

// listen binds addr and serves on it, retiring any previous listener first.
// Linux refuses to bind 127.0.0.1:P while a wildcard socket listens on P, so
// the old listener is closed before the new bind; on failure the old address
// is re-bound so the server never ends up unreachable.
func (m *listenerMgr) listen(addr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var oldAddr string
	if m.ln != nil {
		oldAddr = m.ln.Addr().String()
		old := m.ln
		m.ln = nil
		_ = old.Close()
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if oldAddr != "" {
			if back, berr := net.Listen("tcp", oldAddr); berr == nil {
				m.serve(back)
			} else {
				err = errors.Join(err, berr)
			}
		}
		return err
	}
	m.serve(ln)
	return nil
}

// serve must be called with m.mu held.
func (m *listenerMgr) serve(ln net.Listener) {
	m.ln = ln
	go func() {
		err := m.srv.Serve(ln)
		m.mu.Lock()
		current := m.ln == ln
		m.mu.Unlock()
		if current {
			select {
			case m.errCh <- err:
			default:
			}
		}
	}()
}

// addr returns the current listen address.
func (m *listenerMgr) addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln == nil {
		return ""
	}
	return m.ln.Addr().String()
}

// port returns the current TCP port (0 if not listening).
func (m *listenerMgr) port() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln == nil {
		return 0
	}
	if a, ok := m.ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func splitHostPort(addr string) (string, int, error) {
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 0 || port > 65535 {
		return "", 0, errors.New("invalid port")
	}
	return host, port, nil
}

func loopbackAddr(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}
