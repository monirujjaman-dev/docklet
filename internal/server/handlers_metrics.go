package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/metrics"
)

func (s *Server) requireMetrics() error {
	if s.metrics == nil {
		return errf(http.StatusServiceUnavailable, "metrics collection is disabled")
	}
	return nil
}

func validScope(scope string) error {
	if scope == metrics.HostScope {
		return nil
	}
	if len(scope) > 4 && scope[:4] == "app:" {
		if _, err := strconv.ParseInt(scope[4:], 10, 64); err == nil {
			return nil
		}
	}
	return errf(http.StatusBadRequest, "scope must be 'host' or 'app:<id>'")
}

// GET /api/metrics/status
func (s *Server) handleMetricsStatus(w http.ResponseWriter, _ *http.Request) error {
	if err := s.requireMetrics(); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.metrics.Status())
	return nil
}

// GET /api/metrics/recent?scope=host&since=<unix ms> — in-memory samples (≤30 min).
func (s *Server) handleMetricsRecent(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireMetrics(); err != nil {
		return err
	}
	scope := r.URL.Query().Get("scope")
	if err := validScope(scope); err != nil {
		return err
	}
	since := s.now().Add(-30 * time.Minute).UnixMilli()
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return errf(http.StatusBadRequest, "invalid since")
		}
		since = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"scope": scope, "samples": s.metrics.Recent(scope, since)})
	return nil
}

var historyRanges = map[string]time.Duration{
	"1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour,
}

// GET /api/metrics/history?scope=host&range=24h — one-minute averages.
func (s *Server) handleMetricsHistory(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireMetrics(); err != nil {
		return err
	}
	scope := r.URL.Query().Get("scope")
	if err := validScope(scope); err != nil {
		return err
	}
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "24h"
	}
	d, ok := historyRanges[rng]
	if !ok {
		return errf(http.StatusBadRequest, "range must be one of 1h, 6h, 24h, 7d, 30d")
	}
	now := s.now()
	samples, err := s.metrics.History(r.Context(), scope, now.Add(-d), now)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"scope": scope, "range": rng, "samples": samples})
	return nil
}

// GET /api/metrics/stream — Server-Sent Events, one snapshot per interval.
func (s *Server) handleMetricsStream(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireMetrics(); err != nil {
		return err
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	ch, cancel := s.metrics.Subscribe()
	defer cancel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return nil
		case snap := <-ch:
			b, _ := json.Marshal(snap)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return nil
			}
			_ = rc.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return nil
			}
			_ = rc.Flush()
		}
	}
}
