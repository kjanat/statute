package statute

import (
	"fmt"
	"html"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCacheNativeStaticAndFallbackAssembly(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", fallback), func(t *testing.T) {
			var selectedCalls, publicCalls atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/selected" {
					selectedCalls.Add(1)
					w.Header().Set("Vary", "X-Forwarded-For")
					_, _ = io.WriteString(w, html.EscapeString(r.Header.Get("X-Forwarded-For")))
					return
				}
				_, _ = fmt.Fprint(w, publicCalls.Add(1))
			}))
			t.Cleanup(origin.Close)
			routes := Routes{
				Match("/selected").ProxyTo("shared").With(Cache("1h")),
				Match("/public").ProxyTo("shared").With(Cache("1h")),
			}
			cfg := Config{
				Listeners: Listeners{HTTP(":0")},
				Upstreams: Upstreams{"shared": Pool{Backends: []Backend{{Address: origin.URL}}}},
				Routes:    routes,
			}
			if fallback {
				cfg.Routes, cfg.FallbackRoutes = nil, routes
			}
			h := fallbackRouter(t, cfg)
			for _, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.1"} {
				assertNativeCacheResponse(t, h, "/selected", ip, ip)
				assertNativeCacheResponse(t, h, "/public", ip, "1")
			}
			assertNativeCacheCalls(t, &selectedCalls, &publicCalls, 3, 1)
		})
	}
}

func assertNativeCacheCalls(t *testing.T, selectedCalls, publicCalls *atomic.Int32, wantSelected, wantPublic int32) {
	t.Helper()
	if selectedCalls.Load() != wantSelected || publicCalls.Load() != wantPublic {
		t.Fatalf("origin calls: selected=%d want=%d public=%d want=%d", selectedCalls.Load(), wantSelected, publicCalls.Load(), wantPublic)
	}
}

func assertNativeCacheResponse(t *testing.T, h http.Handler, target, ip, want string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.RemoteAddr = ip + ":1234"
	rec := runRequest(t, h, r)
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("%s peer=%s: status=%d body=%q want=%q", target, ip, rec.Code, rec.Body.String(), want)
	}
}

func TestCacheNativeDockerSharedPoolAndGeneration(t *testing.T) {
	var selectedCalls, publicCalls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/selected" {
			selectedCalls.Add(1)
			w.Header().Set("Vary", "X-Forwarded-For")
			_, _ = io.WriteString(w, html.EscapeString(r.Header.Get("X-Forwarded-For")))
			return
		}
		publicCalls.Add(1)
		_, _ = io.WriteString(w, "public:"+html.EscapeString(r.Header.Get("X-Generation")))
	}))
	t.Cleanup(origin.Close)
	host, port := backendHostPort(t, origin)
	cfg, err := resolveDocker(Docker().TraefikLabels().DefaultMiddleware(Cache("1h")).Middleware("next", SetRequestHeader("X-Generation", "next")))
	if err != nil {
		t.Fatal(err)
	}
	c := fakeDaemonContainer{name: "app", ip: host, port: port, labels: map[string]string{
		"traefik.enable":                                        "true",
		"traefik.http.routers.selected.rule":                    "Host(`app.example`) && Path(`/selected`)",
		"traefik.http.routers.selected.service":                 "shared",
		"traefik.http.routers.public.rule":                      "Host(`app.example`) && Path(`/public`)",
		"traefik.http.routers.public.service":                   "shared",
		"traefik.http.services.shared.loadbalancer.server.port": fmt.Sprint(port),
	}}
	p, srv, replace := newFakeProvider(t, cfg, []fakeDaemonContainer{c})
	mustSync(t, p)
	first := srv.dynamic.Load()
	if len(first.pools) != 1 || len(first.routes) != 2 {
		t.Fatalf("expected two routes sharing one pool: pools=%d routes=%d", len(first.pools), len(first.routes))
	}
	h := srv.buildRouter()
	for _, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.1"} {
		assertNativeCacheResponse(t, h, "http://app.example/selected", ip, ip)
		assertNativeCacheResponse(t, h, "http://app.example/public", ip, "public:")
	}
	assertNativeCacheCalls(t, &selectedCalls, &publicCalls, 3, 1)
	c.labels = maps.Clone(c.labels)
	c.labels["traefik.http.routers.public.middlewares"] = "next"
	replace([]fakeDaemonContainer{c})
	mustSync(t, p)
	second := srv.dynamic.Load()
	for name, pool := range first.pools {
		if second.pools[name] != pool {
			t.Fatalf("middleware-only replacement changed shared pool %q", name)
		}
	}
	assertNativeCacheResponse(t, h, "http://app.example/public", "192.0.2.1", "public:next")
	assertNativeCacheResponse(t, h, "http://app.example/public", "192.0.2.2", "public:next")
	assertNativeCacheResponse(t, h, "http://app.example/selected", "192.0.2.2", "192.0.2.2")
	assertNativeCacheCalls(t, &selectedCalls, &publicCalls, 4, 2)
}

