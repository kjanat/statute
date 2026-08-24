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
	// Multiple exposed ports without a label: pick the lowest (Traefik's
	// rule) and warn about the ambiguity.
	c := webContainer(map[string]string{"statute.enable": "true"})
	c.Ports = []int{8080, 9090}
	svcs, warns := Extract(c, ExtractOptions{})
	if len(svcs) != 1 || svcs[0].Backend.Address != "172.17.0.2:8080" {
		t.Errorf("lowest port not picked: %+v", svcs)
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

func TestExtractHostFragments(t *testing.T) {
	// A trailing comma must not create a catch-all route.
	c := webContainer(map[string]string{
		"statute.enable": "true",
		"statute.host":   "API.example.com,",
	})
	svcs, _ := Extract(c, ExtractOptions{})
	want := []Matcher{{Host: "api.example.com", Path: "/*"}}
	if len(svcs) != 1 || !reflect.DeepEqual(svcs[0].Routes, want) {
		t.Errorf("Routes = %+v, want %+v", svcs[0].Routes, want)
	}

	// A whitespace-only host list falls back to the host-less default.
	c.Labels["statute.host"] = " , "
	svcs, _ = Extract(c, ExtractOptions{})
	want = []Matcher{{Path: "/*"}}
	if len(svcs) != 1 || !reflect.DeepEqual(svcs[0].Routes, want) {
		t.Errorf("Routes = %+v, want %+v", svcs[0].Routes, want)
	}
}

func TestExtractBackupAndWeight(t *testing.T) {
	c := webContainer(map[string]string{
		"statute.enable": "true",
		"statute.backup": "true",
		"statute.weight": "banana",
	})
	svcs, warns := Extract(c, ExtractOptions{})
	if len(svcs) != 1 || !svcs[0].Backend.Backup {
		t.Errorf("backup not set: %+v", svcs)
	}
	if svcs[0].Backend.Weight != 1 {
		t.Errorf("invalid weight fallback = %d", svcs[0].Backend.Weight)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "invalid statute.weight") {
			found = true
		}
	}
	if !found {
		t.Errorf("no weight warning: %v", warns)
	}
}

func TestExtractInvalidBackup(t *testing.T) {
	// Invalid backup value: warn and treat as false.
	c := webContainer(map[string]string{"statute.enable": "true", "statute.backup": "yep"})
	svcs, warns := Extract(c, ExtractOptions{})
	if len(svcs) != 1 || svcs[0].Backend.Backup {
		t.Errorf("invalid backup not false: %+v", svcs)
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "invalid boolean") {
		t.Errorf("warns = %v", warns)
	}
}

func TestExtractEnableParsing(t *testing.T) {
	// ParseBool variants register, as in Traefik.
	for _, v := range []string{"True", "1", "t"} {
		c := webContainer(map[string]string{"statute.enable": v})
		if svcs, _ := Extract(c, ExtractOptions{}); len(svcs) != 1 {
			t.Errorf("statute.enable=%q did not register", v)
		}
	}
	// Unparseable enable: false, with a warning.
	c := webContainer(map[string]string{"statute.enable": "banana"})
	svcs, warns := Extract(c, ExtractOptions{})
	if len(svcs) != 0 {
		t.Errorf("invalid enable registered: %+v", svcs)
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "invalid boolean") {
		t.Errorf("warns = %v", warns)
	}
}

func TestExtractSchemeCaseFold(t *testing.T) {
	c := webContainer(map[string]string{"statute.enable": "true", "statute.scheme": "HTTPS"})
	svcs, warns := Extract(c, ExtractOptions{})
	if len(warns) != 0 || len(svcs) != 1 || svcs[0].Backend.Address != "https://172.17.0.2:8080" {
		t.Errorf("HTTPS scheme not folded: %+v warns=%v", svcs, warns)
	}

	// Unknown schemes (h2c included) skip the service instead of
	// silently registering it with the wrong protocol.
	for _, scheme := range []string{"h2c", "ftp"} {
		c.Labels["statute.scheme"] = scheme
		svcs, warns = Extract(c, ExtractOptions{})
		if len(svcs) != 0 {
			t.Errorf("scheme %q still registered: %+v", scheme, svcs)
		}
		if len(warns) == 0 || !strings.Contains(warns[0], "unsupported backend scheme") {
			t.Errorf("scheme %q warns = %v", scheme, warns)
		}
	}
}

func TestExtractTraefikH2CSkipped(t *testing.T) {
	c := webContainer(map[string]string{
		"traefik.enable":                                       "true",
		"traefik.http.routers.web.rule":                        "Host(`a.example.com`)",
		"traefik.http.services.web.loadbalancer.server.scheme": "h2c",
		"traefik.http.services.web.loadbalancer.server.port":   "8080",
	})
	svcs, warns := Extract(c, ExtractOptions{TraefikLabels: true})
	if len(svcs) != 0 {
		t.Errorf("h2c service registered: %+v", svcs)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "unsupported backend scheme") {
			found = true
		}
	}
	if !found {
		t.Errorf("no h2c warning: %v", warns)
	}
}

