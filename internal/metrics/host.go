package metrics

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Host samples whole-machine usage.
type Host struct {
	proc     string // usually /proc; /host/proc when Docklet runs in a container
	diskPath string // filesystem to report (Docklet's data directory)
	prev     *counters
	cores    atomic.Int64 // read concurrently by Status
}

// NewHost returns a host sampler. procRoot is "/proc" on the host; inside a
// container with the host's /proc mounted at /host/proc, pass that, and the
// host network namespace is read through /host/proc/1/net/dev.
func NewHost(procRoot, diskPath string) *Host {
	return &Host{proc: procRoot, diskPath: diskPath}
}

// Cores returns the number of CPUs seen in the last sample.
func (h *Host) Cores() int { return int(h.cores.Load()) }

// Sample reads the current values. The first call returns zero rates.
func (h *Host) Sample(now time.Time) (Sample, error) {
	s := Sample{Ts: now.UnixMilli()}
	c := counters{ts: now.UnixNano()}

	busy, total, cores, err := readCPU(filepath.Join(h.proc, "stat"))
	if err != nil {
		return s, err
	}
	c.cpuUsage, c.cpuTotal = busy, total
	h.cores.Store(int64(cores))

	memTotal, memAvail, err := readMeminfo(filepath.Join(h.proc, "meminfo"))
	if err != nil {
		return s, err
	}
	s.MemLimit, s.MemUsed = memTotal, memTotal-min(memAvail, memTotal)

	// /proc/net/dev shows the reading process's network namespace. With the
	// host's /proc mounted into a container, PID 1 there is the host's init.
	netPath := filepath.Join(h.proc, "net", "dev")
	if h.proc != "/proc" {
		if p := filepath.Join(h.proc, "1", "net", "dev"); fileExists(p) {
			netPath = p
		}
	}
	if rx, tx, err := readNetDev(netPath, true); err == nil {
		c.netRx, c.netTx = rx, tx
	}
	if r, w, err := readDiskstats(filepath.Join(h.proc, "diskstats")); err == nil {
		c.blkRead, c.blkWrite = r, w
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(h.diskPath, &st); err == nil {
		bs := uint64(st.Bsize) // #nosec G115 -- block size is positive
		s.DiskTotal = st.Blocks * bs
		s.DiskUsed = (st.Blocks - st.Bfree) * bs
	}

	if p := h.prev; p != nil {
		secs := float64(c.ts-p.ts) / 1e9
		if dt := c.cpuTotal - p.cpuTotal; c.cpuTotal > p.cpuTotal && dt > 0 {
			s.CPU = float64(c.cpuUsage-p.cpuUsage) / float64(dt) * 100
		}
		s.NetRx, s.NetTx = rate(c.netRx, p.netRx, secs), rate(c.netTx, p.netTx, secs)
		s.BlkRead, s.BlkWrite = rate(c.blkRead, p.blkRead, secs), rate(c.blkWrite, p.blkWrite, secs)
	}
	h.prev = &c
	return s, nil
}

// readCPU returns busy and total jiffies from the aggregate "cpu" line.
func readCPU(path string) (busy, total uint64, cores int, err error) {
	b, err := os.ReadFile(path) // #nosec G304 -- fixed /proc path
	if err != nil {
		return 0, 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "cpu" {
			var vals []uint64
			for _, x := range f[1:] {
				v, _ := strconv.ParseUint(x, 10, 64)
				vals = append(vals, v)
			}
			if len(vals) < 4 {
				return 0, 0, 0, errors.New("metrics: malformed cpu line")
			}
			// user nice system idle iowait irq softirq steal (guest is included in user)
			for i, v := range vals {
				if i < 8 {
					total += v
				}
			}
			idle := vals[3]
			if len(vals) > 4 {
				idle += vals[4]
			}
			busy = total - idle
		} else if strings.HasPrefix(f[0], "cpu") {
			cores++
		}
	}
	if total == 0 {
		return 0, 0, 0, errors.New("metrics: no cpu line in " + path)
	}
	return busy, total, cores, nil
}

func readMeminfo(path string) (total, avail uint64, err error) {
	f, err := os.Open(path) // #nosec G304 -- fixed /proc path
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		switch k {
		case "MemTotal":
			total = n * 1024
		case "MemAvailable":
			avail = n * 1024
		}
	}
	if total == 0 {
		return 0, 0, errors.New("metrics: MemTotal missing")
	}
	return total, avail, sc.Err()
}

// readNetDev sums rx/tx bytes. With physicalOnly, loopback and virtual
// Docker interfaces are skipped so traffic is not counted twice.
func readNetDev(path string, physicalOnly bool) (rx, tx uint64, err error) {
	b, err := os.ReadFile(path) // #nosec G304 -- fixed /proc path
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if physicalOnly && (name == "lo" || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-")) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx, nil
}

// readDiskstats sums sectors read/written over whole disks (not partitions).
func readDiskstats(path string) (read, write uint64, err error) {
	b, err := os.ReadFile(path) // #nosec G304 -- fixed /proc path
	if err != nil {
		return 0, 0, err
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) < 10 {
			continue
		}
		name := f[2]
		if !isWholeDisk(name) {
			continue
		}
		r, _ := strconv.ParseUint(f[5], 10, 64)
		w, _ := strconv.ParseUint(f[9], 10, 64)
		read += r * 512
		write += w * 512
	}
	return read, write, nil
}

func isWholeDisk(name string) bool {
	for _, p := range []string{"loop", "ram", "dm-", "sr", "md"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	switch {
	case strings.HasPrefix(name, "nvme"), strings.HasPrefix(name, "mmcblk"):
		return !strings.Contains(name, "p") // nvme0n1 yes, nvme0n1p1 no
	case strings.HasPrefix(name, "sd"), strings.HasPrefix(name, "vd"), strings.HasPrefix(name, "xvd"), strings.HasPrefix(name, "hd"):
		last := name[len(name)-1]
		return last < '0' || last > '9'
	}
	return false
}

// String helps debugging.
func (s Sample) String() string {
	return fmt.Sprintf("cpu=%.1f%% mem=%d/%d net=%.0f/%.0f", s.CPU, s.MemUsed, s.MemLimit, s.NetRx, s.NetTx)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
