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
    twin:
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
    three:
      rule: Host(` + "`three.example`" + `)
      service: api
      middlewares: [tag, twin]
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
	assertSharedPolicySource(t, string(result.Go))
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
		{"three.example", http.StatusOK},
		{"three.example", http.StatusTooManyRequests},
		{"two.example", http.StatusTooManyRequests},
	} {
		status, policy := hostStatus(t, client, address, step.host)
		if status != step.status || policy != "shared" {
			t.Fatalf("%s: status=%d policy=%q, want %d with shared policy", step.host, status, policy, step.status)
		}
	}
}

func assertSharedPolicySource(t *testing.T, source string) {
	t.Helper()
	for _, want := range []string{"func limit() statute.Middleware {", "func twin() statute.Middleware {", "func tag() statute.Middleware {", "func makeRateLimit(s rateLimitSettings) statute.Middleware {", "With(tag(), twin()),"} {
		if got := strings.Count(source, want); got != 1 {
			t.Fatalf("%q occurs %d times, want 1\n%s", want, got, source)
		}
	}
	if got := strings.Count(source, "With(tag(), limit()),"); got != 2 || strings.Count(source, "makeRateLimit(rateLimitSettings{") != 2 {
		t.Fatalf("shared calls occur %d times, want 2 with two constructor calls\n%s", got, source)
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
	g := &generator{reusables: map[reusableKey]*reusable{}, reusableNames: map[string]bool{}}
	for _, step := range []struct{ key, base, want string }{
		{"a-b@file", "aB", "aB()"},
		{"a.b@file", "aB", "aB2()"},
		{"a-b@file", "other", "aB()"},
		{"strings@file", "strings", "strings2()"},
		{"panic@file", "panic", "panic2()"},
		{"type@file", "type", "type2()"},
		{"configuration@file", "configuration", "configuration2()"},
		{"make-rate-limit@file", "makeRateLimit", "makeRateLimit2()"},
		{"migration-candidate0@file", "migrationCandidate0", "sourceMigrationCandidate0()"},
	} {
		if got := g.reuse(singleMiddlewareType, step.key, step.base, textExpr("statute.Retry(1)"), "middlewares:"+step.key).call(); got != step.want {
			t.Errorf("reuse(%q) = %q, want %q", step.key, got, step.want)
		}
	}
}

func TestMalformedReusableSourceFails(t *testing.T) {
	g := &generator{r: &Result{}, defaults: "statute.Defaults{}", reusableNames: map[string]bool{}}
	g.reusables = map[reusableKey]*reusable{{singleMiddlewareType, "broken"}: {name: "broken", typ: singleMiddlewareType, body: textExpr("statute.(")}}
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
	g.reusables = map[reusableKey]*reusable{{singleMiddlewareType, "partial"}: {name: "partial", typ: singleMiddlewareType, body: wrapExpr("statute.InOrder(", []expr{textExpr("statute.Retry(1)")}, "")}}
	if _, err := g.reusableSource("func f() { partial() }"); err == nil {
		t.Fatal("unterminated helper body accepted")
	}
	spec := &rateLimitSpec{rate: "1/s", burst: 1, reserve: "500ms", idle: "2s", selection: "x),y("}
	g.reusables = map[reusableKey]*reusable{
		{singleMiddlewareType, "a"}: {name: "a", typ: singleMiddlewareType, body: wrapExpr("statute.InOrder(", []expr{{rate: spec}}, ")")},
		{singleMiddlewareType, "b"}: {name: "b", typ: singleMiddlewareType, body: wrapExpr("statute.InOrder(", []expr{{rate: spec}}, ")")},
	}
	if _, err := g.reusableSource("func f() { a(); b() }"); err == nil {
		t.Fatal("malformed constructor call accepted")
	}
}

func TestReachabilityFollowsHelperReferences(t *testing.T) {
	g := &generator{reusables: map[reusableKey]*reusable{}, reusableNames: map[string]bool{}}
	inner := g.middlewareHelper("inner@file", []middlewarePart{{code: []expr{textExpr("statute.Retry(1)")}}}, "middlewares:inner@file")
	stack := g.middlewareHelper("stack@file", []middlewarePart{inner, inner}, "middlewares:stack@file")
	edge := g.middlewareHelper("edge@file", []middlewarePart{{code: []expr{textExpr("statute.Retry(2)")}}}, "middlewares:edge@file")
	entry := g.middlewareHelper("entrypointMiddleware", []middlewarePart{{code: []expr{wrapExpr(`statute.OnListeners([]string{"web"},`, []expr{variadicArgs([]middlewarePart{edge})}, ")")}}}, "entrypoints:web@static")
	g.middlewareHelper("orphan@file", []middlewarePart{{code: []expr{textExpr("statute.Retry(3)")}}}, "middlewares:orphan@file")
	used, err := g.usedReusables("func f() { _ = " + inline.render(sliceExpr([]middlewarePart{entry, stack})) + " }")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(keys(used), ","); got != "edge,entrypointMiddleware,inner,stack" {
		t.Fatalf("reachable helpers = %s, want edge,entrypointMiddleware,inner,stack", got)
	}
}

func TestMiddlewarePartRendering(t *testing.T) {
	single := middlewarePart{ref: &reusable{name: "secure"}, single: true}
	slice := middlewarePart{ref: &reusable{name: "stack"}}
	direct := middlewarePart{code: []expr{textExpr("statute.Retry(1)"), textExpr("statute.Retry(2)")}}
	for _, check := range []struct{ got, want string }{
		{inline.render(sliceExpr([]middlewarePart{single, direct})), "[]statute.Middleware{secure(),statute.Retry(1),statute.Retry(2)}"},
		{inline.render(sliceExpr([]middlewarePart{single, slice, direct})), "slices.Concat([]statute.Middleware{secure()},stack(),[]statute.Middleware{statute.Retry(1),statute.Retry(2)})"},
		{inline.render(variadicArgs([]middlewarePart{single, direct})), "secure(),statute.Retry(1),statute.Retry(2)"},
		{inline.render(variadicArgs([]middlewarePart{single, slice})), "slices.Concat([]statute.Middleware{secure()},stack())..."},
		{withCalls("With", []middlewarePart{single, slice, direct}), ".With(secure()).With(stack()...).With(statute.Retry(1),statute.Retry(2))"},
		{middlewareArguments(nil), ""},
		{middlewareArguments([]middlewarePart{single}), ",secure()"},
	} {
		if check.got != check.want {
			t.Errorf("got %s, want %s", check.got, check.want)
		}
	}
}

func TestRateLimitSpecSurvivesWrappers(t *testing.T) {
	specs := []*rateLimitSpec{
		{rate: "20/s", burst: 40, reserve: "25ms", idle: "2s", selection: `statute.IPSelection{Mode:"peer"}`},
		{rate: "5/s", burst: 10, reserve: "100ms", idle: "2s", key: hostRateKey},
		{rate: "5/s", burst: 10, reserve: "100ms", idle: "2s", key: headerRateKey, header: "X-Client"},
	}
	leaves := make([]expr, 0, len(specs))
	for _, spec := range specs {
		leaves = append(leaves, wrapExpr("statute.InOrder(", []expr{{rate: spec}}, ")"))
	}
	group := middlewarePart{code: leaves}
	nested := wrapExpr(`statute.OnListeners([]string{"web"},`, []expr{variadicArgs([]middlewarePart{{ref: &reusable{name: "stack"}}, group})}, ")")
	found := map[string]bool{}
	nested.constructors(found)
	if len(found) != 3 || !found["makeRateLimit"] || !found["makeHostRateLimit"] || !found["makeHeaderRateLimit"] {
		t.Fatalf("constructors through wrappers: %v", found)
	}
	shared := renderer{"makeRateLimit": true, "makeHostRateLimit": true, "makeHeaderRateLimit": true}
	for _, check := range []struct {
		r    renderer
		want string
	}{
		{inline, `statute.OnListeners([]string{"web"},slices.Concat(stack(),[]statute.Middleware{statute.InOrder(statute.RateLimit("20/s").Burst(40).Reserve("25ms").ExpireIdle("2s").EvictOldest().SelectIP(statute.IPSelection{Mode:"peer"})),statute.InOrder(statute.RateLimit("5/s").Burst(10).Reserve("100ms").ExpireIdle("2s").EvictOldest().Per(statute.HostHeader)),statute.InOrder(statute.RateLimit("5/s").Burst(10).Reserve("100ms").ExpireIdle("2s").EvictOldest().FromHeader("X-Client"))})...)`},
		{shared, `statute.OnListeners([]string{"web"},slices.Concat(stack(),[]statute.Middleware{statute.InOrder(makeRateLimit(rateLimitSettings{Rate:"20/s",Burst:40,Reserve:"25ms",ExpireIdle:"2s",Selection:statute.IPSelection{Mode:"peer"}})),statute.InOrder(makeHostRateLimit(rateLimitSettings{Rate:"5/s",Burst:10,Reserve:"100ms",ExpireIdle:"2s"})),statute.InOrder(makeHeaderRateLimit(rateLimitSettings{Rate:"5/s",Burst:10,Reserve:"100ms",ExpireIdle:"2s",Header:"X-Client"}))})...)`},
	} {
		if got := check.r.render(nested); got != check.want {
			t.Errorf("render:\n got %s\nwant %s", got, check.want)
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
	in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  web:\n    address: ':8080'\n    http: {sanitizePath: false, middlewares: [stack@file]}\nping: {entryPoint: web}\nproviders:\n  docker: {exposedByDefault: false}\n")}
	in.Compose = []string{fixture(t, "compose.yml", "services:\n  app:\n    labels:\n      traefik.enable: 'true'\n      traefik.http.routers.app.rule: Host(`app.example`)\n      traefik.http.routers.app.middlewares: stack@file\n      traefik.http.services.app.loadbalancer.server.port: '8080'\n")}
	result := audit(t, in)
	source := string(result.Go)
	for _, want := range []string{
		"func stack() []statute.Middleware {",
		`statute.OnListeners([]string{"web"}, stack()...)`,
		`Middleware("stack@file", stack()...)`,
		"With(stack()...)",
		"WithOuter(entrypointMiddleware())",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("missing %q:\n%s", want, source)
		}
	}
	assertGeneratedParity(t, result)
}

func TestRateLimitConstructionIsShared(t *testing.T) {
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [a, b, c, d, e, f]", 1) + `  middlewares:
    a:
      rateLimit: {average: 10, burst: 20}
    b:
      rateLimit: {average: 10, burst: 20}
    c:
      rateLimit: {average: 5, burst: 5, sourceCriterion: {requestHost: true}}
    d:
      rateLimit: {average: 5, burst: 6, sourceCriterion: {requestHost: true}}
    e:
      rateLimit: {average: 1, burst: 1, sourceCriterion: {requestHeaderName: X-Key}}
    f:
      rateLimit: {average: 1, burst: 1, sourceCriterion: {requestHeaderName: Y-Key}}
`
	result := audit(t, basicInputs(t, body))
	source := string(result.Go)
	for _, want := range []struct {
		text  string
		count int
	}{
		{"With(a(), b(), c(), d(), e(), f())", 1},
		{"// middlewares:a@file\nfunc a() statute.Middleware {", 1},
		{"// middlewares:b@file\nfunc b() statute.Middleware {", 1},
		{"type rateLimitSettings struct {", 1},
		{"func makeRateLimit(s rateLimitSettings) statute.Middleware {", 1},
		{"func makeHostRateLimit(s rateLimitSettings) statute.Middleware {", 1},
		{"func makeHeaderRateLimit(s rateLimitSettings) statute.Middleware {", 1},
		{"makeRateLimit(rateLimitSettings{\n\t\t\tRate:       \"10/s\",\n\t\t\tBurst:      20,", 2},
		{"makeHostRateLimit(rateLimitSettings{\n\t\t\tRate:       \"5/s\",", 2},
		{`Header:     "X-Key",`, 1},
		{`Header:     "Y-Key",`, 1},
		{"statute.RateLimit(", 3},
	} {
		if got := strings.Count(source, want.text); got != want.count {
			t.Errorf("%q occurs %d times, want %d\n%s", want.text, got, want.count, source)
		}
	}
	assertGeneratedParity(t, result)
}

func TestLongHelperNamesKeepEmptyCalls(t *testing.T) {
	name := strings.Repeat("a", 81)
	for src, want := range map[string]string{name + "()": name + "()", name + "{Zero: 0}": name + "{}"} {
		if got, err := layoutExpr(src); err != nil || got != want {
			t.Errorf("layoutExpr(%q) = %q, %v; want %q", src, got, err, want)
		}
	}
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: ["+name+"]", 1) + "  middlewares:\n    " + name + ":\n      rateLimit: {average: 20, burst: 40}\n"
	result := audit(t, basicInputs(t, body))
	if !strings.Contains(string(result.Go), "With(\n\t\t\t\t"+name+"(),\n\t\t\t),") {
		t.Fatalf("long helper call missing:\n%s", result.Go)
	}
	assertGeneratedParity(t, result)
}

func TestHelperKeysSeparatePolicyKinds(t *testing.T) {
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: ['serversTransports:backend']", 1)
	body = strings.Replace(body, "      loadBalancer:\n", "      loadBalancer:\n        serversTransport: backend\n", 1) + `  middlewares:
    serversTransports:backend:
      rateLimit: {average: 20, burst: 40}
  serversTransports:
    backend:
      forwardingTimeouts: {dialTimeout: 5s}
`
	result := audit(t, basicInputs(t, body))
	source := string(result.Go)
	for _, want := range []string{"func serversTransportsBackend() statute.Middleware {", "func backendTransport() statute.Transport {", "Transport: backendTransport(),"} {
		if !strings.Contains(source, want) {
			t.Fatalf("missing %q:\n%s", want, source)
		}
	}
	assertGeneratedParity(t, result)
}

const compactDynamic = `http:
  routers:
    multi:
      rule: Host(` + "`a.example`" + `) || Host(` + "`b.example`" + `)
      service: api
    folded:
      rule: Host(` + "`c.example`" + `) || Host(` + "`C.example`" + `)
      service: web
    redirect:
      rule: Host(` + "`r.example`" + `) || Host(` + "`s.example`" + `)
      service: noop@internal
      middlewares: [moved]
  middlewares:
    moved:
      redirectRegex: {regex: '^https?://([^/]+)/(.*)', replacement: 'https://new.example/${2}'}
  services:
    api:
      loadBalancer:
        servers:
          - url: http://127.0.0.1:9000
    web:
      loadBalancer:
        servers:
          - url: http://127.0.0.1:9001
    checked:
      loadBalancer:
        servers:
          - url: http://127.0.0.1:9002
        healthCheck: {path: /health}
`

func TestCompactRoutesAndPools(t *testing.T) {
	result := audit(t, basicInputs(t, compactDynamic))
	source := string(result.Go)
	for _, want := range []struct {
		text  string
		count int
	}{
		{`Hosts("a.example", "a.example.", "b.example", "b.example.")`, 1},
		{`Hosts("c.example", "c.example.")`, 2},
		{`Hosts("r.example", "r.example.", "s.example", "s.example.")`, 1},
		{"func migrationNoop() http.Handler {", 1},
		{"Handle(migrationNoop())", 1},
		{"StatusTeapot", 1},
		{"func makePool(address string, transport statute.Transport) statute.Pool {", 1},
		{`"api@file": makePool("http://127.0.0.1:9000", defaultTransport()),`, 1},
		{`"web@file": makePool("http://127.0.0.1:9001", defaultTransport()),`, 1},
		{`"checked@file": statute.Pool{`, 1},
	} {
		if got := strings.Count(source, want.text); got != want.count {
			t.Errorf("%q occurs %d times, want %d\n%s", want.text, got, want.count, source)
		}
	}
	assertGeneratedParity(t, result)
}

func TestSinglePoolKeepsLiteral(t *testing.T) {
	result := audit(t, basicInputs(t, simpleDynamic))
	source := string(result.Go)
	if strings.Contains(source, "makePool") || !strings.Contains(source, `"api@file": statute.Pool{`) {
		t.Fatalf("single simple pool uses a shared constructor:\n%s", source)
	}
}
