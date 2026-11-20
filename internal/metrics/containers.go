package metrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker"
)

// StatsAPI is the Docker client subset the fallback source needs.
type StatsAPI interface {
	ContainerStats(ctx context.Context, id string) (docker.Stats, error)
	ContainerInspect(ctx context.Context, id string) (docker.ContainerInfo, error)
}

// ContainerSource samples containers by id.
type ContainerSource interface {
	Name() string
	Sample(ctx context.Context, ids []string, now time.Time) map[string]Sample
}

// --- cgroup v2 -----------------------------------------------------------------

// CgroupSource reads cgroup v2 files directly: no Docker API calls per sample.
type CgroupSource struct {
	root    string // /sys/fs/cgroup (or a host mount)
	proc    string // for per-container network via /proc/<pid>/net/dev
	api     StatsAPI
	hostMem uint64
	mu      sync.Mutex
	paths   map[string]string
	pids    map[string]int
	prev    map[string]counters
}

// NewCgroupSource returns a source if root is a cgroup v2 hierarchy that
// shows Docker's containers, or nil.
func NewCgroupSource(root, proc string, api StatsAPI, hostMem uint64) *CgroupSource {
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return nil
	}
	return &CgroupSource{root: root, proc: proc, api: api, hostMem: hostMem,
		paths: map[string]string{}, pids: map[string]int{}, prev: map[string]counters{}}
}

// Name implements ContainerSource.
func (c *CgroupSource) Name() string { return "cgroup-v2" }

// Find returns the cgroup directory for a container, or "".
func (c *CgroupSource) find(id string) string {
	for _, p := range []string{
		filepath.Join(c.root, "system.slice", "docker-"+id+".scope"), // systemd driver
		filepath.Join(c.root, "docker", id),                          // cgroupfs driver
		filepath.Join(c.root, "docker.slice", "docker-"+id+".scope"),
	} {
		if _, err := os.Stat(filepath.Join(p, "memory.current")); err == nil {
			return p
		}
	}
	return ""
}

// Visible reports whether any of ids can be found (used for auto-detection).
func (c *CgroupSource) Visible(ids []string) bool {
	for _, id := range ids {
		if c.find(id) != "" {
			return true
		}
	}
	return false
}

// Sample implements ContainerSource.
func (c *CgroupSource) Sample(ctx context.Context, ids []string, now time.Time) map[string]Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]Sample, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
		dir, ok := c.paths[id]
		if !ok {
			dir = c.find(id)
			c.paths[id] = dir
		}
		if dir == "" {
			continue
		}
		s := Sample{Ts: now.UnixMilli()}
		cur := counters{ts: now.UnixNano()}
		mem, err := readUint(filepath.Join(dir, "memory.current"))
		if err != nil {
			delete(c.paths, id) // container gone or restarted elsewhere
			continue
		}
		// Like `docker stats`, exclude reclaimable page cache.
		if inactive := statField(filepath.Join(dir, "memory.stat"), "inactive_file"); inactive < mem {
			mem -= inactive
		}
		s.MemUsed = mem
		s.MemLimit = c.hostMem
		if lim, err := os.ReadFile(filepath.Join(dir, "memory.max")); err == nil { // #nosec G304 -- cgroup path from a container id
			if v, err := strconv.ParseUint(strings.TrimSpace(string(lim)), 10, 64); err == nil {
				s.MemLimit = v
			}
		}
		cur.cpuUsage = statField(filepath.Join(dir, "cpu.stat"), "usage_usec") * 1000 // ns
		cur.blkRead, cur.blkWrite = readIOStat(filepath.Join(dir, "io.stat"))
		if pid := c.pid(ctx, id); pid > 0 {
			cur.netRx, cur.netTx, _ = readNetDev(filepath.Join(c.proc, strconv.Itoa(pid), "net", "dev"), false)
		}
		applyRates(&s, cur, c.prev[id])
		c.prev[id] = cur
		out[id] = s
	}
	for id := range c.prev {
		if !seen[id] {
			delete(c.prev, id)
			delete(c.paths, id)
			delete(c.pids, id)
		}
	}
	return out
}

