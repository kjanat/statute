package docker

import (
	"reflect"
	"strings"
	"testing"
)

func webContainer(labels map[string]string) Container {
	return Container{
		ID:       "abc123",
		Name:     "web-1",
		Labels:   labels,
		Networks: map[string]string{"bridge": "172.17.0.2"},
		Ports:    []int{8080},
	}
}

func TestExtractNativeMinimal(t *testing.T) {
	c := webContainer(map[string]string{"statute.enable": "true"})
	svcs, warns := Extract(c, ExtractOptions{})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if len(svcs) != 1 {
		t.Fatalf("got %d services, want 1", len(svcs))
	}
	svc := svcs[0]
	if svc.Name != "web-1" {
		t.Errorf("Name = %q, want container name", svc.Name)
	}
	if svc.Backend.Address != "172.17.0.2:8080" {
		t.Errorf("Address = %q", svc.Backend.Address)
	}
	if want := []Matcher{{Path: "/*"}}; !reflect.DeepEqual(svc.Routes, want) {
		t.Errorf("Routes = %+v, want %+v", svc.Routes, want)
	}
}

func TestExtractNativeFull(t *testing.T) {
	c := webContainer(map[string]string{
		"statute.enable":               "true",
		"statute.service":              "api",
		"statute.host":                 "api.example.com, alt.example.com",
		"statute.path":                 "/v1/*",
		"statute.port":                 "9000",
		"statute.scheme":               "https",
		"statute.weight":               "3",
		"statute.strategy":             "least_connections",
		"statute.healthcheck.path":     "/healthz",
		"statute.healthcheck.interval": "5s",
		"statute.timeout":              "30s",
		"statute.ratelimit":            "100/min",
		"statute.compress":             "gzip,br",
	})
	svcs, warns := Extract(c, ExtractOptions{})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	want := []Service{{
		Name: "api",
		Routes: []Matcher{
			{Host: "api.example.com", Path: "/v1/*"},
			{Host: "alt.example.com", Path: "/v1/*"},
		},
		Backend:             Backend{Address: "https://172.17.0.2:9000", Weight: 3},
		Strategy:            "least_connections",
		HealthCheckPath:     "/healthz",
		HealthCheckInterval: "5s",
		Timeout:             "30s",
		RateLimit:           "100/min",
		Compress:            "gzip,br",
	}}
	if !reflect.DeepEqual(svcs, want) {
		t.Errorf("Extract = %+v, want %+v", svcs, want)
	}
}

func TestExtractNativeNamedRoutes(t *testing.T) {
	c := webContainer(map[string]string{
		"statute.enable":            "true",
		"statute.host":              "app.example.com",
		"statute.routes.admin.host": "admin.example.com",
		"statute.routes.admin.path": "/admin/*",
	})
	svcs, _ := Extract(c, ExtractOptions{})
	if len(svcs) != 1 {
		t.Fatalf("got %d services", len(svcs))
	}
	want := []Matcher{
		{Host: "app.example.com", Path: "/*"},
		{Host: "admin.example.com", Path: "/admin/*"},
	}
	if !reflect.DeepEqual(svcs[0].Routes, want) {
		t.Errorf("Routes = %+v, want %+v", svcs[0].Routes, want)
	}
}

func TestExtractOptInPolicy(t *testing.T) {
	// No labels, opt-in default: nothing registers.
	c := webContainer(nil)
	if svcs, _ := Extract(c, ExtractOptions{}); len(svcs) != 0 {
		t.Errorf("unlabeled container registered: %+v", svcs)
	}
	// No labels but ExposedByDefault: registers.
	if svcs, _ := Extract(c, ExtractOptions{ExposedByDefault: true}); len(svcs) != 1 {
		t.Errorf("ExposedByDefault did not register container")
	}
	// Explicit disable wins over ExposedByDefault.
	c = webContainer(map[string]string{"statute.enable": "false"})
	if svcs, _ := Extract(c, ExtractOptions{ExposedByDefault: true}); len(svcs) != 0 {
		t.Errorf("statute.enable=false ignored")
	}
	// Carrying statute labels without an explicit enable registers.
	c = webContainer(map[string]string{"statute.host": "a.example.com"})
	if svcs, _ := Extract(c, ExtractOptions{}); len(svcs) != 1 {
		t.Errorf("labeled container without enable did not register")
	}
}

