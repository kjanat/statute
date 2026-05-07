package statute

import (
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/kjanat/statute/resolved"
)

// accessLogMiddleware writes a single JSON line per request to the configured
// destination. The line is written after the response is committed so that
// upstream errors and final status codes are visible.
//
// When SampleRate < 1.0, successful (2xx, 3xx) requests are sampled at the
// configured rate. Errors (4xx, 5xx) are always logged so misbehaving paths
// remain visible even at low sampling rates.
func accessLogMiddleware(cfg resolved.AccessLog, next http.Handler) http.Handler {
	if !cfg.Enabled || cfg.Writer == nil {
		return next
	}
	enc := newSafeEncoder(cfg.Writer)
	rate := cfg.SampleRate
	if rate <= 0 {
		rate = 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		if !shouldLog(ww.status, rate) {
			return
		}
		entry := map[string]any{
			"ts":            start.UTC().Format(time.RFC3339Nano),
			"method":        r.Method,
			"host":          r.Host,
			"path":          r.URL.Path,
			"query":         r.URL.RawQuery,
			"remote":        clientIP(r),
			"user_agent":    r.UserAgent(),
			"referer":       r.Referer(),
			"status":        ww.status,
			"duration_us":   time.Since(start).Microseconds(),
			"proto":         r.Proto,
			"forwarded_for": r.Header.Get("X-Forwarded-For"),
		}
		enc.Encode(entry)
	})
}

// shouldLog returns true when this request should be written. Errors are
// always logged; successful requests pass at the configured sample rate.
func shouldLog(status int, rate float64) bool {
	if status >= 400 {
		return true
	}
	if rate >= 1 {
		return true
	}
	return rand.Float64() < rate
}

// safeEncoder serializes JSON writes so multiple goroutines do not interleave.
type safeEncoder struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newSafeEncoder(w jsonWriter) *safeEncoder {
	return &safeEncoder{enc: json.NewEncoder(w)}
}

func (s *safeEncoder) Encode(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(v)
}

// jsonWriter is satisfied by io.Writer; aliased for clarity at call site.
type jsonWriter interface {
	Write(p []byte) (n int, err error)
}

// clientIP returns the best-effort client IP for the request. When the
// request arrived on a listener marked BehindCloudflare, the
// CF-Connecting-IP and True-Client-IP headers are trusted (set by
// Cloudflare's edge and not user-controllable in that path).
//
// Otherwise X-Forwarded-For is consulted; the first entry is the originating
// client per RFC 7239. Note that XFF is forgeable when a request reaches a
// listener that is not actually behind a trusted proxy — only enable trust
// when you control the network path.
func clientIP(r *http.Request) string {
	if isBehindCloudflare(r) {
		if cf := r.Header.Get("CF-Connecting-IP"); cf != "" {
			return cf
		}
		if tc := r.Header.Get("True-Client-IP"); tc != "" {
			return tc
		}
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	return r.RemoteAddr
}