func TestCacheNativeForwardedOperationsAcrossRetry(t *testing.T) {
	for _, cacheOutside := range []bool{false, true} {
		for _, op := range []struct {
			name string
			mw   Middleware
			want func(string) string
		}{
			{"set", SetRequestHeader("X-Forwarded-For", "198.51.100.7"), func(string) string { return "198.51.100.7" }},
			{"add", AddRequestHeader("X-Forwarded-For", "198.51.100.7"), func(ip string) string { return ip + "|198.51.100.7" }},
			{"remove", RemoveRequestHeader("X-Forwarded-For"), func(string) string { return "" }},
		} {
			t.Run(fmt.Sprintf("%s/cache-outside=%t", op.name, cacheOutside), func(t *testing.T) {
				var calls atomic.Int32
				var observedMu sync.Mutex
				var observed []string
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					value := strings.Join(r.Header.Values("X-Forwarded-For"), "|")
					observedMu.Lock()
					observed = append(observed, value)
					observedMu.Unlock()
					w.Header().Set("Vary", "X-Forwarded-For")
					if calls.Add(1)%2 == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					_, _ = io.WriteString(w, html.EscapeString(value))
				}))
				t.Cleanup(origin.Close)
				mws := []Middleware{Retry(2, OnStatus(503)), Cache("1h"), op.mw}
				if cacheOutside {
					mws[0], mws[1] = mws[1], mws[0]
				}
				h := fallbackRouter(t, Config{
					Listeners: Listeners{HTTP(":0")},
					Upstreams: Upstreams{"shared": Pool{Backends: []Backend{{Address: origin.URL}}}},
					Routes:    Routes{Match("/*").ProxyTo("shared").With(mws...)},
				})
				for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
					r := httptest.NewRequest(http.MethodGet, "/", nil)
					r.RemoteAddr = ip + ":1234"
					r.Header.Set("X-Forwarded-For", "spoofed")
					rec := runRequest(t, h, r)
					if rec.Code != http.StatusOK || rec.Body.String() != op.want(ip) {
						t.Fatalf("peer=%s status=%d body=%q want=%q", ip, rec.Code, rec.Body.String(), op.want(ip))
					}
				}
				if calls.Load() != 4 {
					t.Fatalf("origin calls=%d want=4", calls.Load())
				}
				observedMu.Lock()
				defer observedMu.Unlock()
				assertNativeCacheRetryForwarded(t, observed, op.want)
			})
		}
	}
}

func assertNativeCacheRetryForwarded(t *testing.T, observed []string, want func(string) string) {
	t.Helper()
	for i, ip := range []string{"192.0.2.1", "192.0.2.1", "192.0.2.2", "192.0.2.2"} {
		if observed[i] != want(ip) {
			t.Fatalf("attempt %d forwarded=%q want=%q", i, observed[i], want(ip))
		}
	}
}

func TestCacheCustomHandleForwardedVary(t *testing.T) {
	for _, field := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		t.Run(field, func(t *testing.T) {
			calls := 0
			h := fallbackRouter(t, Config{
				Listeners: Listeners{HTTP(":0")},
				Routes: Routes{Match("/*").Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("Vary", field)
					_, _ = io.WriteString(w, html.EscapeString(strings.Join(r.Header.Values(field), "|")))
				})).With(Cache("1h"), AddRequestHeader(field, "route"))},
			})
			for _, value := range []string{"first", "second", "first"} {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Header.Set(field, value)
				rec := runRequest(t, h, r)
				if rec.Code != http.StatusOK || rec.Body.String() != value+"|route" {
					t.Fatalf("value=%s status=%d body=%q", value, rec.Code, rec.Body.String())
				}
			}
			if calls != 2 {
				t.Fatalf("custom Handle lost header variant reuse: calls=%d", calls)
			}
		})
	}
}
