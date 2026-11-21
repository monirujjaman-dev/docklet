package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/host"
	"github.com/monirujjaman-dev/docklet/internal/release"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// hostState resolves how host commands run, lazily: in a container the
// helper image is Docklet's own, found by inspecting its container.
type hostState struct {
	mu   sync.Mutex
	svc  *host.Service
	done bool
}

// WithHostRunner sets the runner for host commands (tests).
func WithHostRunner(r host.Runner) Option {
	return func(s *Server) { s.host.svc, s.host.done = &host.Service{R: r}, true }
}

func (s *Server) hostService(ctx context.Context) (*host.Service, error) {
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	if s.host.done && s.host.svc != nil {
		return s.host.svc, nil
	}
	switch {
	case release.InContainer():
		dk, ok := s.docker.(*docker.Client)
		if !ok || dk == nil {
			return nil, errf(http.StatusServiceUnavailable, "server management needs access to Docker")
		}
		img, err := host.SelfImage(ctx, dk.ContainerInspect)
		if err != nil {
			return nil, errf(http.StatusServiceUnavailable, "server management is unavailable: %v", err)
		}
		pid := 1
		if v, err := strconv.Atoi(os.Getenv("DOCKLET_HOST_PID")); err == nil && v > 0 {
			pid = v // testing against a stand-in host
		}
		s.host.svc = &host.Service{R: &host.Helper{DK: dk, Image: img, TargetPID: pid}}
	case runtime.GOOS == "linux" && os.Geteuid() == 0:
		s.host.svc = &host.Service{R: host.Direct{}}
	default:
		return nil, errf(http.StatusServiceUnavailable, "server management needs Docklet to run as root on Linux, or in its container")
	}
	s.host.done = true
	return s.host.svc, nil
}

func hostErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, host.ErrUnsupported) {
		return errf(http.StatusConflict, "%v", err)
	}
	var ie *host.InputError
	var ce *host.ErrCommand
	if errors.As(err, &ie) || errors.As(err, &ce) {
		return errf(http.StatusBadRequest, "%v", err)
	}
	return err
}

// withHost runs fn with the host service, mapping errors.
func (s *Server) withHost(w http.ResponseWriter, r *http.Request, fn func(h *host.Service) (any, error)) error {
	h, err := s.hostService(r.Context())
	if err != nil {
		return err
	}
	out, err := fn(h)
	if err != nil {
		return hostErr(err)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) auditServer(r *http.Request, action string, details map[string]any) error {
	ctx := r.Context()
	return s.st.Tx(ctx, func(q *db.Queries) error { return s.audit(ctx, q, r, action, "server", details) })
}

// GET /api/server/overview
func (s *Server) handleServerOverview(w http.ResponseWriter, r *http.Request) error {
	return s.withHost(w, r, func(h *host.Service) (any, error) { return h.Overview(r.Context()) })
}

// GET /api/server/timezones
func (s *Server) handleServerTimezones(w http.ResponseWriter, r *http.Request) error {
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		z, err := h.Timezones(r.Context())
		return map[string]any{"timezones": z}, err
	})
}

// PUT /api/server/hostname {"hostname": "..."}
func (s *Server) handleServerHostname(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Hostname string `json:"hostname"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		before, _ := h.Overview(r.Context())
		if err := h.SetHostname(r.Context(), in.Hostname); err != nil {
			return nil, err
		}
		after, err := h.Overview(r.Context())
		if aerr := s.auditServer(r, audit.ActionServerHostname, map[string]any{"from": before.Hostname, "to": after.Hostname}); aerr != nil {
			return nil, aerr
		}
		return after, err
	})
}

// PUT /api/server/timezone {"timezone": "Asia/Dhaka"}
func (s *Server) handleServerTimezone(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Timezone string `json:"timezone"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		before, _ := h.Overview(r.Context())
		if err := h.SetTimezone(r.Context(), strings.TrimSpace(in.Timezone)); err != nil {
			return nil, err
		}
		after, err := h.Overview(r.Context())
		if aerr := s.auditServer(r, audit.ActionServerTimezone, map[string]any{"from": before.Timezone, "to": after.Timezone}); aerr != nil {
			return nil, aerr
		}
		return after, err
	})
}

