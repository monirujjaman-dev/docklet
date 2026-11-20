package metrics

import (
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// fakeProc writes a minimal /proc with the given cumulative values.
func fakeProc(t *testing.T, dir string, user, idle, rxBytes, readSectors uint64) {
	write(t, filepath.Join(dir, "stat"), strings.Join([]string{
		"cpu  " + itoa(user) + " 0 0 " + itoa(idle) + " 0 0 0 0 0 0",
		"cpu0 1 0 0 1 0 0 0 0 0 0",
		"cpu1 1 0 0 1 0 0 0 0 0 0",
		"intr 1",
	}, "\n"))
	write(t, filepath.Join(dir, "meminfo"), "MemTotal:       2048000 kB\nMemFree:         100000 kB\nMemAvailable:    1024000 kB\n")
	write(t, filepath.Join(dir, "net", "dev"), "Inter-|   Receive\n face |bytes\n"+
		"    lo: 999 0 0 0 0 0 0 0 999 0 0 0 0 0 0 0\n"+
		"  eth0: "+itoa(rxBytes)+" 0 0 0 0 0 0 0 "+itoa(rxBytes/2)+" 0 0 0 0 0 0 0\n"+
		"vethab: 5000 0 0 0 0 0 0 0 5000 0 0 0 0 0 0 0\n")
	write(t, filepath.Join(dir, "diskstats"),
		"   8       0 sda 100 0 "+itoa(readSectors)+" 0 50 0 200 0 0 0 0\n"+
			"   8       1 sda1 100 0 999999 0 50 0 999999 0 0 0 0\n"+
			"   7       0 loop0 1 0 888888 0 0 0 0 0 0 0 0\n")
}

func itoa(v uint64) string { return strings.TrimSpace(strings.Repeat(" ", 0) + uintStr(v)) }

func uintStr(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

func TestHostSample(t *testing.T) {
	dir := t.TempDir()
	fakeProc(t, dir, 100, 900, 1000, 10)
	h := NewHost(dir, dir)
	t0 := time.Unix(1_800_000_000, 0)
	s, err := h.Sample(t0)
	if err != nil {
		t.Fatal(err)
	}
	if s.MemLimit != 2048000*1024 || s.MemUsed != 1024000*1024 || s.CPU != 0 {
		t.Fatalf("first sample = %+v", s)
	}
	if h.Cores() != 2 || s.DiskTotal == 0 {
		t.Fatalf("cores=%d disk=%d", h.Cores(), s.DiskTotal)
	}
	// +50 busy and +50 idle jiffies => 50%; +2000 rx bytes over 2 s => 1000 B/s.
	fakeProc(t, dir, 150, 950, 3000, 30)
	s, err = h.Sample(t0.Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !near(s.CPU, 50) || !near(s.NetRx, 1000) || !near(s.NetTx, 500) || !near(s.BlkRead, 20*512/2) {
		t.Fatalf("second sample = %+v", s)
	}
}

func TestIsWholeDisk(t *testing.T) {
	for name, want := range map[string]bool{
		"sda": true, "sda1": false, "nvme0n1": true, "nvme0n1p2": false, "vdb": true,
		"loop3": false, "dm-0": false, "mmcblk0": true, "mmcblk0p1": false,
	} {
		if isWholeDisk(name) != want {
			t.Errorf("%s: want %v", name, want)
		}
	}
}

func TestCgroupSource(t *testing.T) {
	root := t.TempDir()
	proc := t.TempDir()
	write(t, filepath.Join(root, "cgroup.controllers"), "cpu memory io")
	cg := filepath.Join(root, "system.slice", "docker-abc.scope")
	set := func(usageUsec, rbytes uint64) {
		write(t, filepath.Join(cg, "memory.current"), "209715200")
		write(t, filepath.Join(cg, "memory.stat"), "anon 1\ninactive_file 104857600\n")
		write(t, filepath.Join(cg, "memory.max"), "536870912")
		write(t, filepath.Join(cg, "cpu.stat"), "usage_usec "+uintStr(usageUsec)+"\nuser_usec 1\n")
		write(t, filepath.Join(cg, "io.stat"), "8:0 rbytes="+uintStr(rbytes)+" wbytes=0 rios=1 wios=0\n")
	}
	set(1_000_000, 0)
	src := NewCgroupSource(root, proc, nil, 8<<30)
	if src == nil || !src.Visible([]string{"abc"}) || src.Visible([]string{"zzz"}) {
		t.Fatal("cgroup source detection")
	}
	src.pids["abc"] = -1 // no network lookup in this test
	t0 := time.Unix(1_800_000_000, 0)
	src.Sample(context.Background(), []string{"abc"}, t0)
	set(2_000_000, 4096) // +1 s CPU over 2 s wall => 50% of one core
	got := src.Sample(context.Background(), []string{"abc"}, t0.Add(2*time.Second))["abc"]
	if !near(got.CPU, 50) || got.MemUsed != 100<<20 || got.MemLimit != 512<<20 || !near(got.BlkRead, 2048) {
		t.Fatalf("sample = %+v", got)
	}
	if NewCgroupSource(t.TempDir(), proc, nil, 0) != nil {
		t.Fatal("non-v2 root accepted")
	}
}

type fakeStats struct {
	mu    sync.Mutex
	usage map[string]uint64
}

func (f *fakeStats) ContainerStats(_ context.Context, id string) (docker.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s docker.Stats
	s.CPUStats.CPUUsage.TotalUsage = f.usage[id]
	s.MemoryStats.Usage, s.MemoryStats.Limit = 300<<20, 1<<30
	s.MemoryStats.Stats = map[string]uint64{"total_inactive_file": 100 << 20}
	s.Networks = map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	}{"eth0": {RxBytes: f.usage[id] / 1000, TxBytes: 0}}
	return s, nil
}

func (f *fakeStats) ContainerInspect(context.Context, string) (docker.ContainerInfo, error) {
	return docker.ContainerInfo{}, nil
}

func TestDockerStatsSource(t *testing.T) {
	api := &fakeStats{usage: map[string]uint64{"a": 1e9, "b": 0}}
	src := NewDockerStatsSource(api)
	t0 := time.Unix(1_800_000_000, 0)
	src.Sample(context.Background(), []string{"a", "b"}, t0)
	api.usage["a"] = 3e9 // +2 s of CPU in 2 s => 100% of one core
	got := src.Sample(context.Background(), []string{"a", "b"}, t0.Add(2*time.Second))
	if !near(got["a"].CPU, 100) || got["a"].MemUsed != 200<<20 || !near(got["a"].NetRx, 1e6) {
		t.Fatalf("a = %+v", got["a"])
	}
	if got["b"].CPU != 0 {
		t.Fatalf("b = %+v", got["b"])
	}
}

func TestRing(t *testing.T) {
	r := NewRing(3)
	if _, ok := r.Last(); ok {
		t.Fatal("empty ring has a last sample")
	}
	for i := int64(1); i <= 5; i++ {
		r.Add(Sample{Ts: i})
	}
	got := r.Since(0)
	if len(got) != 3 || got[0].Ts != 3 || got[2].Ts != 5 {
		t.Fatalf("got %+v", got)
	}
	if got := r.Since(5); len(got) != 1 {
		t.Fatalf("since: %+v", got)
	}
	if last, _ := r.Last(); last.Ts != 5 {
		t.Fatal("last")
	}
}

func TestCollectorAggregatesAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fakeProc(t, dir, 100, 900, 0, 0)
	c := New(st, nil, Config{Proc: dir, DiskPath: dir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	clock := time.Unix(1_800_000_000, 0) // minute boundary
	c.SetClock(func() time.Time { return clock })
	c.hostRing = NewRing(10)

	ch, cancel := c.Subscribe()
	defer cancel()
	for i := 0; i < 40; i++ { // 80 s at 2 s => crosses one minute boundary
		c.tick(ctx, i)
		clock = clock.Add(2 * time.Second)
	}
	select {
	case s := <-ch:
		if s.Host == nil {
			t.Fatal("snapshot without host sample")
		}
	default:
		t.Fatal("no live snapshot delivered")
	}
	if n := len(c.Recent(HostScope, 0)); n != 10 {
		t.Fatalf("ring holds %d", n)
	}
	rows, _ := st.Q.ListMetrics(ctx, db.ListMetricsParams{Scope: HostScope, FromTs: 0, ToTs: math.MaxInt32})
	if len(rows) != 1 {
		t.Fatalf("stored minutes before shutdown = %d, want 1", len(rows))
	}
	c.flushAll(ctx) // what Run does on shutdown
	_ = st.Close()

	st2, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	c2 := New(st2, nil, Config{Proc: dir, DiskPath: dir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hist, err := c2.History(ctx, HostScope, time.Unix(0, 0), clock)
	if err != nil || len(hist) != 2 || hist[0].MemLimit != 2048000*1024 {
		t.Fatalf("history after restart = %d rows (%v)", len(hist), err)
	}

	// Retention pruning.
	c2.Retention = time.Hour
	c2.SetClock(func() time.Time { return clock.Add(2 * time.Hour) })
	c2.prune(ctx)
	if hist, _ := c2.History(ctx, HostScope, time.Unix(0, 0), clock.Add(3*time.Hour)); len(hist) != 0 {
		t.Fatalf("not pruned: %d", len(hist))
	}
}
