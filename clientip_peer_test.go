package statute

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIPPeerNormalization(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ peer, want string }{
		{"192.0.2.1:1234", "192.0.2.1"},
		{"192.0.2.1:5678", "192.0.2.1"},
		{"[2001:db8::1]:1234", "2001:db8::1"},
		{"[2001:0db8:0:0:0:0:0:1]:5678", "2001:db8::1"},
		{"[::ffff:192.0.2.1]:1234", "192.0.2.1"},
		{"192.0.2.1", "192.0.2.1"},
		{"2001:db8::1", "2001:db8::1"},
		{"", ""},
		{"custom-peer:1234", "custom-peer:1234"},
		{"192.0.2.1:bad", "192.0.2.1:bad"},
	} {
		t.Run(tc.peer, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.peer
			if got := clientIP(r); got != tc.want || r.RemoteAddr != tc.peer {
				t.Fatalf("client=%q peer=%q, want client=%q unchanged peer=%q", got, r.RemoteAddr, tc.want, tc.peer)
			}
			h := behindCloudflareMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				if got := clientIP(r); got != tc.want {
					t.Fatalf("Cloudflare without identity headers: got %q want %q", got, tc.want)
				}
			}))
			h.ServeHTTP(httptest.NewRecorder(), r)
		})
	}
}

func TestRateLimitPeerPortChurn(t *testing.T) {
	t.Parallel()
	for _, peers := range [][]string{
		{"192.0.2.1:1234", "192.0.2.1:5678", "[::ffff:192.0.2.1]:9999", "192.0.2.2:1234"},
		{"[2001:db8::1]:1234", "[2001:db8::1]:5678", "[2001:0db8:0:0:0:0:0:1]:9999", "[2001:db8::2]:1234"},
	} {
		m, err := resolveRateLimitMW(RateLimit("1/h").MaxBuckets(1))
		if err != nil {
			t.Fatal(err)
		}
		h := rateLimitHandler(m, noContentHandler)
		for i, peer := range peers {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = peer
			for _, header := range []string{"X-Forwarded-For", "CF-Connecting-IP", "True-Client-IP"} {
				r.Header.Set(header, fmt.Sprintf("198.51.100.%d", i+1))
			}
			want := []int{204, 429, 429, 503}[i]
			if rec := runRequest(t, h, r); rec.Code != want {
				t.Fatalf("peer %s: got %d want %d", peer, rec.Code, want)
			}
		}
	}
}

func TestPeerPortAttributionConsumers(t *testing.T) {
	t.Parallel()
	backends := mkStates("a", "b", "c")
	picker := &ipHashPicker{}
	want := picker.pick(backends, "192.0.2.1")
	for port := 1000; port < 1100; port++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = fmt.Sprintf("192.0.2.1:%d", port)
		r.Header.Set("X-Forwarded-For", "198.51.100.1, untrusted")
		if got := picker.pick(backends, clientIP(r)); got != want {
			t.Fatalf("source port %d changed IPHash affinity", port)
		}
		entry := accessLogEntry(r, &statusRecorder{}, time.Now())
		if entry["remote"] != "192.0.2.1" || entry["forwarded_for"] != r.Header.Get("X-Forwarded-For") {
			t.Fatalf("unexpected attribution: %v", entry)
		}
	}
}
