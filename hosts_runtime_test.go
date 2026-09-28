package statute

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestHostsNativeMatching(t *testing.T) {
	t.Parallel()
	router := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes:    Routes{Match("/*").Hosts("App.example.", "b.example").Handle(noContentHandler)},
	})
	for _, tc := range []struct {
		host string
		want int
	}{
		{"APP.EXAMPLE.", http.StatusNoContent},
		{"app.example", http.StatusNotFound},
		{"B.EXAMPLE:8080", http.StatusNoContent},
		{"b.example.", http.StatusNotFound},
		{"unknown.example", http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://example/", nil)
		req.Host = tc.host
		if got := runRequest(t, router, req).Code; got != tc.want {
			t.Errorf("host %s: status=%d want=%d", tc.host, got, tc.want)
		}
	}
}

func TestHostsRetryTransforms(t *testing.T) {
	t.Parallel()
	attempts := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if r.URL.Path != "/v2/api/item" || !slices.Equal(r.Header.Values("X-Tag"), []string{"once"}) {
			t.Errorf("retry saw path=%q tags=%v", r.URL.Path, r.Header.Values("X-Tag"))
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	router := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{Match("/api/*").Hosts("a.example", "b.example").Handle(handler).With(
			Retry(2, OnStatus(http.StatusBadGateway)), AddPrefix("/v2"), AddRequestHeader("X-Tag", "once"),
		)},
	})
	for _, host := range []string{"a.example", "b.example"} {
		attempts = 0
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/item", nil)
		rec := runRequest(t, router, req)
		if rec.Code != http.StatusBadGateway || attempts != 2 {
			t.Fatalf("host %s: status=%d attempts=%d", host, rec.Code, attempts)
		}
		if req.URL.Path != "/api/item" {
			t.Fatal("route transform mutated the original request URL")
		}
	}
}

func TestHostsIndependentRateLimits(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	router := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{Match("/*").Hosts("a.example", "b.example").Handle(countingFallback(&calls)).
			With(RateLimit("1/h").Per(ClientIP))},
	})
	for _, host := range []string{"a.example", "b.example"} {
		for _, want := range []int{http.StatusTeapot, http.StatusTooManyRequests} {
			req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
			req.RemoteAddr = "192.0.2.1:1234"
			if got := runRequest(t, router, req).Code; got != want {
				t.Fatalf("host %s: status=%d want=%d", host, got, want)
			}
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("shared application handler calls=%d want=2", calls.Load())
	}
}

func TestHostsSharedPoolPolicyIsolation(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen", r.Header.Get("X-Policy"))
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	router := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{"shared": Pool{Backends: []Backend{{Address: strings.TrimPrefix(backend.URL, "http://")}}}},
		Routes: Routes{
			Match("/*").Hosts("a.example", "b.example").ProxyTo("shared").With(SetRequestHeader("X-Policy", "multi")),
			Match("/*").Host("c.example").ProxyTo("shared").With(SetRequestHeader("X-Policy", "single")),
		},
	})
	for _, tc := range []struct{ host, policy string }{
		{"a.example", "multi"}, {"b.example", "multi"}, {"c.example", "single"},
	} {
		rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/", nil))
		if rec.Code != http.StatusNoContent || rec.Header().Get("X-Seen") != tc.policy {
			t.Fatalf("host %s: status=%d policy=%q", tc.host, rec.Code, rec.Header().Get("X-Seen"))
		}
	}
}

func TestHostsClientIPFallthrough(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/*").Hosts("a.example", "b.example").ClientIPs("10.0.0.0/8").Handle(noContentHandler),
			Match("/*").Hosts("a.example", "b.example").RedirectTo("/login", http.StatusFound),
		},
	}
	l := trustedListener(t, TrustedProxy("192.0.2.0/24"))
	router := trustedProxyMiddleware(l, fallbackRouter(t, cfg))
	for _, host := range []string{"a.example", "b.example"} {
		for _, tc := range []struct {
			peer string
			want int
		}{
			{"192.0.2.1:1234", http.StatusNoContent},
			{"198.51.100.1:1234", http.StatusFound},
		} {
			req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
			req.RemoteAddr = tc.peer
			req.Header.Set("X-Forwarded-For", "10.0.0.1")
			if got := runRequest(t, router, req).Code; got != tc.want {
				t.Errorf("host %s peer %s: status=%d want=%d", host, tc.peer, got, tc.want)
			}
		}
	}
}

func TestHostsStaticDockerPrecedence(t *testing.T) {
	backend := httptest.NewServer(noContentHandler)
	t.Cleanup(backend.Close)
	ip, port := backendHostPort(t, backend)
	containers := make([]fakeDaemonContainer, 0, 2)
	for _, host := range []string{"a.example", "b.example"} {
		containers = append(containers, fakeDaemonContainer{
			name: host, ip: ip, port: port,
			labels: map[string]string{"statute.enable": "true", "statute.host": host},
		})
	}
	p, srv, _ := newFakeProvider(t, &resolved.Docker{}, containers)
	var calls atomic.Int64
	srv.cfg = mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{Match("/static").Hosts("a.example", "b.example").
			RedirectTo("/target", http.StatusFound)},
		Fallback: countingFallback(&calls),
	})
	mustSync(t, p)
	router := srv.buildRouter()
	for _, tc := range []struct {
		url  string
		want int
	}{
		{"http://a.example/static", http.StatusFound},
		{"http://b.example/static", http.StatusFound},
		{"http://a.example/dynamic", http.StatusNoContent},
		{"http://b.example/dynamic", http.StatusNoContent},
		{"http://other.example/static", http.StatusTeapot},
	} {
		if got := runRequest(t, router, httptest.NewRequest(http.MethodGet, tc.url, nil)).Code; got != tc.want {
			t.Errorf("%s: status=%d want=%d", tc.url, got, tc.want)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fallback calls=%d want=1", calls.Load())
	}
}
