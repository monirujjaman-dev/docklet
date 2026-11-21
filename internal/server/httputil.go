package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

const maxJSONBody = 1 << 20 // 1 MiB

type apiError struct {
	Status int
	Msg    string
	Extra  map[string]any
}

func (e *apiError) Error() string { return e.Msg }

func errf(status int, format string, args ...any) *apiError {
	return &apiError{Status: status, Msg: fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr writes a JSON error. Unknown errors become 500 and are logged.
func (s *Server) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		body := map[string]any{"error": ae.Msg}
		for k, v := range ae.Extra {
			body[k] = v
		}
		writeJSON(w, ae.Status, body)
		return
	}
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

// errAlreadyWritten ends a handler whose response has started (a stream).
var errAlreadyWritten = errors.New("response already written")

// readJSON decodes a JSON request body strictly.
func readJSON(r *http.Request, dst any) error { return readJSONMax(r, dst, maxJSONBody) }

// readJSONMax is readJSON with a custom size limit.
func readJSONMax(r *http.Request, dst any, max int64) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return errf(http.StatusUnsupportedMediaType, "content-type must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, max))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errf(http.StatusBadRequest, "invalid JSON body")
	}
	return nil
}

// handler adapts error-returning handlers.
func (s *Server) handler(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil && !errors.Is(err, errAlreadyWritten) {
			s.writeErr(w, r, err)
		}
	}
}

type clientKeyT struct{}

var clientKey clientKeyT

type clientInfo struct {
	ip    string
	https bool
}

// isTrustedPeer reports whether the direct peer is loopback or one of the
// configured proxies, whose forwarding headers we accept.
func isTrustedPeer(r *http.Request, trusted []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return true
	}
	for _, p := range trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func resolveClient(r *http.Request, trusted []netip.Prefix) clientInfo {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ci := clientInfo{ip: host, https: r.TLS != nil}
	if isTrustedPeer(r, trusted) {
		// Caddy appends the real client as the last X-Forwarded-For entry.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
				ci.ip = ip.String()
			}
		}
		if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			ci.https = true
		}
	}
	return ci
}

// withClientInfo resolves the client IP and scheme once per request.
func (s *Server) withClientInfo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ci := resolveClient(r, s.cfg.TrustedProxies)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey, ci)))
	})
}

func clientFrom(r *http.Request) clientInfo {
	if ci, ok := r.Context().Value(clientKey).(clientInfo); ok {
		return ci
	}
	return resolveClient(r, nil)
}

// clientIP returns the end-user IP.
func clientIP(r *http.Request) string { return clientFrom(r).ip }

// isHTTPS reports whether the end user connected over HTTPS.
func isHTTPS(r *http.Request) bool { return clientFrom(r).https }
