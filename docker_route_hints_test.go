package statute

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"statute.kjanat.dev/resolved"
)

// All requests use the same client address and therefore the same rate bucket.
func routeHintsStatus(t *testing.T, frontend *httptest.Server, path string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, frontend.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "app.example.com"
	res, err := frontend.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode
}

func routeHintsLabels(path string, hints map[string]string) map[string]string {
	labels := map[string]string{
		"statute.enable": "true", "statute.service": "shared",
		"statute.host": "app.example.com", "statute.path": path,
	}
	maps.Copy(labels, hints)
	return labels
}

func assertRouteHintRates(t *testing.T, routes []compiledRoute, otherRate string) {
	t.Helper()
	for _, route := range routes {
		wantRate := 1.0 / 3600
		if route.route.Pattern == "/other" {
			wantRate = 0
			if otherRate != "" {
				wantRate = 2.0 / 3600
			}
		}
		var gotRate float64
		for _, mw := range route.route.Middleware {
			if mw.Type == resolved.MWRateLimit {
				gotRate = mw.RateLimitPerSecond
			}
		}
		if gotRate != wantRate {
			t.Errorf("%s: normalized rate=%g want=%g", route.route.Pattern, gotRate, wantRate)
		}
	}
}

func TestDockerRouteHintsIndependentOnSharedPool(t *testing.T) {
	backend := httptest.NewServer(noContentHandler)
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)
	for _, limitedName := range []string{"a-limited", "z-limited"} {
		for _, otherRate := range []string{"", "2/h"} {
			t.Run(limitedName+"/"+otherRate, func(t *testing.T) {
				cfg, err := resolveDocker(Docker().DefaultMiddleware(RequestID()))
				if err != nil {
					t.Fatal(err)
				}
				p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{
					{name: limitedName, ip: host, port: port, labels: routeHintsLabels("/limited", map[string]string{"statute.ratelimit": "1/h"})},
					{name: "m-other", ip: host, port: port, labels: routeHintsLabels("/other", map[string]string{"statute.ratelimit": otherRate})},
				})
				mustSync(t, p)
				tab := srv.dynamic.Load()
				if len(tab.routes) != 2 || len(tab.pools) != 1 {
					t.Fatalf("routes=%d pools=%d", len(tab.routes), len(tab.pools))
				}
				if tab.routes[0].route.Upstream != tab.routes[1].route.Upstream {
					t.Fatal("routes no longer share their upstream pool")
				}
				assertRouteHintRates(t, tab.routes, otherRate)
				frontend := httptest.NewServer(srv.buildRouter())
				t.Cleanup(frontend.Close)
				otherSecond := 204
				if otherRate != "" {
					otherSecond = 429
				}
				for _, tc := range []struct {
					path string
					want int
				}{
					{"/limited", 204},
					{"/limited", 429},
					{"/other", 204},
					{"/other", otherSecond},
					{"/other", otherSecond},
				} {
					if got := routeHintsStatus(t, frontend, tc.path); got != tc.want {
						t.Errorf("%s: status=%d want=%d", tc.path, got, tc.want)
					}
				}
			})
		}
	}
}

func TestDockerRouteHintsConflictRefusesAndRepairs(t *testing.T) {
	backend := httptest.NewServer(noContentHandler)
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)
	for _, conflictName := range []string{"a-conflict", "z-conflict"} {
		for _, tc := range []struct {
			name          string
			first, second map[string]string
		}{
			{"rate", map[string]string{"statute.ratelimit": "1/h"}, map[string]string{"statute.ratelimit": "2/h"}},
			{"absent", map[string]string{"statute.ratelimit": "1/h"}, nil},
			{"timeout", map[string]string{"statute.timeout": "5s"}, map[string]string{"statute.timeout": "10s"}},
			{"compress", map[string]string{"statute.compress": "gzip"}, map[string]string{"statute.compress": "br"}},
		} {
			t.Run(conflictName+"/"+tc.name, func(t *testing.T) {
				first := fakeDaemonContainer{name: "m-first", ip: host, port: port, labels: routeHintsLabels("/admin/*", tc.first)}
				second := fakeDaemonContainer{name: conflictName, ip: host, port: port, labels: routeHintsLabels("/admin/*", tc.second)}
				sibling := fakeDaemonContainer{name: "sibling", ip: host, port: port, labels: routeHintsLabels("/*", nil)}
				sibling.labels["statute.routes.health.path"] = "/admin/health"
				sibling.labels["statute.routes.health.host"] = "app.example.com"
				p, srv, replace := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{first, second, sibling})
				fallback := fallbackServer(t, srv, Routes{Match("/admin/static").Handle(noContentHandler)})
				mustSync(t, p)
				frontend := httptest.NewServer(srv.buildRouter())
				t.Cleanup(frontend.Close)
				for _, check := range []struct {
					path string
					want int
				}{
					{"/admin/private", 404},
					{"/public", 204},
					{"/admin/health", 204},
					{"/admin/static", 204},
				} {
					if got := routeHintsStatus(t, frontend, check.path); got != check.want {
						t.Errorf("conflict %s: status=%d want=%d", check.path, got, check.want)
					}
				}
				if fallback.Load() != 0 || len(srv.dynamic.Load().rejections) == 0 || len(srv.dynamic.Load().pools) != 1 {
					t.Errorf("fallback=%d refusals=%d pools=%d", fallback.Load(), len(srv.dynamic.Load().rejections), len(srv.dynamic.Load().pools))
				}
				pool := srv.dynamic.Load().pools["shared"]
				second.labels = maps.Clone(first.labels)
				replace([]fakeDaemonContainer{first, second, sibling})
				mustSync(t, p)
				if got := routeHintsStatus(t, frontend, "/admin/private"); got != 204 {
					t.Errorf("repaired route: status=%d want=204", got)
				}
				assertRouteHintsRepair(t, srv.dynamic.Load(), pool)
			})
		}
	}
}

