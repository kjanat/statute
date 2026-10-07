package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/internal/docker"
)

func TestResolveDuplicateRequestID(t *testing.T) {
	t.Parallel()
	for _, fallback := range []bool{false, true} {
		cfg := Config{Listeners: Listeners{HTTP(":8080")}, Routes: Routes{
			Match("/*").Handle(http.NotFoundHandler()).With(RequestID(), Retry(2), RequestID().Header("X-Trace").From("X-Inbound")),
		}}
		if fallback {
			cfg.FallbackRoutes, cfg.Routes = cfg.Routes, nil
		}
		_, err := Resolve(cfg)
		if err == nil || !strings.Contains(err.Error(), "only one RequestID") {
			t.Fatalf("fallback=%v: %v", fallback, err)
		}
	}
}

func TestDockerDuplicateRequestID(t *testing.T) {
	t.Parallel()
	for _, config := range []*DockerConfig{
		Docker().DefaultMiddleware(RequestID(), RequestID()),
		Docker().Middleware("ids", RequestID(), RequestID()),
	} {
		if _, err := resolveDocker(config); err == nil || !strings.Contains(err.Error(), "only one RequestID") {
			t.Fatalf("duplicate chain accepted: %v", err)
		}
	}
}

func TestDockerRequestIDAssembledDuplicates(t *testing.T) {
	t.Parallel()
	for _, defaults := range []bool{false, true} {
		config := Docker().Middleware("first", RequestID()).Middleware("second", RequestID().Header("X-Trace"))
		if defaults {
			config.DefaultMiddleware(RequestID())
		}
		cfg, err := resolveDocker(config)
		if err != nil {
			t.Fatal(err)
		}
		p := &dockerProvider{cfg: cfg}
		for _, names := range [][]string{{"first", "second"}, {"first", "first"}} {
			_, warning := p.routeMiddleware(&docker.Service{Name: "shared"}, docker.Matcher{Middlewares: names}, nil)
			if !strings.Contains(warning, "only one RequestID") {
				t.Fatalf("defaults=%v names=%v warning=%q", defaults, names, warning)
			}
		}
		names := []string{"first"}
		if defaults {
			names = nil
		}
		if _, warning := p.routeMiddleware(&docker.Service{Name: "shared"}, docker.Matcher{Middlewares: names}, nil); warning != "" {
			t.Fatalf("single owner rejected: %s", warning)
		}
	}
}

func TestDockerDuplicateRequestIDRefusesOnlyConflictingRouter(t *testing.T) {
	cfg, err := resolveDocker(Docker().TraefikLabels().DefaultMiddleware(RequestID()).Middleware("id", RequestID()))
	if err != nil {
		t.Fatal(err)
	}
	containers := []fakeDaemonContainer{{
		name: "app", ip: "10.0.0.9", port: 3000,
		labels: map[string]string{
			"traefik.enable":                       "true",
			"traefik.http.routers.bad.rule":        "Host(`bad.example.com`)",
			"traefik.http.routers.bad.service":     "app",
			"traefik.http.routers.bad.middlewares": "id",
			"traefik.http.routers.good.rule":       "Host(`good.example.com`)",
			"traefik.http.routers.good.service":    "app",
		},
	}}
	p, srv, update := newFakeProvider(t, cfg, containers)
	mustSync(t, p)
	tab := srv.dynamic.Load()
	if len(tab.routes) != 1 || tab.routes[0].route.Host != "good.example.com" {
		t.Fatalf("healthy sibling missing: %+v", tab.routes)
	}
	r := httptest.NewRequest("GET", "http://bad.example.com/", nil)
	h := findDynamicHandler(tab, r.Host, r)
	if h == nil || runRequest(t, h, r).Code != http.StatusNotFound {
		t.Fatal("conflicting router did not refuse before fallback")
	}
	delete(containers[0].labels, "traefik.http.routers.bad.middlewares")
	update(containers)
	mustSync(t, p)
	if len(srv.dynamic.Load().routes) != 2 {
		t.Fatal("corrected router did not recover")
	}
}
