package statute

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestCompiledServiceSurvivesRefusedDockerRouter(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		backend := httptest.NewServer(statusHandler(http.StatusAccepted))
		t.Cleanup(backend.Close)
		host, port := backendHostPort(t, backend)
		cfg, err := resolveDocker(Docker().TraefikLabels())
		if err != nil {
			t.Fatal(err)
		}
		labels := map[string]string{"traefik.enable": "true", "traefik.http.services.api.loadbalancer.server.port": fmt.Sprint(port), "traefik.http.routers.private.rule": "Host(`private.example`)", "traefik.http.routers.private.service": "api", "traefik.http.routers.private.middlewares": "missing-auth"}
		p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{{name: "api", ip: host, port: port, labels: labels}})
		routes := []*resolved.Route{{Host: "compiled.example", Pattern: "/*", DiscoveredUpstream: "api@traefik"}}
		if fallback {
			srv.cfg.FallbackRoutes = routes
		} else {
			srv.cfg.Routes = routes
		}
		mustSync(t, p)
		for _, tc := range []struct {
			host   string
			status int
		}{{"compiled.example", http.StatusAccepted}, {"private.example", http.StatusNotFound}} {
			w := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/", nil))
			if w.Code != tc.status {
				t.Fatalf("fallback=%t host=%s status=%d want=%d", fallback, tc.host, w.Code, tc.status)
			}
		}
	}
}

func TestDockerRoutersUseCompiledServiceBindings(t *testing.T) {
	backend := httptest.NewServer(statusHandler(http.StatusAccepted))
	t.Cleanup(backend.Close)
	discovery := Docker().TraefikLabels().BindUpstream("api@file", "compiled").BindHandler("noop@internal", statusHandler(http.StatusNoContent))
	c := mustResolve(t, Config{Listeners: Listeners{HTTP(":8080")}, Docker: discovery, Upstreams: Upstreams{"compiled": {Backends: []Backend{{Address: strings.TrimPrefix(backend.URL, "http://")}}}}})
	labels := map[string]string{"traefik.enable": "true", "traefik.http.routers.api.rule": "Host(`api.example`)", "traefik.http.routers.api.service": "api@file", "traefik.http.routers.noop.rule": "Host(`noop.example`)", "traefik.http.routers.noop.service": "noop@internal"}
	p, srv, _ := newFakeProvider(t, c.Docker, []fakeDaemonContainer{{name: "app", ip: "127.0.0.1", port: 8080, labels: labels}})
	srv.cfg.Upstreams = c.Upstreams
	pool, err := newPoolHandler(c.Upstreams["compiled"])
	if err != nil {
		t.Fatal(err)
	}
	srv.pools = map[string]*poolHandler{"compiled": pool}
	mustSync(t, p)
	for _, tc := range []struct {
		host   string
		status int
	}{{"api.example", 202}, {"noop.example", 204}} {
		w := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/", nil))
		if w.Code != tc.status {
			t.Fatalf("binding %s status=%d body=%s", tc.host, w.Code, w.Body.String())
		}
	}
}

func TestDockerServiceBindingsRejectUnavailableTargets(t *testing.T) {
	for _, discovery := range []*DockerConfig{
		Docker().BindUpstream("api@file", "missing"), Docker().BindUpstream("", "pool"), Docker().BindHandler("api@file", nil),
	} {
		_, err := Resolve(Config{Listeners: Listeners{HTTP(":8080")}, Docker: discovery, Upstreams: Upstreams{"pool": {Backends: []Backend{{Address: "127.0.0.1:8080"}}}}})
		if err == nil {
			t.Fatal("unavailable service binding accepted")
		}
	}
	_, err := Resolve(Config{Listeners: Listeners{HTTP(":8080")}, Routes: Routes{Match("/*").ProxyToDiscovered("missing@traefik")}})
	if err == nil {
		t.Fatal("discovered upstream accepted without Docker")
	}
}
