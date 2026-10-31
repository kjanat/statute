package migrate

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

const (
	largeFileRouters   = 300
	largeDockerRouters = 120
	largeReuseStride   = 10
	maxGeneratedWidth  = 100
)

func largeMigrationInputs(t *testing.T) Inputs {
	t.Helper()
	var dynamic strings.Builder
	dynamic.WriteString("http:\n  middlewares:\n    limit:\n      rateLimit: {average: 20, burst: 40}\n    twin:\n      rateLimit: {average: 20, burst: 40}\n    other:\n      rateLimit: {average: 20, burst: 41}\n  serversTransports:\n    backend:\n      forwardingTimeouts: {dialTimeout: 5s}\n  routers:\n")
	for i := range largeFileRouters {
		refs := "limit"
		switch i % largeReuseStride {
		case 0:
			refs += ", twin"
		case 1:
			refs += ", other"
		}
		fmt.Fprintf(&dynamic, "    router%03d:\n      rule: Host(`svc%03d.example.test`) && PathPrefix(`/api`)\n      service: svc%03d\n      middlewares: [%s]\n", i, i, i, refs)
	}
	dynamic.WriteString("  services:\n")
	for i := range largeFileRouters {
		fmt.Fprintf(&dynamic, "    svc%03d:\n      loadBalancer:\n        passHostHeader: false\n        serversTransport: backend\n        servers: [{url: 'http://127.0.0.1:%d'}]\n", i, 20000+i)
	}
	var compose strings.Builder
	compose.WriteString("services:\n")
	for i := range largeDockerRouters {
		fmt.Fprintf(&compose, "  app%03d:\n    labels:\n      traefik.enable: 'true'\n      traefik.http.routers.app%03d.rule: Host(`app%03d.example.test`)\n      traefik.http.routers.app%03d.service: app%03d\n      traefik.http.routers.app%03d.middlewares: limit@file\n      traefik.http.services.app%03d.loadbalancer.server.port: '8080'\n      traefik.http.services.app%03d.loadbalancer.passhostheader: 'false'\n      traefik.http.services.app%03d.loadbalancer.serverstransport: backend@file\n", i, i, i, i, i, i, i, i, i)
	}
	static := "entryPoints:\n  web: {address: ':8080', http: {sanitizePath: false}}\nproviders:\n  docker: {exposedByDefault: false}\n"
	return Inputs{
		Static:  []string{fixture(t, "static.yml", static)},
		Dynamic: []string{fixture(t, "dynamic.yml", dynamic.String())},
		Compose: []string{fixture(t, "compose.yml", compose.String())},
	}
}

func TestLargeMigrationEmitsStructuredGo(t *testing.T) {
	in := largeMigrationInputs(t)
	r := audit(t, in)
	if hasBlockingGaps(r) {
		t.Fatalf("large fixture blocked: %s", findings(r))
	}
	if again := audit(t, in); !bytes.Equal(r.Go, again.Go) {
		t.Fatal("identical inputs produced different source")
	}
	source := string(r.Go)
	for _, section := range []string{"func generatedListeners() statute.Listeners", "func generatedUpstreams() statute.Upstreams", "func generatedRoutes() statute.Routes", "func generatedDocker() *statute.DockerConfig"} {
		if !strings.Contains(source, section) {
			t.Errorf("missing section %q", section)
		}
	}
	for i := range largeFileRouters {
		for _, origin := range []string{fmt.Sprintf("// routers:router%03d@file\n", i), fmt.Sprintf("// services:svc%03d@file\n", i)} {
			if !strings.Contains(source, origin) {
				t.Fatalf("missing source comment %q", origin)
			}
		}
	}
	assertGeneratedWidth(t, source)
	assertReusedFragments(t, source)
	assertGeneratedParity(t, r)
	binary := compileGenerated(t, r)
	for _, flag := range []string{"-validate", "-lint", "-export", "-graph"} {
		// #nosec G204 -- Execute only the generated test binary.
		if out, err := exec.CommandContext(t.Context(), binary, flag).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", flag, err, out)
		}
	}
}

func assertReusedFragments(t *testing.T, source string) {
	t.Helper()
	twins := (largeFileRouters + largeReuseStride - 1) / largeReuseStride
	others := (largeFileRouters + largeReuseStride - 2) / largeReuseStride
	for _, want := range []struct {
		text  string
		count int
	}{
		{"// middlewares:limit@file\nfunc limit() statute.Middleware {", 1},
		{"// middlewares:twin@file\nfunc twin() statute.Middleware {", 1},
		{"// middlewares:other@file\nfunc other() statute.Middleware {", 1},
		{"func makeRateLimit(", 1},
		{"makeRateLimit(", 4},
		{"statute.RateLimit(", 1},
		{"With(limit()),", largeFileRouters - twins - others},
		{"With(limit(), twin()),", twins},
		{"With(limit(), other()),", others},
		{`Middleware("limit@file", limit())`, 1},
		{"// serversTransports:backend@file\nfunc backendTransport() statute.Transport {", 1},
		{"backendTransport(),", largeFileRouters + largeDockerRouters},
		{"statute.Transport{", 1},
		{"...", 0},
	} {
		if got := strings.Count(source, want.text); got != want.count {
			t.Errorf("%q occurs %d times, want %d", want.text, got, want.count)
		}
	}
}

func assertGeneratedWidth(t *testing.T, source string) {
	t.Helper()
	for n, line := range strings.Split(source, "\n") {
		width := len(strings.ReplaceAll(line, "\t", "    "))
		if width > maxGeneratedWidth && !strings.HasPrefix(strings.TrimSpace(line), `"`) {
			t.Errorf("line %d has width %d: %s", n+1, width, line)
		}
	}
}

func TestLayoutExpandsChainsAndOmitsZeroFields(t *testing.T) {
	got, err := layoutExpr(`statute.Match("/*").ProxyTo("one").Priority(10).OnListeners("websecure").With(statute.Pool{Backends:[]statute.Backend{{Address:"http://a"}},AllowEmpty:false,Transport:statute.Transport{InsecureSkipVerify:false}})`)
	if err != nil {
		t.Fatal(err)
	}
	want := "statute.Match(\"/*\").\nProxyTo(\"one\").\nPriority(10).\nOnListeners(\"websecure\").\nWith(statute.Pool{Backends: []statute.Backend{{Address: \"http://a\"}}})"
	if got != want {
		t.Fatalf("layout:\n%s\nwant:\n%s", got, want)
	}
	if got, err := layoutExpr(`statute.Docker().TraefikLabels()`); err != nil || got != "statute.Docker().TraefikLabels()" {
		t.Fatalf("short chain = %q, %v", got, err)
	}
	if got, err := layoutExpr(`map[string]string{"A":""}`); err != nil || got != `map[string]string{"A": ""}` {
		t.Fatalf("map zero value removed: %q, %v", got, err)
	}
	if originComment("routers:a\nb@file") != `// routers:a\nb@file` {
		t.Fatal("origin comment can break its line")
	}
}
