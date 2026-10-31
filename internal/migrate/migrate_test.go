package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	statute "statute.kjanat.dev"
	"statute.kjanat.dev/internal/docker"
)

func fixture(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func basicInputs(t *testing.T, body string) Inputs {
	t.Helper()
	return Inputs{Static: []string{fixture(t, "static.yml", "entryPoints:\n  web:\n    address: ':8080'\n    http: {sanitizePath: false}\n")}, Dynamic: []string{fixture(t, "dynamic.yml", body)}}
}

const simpleDynamic = `http:
  routers:
    api:
      rule: Host(` + "`api.example`" + `) && Path(` + "`/health`" + `)
      service: api
  services:
    api:
      loadBalancer:
        servers:
          - url: http://127.0.0.1:9000
`

func audit(t *testing.T, in Inputs) *Result {
	t.Helper()
	r, err := Audit(in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDeterministicGenerationAndCanonicalParity(t *testing.T) {
	in := basicInputs(t, simpleDynamic)
	a, b := audit(t, in), audit(t, in)
	ja, _ := json.Marshal(a.Report)
	jb, _ := json.Marshal(b.Report)
	if !bytes.Equal(a.Go, b.Go) || !bytes.Equal(ja, jb) {
		t.Fatal("nondeterministic artifacts")
	}
	if hasBlockingGaps(a) {
		t.Fatalf("unexpected blockers: %s", ja)
	}
	if a.Report.Routes != 1 || a.Report.Hosts != 1 || a.Report.GeneratedRoutes != 2 {
		t.Fatalf("counts: %+v", a.Report)
	}
	assertGeneratedParity(t, a)
}

func assertGeneratedParity(t *testing.T, a *Result) {
	t.Helper()
	var exported bytes.Buffer
	if err := statute.Export(a.Config, &exported); err != nil {
		t.Fatal(err)
	}
	// Run canonical tooling on the generated Go to verify its emitted builders.
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	test := filepath.Join(dir, "main_test.go")
	if err := os.WriteFile(source, a.Go, 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`package main
import ("bytes"; "testing"; statute "statute.kjanat.dev")
func TestGenerated(t *testing.T) {
 var b bytes.Buffer
 if err:=statute.Export(configuration(),&b);err!=nil{t.Fatal(err)}
 if b.String()!=%q {t.Fatal("generated source and audited configuration differ")}
 if err:=statute.GraphDOT(configuration(),&bytes.Buffer{});err!=nil{t.Fatal(err)}
 if _,err:=statute.Lint(configuration());err!=nil{t.Fatal(err)}
}
`, exported.String())
	if err := os.WriteFile(test, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G204 -- Compile only the generated fixture in a test-owned directory.
	cmd := exec.CommandContext(t.Context(), "go", "test", source, test)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated Go: %v\n%s", err, out)
	}
}

func TestHostExpansionMatchesSharedParser(t *testing.T) {
	for _, host := range []string{"example.com", "example.com.", "example.com.."} {
		ms, err := docker.ParseRule("Host(`" + host + "`)")
		if err != nil {
			t.Fatal(err)
		}
		for _, request := range []string{"example.com", "EXAMPLE.com", "example.com.", "example.com..", "example.com...", "other.com"} {
			got := false
			for _, h := range hostSpellings(host) {
				got = got || strings.EqualFold(h, request)
			}
			if got != ms[0].Match(request, "/") {
				t.Fatalf("host=%q request=%q", host, request)
			}
		}
	}
}

func TestUnsupportedMatchersAndLiteralWildcardNeverBroaden(t *testing.T) {
	for _, rule := range []string{"HostRegexp(`.*`)", "!Host(`private.example`)"} {
		t.Run(rule, func(t *testing.T) {
			body := strings.Replace(simpleDynamic, "Host(`api.example`) && Path(`/health`)", strconv.Quote(rule), 1)
			r := audit(t, basicInputs(t, body))
			if !hasBlockingGaps(r) || len(r.Config.Routes) > 0 || !bytes.Contains(r.Go, []byte("panic(")) {
				t.Fatal("unsafe rule generated a serving program")
			}
		})
	}
}

func TestEveryUnknownFieldIsBlockingAndValueFree(t *testing.T) {
	body := simpleDynamic + `  middlewares:
    credentials:
      basicAuth:
        users: ["user:TOP_SECRET_INLINE", 'user:$2y$12$TOP_SECRET_HASH']
        usersFile: /run/secrets/auth
    header:
      headers:
        customRequestHeaders:
          X-Unusual: TOP_SECRET_HEADER
    template:
      headers:
        customRequestHeaders:
          Authorization: ${TOKEN:-TOP_SECRET_DEFAULT}
  unknown:
    object:
      strange: TOP_SECRET_UNKNOWN
`
	r := audit(t, basicInputs(t, body))
	b, _ := json.Marshal(r.Report)
	var human bytes.Buffer
	if err := r.Report.WriteHuman(&human); err != nil {
		t.Fatal(err)
	}
	for _, out := range [][]byte{r.Go, b, human.Bytes()} {
		if bytes.Contains(out, []byte("TOP_SECRET")) {
			t.Fatalf("secret escaped: %s", out)
		}
	}
	if !bytes.Contains(b, []byte("env:TOKEN")) || !bytes.Contains(b, []byte("file:/run/secrets/auth")) {
		t.Fatal("references were lost")
	}
	if !hasBlockingGaps(r) || len(r.Report.Unmatched) < 4 {
		t.Fatal("unknown source objects were omitted")
	}
}

func TestMalformedInputDoesNotEchoSecrets(t *testing.T) {
	for _, body := range []string{"secret: TOP_SECRET: [", "a: 1\na: TOP_SECRET\n", "a: TOP_SECRET\n---\nb: 2"} {
		_, err := Audit(Inputs{Static: []string{fixture(t, "bad.yml", body)}})
		if err == nil || strings.Contains(err.Error(), "TOP_SECRET") {
			t.Fatalf("unsafe parser error: %v", err)
		}
	}
}

func TestProviderReferencesAndSharedPoolIsolation(t *testing.T) {
	body := `http:
  routers:
    guarded:
      rule: Host(` + "`private.example`" + `)
      service: api
      middlewares: [allow@file]
    public:
      rule: Host(` + "`public.example`" + `)
      service: api
  services:
    api:
      loadBalancer:
        servers: [{url: 'http://127.0.0.1:9000'}]
  middlewares:
    allow:
      ipAllowList:
        sourceRange: [192.0.2.0/24]
`
	r := audit(t, basicInputs(t, body))
	if hasBlockingGaps(r) {
		t.Fatalf("unexpected blockers: %+v", r.Report)
	}
	resolved, err := statute.Resolve(r.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range resolved.Routes {
		want := 0
		if strings.HasPrefix(route.Host, "private.") {
			want = 1
		}
		if len(route.Middleware) != want {
			t.Fatal("middleware leaked between routes sharing a pool")
		}
	}
	bad := audit(t, basicInputs(t, strings.Replace(body, "allow@file", "allow@docker", 1)))
	if !hasBlockingGaps(bad) {
		t.Fatal("provider-qualified missing policy accepted")
	}
	resolvedBad, err := statute.Resolve(bad.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range resolvedBad.Routes {
		if strings.HasPrefix(route.Host, "private.") {
			t.Fatal("missing policy retained an unprotected route")
		}
	}
}

func TestChainsRejectMissingAndCyclicReferences(t *testing.T) {
	for _, ref := range []string{"missing", "a"} {
		body := simpleDynamic + "  middlewares:\n    a:\n      chain:\n        middlewares: [" + ref + "]\n"
		r := audit(t, basicInputs(t, body))
		if !hasBlockingGaps(r) {
			t.Fatal("invalid chain accepted")
		}
	}
}

func TestTOMLAndYAMLGenerateSameConfiguration(t *testing.T) {
	y := audit(t, basicInputs(t, simpleDynamic))
	toml := `[http.routers.api]
rule = "Host(` + "`api.example`" + `) && Path(` + "`/health`" + `)"
service = "api"
[[http.services.api.loadBalancer.servers]]
url = "http://127.0.0.1:9000"
`
	in := basicInputs(t, simpleDynamic)
	in.Dynamic = []string{fixture(t, "dynamic.toml", toml)}
	r := audit(t, in)
	if !bytes.Equal(y.Go, r.Go) {
		t.Fatalf("TOML/YAML differ\n%s\n%s", y.Go, r.Go)
	}
}

func TestDockerCodeOwnedRegistrationAndPolicy(t *testing.T) {
	static := `entryPoints:
  web:
    address: ':8080'
    http: {sanitizePath: false}
providers:
  docker:
    exposedByDefault: false
`
	compose := `services:
  api:
    labels:
      - traefik.enable=true
      - traefik.http.routers.api.rule=Host(` + "`api.example`" + `)
      - traefik.http.routers.api.service=api
      - traefik.http.routers.api.middlewares=allow@docker
      - traefik.http.services.api.loadbalancer.server.port=8080
      - traefik.http.services.api.loadbalancer.passhostheader=false
      - traefik.http.middlewares.allow.ipallowlist.sourcerange=192.0.2.0/24
`
	r := audit(t, Inputs{Static: []string{fixture(t, "static.yml", static)}, Compose: []string{fixture(t, "compose.yml", compose)}})
	if hasBlockingGaps(r) {
		b, _ := json.Marshal(r.Report)
		t.Fatalf("blocked: %s", b)
	}
	if !regexp.MustCompile(`Middleware\(\s*"allow@docker"`).Match(r.Go) || !regexp.MustCompile(`PoolPolicy\(\s*"api@traefik"`).Match(r.Go) {
		t.Fatalf("registrations missing: %s", r.Go)
	}
	assertGeneratedParity(t, r)
}

func TestPrecedenceAndDuplicateIdentitiesBlockServing(t *testing.T) {
	body := strings.Replace(simpleDynamic, "      service: api", "      service: api\n      priority: 42", 1)
	body = strings.Replace(body, "  services:", "    other:\n      rule: Path(`/health`)\n      service: api\n      priority: 42\n  services:", 1)
	r := audit(t, basicInputs(t, body))
	requireMigrationGaps(t, r)
	if !r.Config.PrioritizeRoutes {
		t.Fatal("source priority ordering lost")
	}
	in := basicInputs(t, simpleDynamic)
	in.Dynamic = append(in.Dynamic, in.Dynamic[0])
	r = audit(t, in)
	if !hasBlockingGaps(r) {
		t.Fatal("duplicate provider identity accepted")
	}
}

func TestFallbackIsGeneratedOnlyWithVisiblePriorityReview(t *testing.T) {
	body := strings.Replace(simpleDynamic, "Host(`api.example`) && Path(`/health`)", "PathPrefix(`/`)", 1)
	r := audit(t, basicInputs(t, body))
	if len(r.Config.Routes) != 1 || !r.Config.PrioritizeRoutes || hasBlockingGaps(r) {
		t.Fatalf("fallback missing: %+v", r.Report)
	}
	assertGeneratedParity(t, r)
}

func TestNamedTransportAndHostPolicyGeneratedParity(t *testing.T) {
	body := strings.Replace(simpleDynamic, "      loadBalancer:", "      loadBalancer:\n        passHostHeader: false\n        serversTransport: origin@file", 1)
	body += "  serversTransports:\n    origin:\n      serverName: origin.example\n      forwardingTimeouts:\n        responseHeaderTimeout: 5s\n    unused:\n      serverName: unused.example\n"
	r := audit(t, basicInputs(t, body))
	if hasBlockingGaps(r) {
		t.Fatal("supported transport blocked")
	}
	pool := r.Config.Upstreams["api@file"]
	if pool.Transport.ServerName != "origin.example" || pool.Transport.ResponseHeaderTimeout != "5s" || pool.UpstreamHost != statute.TargetHost {
		t.Fatal("pool policy drift")
	}
	for _, o := range r.Report.Objects {
		if o.Kind == "serversTransports" && o.Generated != (o.Name == "origin") {
			t.Fatal("transport contribution accounting drift")
		}
	}
	assertGeneratedParity(t, r)
}

func TestTLSAndRedirectCandidatesRemainGuarded(t *testing.T) {
	body := `tls:
  certificates:
    - certFile: /run/tls/cert.pem
      keyFile: /run/tls/key.pem
http:
  routers:
    alias:
      rule: Host(` + "`alias.example`" + `)
      service: noop@internal
      middlewares: [redirect]
  middlewares:
    redirect:
      redirectScheme:
        scheme: https
        permanent: true
`
	in := basicInputs(t, body)
	in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  websecure:\n    address: ':8443'\n    http:\n      sanitizePath: false\n      tls: {}\n")}
	r := audit(t, in)
	if !hasBlockingGaps(r) || !bytes.Contains(r.Go, []byte("statute.HTTPS(")) || !bytes.Contains(r.Go, []byte("statute.RedirectScheme(")) {
		t.Fatalf("guarded candidates missing: %s", r.Go)
	}
	assertGeneratedParity(t, r)
}

func TestDottedUnknownFieldCannotHideBehindRecognizedField(t *testing.T) {
	body := strings.Replace(simpleDynamic, "      loadBalancer:", "      loadBalancer.servers: [{url: 'http://hidden.invalid:9000'}]\n      loadBalancer:", 1)
	r := audit(t, basicInputs(t, body))
	if !hasBlockingGaps(r) || len(r.Config.Upstreams) != 0 {
		t.Fatal("unknown dotted field hidden by a recognized nested path")
	}
}

func TestUnknownFileSuffixDoesNotExposeValues(t *testing.T) {
	r := audit(t, basicInputs(t, simpleDynamic+"  unknown:\n    app:\n      profile: TOP_SECRET_PROFILE\n"))
	b, _ := json.Marshal(r.Report)
	if bytes.Contains(b, []byte("TOP_SECRET")) {
		t.Fatal("unknown field was treated as a file reference")
	}
}

func TestUnresolvedReferenceDoesNotExposeDefault(t *testing.T) {
	body := strings.Replace(simpleDynamic, "      service: api", "      service: api\n      middlewares: ['${POLICY:-TOP_SECRET_DEFAULT}']", 1)
	r := audit(t, basicInputs(t, body))
	b, _ := json.Marshal(r.Report)
	if !hasBlockingGaps(r) || bytes.Contains(b, []byte("TOP_SECRET")) || bytes.Contains(r.Go, []byte("TOP_SECRET")) {
		t.Fatal("unresolved reference leaked its default")
	}
}

func TestUnusedMiddlewareIsAnUnmatchedSourceObject(t *testing.T) {
	r := audit(t, basicInputs(t, simpleDynamic+"  middlewares:\n    unused:\n      ipAllowList:\n        sourceRange: [192.0.2.0/24]\n"))
	found := false
	for _, id := range r.Report.Unmatched {
		if id == "middlewares:unused@file" {
			found = true
		}
	}
	if !found {
		t.Fatal("unused middleware silently reported as generated")
	}
}

func TestMultipleMiddlewareTypesCannotBecomeAnInventedChain(t *testing.T) {
	body := simpleDynamic + "  middlewares:\n    ambiguous:\n      ipAllowList:\n        sourceRange: [192.0.2.0/24]\n      headers:\n        customResponseHeaders:\n          X-Powered-By: ''\n"
	r := audit(t, basicInputs(t, body))
	if !hasBlockingGaps(r) {
		t.Fatal("multiple middleware types silently combined")
	}
}

func TestMiddlewareExpansionIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(simpleDynamic + "  middlewares:\n    m0:\n      ipAllowList:\n        sourceRange: [192.0.2.0/24]\n")
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "    m%d:\n      chain:\n        middlewares: [m%d, m%d]\n", i, i-1, i-1)
	}
	r := audit(t, basicInputs(t, b.String()))
	if !hasBlockingGaps(r) {
		t.Fatal("exponential chain expansion accepted")
	}
	for _, o := range r.Report.Objects {
		if o.Name == "m10" && o.Generated {
			t.Fatal("oversized chain generated")
		}
	}
}

func TestDockerMissingDependenciesAreBlocking(t *testing.T) {
	static := fixture(t, "static.yml", "entryPoints:\n  web:\n    address: ':8080'\n    http: {sanitizePath: false}\nproviders:\n  docker:\n    exposedByDefault: false\n")
	compose := fixture(t, "compose.yml", "services:\n  app:\n    labels:\n      traefik.http.routers.app.rule: Host(`app.example`)\n      traefik.http.routers.app.service: missing\n")
	r := audit(t, Inputs{Static: []string{static}, Compose: []string{compose}})
	if !hasBlockingGaps(r) || r.Report.GeneratedRoutes != 0 {
		t.Fatal("missing Docker dependency accepted")
	}
}
