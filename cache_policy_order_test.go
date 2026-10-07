package statute

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func TestResolveCacheIPPolicyOrder(t *testing.T) {
	t.Parallel()
	for _, policy := range []Middleware{AllowIPs("192.0.2.0/24"), DenyIPs("198.51.100.0/24")} {
		for _, fallback := range []bool{false, true} {
			cfg := Config{Listeners: Listeners{HTTP(":8080")}, Routes: Routes{
				Match("/*").Handle(http.NotFoundHandler()).With(Cache("1h"), ETag(), Retry(2), policy, Cache("1m")),
			}}
			path := "route[0]"
			if fallback {
				cfg.FallbackRoutes, cfg.Routes = cfg.Routes, nil
				path = "fallback_routes[0]"
			}
			_, err := Resolve(cfg)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "IP policy must precede Cache") {
				t.Fatalf("fallback=%v policy=%T: %v", fallback, policy, err)
			}
		}
	}
}

func TestCacheIPPolicySafeOrders(t *testing.T) {
	t.Parallel()
	for _, policy := range []Middleware{AllowIPs("192.0.2.0/24"), DenyIPs("198.51.100.0/24")} {
		for _, mws := range [][]Middleware{
			{policy, Cache("1h"), ETag(), Cache("1m")},
			{Cache("0s"), policy, Cache("1h")},
		} {
			resolvedMWs, err := resolveMiddlewares(mws)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			h := wrapMiddleware(resolvedMWs, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusOK)
			}))
			for _, ip := range []string{"192.0.2.1:1234", "198.51.100.1:1234", "192.0.2.2:1234"} {
				r := httptest.NewRequest("GET", "/", nil)
				r.RemoteAddr = ip
				want := http.StatusOK
				if strings.HasPrefix(ip, "198.") {
					want = http.StatusForbidden
				}
				if rec := runRequest(t, h, r); rec.Code != want {
					t.Fatalf("%s: status=%d want=%d", ip, rec.Code, want)
				}
			}
			if calls != 1 {
				t.Fatalf("safe cache not preserved: %d calls", calls)
			}
		}
	}
}

func TestDockerCacheIPPolicyOrder(t *testing.T) {
	t.Parallel()
	for _, cfg := range []*DockerConfig{
		Docker().DefaultMiddleware(Cache("1h"), AllowIPs("192.0.2.0/24")),
		Docker().Middleware("bad", Cache("1h"), DenyIPs("198.51.100.0/24")),
	} {
		if _, err := resolveDocker(cfg); err == nil || !strings.Contains(err.Error(), "IP policy must precede Cache") {
			t.Fatalf("unsafe Docker configuration: %v", err)
		}
	}
	cfg, err := resolveDocker(Docker().Middleware("cache", Cache("1h")).Middleware("acl", AllowIPs("192.0.2.0/24")))
	if err != nil {
		t.Fatal(err)
	}
	p := &dockerProvider{cfg: cfg}
	for _, names := range [][]string{{"cache", "acl"}, {"acl", "cache"}, {"cache"}} {
		_, warning := p.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Middlewares: names}, nil)
		wantFailure := len(names) == 2 && names[0] == "cache"
		if (warning != "") != wantFailure {
			t.Fatalf("%v: %q", names, warning)
		}
	}
}

func TestDockerDefaultCacheBeforeNamedIPPolicy(t *testing.T) {
	t.Parallel()
	cfg, err := resolveDocker(Docker().DefaultMiddleware(Cache("1h")).Middleware("acl", DenyIPs("198.51.100.0/24")))
	if err != nil {
		t.Fatal(err)
	}
	p := &dockerProvider{cfg: cfg}
	_, warning := p.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Middlewares: []string{"acl"}}, nil)
	if !strings.Contains(warning, "IP policy must precede Cache") {
		t.Fatalf("default cache bypassed named policy: %q", warning)
	}
}

