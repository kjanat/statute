package statute

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// BenchmarkMatchPattern measures the cost of a single matchPattern call
// across the typical pattern shapes.
func BenchmarkMatchPattern(b *testing.B) {
	cases := []struct{ pattern, path string }{
		{"/", "/"},
		{"/api", "/api"},
		{"/api/*", "/api/v1/users"},
		{"/static/*", "/static/css/site.css"},
		{"/*", "/anything"},
	}
	for _, c := range cases {
		b.Run(c.pattern+"_"+c.path, func(b *testing.B) {
			for b.Loop() {
				_ = matchPattern(c.pattern, c.path)
			}
		})
	}
}

// BenchmarkResponseBufferReplay measures buffer write + replay cost across
// payload sizes. Cache, ETag, and Retry all hit this path.
func BenchmarkResponseBufferReplay(b *testing.B) {
	sizes := []int{256, 4 * 1024, 64 * 1024, 512 * 1024}
	for _, n := range sizes {
		payload := bytes.Repeat([]byte("x"), n)
		b.Run(formatSize(n), func(b *testing.B) {
			b.SetBytes(int64(n))
			for b.Loop() {
				buf := newResponseBuffer()
				buf.Header().Set("Content-Type", "text/plain")
				buf.WriteHeader(http.StatusOK)
				_, _ = buf.Write(payload)
				rec := httptest.NewRecorder()
				buf.replay(rec)
			}
		})
	}
}

// BenchmarkGzipCompression measures end-to-end compression for the wrapper
// path used by the Compress middleware. Provides a baseline to compare
// against future brotli numbers.
func BenchmarkGzipCompression(b *testing.B) {
	payload := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 1024)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	})
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		serveGzip(rec, req, inner)
		// Drain so the gzip Writer flushes.
		_, _ = io.Copy(io.Discard, rec.Body)
	}
	_ = gzip.NewWriter // keep import even if SetBytes calc shifts
}

// BenchmarkBucketAllowContended measures the rate limiter under contention
// to validate the token-bucket implementation scales to multi-core access.
func BenchmarkBucketAllowContended(b *testing.B) {
	bucket := newBucket(1000, 1000)
	var dropped int64
	var mu sync.Mutex
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if !bucket.allow() {
				mu.Lock()
				dropped++
				mu.Unlock()
			}
		}
	})
	b.ReportMetric(float64(dropped), "dropped")
}

func formatSize(n int) string {
	switch {
	case n >= 1<<20:
		return mustItoa(n>>20) + "MiB"
	case n >= 1<<10:
		return mustItoa(n>>10) + "KiB"
	default:
		return mustItoa(n) + "B"
	}
}

func mustItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