func TestExtractNamedRouteMalformed(t *testing.T) {
	c := webContainer(map[string]string{
		"statute.enable":             "true",
		"statute.routes.admin":       "oops",
		"statute.routes.extra.bogus": "x",
	})
	_, warns := Extract(c, ExtractOptions{})
	var malformed, unknown bool
	for _, w := range warns {
		if strings.Contains(w, "malformed label") {
			malformed = true
		}
		if strings.Contains(w, "unknown route field") {
			unknown = true
		}
	}
	if !malformed || !unknown {
		t.Errorf("expected malformed + unknown-field warnings, got %v", warns)
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
	// No server.port label: port from the single exposed port, router
	// bound to the sole defined service, healthcheck carried.
	c := webContainer(map[string]string{
		"traefik.enable":                                              "true",
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

func TestExtractTraefikMiddlewares(t *testing.T) {
	c := webContainer(map[string]string{
		"traefik.enable":                       "true",
		"traefik.http.routers.web.rule":        "Host(`a.example.com`)",
		"traefik.http.routers.web.middlewares": "auth@docker",
	})
	svcs, warns := Extract(c, ExtractOptions{TraefikLabels: true})
	if len(warns) != 0 {
		t.Fatalf("warns = %v", warns)
	}
	if len(svcs) != 1 {
		t.Fatalf("router with middlewares label was dropped: %+v", svcs)
	}
	want := []Matcher{{Host: "a.example.com", Path: "/*", Middlewares: []string{"auth@docker"}}}
	if !reflect.DeepEqual(svcs[0].Routes, want) {
		t.Errorf("Routes = %+v, want %+v", svcs[0].Routes, want)
	}
}

func TestExtractTraefikMiddlewaresMultiple(t *testing.T) {
	// Whitespace is trimmed, empty fragments dropped, label order kept,
	// and every matcher a rule expands to carries the router's references.
	c := webContainer(map[string]string{
		"traefik.enable":                       "true",
		"traefik.http.routers.web.rule":        "Host(`a.example.com`) || Host(`b.example.com`)",
		"traefik.http.routers.web.middlewares": " auth@file , ratelimit@docker,,",
	})
	svcs, warns := Extract(c, ExtractOptions{TraefikLabels: true})
	if len(warns) != 0 {
		t.Fatalf("warns = %v", warns)
	}
	want := []string{"auth@file", "ratelimit@docker"}
	if len(svcs[0].Routes) != 2 {
		t.Fatalf("Routes = %+v", svcs[0].Routes)
	}
	for _, r := range svcs[0].Routes {
		if !reflect.DeepEqual(r.Middlewares, want) {
			t.Errorf("route %s Middlewares = %+v, want %+v", r.Host, r.Middlewares, want)
		}
	}
}

func TestExtractTraefikRouterScopedMiddlewares(t *testing.T) {
	// Routers sharing one service keep their own middleware references on
	// their own routes, as in Traefik — nothing leaks across routers.
	c := webContainer(map[string]string{
		"traefik.enable":                                     "true",
		"traefik.http.routers.a.rule":                        "Host(`a.example.com`)",
		"traefik.http.routers.a.middlewares":                 "auth@file",
		"traefik.http.routers.b.rule":                        "Host(`b.example.com`)",
		"traefik.http.services.web.loadbalancer.server.port": "8080",
	})
	svcs, warns := Extract(c, ExtractOptions{TraefikLabels: true})
	if len(warns) != 0 {
		t.Fatalf("warns = %v", warns)
	}
	if len(svcs) != 1 {
		t.Fatalf("got %d services: %+v", len(svcs), svcs)
	}
	want := []Matcher{
		{Host: "a.example.com", Path: "/*", Middlewares: []string{"auth@file"}},
		{Host: "b.example.com", Path: "/*"},
	}
	if !reflect.DeepEqual(svcs[0].Routes, want) {
		t.Errorf("Routes = %+v, want %+v", svcs[0].Routes, want)
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
	// With no service labels the implicit service is named after the
	// container, as in Traefik.
	if svcs[0].Name != "web-1@traefik" {
		t.Errorf("Name = %q", svcs[0].Name)
	}
}

func TestExtractTraefikRequiresExplicitEnable(t *testing.T) {
	// Traefik semantics: router labels alone do not expose a container
	// when ExposedByDefault is off.
	c := webContainer(map[string]string{
		"traefik.http.routers.web.rule": "Host(`a.example.com`)",
	})
	if svcs, _ := Extract(c, ExtractOptions{TraefikLabels: true}); len(svcs) != 0 {
		t.Errorf("router labels without traefik.enable registered: %+v", svcs)
	}
	// ParseBool variants work.
	c.Labels["traefik.enable"] = "1"
	if svcs, _ := Extract(c, ExtractOptions{TraefikLabels: true}); len(svcs) != 1 {
		t.Errorf("traefik.enable=1 did not register")
	}
}

func TestExtractTraefikImplicitServiceShared(t *testing.T) {
	// Two routers with no service labels share the container-named
	// implicit service instead of creating one backend pool per router.
	c := webContainer(map[string]string{
		"traefik.enable":              "true",
		"traefik.http.routers.a.rule": "Host(`a.example.com`)",
		"traefik.http.routers.b.rule": "Host(`b.example.com`)",
	})
	svcs, _ := Extract(c, ExtractOptions{TraefikLabels: true})
	if len(svcs) != 1 {
		t.Fatalf("got %d services, want 1: %+v", len(svcs), svcs)
	}
	if svcs[0].Name != "web-1@traefik" || len(svcs[0].Routes) != 2 {
		t.Errorf("service = %+v", svcs[0])
	}
}