func TestCacheIPPolicyFallbackAndTrustedProxy(t *testing.T) {
	t.Parallel()
	calls := 0
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	})
	h := fallbackRouter(t, Config{
		Listeners:      Listeners{HTTP(":0")},
		FallbackRoutes: Routes{Match("/*").Handle(base).With(AllowIPs("2001:db8::/32"), Cache("1h"))},
	})
	h = trustedProxyMiddleware(&resolved.Listener{TrustedProxies: []string{"192.0.2.0/24"}, ClientIPHeader: "X-Forwarded-For"}, h)
	for _, tc := range []struct {
		ip     string
		status int
	}{
		{"2001:db8::1", http.StatusOK},
		{"2001:db9::1", http.StatusForbidden},
		{"2001:db8::2", http.StatusOK},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("X-Forwarded-For", tc.ip)
		if rec := runRequest(t, h, r); rec.Code != tc.status {
			t.Fatalf("%s: status=%d want=%d", tc.ip, rec.Code, tc.status)
		}
	}
	if calls != 1 {
		t.Fatalf("fallback cache not preserved: %d calls", calls)
	}
}

func TestDockerCacheIPConflictRefusal(t *testing.T) {
	cfg, err := resolveDocker(Docker().TraefikLabels().Middleware("cache", Cache("1h")).Middleware("acl", AllowIPs("192.0.2.0/24")))
	if err != nil {
		t.Fatal(err)
	}
	containers := []fakeDaemonContainer{{
		name: "app", ip: "10.0.0.9", port: 3000,
		labels: map[string]string{
			"traefik.enable":                        "true",
			"traefik.http.routers.bad.rule":         "Host(`bad.example.com`)",
			"traefik.http.routers.bad.service":      "app",
			"traefik.http.routers.bad.middlewares":  "cache,acl",
			"traefik.http.routers.good.rule":        "Host(`good.example.com`)",
			"traefik.http.routers.good.service":     "app",
			"traefik.http.routers.good.middlewares": "acl,cache",
		},
	}}
	p, srv, update := newFakeProvider(t, cfg, containers)
	mustSync(t, p)
	tab := srv.dynamic.Load()
	if len(tab.routes) != 1 || tab.routes[0].route.Host != "good.example.com" {
		t.Fatalf("routes=%+v", tab.routes)
	}
	r := httptest.NewRequest("GET", "http://bad.example.com/", nil)
	h := findDynamicHandler(tab, r.Host, r)
	if h == nil {
		t.Fatal("unsafe route fell through to fallback")
	}
	if rec := runRequest(t, h, r); rec.Code != http.StatusNotFound {
		t.Fatalf("refusal=%d", rec.Code)
	}
	containers[0].labels["traefik.http.routers.bad.middlewares"] = "acl,cache"
	update(containers)
	mustSync(t, p)
	h = findDynamicHandler(srv.dynamic.Load(), r.Host, r)
	if h == nil {
		t.Fatal("repaired route missing")
	}
	r.RemoteAddr = "198.51.100.1:1234"
	if rec := runRequest(t, h, r); rec.Code != http.StatusForbidden {
		t.Fatalf("repaired route did not enforce IP policy: %d", rec.Code)
	}
}

func TestCacheIPPolicySharedPoolIsolation(t *testing.T) {
	t.Parallel()
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("X-Call", fmt.Sprint(calls))
	}))
	defer backend.Close()
	h := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{"shared": Pool{Backends: []Backend{{Address: backend.URL}}}},
		Routes: Routes{
			Match("/private").ProxyTo("shared").With(AllowIPs("192.0.2.0/24"), Cache("1h")),
			Match("/public").ProxyTo("shared").With(Cache("1h")),
		},
	})
	for _, tc := range []struct {
		path, ip, call string
		status         int
	}{
		{"/private", "192.0.2.1:1234", "1", 200},
		{"/private", "198.51.100.1:1234", "", 403},
		{"/public", "198.51.100.1:1234", "2", 200},
		{"/private", "192.0.2.2:1234", "1", 200},
		{"/public", "192.0.2.2:1234", "2", 200},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.RemoteAddr = tc.ip
		rec := runRequest(t, h, r)
		if rec.Code != tc.status || rec.Header().Get("X-Call") != tc.call {
			t.Fatalf("%s %s: status=%d headers=%v", tc.path, tc.ip, rec.Code, rec.Header())
		}
	}
}
