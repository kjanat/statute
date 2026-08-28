package statute

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

// fakeDaemonContainer is the wire shape /containers/json responses are
// built from in these tests.
type fakeDaemonContainer struct {
	name   string
	ip     string
	port   int
	labels map[string]string
}

func daemonJSON(t *testing.T, containers []fakeDaemonContainer) string {
	t.Helper()
	type netJSON struct {
		IPAddress string `json:"IPAddress"`
	}
	out := make([]map[string]any, 0, len(containers))
	for i, c := range containers {
		out = append(out, map[string]any{
			"Id":     fmt.Sprintf("id-%d", i),
			"Names":  []string{"/" + c.name},
			"Labels": c.labels,
			"Ports":  []map[string]any{{"PrivatePort": c.port, "Type": "tcp"}},
			"NetworkSettings": map[string]any{
				"Networks": map[string]netJSON{"bridge": {IPAddress: c.ip}},
			},
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// newFakeProvider builds a dockerProvider wired to a fake daemon serving
// the given container list. The returned function swaps the daemon's
// container list for reconcile tests.
func newFakeProvider(t *testing.T, cfg *resolved.Docker, containers []fakeDaemonContainer) (*dockerProvider, *server, func([]fakeDaemonContainer)) {
	t.Helper()
	current := daemonJSON(t, containers)
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("OK")) })
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(current))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	cfg.Endpoint = "tcp://" + strings.TrimPrefix(ts.URL, "http://")
	srv := &server{cfg: &resolved.Config{}, stats: newStats()}
	p, err := newDockerProvider(cfg, srv)
	if err != nil {
		t.Fatalf("newDockerProvider: %v", err)
	}
	t.Cleanup(func() {
		if tab := srv.dynamic.Load(); tab != nil {
			for _, ph := range tab.pools {
				ph.shutdown()
			}
		}
	})
	return p, srv, func(cs []fakeDaemonContainer) { current = daemonJSON(t, cs) }
}

// mustSync reconciles once, failing the test on error.
func mustSync(t *testing.T, p *dockerProvider) {
	t.Helper()
	if err := p.sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func TestDockerSyncBuildsRoutes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Host", r.Host)
		_, _ = w.Write([]byte("hello from backend"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	p, srv, _ := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{{
		name: "web-1", ip: host, port: port,
		labels: map[string]string{
			"statute.enable": "true",
			"statute.host":   "app.example.com",
		},
	}})
	mustSync(t, p)

	tab := srv.dynamic.Load()
	if tab == nil || len(tab.routes) != 1 {
		t.Fatalf("dynamic table = %+v", tab)
	}

	// Wrong host: no match.
	if h := findHandler(tab.routes, "other.example.com", httptest.NewRequest("GET", "http://x/", nil)); h != nil {
		t.Fatalf("route matched wrong host")
	}
	// Right host: proxies to the real backend.
	h := findHandler(tab.routes, "app.example.com", httptest.NewRequest("GET", "http://x/x", nil))
	if h == nil {
		t.Fatalf("no handler for app.example.com")
	}
	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello from backend" {
		t.Fatalf("proxy response: %d %q", rec.Code, rec.Body.String())
	}
	// The upstream sees the original Host header.
	if got := rec.Header().Get("X-Seen-Host"); got != "app.example.com" {
		t.Fatalf("host header not preserved: %q", got)
	}
	// Host matching is case-insensitive per RFC 9110.
	if h := findHandler(tab.routes, "APP.Example.COM", httptest.NewRequest("GET", "http://x/x", nil)); h == nil {
		t.Fatal("host match is case-sensitive")
	}
}

// TestDockerRun_RollbackRetryDoesNotReuseRetiredGeneration exercises the
// provider run boundary directly: stopping one attempt waits its workers,
// retires its pools, and a retry builds fresh state. Reusing the stale handle
// afterward must not tear down the later generation.
func TestDockerRun_RollbackRetryDoesNotReuseRetiredGeneration(t *testing.T) {
	p, srv, _ := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{{
		name: "web-1", ip: "127.0.0.1", port: 1,
		labels: map[string]string{"statute.enable": "true", "statute.host": "app.example.com"},
	}})

	first, err := p.start()
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	firstTable := srv.dynamic.Load()
	firstPool := firstTable.pools["web-1"]
	first.stop()
	if srv.dynamic.Load() != nil {
		t.Fatal("stopped Docker run left its generation published")
	}
	if firstPool.isLive() {
		t.Fatal("stopped Docker run left its pool live")
	}

	second, err := p.start()
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	defer second.stop()
	secondTable := srv.dynamic.Load()
	secondPool := secondTable.pools["web-1"]
	if secondPool == firstPool {
		t.Fatal("retry adopted a retired pool by fingerprint")
	}
	first.stop()
	if srv.dynamic.Load() != secondTable || !secondPool.isLive() {
		t.Fatal("stale Docker run stopped the later generation")
	}
}