// PUT /api/server/swap {"sizeMb": 2048} (0 removes /swapfile)
func (s *Server) handleServerSwap(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		SizeMB int `json:"sizeMb"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		before, _ := h.Overview(r.Context())
		if err := h.SetSwap(r.Context(), in.SizeMB); err != nil {
			return nil, err
		}
		after, err := h.Overview(r.Context())
		if aerr := s.auditServer(r, audit.ActionServerSwap, map[string]any{"fromMb": before.SwapFile >> 20, "toMb": in.SizeMB}); aerr != nil {
			return nil, aerr
		}
		return after, err
	})
}

// PUT /api/server/swappiness {"value": 10}
func (s *Server) handleServerSwappiness(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Value int `json:"value"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		before, _ := h.Overview(r.Context())
		if err := h.SetSwappiness(r.Context(), in.Value); err != nil {
			return nil, err
		}
		after, err := h.Overview(r.Context())
		if aerr := s.auditServer(r, audit.ActionServerSwappiness, map[string]any{"from": before.Swappiness, "to": in.Value}); aerr != nil {
			return nil, aerr
		}
		return after, err
	})
}

// POST /api/server/reboot {"hostname": "<typed>", "password": "..."}
func (s *Server) handleServerReboot(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Hostname string `json:"hostname"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := checkPassword(authFrom(r.Context()).User, in.Password); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		o, err := h.Overview(r.Context())
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Hostname) != o.Hostname {
			return nil, errf(http.StatusBadRequest, "type the server's host name (%s) to confirm", o.Hostname)
		}
		if err := s.auditServer(r, audit.ActionServerReboot, map[string]any{"hostname": o.Hostname, "uptimeSec": o.UptimeSec}); err != nil {
			return nil, err
		}
		if err := h.Reboot(r.Context()); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "inSeconds": 5}, nil
	})
}

// GET /api/server/ports
func (s *Server) handleServerPorts(w http.ResponseWriter, r *http.Request) error {
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		ports, err := h.ListeningPorts(r.Context())
		if err != nil {
			return nil, err
		}
		s.annotatePorts(r.Context(), ports)
		return map[string]any{"ports": ports}, nil
	})
}

// annotatePorts names the container (and the Docklet app or database) that
// publishes a port.
func (s *Server) annotatePorts(ctx context.Context, ports []host.Port) {
	dk, ok := s.docker.(interface {
		ContainerList(ctx context.Context, labels map[string]string) ([]docker.ContainerSummary, error)
	})
	if !ok || s.docker == nil {
		return
	}
	list, err := dk.ContainerList(ctx, nil)
	if err != nil {
		return
	}
	type owner struct{ container, owner string }
	byPort := map[string]owner{}
	for _, c := range list {
		name := strings.TrimPrefix(strings.Join(c.Names, ","), "/")
		o := owner{container: name}
		switch {
		case c.Labels["docklet.database"] != "":
			if id, err := strconv.ParseInt(c.Labels["docklet.database"], 10, 64); err == nil {
				if d, err := s.st.Q.GetDatabase(ctx, id); err == nil {
					o.owner = "database " + d.Name
				}
			}
		case c.Labels["docklet.app"] != "":
			if id, err := strconv.ParseInt(c.Labels["docklet.app"], 10, 64); err == nil {
				if a, err := s.st.Q.GetApp(ctx, id); err == nil {
					o.owner = "app " + a.Name
				}
			}
		case c.Labels["com.docker.compose.project"] == "docklet":
			o.owner = map[string]string{"docklet": "Docklet", "caddy": "Caddy"}[c.Labels["com.docker.compose.service"]]
		}
		for _, p := range c.Ports {
			if p.PublicPort > 0 {
				byPort[p.Type+"/"+strconv.Itoa(p.PublicPort)] = o
			}
		}
	}
	for i := range ports {
		if o, ok := byPort[ports[i].Proto+"/"+strconv.Itoa(ports[i].Port)]; ok {
			ports[i].Container, ports[i].Owner = o.container, o.owner
		}
	}
}

// GET /api/server/processes
func (s *Server) handleServerProcesses(w http.ResponseWriter, r *http.Request) error {
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		p, err := h.Processes(r.Context(), 100)
		return map[string]any{"processes": p}, err
	})
}

// POST /api/server/processes/{pid}/signal {"signal": "TERM"}
func (s *Server) handleServerSignal(w http.ResponseWriter, r *http.Request) error {
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil || pid <= 0 {
		return errf(http.StatusBadRequest, "invalid pid")
	}
	var in struct {
		Signal string `json:"signal"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		p, err := h.Signal(r.Context(), pid, in.Signal)
		if err != nil {
			return nil, err
		}
		if err := s.auditServer(r, audit.ActionServerSignal, map[string]any{"pid": pid, "process": p.Comm, "user": p.User, "signal": in.Signal}); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	})
}

