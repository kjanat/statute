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
		{"func limitFileMiddleware() []statute.Middleware {", 1},
		{"func tagFileMiddleware() []statute.Middleware {", 1},
		{"With(tagFileMiddleware()...).\n\t\t\tWith(limitFileMiddleware()...)", 2},
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
	if !strings.Contains(source, "func migrationCandidate0() []statute.Middleware {\n\treturn limitFileMiddleware()\n}") || strings.Count(source, "func limitFileMiddleware()") != 1 {
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
		"common-headers@file": "commonHeadersFile",
		"1api@docker":         "source1apiDocker",
		"cafés@file":          "cafSFile",
		"@@":                  "source",
	} {
		if got := helperIdentifier(source); got != want {
			t.Errorf("helperIdentifier(%q) = %q, want %q", source, got, want)
		}
	}
	g := &generator{reusables: map[string]*reusable{}, reusableNames: map[string]bool{}}
	first := g.reuse(middlewareType, "a-b@file", "middlewares:a-b@file", "[]statute.Middleware{statute.Retry(1)}")
	second := g.reuse(middlewareType, "a.b@file", "middlewares:a.b@file", "[]statute.Middleware{statute.Retry(2)}")
	same := g.reuse(middlewareType, "c@file", "middlewares:c@file", "[]statute.Middleware{statute.Retry(1)}")
	if first != "aBFileMiddleware()" || second != "aBFileMiddleware2()" || same != first {
		t.Fatalf("names: %s %s %s", first, second, same)
	}
	if origins := g.reusables[middlewareType+"\x00[]statute.Middleware{statute.Retry(1)}"].origins; len(origins) != 2 {
		t.Fatalf("origins: %v", origins)
	}
}