// TestDockerDynamicRoutesThroughRouter — dynamic routes dispatch through
// buildRouter's fallback once the (empty) static table misses: the request
// reaches the discovered pool (whose refused backend answers 502) instead
// of the router's 404.
func TestDockerDynamicRoutesThroughRouter(t *testing.T) {
	p, srv, _ := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{{
		name: "web-1", ip: "127.0.0.1", port: 1,
		labels: map[string]string{"statute.enable": "true", "statute.host": "app.example.com"},
	}})
	mustSync(t, p)
	rr := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://app.example.com/x", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("router dispatch: got %d, want 502 from the discovered pool", rr.Code)
	}
}

func TestDockerSyncTraefikLabels(t *testing.T) {
	p, srv, _ := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{{
		name: "legacy", ip: "10.0.0.9", port: 3000,
		labels: map[string]string{
			"traefik.enable":                                     "true",
			"traefik.http.routers.app.rule":                      "Host(`legacy.example.com`) && PathPrefix(`/api`)",
			"traefik.http.services.app.loadbalancer.server.port": "3000",
		},
	}})
	mustSync(t, p)
	tab := srv.dynamic.Load()
	if len(tab.routes) != 1 {
		t.Fatalf("routes = %+v", tab.routes)
	}
	r := tab.routes[0].route
	if r.Host != "legacy.example.com" || r.Pattern != "/api" || !tab.routes[0].bytePrefix {
		t.Fatalf("route = %+v bytePrefix=%v", r, tab.routes[0].bytePrefix)
	}
	if r.Upstream.Name != "app@traefik" || r.Upstream.Backends[0].Address != "10.0.0.9:3000" {
		t.Fatalf("upstream = %+v", r.Upstream)
	}
	if h := findHandler(tab.routes, "legacy.example.com", httptest.NewRequest("GET", "http://x/api/users", nil)); h == nil {
		t.Fatal("PathPrefix route did not match subpath")
	}
	if h := findHandler(tab.routes, "legacy.example.com", httptest.NewRequest("GET", "http://x/api-secret", nil)); h == nil {
		t.Fatal("PathPrefix route did not match Traefik byte prefix /api-secret")
	}
	if h := findHandler(tab.routes, "legacy.example.com", httptest.NewRequest("GET", "http://x/other", nil)); h != nil {
		t.Fatal("route matched outside prefix")
	}
}