func assertRouteHintsRepair(t *testing.T, table *dynamicTable, pool *runningPool) {
	t.Helper()
	if len(table.rejections) != 0 {
		t.Error("repair retained rejected claims")
	}
	if table.pools["shared"] != pool {
		t.Error("route policy repair replaced the shared pool runtime")
	}
}

func TestDockerRouteHintsEquivalentReplicasCoalesce(t *testing.T) {
	backend := httptest.NewServer(noContentHandler)
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)
	for _, tc := range []struct {
		name          string
		first, second map[string]string
	}{
		{"identical", map[string]string{"statute.ratelimit": "1/h"}, map[string]string{"statute.ratelimit": "1/h"}},
		{"duration", map[string]string{"statute.timeout": "1m"}, map[string]string{"statute.timeout": "60s"}},
		{"rate", map[string]string{"statute.ratelimit": "60/h"}, map[string]string{"statute.ratelimit": "1/min"}},
		{"compression-set", map[string]string{"statute.compress": "gzip,br,gzip"}, map[string]string{"statute.compress": "br,gzip"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, srv, _ := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{
				{name: "a-replica", ip: host, port: port, labels: routeHintsLabels("/app", tc.first)},
				{name: "z-replica", ip: host, port: port, labels: routeHintsLabels("/app", tc.second)},
			})
			mustSync(t, p)
			tab := srv.dynamic.Load()
			if len(tab.routes) != 1 || len(tab.pools) != 1 || len(tab.rejections) != 0 {
				t.Fatalf("routes=%d pools=%d refusals=%d", len(tab.routes), len(tab.pools), len(tab.rejections))
			}
			frontend := httptest.NewServer(srv.buildRouter())
			t.Cleanup(frontend.Close)
			if got := routeHintsStatus(t, frontend, "/app"); got != 204 {
				t.Fatalf("equivalent route: status=%d want=204", got)
			}
			if tc.name == "identical" || tc.name == "rate" {
				if got := routeHintsStatus(t, frontend, "/app"); got != 429 {
					t.Fatalf("coalesced rate policy: status=%d want=429", got)
				}
			}
		})
	}
}

func TestDockerRouteHintsPreserveAllRoutePoliciesAndDefaults(t *testing.T) {
	for _, hintedName := range []string{"a-hinted", "z-hinted"} {
		t.Run(hintedName, func(t *testing.T) {
			cfg, err := resolveDocker(Docker().DefaultMiddleware(RequestID()))
			if err != nil {
				t.Fatal(err)
			}
			p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{
				{name: hintedName, ip: "127.0.0.1", port: 1, labels: routeHintsLabels("/first", map[string]string{
					"statute.timeout": "5s", "statute.ratelimit": "1/h", "statute.compress": "gzip",
				})},
				{name: "m-other", ip: "127.0.0.1", port: 1, labels: routeHintsLabels("/second", map[string]string{
					"statute.timeout": "10s", "statute.ratelimit": "2/h", "statute.compress": "br",
				})},
			})
			mustSync(t, p)
			if len(srv.dynamic.Load().routes) != 2 {
				t.Fatalf("routes=%d want=2", len(srv.dynamic.Load().routes))
			}
			for _, route := range srv.dynamic.Load().routes {
				chain := []Middleware{RequestID(), Timeout("5s"), RateLimit("1/h"), Compress(Gzip)}
				if route.route.Pattern == "/second" {
					chain = []Middleware{RequestID(), Timeout("10s"), RateLimit("2/h"), Compress(Brotli)}
				}
				want, err := resolveMiddlewares(chain)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(route.route.Middleware, want) {
					t.Errorf("%s: chain=%+v want=%+v", route.route.Pattern, route.route.Middleware, want)
				}
			}
		})
	}
}
