package metrics

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// HostScope is the scope name of host metrics; apps use AppScope(id).
const HostScope = "host"

// AppScope returns the scope name for an app.
func AppScope(appID int64) string { return "app:" + strconv.FormatInt(appID, 10) }

// Snapshot is what live subscribers receive every interval.
type Snapshot struct {
	Ts   int64            `json:"ts"`
	Host *Sample          `json:"host,omitempty"`
	Apps map[int64]Sample `json:"apps,omitempty"`
}

// Status describes the collector.
type Status struct {
	ContainerSource string `json:"containerSource"`
	Cores           int    `json:"cores"`
	IntervalMs      int64  `json:"intervalMs"`
	RetentionDays   int    `json:"retentionDays"`
}

// Collector samples on an interval, keeps recent samples in memory and
// writes one-minute averages to SQLite.
type Collector struct {
	st         *store.Store
	log        *slog.Logger
	host       *Host
	api        StatsAPI // nil disables container metrics
	cgroupRoot string
	proc       string
	now        func() time.Time

	Interval  time.Duration
	Retention time.Duration
	RingSize  int

	mu       sync.Mutex
	src      ContainerSource
	every    int // sample containers every N ticks
	hostRing *Ring
	appRings map[int64]*Ring
	aggs     map[string]*minuteAgg
	subs     map[chan Snapshot]struct{}
}

// Config configures a collector.
type Config struct {
	Proc       string // /proc, or the host's /proc mounted in a container
	CgroupRoot string // /sys/fs/cgroup, or a host mount
	DiskPath   string // filesystem to report for the host
}

// New creates a collector. api may be nil (no container metrics).
func New(st *store.Store, api StatsAPI, cfg Config, log *slog.Logger) *Collector {
	if cfg.Proc == "" {
		cfg.Proc = "/proc"
	}
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = "/sys/fs/cgroup"
	}
	return &Collector{
		st: st, log: log, api: api, host: NewHost(cfg.Proc, cfg.DiskPath),
		cgroupRoot: cfg.CgroupRoot, proc: cfg.Proc, now: time.Now,
		Interval: 2 * time.Second, Retention: 7 * 24 * time.Hour, RingSize: 900, // 30 min at 2 s
		appRings: map[int64]*Ring{}, aggs: map[string]*minuteAgg{}, subs: map[chan Snapshot]struct{}{},
	}
}

// SetClock overrides the time source (tests).
func (c *Collector) SetClock(now func() time.Time) { c.now = now }

// Status returns collector details.
func (c *Collector) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Status{ContainerSource: "none", Cores: c.host.Cores(), IntervalMs: c.Interval.Milliseconds(),
		RetentionDays: int(c.Retention / (24 * time.Hour))}
	if c.src != nil {
		s.ContainerSource = c.src.Name()
	} else if c.api != nil {
		s.ContainerSource = "pending"
	}
	return s
}

// Subscribe returns a channel of live snapshots and a cancel function.
func (c *Collector) Subscribe() (<-chan Snapshot, func()) {
	ch := make(chan Snapshot, 4)
	c.mu.Lock()
	c.subs[ch] = struct{}{}
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		delete(c.subs, ch)
		c.mu.Unlock()
	}
}

// Recent returns in-memory samples for scope since sinceMs.
func (c *Collector) Recent(scope string, sinceMs int64) []Sample {
	c.mu.Lock()
	var r *Ring
	if scope == HostScope {
		r = c.hostRing
	} else if id, ok := parseAppScope(scope); ok {
		r = c.appRings[id]
	}
	c.mu.Unlock()
	if r == nil {
		return []Sample{}
	}
	out := r.Since(sinceMs)
	if out == nil {
		out = []Sample{}
	}
	return out
}

// History returns stored one-minute averages between from and to.
func (c *Collector) History(ctx context.Context, scope string, from, to time.Time) ([]Sample, error) {
	rows, err := c.st.Q.ListMetrics(ctx, db.ListMetricsParams{Scope: scope, FromTs: from.Unix(), ToTs: to.Unix()})
	if err != nil {
		return nil, err
	}
	out := make([]Sample, 0, len(rows))
	for _, r := range rows {
		out = append(out, Sample{
			Ts: r.Ts * 1000, CPU: r.Cpu, MemUsed: u64(r.MemUsed), MemLimit: u64(r.MemLimit),
			DiskUsed: u64(r.DiskUsed), DiskTotal: u64(r.DiskTotal),
			NetRx: r.NetRx, NetTx: r.NetTx, BlkRead: r.BlkRead, BlkWrite: r.BlkWrite,
		})
	}
	// Include the minute in progress so the newest point is current.
	c.mu.Lock()
	if a := c.aggs[scope]; a != nil && a.n > 0 && a.minute >= from.Unix() {
		out = append(out, a.mean())
	}
	c.mu.Unlock()
	return out, nil
}

func parseAppScope(scope string) (int64, bool) {
	if len(scope) < 5 || scope[:4] != "app:" {
		return 0, false
	}
	id, err := strconv.ParseInt(scope[4:], 10, 64)
	return id, err == nil
}