func TestExtractComposeServiceName(t *testing.T) {
	c := webContainer(map[string]string{
		"statute.enable":             "true",
		"com.docker.compose.service": "backend",
	})
	svcs, _ := Extract(c, ExtractOptions{})
	if len(svcs) != 1 || svcs[0].Name != "backend" {
		t.Fatalf("compose service name not used: %+v", svcs)
	}
}

func TestExtractPortSelection(t *testing.T) {
	// Multiple exposed ports without a label: warn and skip.
	c := webContainer(map[string]string{"statute.enable": "true"})
	c.Ports = []int{8080, 9090}
	svcs, warns := Extract(c, ExtractOptions{})
	if len(svcs) != 0 {
		t.Errorf("ambiguous ports still registered: %+v", svcs)
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "multiple exposed ports") {
		t.Errorf("warns = %v", warns)
	}
	// No exposed port and no label: warn and skip.
	c.Ports = nil
	svcs, warns = Extract(c, ExtractOptions{})
	if len(svcs) != 0 || len(warns) == 0 {
		t.Errorf("portless container: svcs=%v warns=%v", svcs, warns)
	}
}

func TestExtractNetworkSelection(t *testing.T) {
	c := webContainer(map[string]string{"statute.enable": "true"})
	c.Networks = map[string]string{"bridge": "172.17.0.2", "proxy": "10.1.0.2"}

	// Provider-level preferred network.
	svcs, warns := Extract(c, ExtractOptions{Network: "proxy"})
	if len(warns) != 0 || len(svcs) != 1 || svcs[0].Backend.Address != "10.1.0.2:8080" {
		t.Errorf("preferred network not used: %+v warns=%v", svcs, warns)
	}

	// Label-level pin overrides.
	c.Labels["statute.network"] = "bridge"
	svcs, _ = Extract(c, ExtractOptions{Network: "proxy"})
	if len(svcs) != 1 || svcs[0].Backend.Address != "172.17.0.2:8080" {
		t.Errorf("label network not used: %+v", svcs)
	}

	// Multiple networks with no pin: deterministic pick plus warning.
	delete(c.Labels, "statute.network")
	svcs, warns = Extract(c, ExtractOptions{})
	if len(svcs) != 1 || svcs[0].Backend.Address != "172.17.0.2:8080" {
		t.Errorf("ambiguous network pick: %+v", svcs)
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "multiple networks") {
		t.Errorf("warns = %v", warns)
	}
}

func TestExtractTraefik(t *testing.T) {
	c := webContainer(map[string]string{
		"traefik.enable":                                     "true",
		"traefik.http.routers.web.rule":                      "Host(`app.example.com`) && PathPrefix(`/api`)",
		"traefik.http.services.web.loadbalancer.server.port": "9000",
	})
	svcs, warns := Extract(c, ExtractOptions{TraefikLabels: true})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if len(svcs) != 1 {
		t.Fatalf("got %d services, want 1", len(svcs))
	}
	svc := svcs[0]
	if svc.Name != "web@traefik" {
		t.Errorf("Name = %q", svc.Name)
	}
	if svc.Backend.Address != "172.17.0.2:9000" {
		t.Errorf("Address = %q", svc.Backend.Address)
	}
	want := []Matcher{{Host: "app.example.com", Path: "/api/*"}}
	if !reflect.DeepEqual(svc.Routes, want) {
		t.Errorf("Routes = %+v, want %+v", svc.Routes, want)
	}
}

func TestExtractTraefikDisabledWithoutOption(t *testing.T) {
	c := webContainer(map[string]string{
		"traefik.enable":                "true",
		"traefik.http.routers.web.rule": "Host(`app.example.com`)",
	})
	if svcs, _ := Extract(c, ExtractOptions{}); len(svcs) != 0 {
		t.Errorf("traefik labels honored without TraefikLabels: %+v", svcs)
	}
}

