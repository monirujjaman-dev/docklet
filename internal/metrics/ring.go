package metrics

import "sync"

// Ring keeps the most recent samples for one series.
type Ring struct {
	mu   sync.Mutex
	buf  []Sample
	next int
	full bool
}

// NewRing holds up to size samples.
func NewRing(size int) *Ring { return &Ring{buf: make([]Sample, size)} }

// Add appends a sample, overwriting the oldest when full.
func (r *Ring) Add(s Sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = s
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// Since returns samples with Ts >= sinceMs, oldest first.
func (r *Ring) Since(sinceMs int64) []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Sample
	n, start := r.next, 0
	if r.full {
		n, start = len(r.buf), r.next
	}
	for i := 0; i < n; i++ {
		s := r.buf[(start+i)%len(r.buf)]
		if s.Ts >= sinceMs {
			out = append(out, s)
		}
	}
	return out
}

// Last returns the newest sample and whether one exists.
func (r *Ring) Last() (Sample, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full && r.next == 0 {
		return Sample{}, false
	}
	return r.buf[(r.next-1+len(r.buf))%len(r.buf)], true
}

// minuteAgg averages samples within a minute.
type minuteAgg struct {
	minute int64 // unix seconds at the start of the minute
	n      int
	sum    Sample
}

func (a *minuteAgg) add(s Sample) {
	a.n++
	a.sum.CPU += s.CPU
	a.sum.MemUsed += s.MemUsed
	a.sum.MemLimit = s.MemLimit
	a.sum.DiskUsed = s.DiskUsed
	a.sum.DiskTotal = s.DiskTotal
	a.sum.NetRx += s.NetRx
	a.sum.NetTx += s.NetTx
	a.sum.BlkRead += s.BlkRead
	a.sum.BlkWrite += s.BlkWrite
}

func (a *minuteAgg) mean() Sample {
	n := float64(a.n)
	return Sample{
		Ts:  a.minute * 1000,
		CPU: a.sum.CPU / n, MemUsed: a.sum.MemUsed / uint64(a.n), MemLimit: a.sum.MemLimit, // #nosec G115 -- n > 0
		DiskUsed: a.sum.DiskUsed, DiskTotal: a.sum.DiskTotal,
		NetRx: a.sum.NetRx / n, NetTx: a.sum.NetTx / n,
		BlkRead: a.sum.BlkRead / n, BlkWrite: a.sum.BlkWrite / n,
	}
}