// GET /api/server/services
func (s *Server) handleServerServices(w http.ResponseWriter, r *http.Request) error {
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		u, err := h.Services(r.Context())
		return map[string]any{"services": u}, err
	})
}

// POST /api/server/services/{unit}/{action}
func (s *Server) handleServerServiceAction(w http.ResponseWriter, r *http.Request) error {
	unit, action := r.PathValue("unit"), r.PathValue("action")
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		if err := h.ServiceAction(r.Context(), unit, action); err != nil {
			return nil, err
		}
		if err := s.auditServer(r, audit.ActionServerService, map[string]any{"unit": unit, "action": action}); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	})
}

// GET /api/server/services/{unit}/logs
func (s *Server) handleServerServiceLogs(w http.ResponseWriter, r *http.Request) error {
	unit := r.PathValue("unit")
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		logs, err := h.ServiceLogs(r.Context(), unit, 200)
		return map[string]string{"logs": logs}, err
	})
}

func (s *Server) cleanupDocker() (host.CleanupDocker, error) {
	dk, ok := s.docker.(host.CleanupDocker)
	if !ok || s.docker == nil {
		return nil, errf(http.StatusServiceUnavailable, "disk cleanup needs access to Docker")
	}
	return dk, nil
}

func (s *Server) keepImages(ctx context.Context) (map[string]bool, error) {
	ids, err := s.st.Q.ListDeploymentImageIDs(ctx)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, id := range ids {
		keep[id] = true
	}
	return keep, nil
}

// GET /api/server/cleanup — what can be freed.
func (s *Server) handleServerCleanupPreview(w http.ResponseWriter, r *http.Request) error {
	dk, err := s.cleanupDocker()
	if err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		keep, err := s.keepImages(r.Context())
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		cats, err := h.CleanupPreview(ctx, dk, keep)
		return map[string]any{"categories": cats}, err
	})
}

// POST /api/server/cleanup {"categories": ["images", ...], "volumes": ["name", ...]}
func (s *Server) handleServerCleanup(w http.ResponseWriter, r *http.Request) error {
	dk, err := s.cleanupDocker()
	if err != nil {
		return err
	}
	var in struct {
		Categories []string `json:"categories"`
		Volumes    []string `json:"volumes"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	return s.withHost(w, r, func(h *host.Service) (any, error) {
		keep, err := s.keepImages(r.Context())
		if err != nil {
			return nil, err
		}
		before, _ := h.Overview(r.Context())
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		res, err := h.Cleanup(ctx, dk, keep, in.Categories, in.Volumes)
		if err != nil {
			return nil, err
		}
		after, _ := h.Overview(r.Context())
		freed := after.DiskAvail - before.DiskAvail
		if aerr := s.auditServer(r, audit.ActionServerCleanup, map[string]any{"categories": in.Categories, "volumes": in.Volumes, "results": res, "freedBytes": freed}); aerr != nil {
			return nil, aerr
		}
		return map[string]any{"results": res, "freedBytes": freed, "diskAvailable": after.DiskAvail}, nil
	})
}