func (c *CgroupSource) pid(ctx context.Context, id string) int {
	if p, ok := c.pids[id]; ok {
		return p
	}
	info, err := c.api.ContainerInspect(ctx, id)
	if err != nil {
		return 0
	}
	c.pids[id] = info.State.Pid
	return info.State.Pid
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- cgroup path built from a container id
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// statField reads "key value" lines (cpu.stat, memory.stat).
func statField(path, key string) uint64 {
	b, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok && k == key {
			n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

// readIOStat sums rbytes/wbytes over all devices.
func readIOStat(path string) (r, w uint64) {
	b, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return 0, 0
	}
	for _, f := range strings.Fields(string(b)) {
		if v, ok := strings.CutPrefix(f, "rbytes="); ok {
			n, _ := strconv.ParseUint(v, 10, 64)
			r += n
		} else if v, ok := strings.CutPrefix(f, "wbytes="); ok {
			n, _ := strconv.ParseUint(v, 10, 64)
			w += n
		}
	}
	return r, w
}

func applyRates(s *Sample, cur counters, prev counters) {
	if prev.ts == 0 || cur.ts <= prev.ts {
		return
	}
	secs := float64(cur.ts-prev.ts) / 1e9
	if cur.cpuUsage >= prev.cpuUsage {
		s.CPU = float64(cur.cpuUsage-prev.cpuUsage) / float64(cur.ts-prev.ts) * 100
	}
	s.NetRx, s.NetTx = rate(cur.netRx, prev.netRx, secs), rate(cur.netTx, prev.netTx, secs)
	s.BlkRead, s.BlkWrite = rate(cur.blkRead, prev.blkRead, secs), rate(cur.blkWrite, prev.blkWrite, secs)
}

// --- Docker stats fallback -------------------------------------------------------

// DockerStatsSource uses GET /containers/{id}/stats?one-shot=true.
type DockerStatsSource struct {
	api  StatsAPI
	mu   sync.Mutex
	prev map[string]counters
}

// NewDockerStatsSource returns the fallback source.
func NewDockerStatsSource(api StatsAPI) *DockerStatsSource {
	return &DockerStatsSource{api: api, prev: map[string]counters{}}
}

// Name implements ContainerSource.
func (d *DockerStatsSource) Name() string { return "docker-stats" }

// Sample implements ContainerSource. Containers are queried in parallel
// (bounded) so 20 containers take about as long as one.
func (d *DockerStatsSource) Sample(ctx context.Context, ids []string, now time.Time) map[string]Sample {
	type res struct {
		id string
		st docker.Stats
	}
	ch := make(chan res, len(ids))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			st, err := d.api.ContainerStats(cctx, id)
			if err == nil && !errors.Is(cctx.Err(), context.DeadlineExceeded) {
				ch <- res{id, st}
			}
		}(id)
	}
	wg.Wait()
	close(ch)

	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]Sample, len(ids))
	seen := map[string]bool{}
	for r := range ch {
		seen[r.id] = true
		st := r.st
		s := Sample{Ts: now.UnixMilli(), MemLimit: st.MemoryStats.Limit}
		mem := st.MemoryStats.Usage
		inactive := st.MemoryStats.Stats["inactive_file"] // cgroup v2
		if inactive == 0 {
			inactive = st.MemoryStats.Stats["total_inactive_file"] // cgroup v1
		}
		if inactive < mem {
			mem -= inactive
		}
		s.MemUsed = mem
		cur := counters{ts: now.UnixNano(), cpuUsage: st.CPUStats.CPUUsage.TotalUsage}
		for _, n := range st.Networks {
			cur.netRx += n.RxBytes
			cur.netTx += n.TxBytes
		}
		for _, e := range st.BlkioStats.IOServiceBytesRecursive {
			switch strings.ToLower(e.Op) {
			case "read":
				cur.blkRead += e.Value
			case "write":
				cur.blkWrite += e.Value
			}
		}
		applyRates(&s, cur, d.prev[r.id])
		d.prev[r.id] = cur
		out[r.id] = s
	}
	for id := range d.prev {
		if !seen[id] && !contains(ids, id) {
			delete(d.prev, id)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
