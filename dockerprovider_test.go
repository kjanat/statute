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
	if h := findHandler(tab.routes, "other.example.com", "/"); h != nil {
		t.Fatalf("route matched wrong host")
	}
	// Right host: proxies to the real backend.
	h := findHandler(tab.routes, "app.example.com", "/x")
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
	if h := findHandler(tab.routes, "APP.Example.COM", "/x"); h == nil {
		t.Fatal("host match is case-sensitive")
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
	if r.Host != "legacy.example.com" || r.Pattern != "/api/*" {
		t.Fatalf("route = %+v", r)
	}
	if r.Upstream.Name != "app@traefik" || r.Upstream.Backends[0].Address != "10.0.0.9:3000" {
		t.Fatalf("upstream = %+v", r.Upstream)
	}
	if h := findHandler(tab.routes, "legacy.example.com", "/api/users"); h == nil {
		t.Fatal("PathPrefix route did not match subpath")
	}
	if h := findHandler(tab.routes, "legacy.example.com", "/other"); h != nil {
		t.Fatal("route matched outside prefix")
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
	h := findHandler(tab.routes, "x.example.com", "/api/v1")
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