func TestDockerDynamicSpecificityAcrossMatcherKinds(t *testing.T) {
	newBackend := func(body string) (string, int) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(ts.Close)
		return backendHostPort(t, ts)
	}
	broadHost, broadPort := newBackend("byte")
	segmentHost, segmentPort := newBackend("segment")
	exactHost, exactPort := newBackend("exact")
	longHost, longPort := newBackend("long-byte")

	cfg, err := resolveDocker(Docker().TraefikLabels().
		Middleware("byte@file", SetResponseHeader("X-Route", "byte")).
		Middleware("exact@file", SetResponseHeader("X-Route", "exact")).
		Middleware("long@file", SetResponseHeader("X-Route", "long-byte")))
	if err != nil {
		t.Fatal(err)
	}
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{
		{name: "broad", ip: broadHost, port: broadPort, labels: map[string]string{
			"traefik.enable":                         "true",
			"traefik.http.routers.broad.rule":        "Host(`routes.example.com`) && PathPrefix(`/a`)",
			"traefik.http.routers.broad.middlewares": "byte@file",
		}},
		{name: "segment", ip: segmentHost, port: segmentPort, labels: map[string]string{
			"statute.enable": "true",
			"statute.host":   "routes.example.com",
			"statute.path":   "/api/*",
		}},
		{name: "exact", ip: exactHost, port: exactPort, labels: map[string]string{
			"traefik.enable":                         "true",
			"traefik.http.routers.exact.rule":        "Host(`routes.example.com`) && Path(`/api/exact`)",
			"traefik.http.routers.exact.middlewares": "exact@file",
		}},
		{name: "long", ip: longHost, port: longPort, labels: map[string]string{
			"traefik.enable":                        "true",
			"traefik.http.routers.long.rule":        "Host(`routes.example.com`) && PathPrefix(`/api/long`)",
			"traefik.http.routers.long.middlewares": "long@file",
		}},
	})
	fallbackServer(t, srv, nil)
	mustSync(t, p)
	router := srv.buildRouter()

	tests := []struct {
		path, body, marker string
	}{
		{"/api/segment", "segment", ""},
		{"/api-secret", "byte", "byte"},
		{"/api/exact", "exact", "exact"},
		{"/api/long/x", "long-byte", "long-byte"},
	}
	for _, tc := range tests {
		rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://routes.example.com"+tc.path, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != tc.body || rec.Header().Get("X-Route") != tc.marker {
			t.Errorf("%s: code=%d body=%q X-Route=%q, want 200 %q %q", tc.path, rec.Code, rec.Body.String(), rec.Header().Get("X-Route"), tc.body, tc.marker)
		}
	}
}

func TestDockerNativeHostDoesNotInheritTraefikDotFolding(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("native"))
	}))
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)
	p, srv, _ := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{{
		name: "native", ip: host, port: port,
		labels: map[string]string{
			"statute.enable": "true",
			"statute.host":   "native.example.com.",
		},
	}})
	calls := fallbackServer(t, srv, nil)
	mustSync(t, p)
	router := srv.buildRouter()

	dotted := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://native.example.com./", nil))
	if dotted.Code != http.StatusOK || dotted.Body.String() != "native" {
		t.Fatalf("dotted native host: code=%d body=%q", dotted.Code, dotted.Body.String())
	}
	undotted := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://native.example.com/", nil))
	if undotted.Code != http.StatusTeapot || calls.Load() != 1 {
		t.Fatalf("undotted native host: code=%d calls=%d, want fallback", undotted.Code, calls.Load())
	}
}

// TestDockerTraefikRuleTruthMatrix keeps its expected traffic independent
// of statute's parser and matcher helpers. Each row states the requests a
// supported Traefik rule should serve and checks the complete router outcome.
func TestDockerTraefikRuleTruthMatrix(t *testing.T) {
	tests := []struct {
		name   string
		rule   string
		hits   [][2]string
		misses [][2]string
	}{
		{"host", "Host(`a.example.com`)", [][2]string{{"a.example.com", "/x"}, {"a.example.com.", "/x"}}, [][2]string{{"b.example.com", "/x"}}},
		{"path", "Path(`/only`)", [][2]string{{"any.example.com", "/only"}}, [][2]string{{"any.example.com", "/only/x"}}},
		{"path prefix", "PathPrefix(`/api`)", [][2]string{{"any.example.com", "/api"}, {"any.example.com", "/api-secret"}}, [][2]string{{"any.example.com", "/ap"}}},
		{"multi argument host", "Host(`a.example.com`, `b.example.com`)", [][2]string{{"a.example.com", "/"}, {"b.example.com", "/"}}, [][2]string{{"c.example.com", "/"}}},
		{"and", "Host(`a.example.com`) && Path(`/x`)", [][2]string{{"a.example.com", "/x"}}, [][2]string{{"a.example.com", "/y"}, {"b.example.com", "/x"}}},
		{"or", "Host(`a.example.com`) || Path(`/shared`)", [][2]string{{"a.example.com", "/x"}, {"b.example.com", "/shared"}}, [][2]string{{"b.example.com", "/x"}}},
		{"parentheses", "(Host(`a.example.com`) || Host(`b.example.com`)) && (Path(`/x`) || PathPrefix(`/api`))", [][2]string{{"a.example.com", "/x"}, {"b.example.com", "/api-secret"}}, [][2]string{{"c.example.com", "/x"}, {"a.example.com", "/y"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("matched"))
			}))
			defer backend.Close()
			host, port := backendHostPort(t, backend)
			p, srv, _ := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{{
				name: "matrix", ip: host, port: port,
				labels: map[string]string{
					"traefik.enable":                   "true",
					"traefik.http.routers.matrix.rule": tc.rule,
				},
			}})
			fallbackServer(t, srv, nil)
			mustSync(t, p)
			router := srv.buildRouter()
			for _, request := range tc.hits {
				rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://"+request[0]+request[1], nil))
				if rec.Code != http.StatusOK || rec.Body.String() != "matched" {
					t.Errorf("expected hit for host=%q path=%q: code=%d body=%q", request[0], request[1], rec.Code, rec.Body.String())
				}
			}
			for _, request := range tc.misses {
				rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://"+request[0]+request[1], nil))
				if rec.Code != http.StatusTeapot {
					t.Errorf("expected fallback for host=%q path=%q: code=%d", request[0], request[1], rec.Code)
				}
			}
		})
	}
}

