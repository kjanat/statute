package statute

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func requestBudgetRetry(size string) Middleware {
	return Retry(2, OnStatus(503)).RequestBufferBudget(size)
}

func retryRequestBudgetOrigin(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "ab" {
			t.Errorf("body=%q error=%v", body, err)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "retry candidate")
	}))
	t.Cleanup(origin.Close)
	return origin, calls
}

func assertRetryRequestBudgetCalls(t *testing.T, h http.Handler, calls *atomic.Int32, target string, want int32) {
	t.Helper()
	before := calls.Load()
	r := httptest.NewRequest(http.MethodPut, target, strings.NewReader("ab"))
	rec := runRequest(t, h, r)
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "retry candidate" || calls.Load()-before != want {
		t.Fatalf("target=%s status=%d body=%q calls=%d want=%d", target, rec.Code, rec.Body.String(), calls.Load()-before, want)
	}
}

func TestRetryRequestBudgetStaticSharedPool(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			origin, calls := retryRequestBudgetOrigin(t)
			cfg := Config{
				Listeners: Listeners{HTTP(":0")},
				Upstreams: Upstreams{"shared": Pool{Backends: []Backend{{Address: origin.URL}}}},
				Routes: Routes{
					Match("/small").ProxyTo("shared").With(requestBudgetRetry("1B")),
					Match("/large").ProxyTo("shared").With(requestBudgetRetry("4KiB")),
				},
			}
			if fallback {
				cfg.FallbackRoutes, cfg.Routes = cfg.Routes, nil
			}
			h := fallbackRouter(t, cfg)
			for range 2 {
				assertRetryRequestBudgetCalls(t, h, calls, "/small", 1)
				assertRetryRequestBudgetCalls(t, h, calls, "/large", 2)
			}
		})
	}
}

func TestRetryRequestBudgetDockerReplacement(t *testing.T) {
	origin, calls := retryRequestBudgetOrigin(t)
	host, port := backendHostPort(t, origin)
	cfg, err := resolveDocker(Docker().TraefikLabels().Middleware("small", requestBudgetRetry("1B")).
		Middleware("large", requestBudgetRetry("4KiB")))
	if err != nil {
		t.Fatal(err)
	}
	c := fakeDaemonContainer{name: "app", ip: host, port: port, labels: map[string]string{
		"traefik.enable":                                        "true",
		"traefik.http.routers.small.rule":                       "Host(`app.example`) && Path(`/small`)",
		"traefik.http.routers.small.service":                    "shared",
		"traefik.http.routers.small.middlewares":                "small",
		"traefik.http.routers.large.rule":                       "Host(`app.example`) && Path(`/large`)",
		"traefik.http.routers.large.service":                    "shared",
		"traefik.http.routers.large.middlewares":                "large",
		"traefik.http.services.shared.loadbalancer.server.port": fmt.Sprint(port),
	}}
	p, srv, replace := newFakeProvider(t, cfg, []fakeDaemonContainer{c})
	mustSync(t, p)
	first := srv.dynamic.Load()
	if len(first.pools) != 1 || len(first.routes) != 2 {
		t.Fatalf("pools=%d routes=%d", len(first.pools), len(first.routes))
	}
	h := srv.buildRouter()
	assertRetryRequestBudgetCalls(t, h, calls, "http://app.example/small", 1)
	assertRetryRequestBudgetCalls(t, h, calls, "http://app.example/large", 2)
	c.labels = maps.Clone(c.labels)
	c.labels["traefik.http.routers.small.middlewares"] = "large"
	replace([]fakeDaemonContainer{c})
	mustSync(t, p)
	second := srv.dynamic.Load()
	for name, pool := range first.pools {
		if second.pools[name] != pool {
			t.Fatalf("request policy replaced shared pool %q", name)
		}
	}
	assertRetryRequestBudgetCalls(t, h, calls, "http://app.example/small", 2)
	assertRetryRequestBudgetCalls(t, h, calls, "http://app.example/large", 2)
}
