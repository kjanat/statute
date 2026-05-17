package statute

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// stats is a tiny in-process metrics store. It exposes the most useful proxy
// counters without pulling in the full Prometheus client library. The output
// format is the Prometheus text exposition format, so existing scrapers work
// unchanged.
type stats struct {
	requests         atomic.Uint64
	requestsByStatus sync.Map // status int -> *atomic.Uint64

	durationSum   atomic.Uint64 // accumulated microseconds
	durationCount atomic.Uint64
}

func newStats() *stats { return &stats{} }

func (s *stats) Observe(status int, dur time.Duration) {
	s.requests.Add(1)
	v, _ := s.requestsByStatus.LoadOrStore(status, &atomic.Uint64{})
	v.(*atomic.Uint64).Add(1)
	// Monotonic-clock durations are never negative in practice; clamp
	// defensively so the uint64 conversion can never wrap.
	us := max(dur/time.Microsecond, 0)
	s.durationSum.Add(uint64(us))
	s.durationCount.Add(1)
}

func (s *stats) WritePrometheus(w io.Writer) {
	pf := func(format string, args ...any) {
		_, _ = fmt.Fprintf(w, format, args...)
	}

	pf("# HELP statute_requests_total Total HTTP requests handled.\n")
	pf("# TYPE statute_requests_total counter\n")
	pf("statute_requests_total %d\n", s.requests.Load())

	pf("# HELP statute_requests_by_status_total HTTP requests by response status.\n")
	pf("# TYPE statute_requests_by_status_total counter\n")
	s.requestsByStatus.Range(func(k, v any) bool {
		pf("statute_requests_by_status_total{status=\"%d\"} %d\n", k.(int), v.(*atomic.Uint64).Load())
		return true
	})

	pf("# HELP statute_request_duration_microseconds_sum Sum of request durations in microseconds.\n")
	pf("# TYPE statute_request_duration_microseconds_sum counter\n")
	pf("statute_request_duration_microseconds_sum %d\n", s.durationSum.Load())

	pf("# HELP statute_request_duration_microseconds_count Count of observed requests.\n")
	pf("# TYPE statute_request_duration_microseconds_count counter\n")
	pf("statute_request_duration_microseconds_count %d\n", s.durationCount.Load())
}

// metricsMiddleware wraps a handler and observes status + duration into stats.
func metricsMiddleware(s *stats, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		s.Observe(ww.status, time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush propagates Flush calls so streaming responses still flush through us.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
