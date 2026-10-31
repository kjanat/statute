package migrate

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const sharedPolicyDynamic = `http:
  middlewares:
    tag:
      headers:
        customResponseHeaders: {X-Policy: '${STATUTE_TEST_SHARED_POLICY}'}
    limit:
      rateLimit: {average: 1, burst: 1}
  routers:
    one:
      rule: Host(` + "`one.example`" + `)
      service: api
      middlewares: [tag, limit]
    two:
      rule: Host(` + "`two.example`" + `)
      service: api
      middlewares: [tag, limit]
  services:
    api:
      loadBalancer:
        servers:
          - url: http://127.0.0.1:9000
`

func TestSharedHelpersKeepRouteState(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	address := testListenAddress(t)
	in := basicInputs(t, strings.ReplaceAll(sharedPolicyDynamic, "http://127.0.0.1:9000", backend.URL))
	in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  web: {address: '"+address+"', http: {sanitizePath: false}}\n")}
	result := audit(t, in)
	source := string(result.Go)
	for _, want := range []struct {
		text  string
		count int
	}{
		{"func limit() statute.Middleware {", 1},
		{"func tag() statute.Middleware {", 1},
		{"With(tag(), limit()),", 2},
	} {
		if got := strings.Count(source, want.text); got != want.count {
			t.Fatalf("%q occurs %d times, want %d\n%s", want.text, got, want.count, source)
		}
	}
	binary := compileGenerated(t, result)
	// #nosec G204 -- Execute only the generated test binary.
	missing := exec.CommandContext(t.Context(), binary, "-validate")
	missing.Env = []string{"STATUTE_TEST_SHARED_POLICY="}
	if err := missing.Run(); err == nil {
		t.Fatal("missing binding accepted")
	}
	t.Setenv("STATUTE_TEST_SHARED_POLICY", "shared")
	// #nosec G204 -- Execute only the generated test binary.
	cmd := exec.CommandContext(t.Context(), binary)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	ready := waitGeneratedResponse(t, client, "http://"+address+"/")
	_ = ready.Body.Close()
	for _, step := range []struct {
		host   string
		status int
	}{
		{"one.example", http.StatusOK},
		{"one.example", http.StatusTooManyRequests},
		{"two.example", http.StatusOK},
	} {
		status, policy := hostStatus(t, client, address, step.host)
		if status != step.status || policy != "shared" {
			t.Fatalf("%s: status=%d policy=%q, want %d with shared policy", step.host, status, policy, step.status)
		}
	}
}

func hostStatus(t *testing.T, client *http.Client, address, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode, response.Header.Get("X-Policy")
}