func TestDockerReplicasPoolTogether(t *testing.T) {
	labels := map[string]string{
		"statute.enable":  "true",
		"statute.service": "api",
		"statute.host":    "api.example.com",
		"statute.port":    "8080",
	}
	p, srv, _ := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{
		{name: "api-1", ip: "10.0.0.1", port: 8080, labels: labels},
		{name: "api-2", ip: "10.0.0.2", port: 8080, labels: labels},
	})
	mustSync(t, p)
	tab := srv.dynamic.Load()
	if len(tab.routes) != 1 {
		t.Fatalf("replicas produced %d routes: %+v", len(tab.routes), tab.routes)
	}
	pool := tab.routes[0].route.Upstream
	if len(pool.Backends) != 2 {
		t.Fatalf("pool backends = %+v", pool.Backends)
	}
	if pool.Backends[0].Address != "10.0.0.1:8080" || pool.Backends[1].Address != "10.0.0.2:8080" {
		t.Fatalf("backend addresses = %+v", pool.Backends)
	}
}

func TestDockerReconcileReusesUnchangedPools(t *testing.T) {
	c1 := fakeDaemonContainer{
		name: "stable", ip: "10.0.0.1", port: 8080,
		labels: map[string]string{"statute.enable": "true", "statute.host": "stable.example.com"},
	}
	p, srv, setContainers := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{c1})
	mustSync(t, p)
	first := srv.dynamic.Load().pools["stable"]
	if first == nil {
		t.Fatal("pool missing after first sync")
	}

	// Add a second, unrelated container: the stable pool handler must be
	// carried over by pointer identity (health state survives).
	c2 := fakeDaemonContainer{
		name: "newcomer", ip: "10.0.0.2", port: 9000,
		labels: map[string]string{"statute.enable": "true", "statute.host": "new.example.com"},
	}
	setContainers([]fakeDaemonContainer{c1, c2})
	mustSync(t, p)
	tab := srv.dynamic.Load()
	if tab.pools["stable"] != first {
		t.Error("unchanged pool handler was rebuilt")
	}
	if tab.pools["newcomer"] == nil {
		t.Error("new pool missing")
	}

	// Change the stable container's port: its handler must be replaced.
	c1changed := c1
	c1changed.port = 8081
	setContainers([]fakeDaemonContainer{c1changed, c2})
	mustSync(t, p)
	tab = srv.dynamic.Load()
	if tab.pools["stable"] == first {
		t.Error("changed pool handler was not rebuilt")
	}

	// Remove both: table empties.
	setContainers(nil)
	mustSync(t, p)
	tab = srv.dynamic.Load()
	if len(tab.routes) != 0 || len(tab.pools) != 0 {
		t.Errorf("table not emptied: %+v", tab)
	}
}

