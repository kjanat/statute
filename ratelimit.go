package statute

import (
	"container/heap"
	"crypto/sha256"
	"fmt"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"statute.kjanat.dev/resolved"
)

const defaultRateLimitMaxBuckets = 65536

// rateLimitHandler owns one bounded store; neither pools nor other routes share it.
func rateLimitHandler(m resolved.Middleware, next http.Handler) http.Handler {
	if m.RateLimitPerSecond <= 0 {
		return next
	}
	buckets := newBucketStore(m.RateLimitPerSecond, m.RateLimitMaxBuckets)
	if m.RateLimitBurst > 0 {
		buckets.capacity = float64(m.RateLimitBurst)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := rateLimitRequestKey(m, r)
		if m.RateLimitReservations {
			serveRateReservation(buckets, m, key, w, r, next)
			return
		}
		switch buckets.allow(key, time.Now()) {
		case http.StatusTooManyRequests:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		case http.StatusServiceUnavailable:
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "rate limit capacity unavailable", http.StatusServiceUnavailable)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// The map and indexed heap have exactly one entry per live bucket. Hashes retain
// no attacker-controlled strings (in particular arbitrarily long Host values).
// The mutex covers token accounting and slot admission, never HTTP work.
type bucketStore struct {
	mu             sync.Mutex
	rate, capacity float64
	limit          int
	buckets        map[[sha256.Size]byte]*bucket
	expiry         bucketHeap
}

type bucket struct {
	key          [sha256.Size]byte
	tokens       float64
	last, fullAt time.Time
	index        int
	limiter      *rate.Limiter
}

func newBucketStore(rate float64, limit int) *bucketStore {
	if limit <= 0 {
		limit = defaultRateLimitMaxBuckets
	}
	capacity := math.MaxFloat64
	if rate <= math.MaxFloat64/2 {
		capacity = max(1, rate*2)
	}
	return &bucketStore{rate: rate, capacity: capacity, limit: limit, buckets: make(map[[sha256.Size]byte]*bucket)}
}

// allow accepts an explicit clock reading to make refill and retirement testable.
// Zero means admitted; denials return their HTTP status.
func (s *bucketStore) allow(key string, now time.Time) int {
	digest := sha256.Sum256([]byte(key))
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[digest]
	if b == nil {
		if len(s.buckets) == s.limit && !s.retire(now) {
			return http.StatusServiceUnavailable
		}
		b = &bucket{key: digest, tokens: s.capacity, last: now}
		s.buckets[digest] = b
		heap.Push(&s.expiry, b)
	}
	s.refill(b, now)
	status := http.StatusTooManyRequests
	if b.tokens >= 1 {
		b.tokens--
		status = 0
	}
	b.fullAt = s.deadline(b)
	heap.Fix(&s.expiry, b.index)
	return status
}

func (s *bucketStore) reserve(key string, now time.Time, cfg resolved.Middleware) (time.Duration, bool) {
	digest := sha256.Sum256([]byte(key))
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[digest]
	if b != nil && cfg.RateLimitIdleTTL > 0 && !now.Before(b.fullAt) {
		heap.Remove(&s.expiry, b.index)
		delete(s.buckets, digest)
		b = nil
	}
	if b == nil {
		if len(s.buckets) >= s.limit {
			if !cfg.RateLimitEvictIdle {
				return 0, false
			}
			oldest := heap.Pop(&s.expiry).(*bucket)
			delete(s.buckets, oldest.key)
		}
		capacity := int(min(s.capacity, float64(math.MaxInt)))
		b = &bucket{key: digest, limiter: rate.NewLimiter(rate.Limit(s.rate), capacity)}
		s.buckets[digest] = b
		heap.Push(&s.expiry, b)
	}
	b.fullAt = now.Add(cfg.RateLimitIdleTTL)
	heap.Fix(&s.expiry, b.index)
	reservation := b.limiter.ReserveN(now, 1)
	if !reservation.OK() {
		return time.Duration(math.MaxInt64), true
	}
	delay := reservation.DelayFrom(now)
	if delay > cfg.RateLimitMaxDelay {
		reservation.CancelAt(now)
	}
	return delay, true
}

func (s *bucketStore) refill(b *bucket, now time.Time) {
	// Backward clock readings preserve the last accounted time. Each elapsed
	// interval replenishes tokens at most once.
	if now.After(b.last) {
		b.tokens = min(s.capacity, b.tokens+now.Sub(b.last).Seconds()*s.rate)
		b.last = now
	}
}

func (s *bucketStore) deadline(b *bucket) time.Time {
	nanos := math.Ceil((s.capacity - b.tokens) / s.rate * float64(time.Second))
	delay := time.Duration(math.MaxInt64)
	if nanos < float64(math.MaxInt64) {
		delay = time.Duration(nanos)
	}
	return b.last.Add(delay)
}

func (s *bucketStore) retire(now time.Time) bool {
	b := s.expiry[0]
	if now.Before(b.fullAt) {
		return false
	}
	// A rounded or saturated deadline is only a hint. Recheck real token debt
	// under the admission lock before releasing the slot.
	s.refill(b, now)
	if b.tokens < s.capacity {
		b.fullAt = s.deadline(b)
		heap.Fix(&s.expiry, b.index)
		return false
	}
	heap.Pop(&s.expiry)
	delete(s.buckets, b.key)
	return true
}

type bucketHeap []*bucket

func (h bucketHeap) Len() int           { return len(h) }
func (h bucketHeap) Less(i, j int) bool { return h[i].fullAt.Before(h[j].fullAt) }
func (h bucketHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}

func (h *bucketHeap) Push(value any) {
	b := value.(*bucket)
	b.index = len(*h)
	*h = append(*h, b)
}

func (h *bucketHeap) Pop() any {
	old := *h
	b := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return b
}

func rateLimitRequestKey(m resolved.Middleware, r *http.Request) string {
	key := selectedRequestIP(m.IPSelection, r)
	switch m.RateLimitKey {
	case resolved.KeyHostHeader:
		return r.Host
	case resolved.KeyRequestHeader:
		return r.Header.Get(m.RateLimitRequestHeader)
	case resolved.KeyPeerIP:
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			return host
		}
		return r.RemoteAddr
	default:
		return key
	}
}

func serveRateReservation(buckets *bucketStore, m resolved.Middleware, key string, w http.ResponseWriter, r *http.Request, next http.Handler) {
	delay, ok := buckets.reserve(key, time.Now(), m)
	if !ok {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "rate limit capacity unavailable", http.StatusServiceUnavailable)
		return
	}
	if delay > m.RateLimitMaxDelay {
		w.Header().Set("Retry-After", fmt.Sprintf("%.0f", math.Ceil(delay.Seconds())))
		w.Header().Set("X-Retry-In", delay.String())
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(http.StatusText(http.StatusTooManyRequests)))
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-r.Context().Done():
		http.Error(w, "context canceled", http.StatusInternalServerError)
		return
	case <-timer.C:
	}
	next.ServeHTTP(w, r)
}
