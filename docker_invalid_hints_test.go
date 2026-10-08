package statute

import (
	"fmt"
	"maps"
	"net/http/httptest"
	"testing"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func invalidHintCases() []map[string]string {
	return []map[string]string{
		{"statute.timeout": "invalid", "statute.ratelimit": "1/h"},
		{"statute.timeout": "30s", "statute.ratelimit": "invalid"},
		{"statute.ratelimit": "5e-324/h"},
		{"statute.ratelimit": "5e-324/min"},
		{"statute.compress": "unknown"},
		{"statute.compress": "gzip,unknown", "statute.ratelimit": "1/h"},
	}
}

func TestDockerInvalidHintsRefuseRegistration(t *testing.T) {
	for _, hints := range invalidHintCases() {
		t.Run(fmt.Sprint(hints), func(t *testing.T) {
			labels := map[string]string{"statute.enable": "true", "statute.host": "app.example.com", "statute.path": "/admin/*"}
			maps.Copy(labels, hints)
			cfg, err := resolveDocker(Docker().DefaultMiddleware(RequestID()))
			if err != nil {
				t.Fatal(err)
			}
			p, srv, _ := newFakeProvider(t, cfg, []fakeDaemonContainer{{name: "broken", ip: "127.0.0.1", port: 1, labels: labels}})
			fallback := fallbackServer(t, srv, nil)
			mustSync(t, p)
			rec := runRequest(t, srv.buildRouter(), httptest.NewRequest("GET", "http://app.example.com/admin/private", nil))
			if rec.Code != 404 || fallback.Load() != 0 || len(srv.dynamic.Load().pools) != 0 {
				t.Fatalf("status=%d fallback=%d pools=%d", rec.Code, fallback.Load(), len(srv.dynamic.Load().pools))
			}
		})
	}
}

func TestDockerInvalidHintsSameServiceAndTraefikSiblings(t *testing.T) {
	backend := httptest.NewServer(noContentHandler)
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)
	for _, badName := range []string{"a-broken", "z-broken"} {
		t.Run(badName, func(t *testing.T) {
			bad := fakeDaemonContainer{name: badName, ip: host, port: port, labels: map[string]string{
				"statute.enable": "true", "statute.service": "shared", "statute.host": "app.example.com",
				"statute.path": "/admin/*", "statute.timeout": "invalid", "statute.ratelimit": "1/h",
				"traefik.enable": "true", "traefik.http.routers.other.rule": "Host(`other.example.com`)",
			}}
			good := fakeDaemonContainer{name: "m-healthy", ip: host, port: port, labels: map[string]string{
				"statute.enable": "true", "statute.service": "shared", "statute.host": "app.example.com",
				"statute.path": "/*", "statute.routes.health.path": "/admin/health", "statute.routes.health.host": "app.example.com",
				"statute.routes.tie.path": "/admin/*", "statute.routes.tie.host": "app.example.com",
			}}
			p, srv, _ := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{bad, good})
			fallback := fallbackServer(t, srv, Routes{Match("/admin/static").Handle(noContentHandler)})
			mustSync(t, p)
			for _, tc := range []struct {
				url  string
				want int
			}{
				{"http://app.example.com/admin/private", 404},
				{"http://app.example.com/public", 204},
				{"http://app.example.com/admin/health", 204},
				{"http://app.example.com/admin/static", 204},
				{"http://other.example.com/", 204},
			} {
				rec := runRequest(t, srv.buildRouter(), httptest.NewRequest("GET", tc.url, nil))
				if rec.Code != tc.want || fallback.Load() != 0 {
					t.Fatalf("%s: status=%d want=%d fallback=%d", tc.url, rec.Code, tc.want, fallback.Load())
				}
			}
		})
	}
}

func TestDockerInvalidHintsRepairRestoresRateLimit(t *testing.T) {
	backend := httptest.NewServer(noContentHandler)
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)
	c := fakeDaemonContainer{name: "app", ip: host, port: port, labels: map[string]string{
		"statute.enable": "true", "statute.host": "app.example.com", "statute.timeout": "invalid", "statute.ratelimit": "1/h",
	}}
	p, srv, replace := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{c})
	for _, tc := range []struct {
		timeout string
		want    []int
	}{
		{"invalid", []int{404}}, {"30s", []int{204, 429}}, {"invalid", []int{404}},
	} {
		c.labels = maps.Clone(c.labels)
		c.labels["statute.timeout"] = tc.timeout
		replace([]fakeDaemonContainer{c})
		mustSync(t, p)
		for _, want := range tc.want {
			rec := runRequest(t, srv.buildRouter(), httptest.NewRequest("GET", "http://app.example.com/", nil))
			if rec.Code != want {
				t.Fatalf("timeout=%q: status=%d want=%d", tc.timeout, rec.Code, want)
			}
		}
	}
}