// TestPoolFingerprintChangesWithHealthPolicy — the generation-reuse
// fingerprint covers the new health fields: a pool whose probe Host,
// accepted statuses, or passive policy changed must not adopt the previous
// generation's handler and health state; an identical config still matches.
func TestPoolFingerprintChangesWithHealthPolicy(t *testing.T) {
	t.Parallel()
	base := func() *resolved.Pool {
		return &resolved.Pool{
			Name:     "svc",
			Backends: []resolved.Backend{{Address: "10.0.0.1:8080", Weight: 1}},
			HealthCheck: resolved.HealthCheck{
				Enabled: true, Path: "/healthz",
				Interval: 10 * time.Second, Timeout: 2 * time.Second,
				Healthy: 2, Unhealthy: 3,
			},
		}
	}
	one, two := base(), base()
	if poolFingerprint(one) != poolFingerprint(two) {
		t.Fatal("identical pools fingerprint differently")
	}

	variants := map[string]func(*resolved.Pool){
		"probe host": func(p *resolved.Pool) { p.HealthCheck.Host = "probe.internal" },
		"statuses":   func(p *resolved.Pool) { p.HealthCheck.Statuses = []int{200, 204} },
		"passive": func(p *resolved.Pool) {
			p.PassiveHealthCheck = resolved.PassiveHealthCheck{Enabled: true, FailureWindow: 30 * time.Second, MaxFailures: 3}
		},
	}
	baseline := poolFingerprint(base())
	for name, mutate := range variants {
		p := base()
		mutate(p)
		if poolFingerprint(p) == baseline {
			t.Errorf("%s change did not change the fingerprint", name)
		}
	}
}

func TestDockerRouteSpecificityOrder(t *testing.T) {
	p, srv, _ := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{
		{
			name: "catchall", ip: "10.0.0.1", port: 80,
			labels: map[string]string{"statute.enable": "true"},
		},
		{
			name: "api", ip: "10.0.0.2", port: 80,
			labels: map[string]string{"statute.enable": "true", "statute.path": "/api/*"},
		},
		{
			name: "hosted", ip: "10.0.0.3", port: 80,
			labels: map[string]string{"statute.enable": "true", "statute.host": "h.example.com"},
		},
	})
	mustSync(t, p)
	tab := srv.dynamic.Load()
	if len(tab.routes) != 3 {
		t.Fatalf("routes = %d", len(tab.routes))
	}
	// Host-scoped first, then longest prefix, then catch-all.
	if tab.routes[0].route.Host != "h.example.com" {
		t.Errorf("route[0] = %+v", tab.routes[0].route)
	}
	if tab.routes[1].route.Pattern != "/api/*" {
		t.Errorf("route[1] = %+v", tab.routes[1].route)
	}
	if tab.routes[2].route.Pattern != "/*" {
		t.Errorf("route[2] = %+v", tab.routes[2].route)
	}

	// The /api path must hit the api pool even though the catch-all also matches.
	h := findHandler(tab.routes, "x.example.com", httptest.NewRequest("GET", "http://x/api/v1", nil))
	if h == nil {
		t.Fatal("no handler")
	}
}

func TestDockerLabelMiddleware(t *testing.T) {
	p, srv, _ := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{{
		name: "mw", ip: "10.0.0.1", port: 80,
		labels: map[string]string{
			"statute.enable":    "true",
			"statute.timeout":   "5s",
			"statute.ratelimit": "10/s",
			"statute.compress":  "gzip",
		},
	}})
	mustSync(t, p)
	mws := srv.dynamic.Load().routes[0].route.Middleware
	if len(mws) != 3 {
		t.Fatalf("middleware = %+v", mws)
	}
	if mws[0].Type != resolved.MWTimeout || mws[1].Type != resolved.MWRateLimit || mws[2].Type != resolved.MWCompress {
		t.Errorf("middleware order/types = %+v", mws)
	}
}

func TestDockerMiddlewareRegistry(t *testing.T) {
	cfg, err := resolveDocker(Docker().TraefikLabels().
		Middleware("auth@file", RateLimit("10/s")).
		DefaultMiddleware(Timeout("5s")))
	if err != nil {
		t.Fatal(err)
	}
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{{
		name: "legacy", ip: "10.0.0.9", port: 3000,
		labels: map[string]string{
			"traefik.enable":                       "true",
			"traefik.http.routers.app.rule":        "Host(`legacy.example.com`)",
			"traefik.http.routers.app.middlewares": "auth@file",
		},
	}})
	mustSync(t, p)
	mws := srv.dynamic.Load().routes[0].route.Middleware
	if len(mws) != 2 {
		t.Fatalf("middleware = %+v", mws)
	}
	// The provider default runs outermost, then the label-referenced chain.
	if mws[0].Type != resolved.MWTimeout || mws[1].Type != resolved.MWRateLimit {
		t.Errorf("middleware order/types = %+v", mws)
	}
}

