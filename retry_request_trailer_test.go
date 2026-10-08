package statute

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/quicvarint"
)

func TestRetryRequestTrailersHTTP3(t *testing.T) {
	for _, tc := range []struct {
		name, budget, body string
		inner              []Middleware
		calls              int32
	}{
		{"empty", "4KiB", "", nil, 2},
		{"buffered", "4KiB", "ab", nil, 2},
		{"growth", "1B", "ab", nil, 1},
		{"oversize", "4MiB", strings.Repeat("x", maxRetryBufferBytes+1), nil, 1},
		{"timeout", "1B", "ab", []Middleware{Timeout("5s")}, 1},
		{"etag-growth", "1B", "ab", []Middleware{ETag()}, 1},
		{"nested-buffered", "1B", "ab", []Middleware{requestBudgetRetry("4KiB"), Timeout("5s")}, 2},
		{"nested-fallback", "1B", "ab", []Middleware{requestBudgetRetry("1B"), Timeout("5s")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			origin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeRetryTrailerReport(t, w, r)
			})
			mws := append([]Middleware{requestBudgetRetry(tc.budget)}, tc.inner...)
			conn := retryTrailerServer(t, chain(t, origin, mws...))
			got := retryTrailerWireRequest(t, conn, tc.body)
			want := fmt.Sprintf("%d:selection", len(tc.body))
			if got != want || calls.Load() != tc.calls {
				t.Fatalf("body=%q want=%q calls=%d want=%d", got, want, calls.Load(), tc.calls)
			}
		})
	}
}

func TestRetryRequestTrailersInitialCapacityFallback(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	origin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			close(entered)
			<-release
			return
		}
		writeRetryTrailerReport(t, w, r)
	})
	h := chain(t, origin, requestBudgetRetry("1B"))
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), retryBodyRequest("/hold", &retryRequestBody{data: "h"}))
	}()
	<-entered
	defer func() { close(release); <-done }()
	conn := retryTrailerServer(t, h)
	if got := retryTrailerWireRequest(t, conn, "ab"); got != "2:selection" {
		t.Fatalf("capacity fallback body=%q", got)
	}
}

func writeRetryTrailerReport(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = fmt.Fprintf(w, "%d:%s", len(data), html.EscapeString(r.Trailer.Get("X-Selection")))
}

func retryTrailerServer(t *testing.T, origin http.Handler) *quic.Conn {
	t.Helper()
	cert, key := writeSelfSignedCert(t, "h3.example")
	srv, err := newServer(mustResolve(t, Config{
		Listeners: Listeners{HTTPS("127.0.0.1:0", StaticTLS(cert, key), HTTP3("127.0.0.1:0"))},
		Routes:    Routes{Match("/*").Handle(origin)}, Shutdown: Shutdown{GracePeriod: "2s"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	return cacheHTTP3Conn(t, srv.run.listeners.http3[0].conn.LocalAddr().String())
}

func retryTrailerWireRequest(t *testing.T, conn *quic.Conn, payload string) string {
	t.Helper()
	stream, err := conn.OpenStreamSync(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// GET / over HTTPS, with no Content-Length or trailer declaration.
	headers := append([]byte{0, 0, 0xd1, 0xd7, 0xc1, 0x50, 10}, "h3.example"...)
	cacheHTTP3WriteHeaders(t, stream, headers)
	frame := quicvarint.Append(nil, 0)
	frame = quicvarint.Append(frame, uint64(len(payload)))
	frame = append(frame, payload...)
	if _, err := stream.Write(frame); err != nil {
		t.Fatal(err)
	}
	trailers := append([]byte{0, 0, 0x27, 4}, "x-selection"...)
	trailers = append(trailers, 9)
	trailers = append(trailers, "selection"...)
	cacheHTTP3WriteHeaders(t, stream, trailers)
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	return cacheHTTP3ReadData(t, response)
}