func TestDockerRouteChainsRejectInvalidHints(t *testing.T) {
	p := &dockerProvider{cfg: &resolved.Docker{}, warned: map[string]bool{}}
	table := &dynamicTable{}
	m := docker.CompileNative("app.example.com", "/admin/*")
	m.Hints = docker.MiddlewareHints{Timeout: "invalid", RateLimit: "1/s"}
	svc := &docker.Service{Name: "broken", Routes: []docker.Matcher{m}}
	chains, tombs := p.routeChains(svc, table)
	if len(chains) != 0 || len(tombs) != 1 || len(table.rejections) != 1 {
		t.Fatalf("chains=%d tombs=%d rejected=%d", len(chains), len(tombs), len(table.rejections))
	}
}

func TestDockerInvalidHintsPreserveWorkloadGrant(t *testing.T) {
	c := fakeDaemonContainer{name: "app", ip: "127.0.0.1", port: 1, stopped: true, labels: map[string]string{
		"statute.enable": "true", "statute.host": "app.example.com",
	}}
	p, srv, replace := newFakeProvider(t, &resolved.Docker{Workloads: map[string]resolved.Workload{"app": testWorkloadPolicy()}}, []fakeDaemonContainer{c})
	mustSync(t, p)
	before := p.workloadFor("app")
	if before == nil {
		t.Fatal("missing initial workload grant")
	}
	c.labels = maps.Clone(c.labels)
	c.labels["statute.timeout"] = "invalid"
	replace([]fakeDaemonContainer{c})
	mustSync(t, p)
	if p.workloadFor("app") != before {
		t.Fatal("middleware rejection replaced or retired the workload grant")
	}
	before.mu.Lock()
	retired := before.retired
	before.mu.Unlock()
	if retired || len(srv.dynamic.Load().routes) != 0 || len(srv.dynamic.Load().rejections) != 1 {
		t.Fatalf("retired=%v routes=%d refusals=%d", retired, len(srv.dynamic.Load().routes), len(srv.dynamic.Load().rejections))
	}
}

func TestDockerInvalidHintsPreserveContributorAmbiguity(t *testing.T) {
	containers := make([]fakeDaemonContainer, 0, 2)
	for _, name := range []string{"a-broken", "z-healthy"} {
		labels := map[string]string{"statute.enable": "true", "statute.service": "shared", "statute.host": name + ".example.com"}
		if name == "a-broken" {
			labels["statute.timeout"] = "invalid"
		}
		containers = append(containers, fakeDaemonContainer{name: name, ip: "127.0.0.1", port: 1, labels: labels})
	}
	p, srv, _ := newFakeProvider(t, &resolved.Docker{Workloads: map[string]resolved.Workload{"shared": testWorkloadPolicy()}}, containers)
	mustSync(t, p)
	if p.workloadFor("shared") != nil || len(srv.dynamic.Load().routes) != 1 || len(srv.dynamic.Load().rejections) != 1 {
		t.Fatalf("middleware validity changed contributor authority: workload=%v routes=%d refusals=%d",
			p.workloadFor("shared"), len(srv.dynamic.Load().routes), len(srv.dynamic.Load().rejections))
	}
}

func TestRateNormalizationRejectsUnderflow(t *testing.T) {
	for _, rate := range []string{"5e-324/min", "5e-324/h"} {
		if _, err := resolveMiddlewares([]Middleware{RateLimit(rate)}); err == nil {
			t.Fatalf("rate %q accepted zero normalized enforcement", rate)
		}
	}
	for _, rate := range []string{"5e-324/s", "1.7976931348623157e308/s"} {
		mws, err := resolveMiddlewares([]Middleware{RateLimit(rate)})
		if err != nil || mws[0].RateLimitPerSecond <= 0 {
			t.Fatalf("rate %q: %v, %v", rate, mws, err)
		}
	}
}