func TestDockerDefaultMiddlewareOnNativeRoutes(t *testing.T) {
	cfg, err := resolveDocker(Docker().DefaultMiddleware(Timeout("5s")))
	if err != nil {
		t.Fatal(err)
	}
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{{
		name: "web-1", ip: "10.0.0.1", port: 80,
		labels: map[string]string{
			"statute.enable":    "true",
			"statute.ratelimit": "10/s",
		},
	}})
	mustSync(t, p)
	mws := srv.dynamic.Load().routes[0].route.Middleware
	if len(mws) != 2 {
		t.Fatalf("middleware = %+v", mws)
	}
	// Default first, label hints after.
	if mws[0].Type != resolved.MWTimeout || mws[1].Type != resolved.MWRateLimit {
		t.Errorf("middleware order/types = %+v", mws)
	}
}

func TestDockerRouterScopedMiddleware(t *testing.T) {
	// Two routers share one service: each derived route carries only its
	// own router's chain while both dispatch to the same pool handler.
	cfg, err := resolveDocker(Docker().TraefikLabels().
		Middleware("auth@file", RateLimit("10/s")).
		Middleware("slow@file", Timeout("5s")))
	if err != nil {
		t.Fatal(err)
	}
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{{
		name: "app-1", ip: "10.0.0.1", port: 3000,
		labels: map[string]string{
			"traefik.enable":                          "true",
			"traefik.http.routers.public.rule":        "Host(`public.example.com`)",
			"traefik.http.routers.public.service":     "app",
			"traefik.http.routers.public.middlewares": "slow@file",
			"traefik.http.routers.admin.rule":         "Host(`admin.example.com`)",
			"traefik.http.routers.admin.service":      "app",
			"traefik.http.routers.admin.middlewares":  "auth@file",
		},
	}})
	mustSync(t, p)
	tab := srv.dynamic.Load()
	if len(tab.routes) != 2 || len(tab.pools) != 1 {
		t.Fatalf("routes/pools = %+v / %+v", tab.routes, tab.pools)
	}
	byHost := map[string]*resolved.Route{}
	for _, r := range tab.routes {
		byHost[r.route.Host] = r.route
	}
	adm, pub := byHost["admin.example.com"], byHost["public.example.com"]
	if len(adm.Middleware) != 1 || adm.Middleware[0].Type != resolved.MWRateLimit {
		t.Errorf("admin middleware = %+v", adm.Middleware)
	}
	if len(pub.Middleware) != 1 || pub.Middleware[0].Type != resolved.MWTimeout {
		t.Errorf("public middleware = %+v", pub.Middleware)
	}
	if adm.Upstream != pub.Upstream {
		t.Errorf("routers no longer share the pool: %p vs %p", adm.Upstream, pub.Upstream)
	}
}

func TestDockerCrossContainerRouteDedup(t *testing.T) {
	// Replicas registering the same router rule and middleware references
	// pool together into one route with one chain.
	cfg, err := resolveDocker(Docker().TraefikLabels().
		Middleware("auth@file", RateLimit("10/s")))
	if err != nil {
		t.Fatal(err)
	}
	labels := func(router string) map[string]string {
		return map[string]string{
			"traefik.enable": "true",
			"traefik.http.routers." + router + ".rule":        "Host(`app.example.com`)",
			"traefik.http.routers." + router + ".service":     "app",
			"traefik.http.routers." + router + ".middlewares": "auth@file",
		}
	}
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{
		{name: "app-1", ip: "10.0.0.1", port: 3000, labels: labels("r1")},
		{name: "app-2", ip: "10.0.0.2", port: 3000, labels: labels("r2")},
	})
	mustSync(t, p)
	tab := srv.dynamic.Load()
	if len(tab.routes) != 1 {
		t.Fatalf("routes = %+v", tab.routes)
	}
	r := tab.routes[0].route
	if len(r.Upstream.Backends) != 2 {
		t.Fatalf("containers did not pool: %+v", r.Upstream.Backends)
	}
	if len(r.Middleware) != 1 || r.Middleware[0].Type != resolved.MWRateLimit {
		t.Errorf("middleware = %+v", r.Middleware)
	}
}