func TestExtractTraefikDefaults(t *testing.T) {
	// No service labels at all: implicit service named after the router,
	// port from the single exposed port, healthcheck carried.
	c := webContainer(map[string]string{
		"traefik.http.routers.app.rule":                               "Host(`app.example.com`)",
		"traefik.http.services.app.loadbalancer.healthcheck.path":     "/ping",
		"traefik.http.services.app.loadbalancer.healthcheck.interval": "10s",
	})
	svcs, warns := Extract(c, ExtractOptions{TraefikLabels: true})
	if len(warns) != 0 {
		t.Fatalf("warns = %v", warns)
	}
	if len(svcs) != 1 {
		t.Fatalf("got %d services", len(svcs))
	}
	if svcs[0].Backend.Address != "172.17.0.2:8080" {
		t.Errorf("Address = %q", svcs[0].Backend.Address)
	}
	if svcs[0].HealthCheckPath != "/ping" || svcs[0].HealthCheckInterval != "10s" {
		t.Errorf("healthcheck not carried: %+v", svcs[0])
	}
}

func TestExtractTraefikMultipleRouters(t *testing.T) {
	c := webContainer(map[string]string{
		"traefik.enable":                                     "true",
		"traefik.http.routers.a.rule":                        "Host(`a.example.com`)",
		"traefik.http.routers.b.rule":                        "Host(`b.example.com`)",
		"traefik.http.services.web.loadbalancer.server.port": "8080",
	})
	svcs, warns := Extract(c, ExtractOptions{TraefikLabels: true})
	if len(warns) != 0 {
		t.Fatalf("warns = %v", warns)
	}
	// Both routers bind to the sole defined service.
	if len(svcs) != 1 {
		t.Fatalf("got %d services: %+v", len(svcs), svcs)
	}
	if len(svcs[0].Routes) != 2 {
		t.Errorf("Routes = %+v", svcs[0].Routes)
	}
}

func TestExtractTraefikUnsupportedRuleSkipsRouter(t *testing.T) {
	c := webContainer(map[string]string{
		"traefik.enable":                 "true",
		"traefik.http.routers.bad.rule":  "HostRegexp(`.*`)",
		"traefik.http.routers.good.rule": "Host(`ok.example.com`)",
	})
	svcs, warns := Extract(c, ExtractOptions{TraefikLabels: true})
	if len(svcs) != 1 || svcs[0].Routes[0].Host != "ok.example.com" {
		t.Errorf("good router lost: %+v", svcs)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "bad") && strings.Contains(w, "not supported") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning for unsupported rule: %v", warns)
	}
}

func TestExtractTraefikMiddlewareWarns(t *testing.T) {
	c := webContainer(map[string]string{
		"traefik.enable":                       "true",
		"traefik.http.routers.web.rule":        "Host(`a.example.com`)",
		"traefik.http.routers.web.middlewares": "auth@docker",
	})
	svcs, warns := Extract(c, ExtractOptions{TraefikLabels: true})
	if len(svcs) != 1 {
		t.Fatalf("router with middlewares label was dropped: %+v", svcs)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "middlewares are not supported") {
			found = true
		}
	}
	if !found {
		t.Errorf("no middleware warning: %v", warns)
	}
}

func TestExtractBothSchemas(t *testing.T) {
	// statute labels take the container; traefik labels on the same
	// container still register their own routers.
	c := webContainer(map[string]string{
		"statute.enable":                "true",
		"statute.host":                  "native.example.com",
		"traefik.enable":                "true",
		"traefik.http.routers.web.rule": "Host(`compat.example.com`)",
	})
	svcs, _ := Extract(c, ExtractOptions{TraefikLabels: true})
	if len(svcs) != 2 {
		t.Fatalf("got %d services, want native + traefik: %+v", len(svcs), svcs)
	}
}

func TestExtractTraefikOnlyContainerSkipsNative(t *testing.T) {
	// A traefik-labeled container with no statute labels must not also
	// register under the native path when ExposedByDefault is on.
	c := webContainer(map[string]string{
		"traefik.enable":                "true",
		"traefik.http.routers.web.rule": "Host(`a.example.com`)",
	})
	svcs, _ := Extract(c, ExtractOptions{TraefikLabels: true, ExposedByDefault: true})
	if len(svcs) != 1 {
		t.Fatalf("got %d services, want 1: %+v", len(svcs), svcs)
	}
	if svcs[0].Name != "web@traefik" {
		t.Errorf("Name = %q", svcs[0].Name)
	}
}
