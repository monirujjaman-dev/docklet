// Package metrics samples host and container resource usage.
//
// Host figures come from /proc and statfs. Container figures come from
// cgroup v2 files when Docklet can see them (cheapest), otherwise from
// Docker's one-shot stats API (works on cgroup v1, Docker Desktop and when
// Docklet itself runs in a container). Live samples are kept in memory;
// one-minute averages are stored in SQLite for history.
package metrics

// Sample is one measurement. Rates are per second over the last interval.
type Sample struct {
	Ts        int64   `json:"ts"`       // unix milliseconds
	CPU       float64 `json:"cpu"`      // percent; host: of all cores, container: of one core
	MemUsed   uint64  `json:"memUsed"`  // bytes
	MemLimit  uint64  `json:"memLimit"` // bytes (host total, or container limit)
	DiskUsed  uint64  `json:"diskUsed,omitempty"`
	DiskTotal uint64  `json:"diskTotal,omitempty"`
	NetRx     float64 `json:"netRx"`    // bytes/s
	NetTx     float64 `json:"netTx"`    // bytes/s
	BlkRead   float64 `json:"blkRead"`  // bytes/s
	BlkWrite  float64 `json:"blkWrite"` // bytes/s
}

// counters are cumulative values used to derive rates between samples.
type counters struct {
	ts       int64 // unix nanoseconds
	cpuUsage uint64
	cpuTotal uint64 // host only: total jiffies
	netRx    uint64
	netTx    uint64
	blkRead  uint64
	blkWrite uint64
}

func rate(cur, prev uint64, seconds float64) float64 {
	if seconds <= 0 || cur < prev { // counter reset (container restart)
		return 0
	}
	return float64(cur-prev) / seconds
}