func TestDockerCrossContainerDistinctRoutesKept(t *testing.T) {
	// Containers whose routers share a rule but differ in middleware
	// references keep separate routes over the shared pool — references
	// are router-scoped, never collapsed across containers.
	cfg, err := resolveDocker(Docker().TraefikLabels().
		Middleware("auth@file", RateLimit("10/s")))
	if err != nil {
		t.Fatal(err)
	}
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{
		{
			name: "app-1", ip: "10.0.0.1", port: 3000,
			labels: map[string]string{
				"traefik.enable":                      "true",
				"traefik.http.routers.r1.rule":        "Host(`app.example.com`)",
				"traefik.http.routers.r1.service":     "app",
				"traefik.http.routers.r1.middlewares": "auth@file",
			},
		},
		{
			name: "app-2", ip: "10.0.0.2", port: 3000,
			labels: map[string]string{
				"traefik.enable":                  "true",
				"traefik.http.routers.r2.rule":    "Host(`app.example.com`)",
				"traefik.http.routers.r2.service": "app",
			},
		},
	})
	mustSync(t, p)
	tab := srv.dynamic.Load()
	if len(tab.routes) != 2 || len(tab.pools) != 1 {
		t.Fatalf("routes/pools = %+v / %+v", tab.routes, tab.pools)
	}
	if len(tab.routes[0].route.Upstream.Backends) != 2 {
		t.Fatalf("containers did not pool: %+v", tab.routes[0].route.Upstream.Backends)
	}
}

func TestParseStrategy(t *testing.T) {
	if st, warn := parseStrategy("svc", ""); st != RoundRobin || warn != "" {
		t.Errorf("empty: %v %q", st, warn)
	}
	if st, warn := parseStrategy("svc", "least_connections"); st != LeastConnections || warn != "" {
		t.Errorf("least_connections: %v %q", st, warn)
	}
	if st, warn := parseStrategy("svc", "bogus"); st != RoundRobin || !strings.Contains(warn, `unknown strategy "bogus"`) {
		t.Errorf("bogus: %v %q", st, warn)
	}
}

