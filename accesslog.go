package statute

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/kjanat/statute/resolved"
)

// accessLogMiddleware writes a single JSON line per request to the configured
// destination. The line is written after the response is committed so that
// upstream errors and final status codes are visible.
func accessLogMiddleware(cfg resolved.AccessLog, next http.Handler) http.Handler {
	if !cfg.Enabled || cfg.Writer == nil {
		return next
	}
	enc := newSafeEncoder(cfg.Writer)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
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

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// First entry is the originating client.
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	return r.RemoteAddr
}
