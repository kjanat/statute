package statute

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"statute.kjanat.dev/internal/docker"
)

func TestDockerRejectedAuthRouteCannotFallThrough(t *testing.T) {
	for _, tc := range []struct {
		name              string
		duplicate, public bool
		want              int
	}{
		{"overlapping public route", true, true, http.StatusNotFound},
		{"no overlapping route", true, false, http.StatusNotFound},
		{"valid protected route", false, true, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte("protected content"))
			}))
			t.Cleanup(backend.Close)
			host, port := backendHostPort(t, backend)
			admin := []Middleware{BasicAuth("admin", map[string]string{"admin": dummyHash})}
			if tc.duplicate {
				admin = append(admin, RequestID())
			}
			cfg, err := resolveDocker(Docker().TraefikLabels().DefaultMiddleware(RequestID()).Middleware("admin", admin...))
			if err != nil {
				t.Fatal(err)
			}
			labels := map[string]string{
				"traefik.enable":                         "true",
				"traefik.http.routers.admin.rule":        "Host(`app.example.com`) && PathPrefix(`/admin`)",
				"traefik.http.routers.admin.service":     "app",
				"traefik.http.routers.admin.middlewares": "admin",
			}
			if tc.public {
				labels["traefik.http.routers.public.rule"] = "Host(`app.example.com`) && PathPrefix(`/`)"
				labels["traefik.http.routers.public.service"] = "app"
			}
			p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{{name: "app", ip: host, port: port, labels: labels}})
			mustSync(t, p)
			rec := runRequest(t, srv.buildRouter(), httptest.NewRequest("GET", "http://app.example.com/admin/secret", nil))
			if rec.Code != tc.want || calls.Load() != 0 {
				t.Fatalf("status=%d backend calls=%d body=%q; want status=%d without backend access", rec.Code, calls.Load(), rec.Body.String(), tc.want)
			}
		})
	}
}

func TestDockerRejectedRoutePrecedenceAndRepair(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)
	cfg, err := resolveDocker(Docker().TraefikLabels().DefaultMiddleware(RequestID()).
		Middleware("broken", RequestID(), BasicAuth("admin", map[string]string{"admin": dummyHash})).
		Middleware("fixed", BasicAuth("admin", map[string]string{"admin": dummyHash})))
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{
		"traefik.enable":                         "true",
		"traefik.http.routers.admin.rule":        "Host(`app.example.com`) && PathPrefix(`/admin`)",
		"traefik.http.routers.admin.service":     "app",
		"traefik.http.routers.admin.middlewares": "broken",
		"traefik.http.routers.public.rule":       "Host(`app.example.com`) && PathPrefix(`/`)",
		"traefik.http.routers.public.service":    "app",
		"traefik.http.routers.health.rule":       "Host(`app.example.com`) && Path(`/admin/health`)",
		"traefik.http.routers.health.service":    "app",
		"traefik.http.routers.malformed.rule":    "ClientIP(`10.0.0.0/8`)",
	}
	c := fakeDaemonContainer{name: "app", ip: host, port: port, labels: labels}
	p, srv, replace := newFakeProvider(t, cfg, []fakeDaemonContainer{c})
	fallbackCalls := fallbackServer(t, srv, Routes{Match("/admin/static").Handle(noContentHandler)})
	mustSync(t, p)
	router := srv.buildRouter()
	for _, tc := range []struct {
		path    string
		status  int
		backend bool
	}{
		{"/admin", 404, false}, {"/admin/users", 404, false}, {"/admin-secret", 404, false},
		{"/%61dmin/users", 404, false}, {"/admin/health", 204, true},
		{"/public", 204, true}, {"/admin/static", 204, false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			before := calls.Load()
			rec := runRequest(t, router, httptest.NewRequest("GET", "http://app.example.com."+tc.path, nil))
			if rec.Code != tc.status || (calls.Load() > before) != tc.backend || fallbackCalls.Load() != 0 {
				t.Fatalf("status=%d backend delta=%d fallback=%d", rec.Code, calls.Load()-before, fallbackCalls.Load())
			}
		})
	}
	// Repair replaces the claim and restores authentication on the same router.
	c.labels = maps.Clone(labels)
	c.labels["traefik.http.routers.admin.middlewares"] = "fixed"
	replace([]fakeDaemonContainer{c})
	mustSync(t, p)
	before := calls.Load()
	rec := runRequest(t, router, httptest.NewRequest("GET", "http://app.example.com/admin/users", nil))
	if rec.Code != http.StatusUnauthorized || calls.Load() != before {
		t.Fatalf("repair: status=%d backend delta=%d", rec.Code, calls.Load()-before)
	}
	replace(nil)
	mustSync(t, p)
	rec = runRequest(t, router, httptest.NewRequest("GET", "http://app.example.com/admin/users", nil))
	if fallbackCalls.Load() != 1 {
		t.Fatalf("removed generation: status=%d fallback=%d", rec.Code, fallbackCalls.Load())
	}
}