func TestDockerUnknownMiddlewareFailsClosed(t *testing.T) {
	// A router referencing an unregistered middleware must not be served
	// without it — the admin route is omitted, warned, while the sibling
	// router on the same service and other containers keep routing.
	cfg, err := resolveDocker(Docker().TraefikLabels().
		Middleware("slow@file", Timeout("5s")))
	if err != nil {
		t.Fatal(err)
	}
	p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{{
		name: "app-1", ip: "10.0.0.9", port: 3000,
		labels: map[string]string{
			"traefik.enable":                          "true",
			"traefik.http.routers.public.rule":        "Host(`public.example.com`)",
			"traefik.http.routers.public.service":     "app",
			"traefik.http.routers.public.middlewares": "slow@file",
			"traefik.http.routers.admin.rule":         "Host(`admin.example.com`)",
			"traefik.http.routers.admin.service":      "app",
			"traefik.http.routers.admin.middlewares":  "auth@file",
		},
	}})
	mustSync(t, p)
	tab := srv.dynamic.Load()
	if len(tab.routes) != 1 || tab.routes[0].route.Host != "public.example.com" {
		t.Fatalf("routes = %+v, want only the public route", tab.routes)
	}
	if h := findHandler(tab.routes, "admin.example.com", httptest.NewRequest("GET", "http://x/", nil)); h != nil {
		t.Fatal("unauthenticated admin route was served")
	}
	found := false
	for w := range p.warned {
		if strings.Contains(w, `unknown middleware "auth@file"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("no unknown-middleware warning: %v", p.warned)
	}
}

func TestDockerAllRoutesFailClosedSkipsPool(t *testing.T) {
	// When every route of a service fails closed, no pool handler is
	// created — nothing health-checks a pool no route can reach.
	p, srv, _ := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{{
		name: "app-1", ip: "10.0.0.9", port: 3000,
		labels: map[string]string{
			"traefik.enable":                       "true",
			"traefik.http.routers.app.rule":        "Host(`app.example.com`)",
			"traefik.http.routers.app.middlewares": "ghost@file",
		},
	}})
	mustSync(t, p)
	tab := srv.dynamic.Load()
	if len(tab.routes) != 0 || len(tab.pools) != 0 {
		t.Fatalf("routes/pools = %+v / %+v", tab.routes, tab.pools)
	}
}

func TestDockerInvalidLabelSkipsServiceNotGeneration(t *testing.T) {
	p, srv, _ := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{
		{
			name: "broken", ip: "10.0.0.1", port: 80,
			labels: map[string]string{"statute.enable": "true", "statute.timeout": "not-a-duration"},
		},
		{
			name: "fine", ip: "10.0.0.2", port: 80,
			labels: map[string]string{"statute.enable": "true", "statute.host": "ok.example.com"},
		},
	})
	mustSync(t, p)
	tab := srv.dynamic.Load()
	// The broken container still routes — only its middleware is dropped.
	if len(tab.pools) != 2 {
		t.Fatalf("pools = %+v", tab.pools)
	}
	if len(tab.routes[0].route.Middleware)+len(tab.routes[1].route.Middleware) != 0 {
		t.Errorf("invalid middleware survived")
	}
}

func TestResolveDockerDefaults(t *testing.T) {
	cfg, err := resolveDocker(Docker())
	if err != nil {
		t.Fatal(err)
	}
	want := &resolved.Docker{Endpoint: "unix:///var/run/docker.sock"}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("resolved = %+v, want %+v", cfg, want)
	}
}

func TestResolveDockerOptions(t *testing.T) {
	cfg, err := resolveDocker(Docker().Endpoint("tcp://1.2.3.4:2375").Network("proxy").TraefikLabels().ExposedByDefault().Refresh("45s"))
	if err != nil {
		t.Fatal(err)
	}
	want := &resolved.Docker{
		Endpoint:         "tcp://1.2.3.4:2375",
		Network:          "proxy",
		ExposedByDefault: true,
		TraefikLabels:    true,
		Refresh:          45 * time.Second,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("resolved = %+v, want %+v", cfg, want)
	}
}

func TestResolveDockerErrors(t *testing.T) {
	if _, err := resolveDocker(Docker().Endpoint("ssh://nope")); err == nil {
		t.Error("bad endpoint scheme accepted")
	}
	if _, err := resolveDocker(Docker().Refresh("bogus")); err == nil {
		t.Error("bad refresh accepted")
	}
	if cfg, err := resolveDocker(nil); err != nil || cfg != nil {
		t.Errorf("nil config: %v %v", cfg, err)
	}
}

func TestResolveDockerMiddleware(t *testing.T) {
	cfg, err := resolveDocker(Docker().
		Middleware("auth@file", RateLimit("10/s")).
		Middleware("slow@file", Timeout("30s")).
		DefaultMiddleware(Timeout("5s")))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Middleware) != 2 {
		t.Fatalf("registry = %+v", cfg.Middleware)
	}
	if got := cfg.Middleware["auth@file"]; len(got) != 1 || got[0].Type != resolved.MWRateLimit {
		t.Errorf("auth@file = %+v", got)
	}
	if got := cfg.DefaultMiddleware; len(got) != 1 || got[0].Type != resolved.MWTimeout || got[0].Timeout != 5*time.Second {
		t.Errorf("defaults = %+v", got)
	}
}

func TestResolveDockerMiddlewareReregisterReplaces(t *testing.T) {
	cfg, err := resolveDocker(Docker().
		Middleware("auth@file", RateLimit("10/s")).
		Middleware("auth@file", Timeout("30s")))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Middleware["auth@file"]; len(got) != 1 || got[0].Type != resolved.MWTimeout {
		t.Errorf("re-registered auth@file = %+v", got)
	}
}

func TestResolveDockerMiddlewareErrors(t *testing.T) {
	_, err := resolveDocker(Docker().Middleware("bad", Timeout("nope")))
	if err == nil || !strings.Contains(err.Error(), `middleware "bad"`) {
		t.Errorf("registry error = %v", err)
	}
	_, err = resolveDocker(Docker().DefaultMiddleware(Timeout("nope")))
	if err == nil || !strings.Contains(err.Error(), "default middleware") {
		t.Errorf("default-chain error = %v", err)
	}
}

func TestResolveConfigCarriesDocker(t *testing.T) {
	rc, err := Resolve(Config{
		Listeners: Listeners{HTTP(":80")},
		Docker:    Docker().TraefikLabels(),
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rc.Docker == nil || !rc.Docker.TraefikLabels {
		t.Fatalf("resolved docker = %+v", rc.Docker)
	}
}
