package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
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

// readJSON decodes a JSON request body strictly.
func readJSON(r *http.Request, dst any) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return errf(http.StatusUnsupportedMediaType, "content-type must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errf(http.StatusBadRequest, "invalid JSON body")
	}
	return nil
}

// handler adapts error-returning handlers.
func (s *Server) handler(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			s.writeErr(w, r, err)
		}
	}
}

// isTrustedProxy reports whether the direct peer is a local reverse proxy
// (Caddy on the same host), whose forwarding headers we accept.
func isTrustedProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// clientIP returns the end-user IP, honouring X-Forwarded-For only from a
// trusted local proxy (Caddy appends the real client as the last entry).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if isTrustedProxy(r) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
				return ip.String()
			}
		}
	}
	return host
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return isTrustedProxy(r) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