func u64(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

func i64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// Run samples until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	c.mu.Lock()
	c.hostRing = NewRing(c.RingSize)
	c.mu.Unlock()

	t := time.NewTicker(c.Interval)
	defer t.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	c.prune(ctx)
	tick := 0
	for {
		c.tick(ctx, tick)
		tick++
		select {
		case <-ctx.Done():
			c.flushAll(context.WithoutCancel(ctx))
			return
		case <-prune.C:
			c.prune(ctx)
		case <-t.C:
		}
	}
}

func (c *Collector) tick(ctx context.Context, n int) {
	now := c.now()
	snap := Snapshot{Ts: now.UnixMilli()}

	if hs, err := c.host.Sample(now); err != nil {
		if n == 0 {
			c.log.Warn("host metrics unavailable", "err", err)
		}
	} else {
		c.hostRing.Add(hs)
		c.aggregate(ctx, HostScope, hs)
		snap.Host = &hs
	}

	if c.api != nil && (c.every == 0 || n%c.every == 0) {
		snap.Apps = c.sampleApps(ctx, now)
	}
	c.broadcast(snap)
}

func (c *Collector) sampleApps(ctx context.Context, now time.Time) map[int64]Sample {
	rows, err := c.st.Q.ListAppContainers(ctx)
	if err != nil {
		c.log.Warn("list app containers", "err", err)
		return nil
	}
	ids := make([]string, 0, len(rows))
	byContainer := make(map[string]int64, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ContainerID)
		byContainer[r.ContainerID] = r.AppID
	}
	c.chooseSource(ids)

	c.mu.Lock()
	src := c.src
	c.mu.Unlock()
	if src == nil || len(ids) == 0 {
		c.dropRings(byContainer)
		return nil
	}
	samples := src.Sample(ctx, ids, now)
	out := make(map[int64]Sample, len(samples))
	for cid, s := range samples {
		appID := byContainer[cid]
		out[appID] = s
		c.mu.Lock()
		r := c.appRings[appID]
		if r == nil {
			r = NewRing(c.RingSize)
			c.appRings[appID] = r
		}
		c.mu.Unlock()
		r.Add(s)
		c.aggregate(ctx, AppScope(appID), s)
	}
	c.dropRings(byContainer)
	return out
}

// chooseSource prefers cgroup v2 files when they show our containers.
func (c *Collector) chooseSource(ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.src != nil || len(ids) == 0 {
		return
	}
	hostMem := uint64(0)
	if last, ok := c.hostRing.Last(); ok {
		hostMem = last.MemLimit
	}
	if cg := NewCgroupSource(c.cgroupRoot, c.proc, c.api, hostMem); cg != nil && cg.Visible(ids) {
		c.src, c.every = cg, 1
	} else {
		// The stats API costs dockerd more per call; sample every 3rd tick.
		c.src, c.every = NewDockerStatsSource(c.api), 3
	}
	c.log.Info("container metrics source selected", "source", c.src.Name())
}

func (c *Collector) dropRings(current map[string]int64) {
	keep := map[int64]bool{}
	for _, id := range current {
		keep[id] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.appRings {
		if !keep[id] {
			delete(c.appRings, id)
		}
	}
}

func (c *Collector) aggregate(ctx context.Context, scope string, s Sample) {
	minute := s.Ts / 60000 * 60
	c.mu.Lock()
	a := c.aggs[scope]
	var done *minuteAgg
	if a == nil || a.minute != minute {
		done = a
		a = &minuteAgg{minute: minute}
		c.aggs[scope] = a
	}
	a.add(s)
	c.mu.Unlock()
	if done != nil && done.n > 0 {
		c.write(ctx, scope, done.mean())
	}
}

func (c *Collector) write(ctx context.Context, scope string, m Sample) {
	err := c.st.Q.UpsertMetric(ctx, db.UpsertMetricParams{
		Scope: scope, Ts: m.Ts / 1000, Cpu: m.CPU, MemUsed: i64(m.MemUsed), MemLimit: i64(m.MemLimit),
		DiskUsed: i64(m.DiskUsed), DiskTotal: i64(m.DiskTotal),
		NetRx: m.NetRx, NetTx: m.NetTx, BlkRead: m.BlkRead, BlkWrite: m.BlkWrite,
	})
	if err != nil && ctx.Err() == nil {
		c.log.Warn("store metrics", "scope", scope, "err", err)
	}
}

// flushAll writes partial minutes on shutdown so a restart loses nothing.
func (c *Collector) flushAll(ctx context.Context) {
	c.mu.Lock()
	aggs := c.aggs
	c.aggs = map[string]*minuteAgg{}
	c.mu.Unlock()
	for scope, a := range aggs {
		if a.n > 0 {
			c.write(ctx, scope, a.mean())
		}
	}
}

func (c *Collector) prune(ctx context.Context) {
	cutoff := c.now().Add(-c.Retention).Unix()
	if n, err := c.st.Q.PruneMetrics(ctx, cutoff); err != nil {
		c.log.Warn("prune metrics", "err", err)
	} else if n > 0 {
		c.log.Debug("pruned old metrics", "rows", n)
	}
}

func (c *Collector) broadcast(s Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ch := range c.subs {
		select {
		case ch <- s:
		default: // slow client: drop this snapshot
		}
	}
}
