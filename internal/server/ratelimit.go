package server

import (
	"sync"
	"time"
)

// rateLimiter is a small per-key token bucket held in memory.
type rateLimiter struct {
	mu       sync.Mutex
	capacity float64
	refill   float64 // tokens per second
	buckets  map[string]*bucket
	now      func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(capacity int, per time.Duration, now func() time.Time) *rateLimiter {
	return &rateLimiter{
		capacity: float64(capacity),
		refill:   float64(capacity) / per.Seconds(),
		buckets:  make(map[string]*bucket),
		now:      now,
	}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.capacity, last: t}
		l.buckets[key] = b
	}
	b.tokens += t.Sub(b.last).Seconds() * l.refill
	if b.tokens > l.capacity {
		b.tokens = l.capacity
	}
	b.last = t
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// prune drops buckets that have fully refilled.
func (l *rateLimiter) prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	for k, b := range l.buckets {
		if b.tokens+t.Sub(b.last).Seconds()*l.refill >= l.capacity {
			delete(l.buckets, k)
		}
	}
}
