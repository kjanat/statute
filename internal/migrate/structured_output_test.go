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
	maxGeneratedWidth  = 100
)

func largeMigrationInputs(t *testing.T) Inputs {
	t.Helper()
	var dynamic strings.Builder
	dynamic.WriteString("http:\n  middlewares:\n    limit:\n      rateLimit: {average: 20, burst: 40}\n  routers:\n")
	for i := range largeFileRouters {
		fmt.Fprintf(&dynamic, "    router%03d:\n      rule: Host(`svc%03d.example.test`) && PathPrefix(`/api`)\n      service: svc%03d\n      middlewares: [limit]\n", i, i, i)
	}
	dynamic.WriteString("  services:\n")
	for i := range largeFileRouters {
		fmt.Fprintf(&dynamic, "    svc%03d:\n      loadBalancer:\n        passHostHeader: false\n        servers: [{url: 'http://127.0.0.1:%d'}]\n", i, 20000+i)
	}
	var compose strings.Builder
	compose.WriteString("services:\n")
	for i := range largeDockerRouters {
		fmt.Fprintf(&compose, "  app%03d:\n    labels:\n      traefik.enable: 'true'\n      traefik.http.routers.app%03d.rule: Host(`app%03d.example.test`)\n      traefik.http.routers.app%03d.service: app%03d\n      traefik.http.services.app%03d.loadbalancer.server.port: '8080'\n      traefik.http.services.app%03d.loadbalancer.passhostheader: 'false'\n", i, i, i, i, i, i, i)
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
	assertGeneratedParity(t, r)
	binary := compileGenerated(t, r)
	for _, flag := range []string{"-validate", "-lint", "-export", "-graph"} {
		// #nosec G204 -- Execute only the generated test binary.
		if out, err := exec.CommandContext(t.Context(), binary, flag).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", flag, err, out)
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
