package statute

import (
	"net/http"
	"sync"
	"time"

	"statute.kjanat.dev/resolved"
)

// rateLimitHandler is a simple per-key token bucket. Buckets are kept in a
// sync.Map; entries are not aggressively reaped, so this is suitable for the
// MVP at moderate cardinality. A production implementation would window-evict
// idle buckets and use sharded mutexes to reduce contention.
func rateLimitHandler(m resolved.Middleware, next http.Handler) http.Handler {
	rate := m.RateLimitPerSecond
	if rate <= 0 {
		return next
	}
	burst := rate * 2
	if burst < 1 {
		burst = 1
	}
	buckets := &sync.Map{}

	keyFor := func(r *http.Request) string {
		switch m.RateLimitKey {
		case resolved.KeyHostHeader:
			return r.Host
		default:
			return clientIP(r)
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := keyFor(r)
		v, _ := buckets.LoadOrStore(k, newBucket(rate, burst))
		b := v.(*bucket)
		if !b.allow() {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type bucket struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	rate     float64 // tokens per second
	last     time.Time
}

func newBucket(rate, capacity float64) *bucket {
	return &bucket{tokens: capacity, capacity: capacity, rate: rate, last: time.Now()}
}

func (b *bucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