func TestDynamicRejectionTiesAndQuarantine(t *testing.T) {
	m := docker.CompileNative("app.example.com", "/admin/*")
	for _, tc := range []struct {
		name, service string
		quarantine    bool
		want          int
	}{
		{"same service tie", "app", false, 404},
		{"different service tie", "other", false, 404},
		{"healthy quarantine tie still respects rejection", "app", true, 404},
		{"unsettled other service remains quarantined", "other", true, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := &dynamicTable{
				routes:     []compiledRoute{{matcher: m, service: tc.service, handler: noContentHandler}},
				rejections: compileRefusals([]docker.Matcher{m}),
			}
			if tc.quarantine {
				table.quarantines = compileQuarantineRoutes([]docker.RouteClaim{{Service: "app", Matcher: m}})
			}
			req := httptest.NewRequest("GET", "http://app.example.com/admin/users", nil)
			rec := runRequest(t, findDynamicHandler(table, req.Host, req), req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestDockerRejectedContributionRemainsContainerScoped(t *testing.T) {
	m := docker.CompileNative("app.example.com", "/admin/*")
	contributions := []dockerContribution{
		{container: docker.Container{ID: "old"}, rejections: []docker.Matcher{m}},
		{container: docker.Container{ID: "new"}, services: []docker.Service{{Name: "app", Routes: []docker.Matcher{m}}}},
	}
	services, _, rejected := mergeContributions(contributions, func(c docker.Container) bool { return c.ID == "old" })
	if len(services) != 1 || len(rejected) != 0 {
		t.Fatalf("quarantined predecessor leaked refusal: services=%d rejected=%d", len(services), len(rejected))
	}
}

func TestDockerRejectedStagesCannotFallThrough(t *testing.T) {
	backend := httptest.NewServer(noContentHandler)
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{"native pool resolution", map[string]string{
			"statute.enable": "true", "statute.host": "app.example.com", "statute.path": "/admin/*",
			"statute.healthcheck.path": "/health", "statute.healthcheck.interval": "banana",
		}},
		{"native extraction", map[string]string{
			"statute.enable": "true", "statute.host": "app.example.com", "statute.path": "/admin/*", "statute.port": "banana",
		}},
		{"unknown middleware", map[string]string{
			"traefik.enable": "true", "traefik.http.routers.admin.rule": "Host(`app.example.com`) && PathPrefix(`/admin`)",
			"traefik.http.routers.admin.middlewares": "missing",
		}},
		{"Traefik extraction", map[string]string{
			"traefik.enable": "true", "traefik.http.routers.admin.rule": "Host(`app.example.com`) && PathPrefix(`/admin`)",
			"traefik.http.services.admin.loadbalancer.server.port": "banana",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := resolveDocker(Docker().TraefikLabels())
			if err != nil {
				t.Fatal(err)
			}
			p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{
				{name: "admin", ip: host, port: port, labels: tc.labels},
				{name: "public", ip: host, port: port, labels: map[string]string{
					"traefik.enable": "true", "traefik.http.routers.public.rule": "Host(`app.example.com`) && PathPrefix(`/`)",
				}},
			})
			mustSync(t, p)
			for _, check := range []struct {
				path   string
				status int
			}{{"/admin/users", 404}, {"/public", 204}} {
				rec := runRequest(t, srv.buildRouter(), httptest.NewRequest("GET", "http://app.example.com"+check.path, nil))
				if rec.Code != check.status {
					t.Fatalf("%s: status=%d, want %d", check.path, rec.Code, check.status)
				}
			}
		})
	}
}
