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

// Observe records one handled request: its status code and latency.
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

// WritePrometheus writes the accumulated stats to w in Prometheus text
// exposition format.
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

// WriteHeader records the first final status written, then forwards it. A
// 1xx is informational: net/http keeps the response open, so it passes
// through without latching — the final status is still to come, and it is
// the one the access log and metrics must see. Latching on the preview
// would also swallow the final WriteHeader, leaving net/http to commit an
// implicit 200 whatever the handler actually answered. The one exception is
// 101 Switching Protocols: net/http excludes it from the informational path
// because no further response may follow it, so it latches as final here too.
func (s *statusRecorder) WriteHeader(code int) {
	if code < 200 && code != http.StatusSwitchingProtocols {
		s.ResponseWriter.WriteHeader(code)
		return
	}
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

// Write treats an un-preceded body write as an implicit 200, then
// forwards the bytes.
func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// ReadFrom treats the copy like Write — an un-preceded one is an implicit
// 200 — and hands it to the underlying writer, so io.Copy keeps the sendfile
// path net/http offers for file responses.
func (s *statusRecorder) ReadFrom(r io.Reader) (int64, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(s.ResponseWriter, r)
}

// Flush propagates Flush calls so streaming responses still flush through us.
// A flush is a commit: net/http writes an implicit 200 when no header has
// been sent yet, so the recorder latches it before forwarding — a later
// WriteHeader can no longer change the client-visible status.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		s.wroteHeader = true
		f.Flush()
	}
}
