package api

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// rateLimiter is an in-memory per-key token bucket. NOTE: per instance; with several
// instances behind a load balancer the effective limit multiplies (move to Redis then).
type rateLimiter struct {
	mu      sync.Mutex
	limit   rate.Limit
	burst   int
	buckets map[string]*bucket
	lastGC  time.Time
}

type bucket struct {
	l    *rate.Limiter
	seen time.Time
}

// newRateLimiter allows n requests per window (bursting up to n).
func newRateLimiter(n int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:   rate.Every(window / time.Duration(n)),
		burst:   n,
		buckets: map[string]*bucket{},
		lastGC:  time.Now(),
	}
}

func (r *rateLimiter) allow(key string) bool {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Sub(r.lastGC) > 10*time.Minute {
		for k, b := range r.buckets {
			if now.Sub(b.seen) > 2*time.Hour {
				delete(r.buckets, k)
			}
		}
		r.lastGC = now
	}
	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{l: rate.NewLimiter(r.limit, r.burst)}
		r.buckets[key] = b
	}
	b.seen = now
	return b.l.AllowN(now, 1)
}