func TestBlockedCandidateCallsSharedHelper(t *testing.T) {
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [limit]", 1) + `  middlewares:
    limit:
      rateLimit: {average: 20, burst: 40}
    guarded:
      chain: {middlewares: [limit, absent]}
`
	result := audit(t, basicInputs(t, body))
	source := string(result.Go)
	if !strings.Contains(source, "func migrationCandidate0() []statute.Middleware {\n\treturn []statute.Middleware{limit()}\n}") || strings.Count(source, "func limit()") != 1 {
		t.Fatalf("blocked chain candidate does not reuse the shared helper:\n%s", source)
	}
	binary := compileGenerated(t, result)
	// #nosec G204 -- Execute only the generated test binary.
	out, err := exec.CommandContext(t.Context(), binary, "-validate").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "migration incomplete") {
		t.Fatalf("blocked migration served: %v\n%s", err, out)
	}
}

func TestReusableHelperNames(t *testing.T) {
	for source, want := range map[string]string{
		"common-headers@file": "commonHeaders",
		"1api@docker":         "source1api",
		"cafés@file":          "cafS",
		"@@":                  "source",
	} {
		if got := helperIdentifier(source); got != want {
			t.Errorf("helperIdentifier(%q) = %q, want %q", source, got, want)
		}
	}
	g := &generator{reusables: map[string]*reusable{}, reusableNames: map[string]bool{}}
	for _, step := range []struct{ base, body, want string }{
		{"aB", "statute.Retry(1)", "aB()"},
		{"aB", "statute.Retry(2)", "aB2()"},
		{"c", "statute.Retry(1)", "aB()"},
		{"strings", "statute.Retry(3)", "strings2()"},
		{"panic", "statute.Retry(4)", "panic2()"},
		{"type", "statute.Retry(5)", "type2()"},
		{"configuration", "statute.Retry(6)", "configuration2()"},
		{"migrationCandidate0", "statute.Retry(7)", "sourceMigrationCandidate0()"},
	} {
		if got := g.reuse(singleMiddlewareType, step.base, "middlewares:"+step.base+"@file", step.body); got != step.want {
			t.Errorf("reuse(%q) = %q, want %q", step.base, got, step.want)
		}
	}
	if origins := g.reusables[singleMiddlewareType+"\x00statute.Retry(1)"].origins; len(origins) != 2 {
		t.Fatalf("origins: %v", origins)
	}
}

func TestMalformedReusableSourceFails(t *testing.T) {
	g := &generator{r: &Result{}, defaults: "statute.Defaults{}", reusableNames: map[string]bool{}}
	g.reusables = map[string]*reusable{"broken": {name: "broken", typ: singleMiddlewareType, body: "statute.("}}
	if _, err := g.reusableSource("func f() {"); err == nil {
		t.Fatal("malformed generated body accepted")
	}
	if _, err := g.reusableSource("func f() { broken() }"); err == nil {
		t.Fatal("malformed helper body accepted")
	}
	g.candidates = map[string]fragment{"migrationCandidate0": {origin: "middlewares:x@file", code: "broken()"}}
	if _, err := g.source(); err == nil {
		t.Fatal("source accepted a malformed helper")
	}
}

func TestMiddlewarePartRendering(t *testing.T) {
	single := middlewarePart{call: "secure()", single: true}
	slice := middlewarePart{call: "stack()"}
	inline := middlewarePart{code: []string{"statute.Retry(1)", "statute.Retry(2)"}}
	for _, check := range []struct{ got, want string }{
		{sliceExpr([]middlewarePart{single, inline}), "[]statute.Middleware{secure(),statute.Retry(1),statute.Retry(2)}"},
		{sliceExpr([]middlewarePart{single, slice, inline}), "slices.Concat([]statute.Middleware{secure()},stack(),[]statute.Middleware{statute.Retry(1),statute.Retry(2)})"},
		{variadicArgs([]middlewarePart{single, inline}), "secure(),statute.Retry(1),statute.Retry(2)"},
		{variadicArgs([]middlewarePart{single, slice}), "slices.Concat([]statute.Middleware{secure()},stack())..."},
		{withCalls("With", []middlewarePart{single, slice, inline}), ".With(secure()).With(stack()...).With(statute.Retry(1),statute.Retry(2))"},
		{middlewareArguments(nil), ""},
		{middlewareArguments([]middlewarePart{single}), ",secure()"},
	} {
		if check.got != check.want {
			t.Errorf("got %s, want %s", check.got, check.want)
		}
	}
}

func TestSliceHelpersSpreadAtEveryUseSite(t *testing.T) {
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [stack]", 1) + `  middlewares:
    limit:
      rateLimit: {average: 20, burst: 40}
    stack:
      chain: {middlewares: [limit, limit]}
`
	in := basicInputs(t, body)
	in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  web:\n    address: ':8080'\n    http: {sanitizePath: false, middlewares: [stack@file]}\nproviders:\n  docker: {exposedByDefault: false}\n")}
	in.Compose = []string{fixture(t, "compose.yml", "services:\n  app:\n    labels:\n      traefik.enable: 'true'\n      traefik.http.routers.app.rule: Host(`app.example`)\n      traefik.http.routers.app.middlewares: stack@file\n      traefik.http.services.app.loadbalancer.server.port: '8080'\n")}
	result := audit(t, in)
	source := string(result.Go)
	for _, want := range []string{
		"func stack() []statute.Middleware {",
		`statute.OnListeners([]string{"web"}, stack()...)`,
		`Middleware("stack@file", stack()...)`,
		"With(stack()...)",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("missing %q:\n%s", want, source)
		}
	}
	assertGeneratedParity(t, result)
}
