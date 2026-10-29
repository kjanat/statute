package migrate

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	statute "statute.kjanat.dev"
	"statute.kjanat.dev/resolved"
)

func policyInputs(t *testing.T, definition string) Inputs {
	t.Helper()
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [candidate]", 1)
	var out strings.Builder
	out.WriteString(body)
	out.WriteString("  middlewares:\n    candidate:\n")
	for line := range strings.SplitSeq(strings.TrimSpace(definition), "\n") {
		out.WriteString("      " + line + "\n")
	}
	return basicInputs(t, out.String())
}

func TestSecurityHeaderTranslation(t *testing.T) {
	r := audit(t, policyInputs(t, `headers:
  contentTypeNosniff: true
  frameDeny: true
  stsSeconds: 31536000
  stsIncludeSubdomains: true
  stsPreload: true
  referrerPolicy: strict-origin-when-cross-origin`))
	requireMigrationGaps(t, r)
	c, err := statute.Resolve(r.Config)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Strict-Transport-Security": "max-age=31536000; includeSubDomains; preload", "Referrer-Policy": "strict-origin-when-cross-origin"}
	for _, m := range c.Routes[0].Middleware {
		if m.HeaderValue != want[m.HeaderName] {
			t.Fatalf("unexpected policy: %+v", m)
		}
		if m.HeaderName == "Strict-Transport-Security" && !m.HeaderHTTPSOnly {
			t.Fatal("HSTS lost TLS-only scope")
		}
		delete(want, m.HeaderName)
	}
	if len(want) != 0 {
		t.Fatalf("missing headers: %v", want)
	}
	assertGeneratedParity(t, r)
}

func TestRedirectSchemeMethodTranslation(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		r := audit(t, policyInputs(t, fmt.Sprintf("redirectScheme: {scheme: https, permanent: %t}", permanent)))
		requireMigrationGaps(t, r)
		c, err := statute.Resolve(r.Config)
		if err != nil {
			t.Fatal(err)
		}
		status, other := 302, 307
		if permanent {
			status, other = 301, 308
		}
		want := resolved.Redirect{Target: "https://{host}{request_uri}", Status: status, NonGETStatus: other}
		if *c.Routes[0].Redirect != want {
			t.Fatalf("redirect=%+v want=%+v", c.Routes[0].Redirect, want)
		}
		assertGeneratedParity(t, r)
	}
}

func TestConsolidatedStaticDockerPriorityDecision(t *testing.T) {
	in := dockerServiceInputs(t, "      traefik.http.routers.api.priority: '100'\n")
	in.Dynamic = []string{fixture(t, "file.yml", strings.Replace(simpleDynamic, "service: api", "service: api\n      priority: 10", 1))}
	r := audit(t, in)
	requireMigrationGaps(t, r, "check.routing.route-precedence-decision")
	count := 0
	for _, check := range r.Report.Checks {
		if check.Code != "route-precedence-decision" {
			continue
		}
		count++
		if len(check.References) != 2 || !strings.Contains(check.Reason, "priority 100") || !strings.Contains(check.Reason, "priority 10") {
			t.Fatalf("ordering decision: %+v", check)
		}
	}
	if count != 1 {
		t.Fatalf("ordering findings=%d", count)
	}
}

func TestImplicitDockerServiceSelection(t *testing.T) {
	in := dockerServiceInputs(t, "")
	compose := `services:
  api:
    labels:
      traefik.enable: 'true'
      traefik.http.routers.api.rule: Host(` + "`api.example`" + `)
      traefik.http.services.api.loadbalancer.server.port: '8080'
`
	in.Compose = []string{fixture(t, "implicit.yml", compose)}
	r := audit(t, in)
	requireMigrationGaps(t, r)
	if r.Report.GeneratedRoutes != 1 {
		t.Fatalf("generated=%d", r.Report.GeneratedRoutes)
	}
	ambiguous := compose + "      traefik.http.services.other.loadbalancer.server.port: '9000'\n"
	in.Compose = []string{fixture(t, "ambiguous.yml", ambiguous)}
	requireMigrationGaps(t, audit(t, in), "routers.$service-selection")
}

func TestPassiveHealthCandidatePreservesSettings(t *testing.T) {
	body := strings.Replace(simpleDynamic, "      loadBalancer:", "      loadBalancer:\n        passiveHealthCheck: {failureWindow: 30s, maxFailedAttempts: 3}", 1)
	r := audit(t, basicInputs(t, body))
	requireMigrationGaps(t, r, "services.$health-passive")
	c, err := statute.Resolve(r.Config)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Upstreams["api@file"].PassiveHealthCheck
	if !p.Enabled || p.MaxFailures != 3 || p.FailureWindow != 30*time.Second {
		t.Fatalf("passive candidate=%+v", p)
	}
	assertGeneratedParity(t, r)
}

func TestOutOfScopeDoesNotObscureHTTP(t *testing.T) {
	in := basicInputs(t, simpleDynamic)
	in.Dynamic = append(in.Dynamic, fixture(t, "tcp.yml", "tcp:\n  routers:\n    ignored: {rule: 'HostSNI(`*`)', service: ignored}\n"))
	r := audit(t, in)
	requireMigrationGaps(t, r)
	var text bytes.Buffer
	if err := r.Report.WriteHuman(&text); err != nil {
		t.Fatal(err)
	}
	if strings.Count(text.String(), "TCP/UDP migration is outside") != 1 {
		t.Fatalf("out-of-scope report repeated: %s", text.String())
	}
}

func TestZeroHSTSDefinesEmptyNativeChain(t *testing.T) {
	r := audit(t, policyInputs(t, "headers: {contentTypeNosniff: false, frameDeny: false, stsSeconds: 0}"))
	requireMigrationGaps(t, r)
	assertGeneratedParity(t, r)
}

func TestSecurityHeaderOrderAcrossMiddlewares(t *testing.T) {
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [outer, inner]", 1)
	body += `  middlewares:
    outer:
      headers: {frameDeny: true}
    inner:
      headers: {customFrameOptionsValue: '${FRAME_POLICY}'}
`
	r := audit(t, basicInputs(t, body))
	requireMigrationGaps(t, r, "routers.$response-header-order")
}

func TestImplicitDockerServiceFromComposeTargetPort(t *testing.T) {
	for _, ports := range []string{"    expose: [8080]", "    ports: ['127.0.0.1:18080:8080']", "    ports: [{target: 8080, published: 18080, protocol: tcp}]"} {
		in := dockerServiceInputs(t, "")
		body := "services:\n  api:\n" + ports + `
    labels:
      traefik.enable: 'true'
      traefik.http.routers.api.rule: Host(` + "`api.example`" + `)
`
		in.Compose = []string{fixture(t, "exposed.yml", body)}
		r := audit(t, in)
		requireMigrationGaps(t, r)
		assertGeneratedParity(t, r)
	}
}

func TestDockerPolicyFamiliesUseCaseInsensitiveLabelPaths(t *testing.T) {
	in := dockerServiceInputs(t, `      traefik.http.services.api.loadbalancer.passivehealthcheck.failurewindow: 30s
      traefik.http.services.api.loadbalancer.passivehealthcheck.maxfailedattempts: '3'
      traefik.http.routers.api.middlewares: limiter
      traefik.http.middlewares.limiter.ratelimit.average: '20'
      traefik.http.middlewares.limiter.ratelimit.burst: '40'
`)
	r := audit(t, in)
	requireMigrationGaps(t, r, "middlewares.$rate-capacity", "middlewares.$rate-delay", "routers.$middleware:limiter@docker", "services.$health-passive")
	if !strings.Contains(string(r.Go), `statute.RateLimit("20/s")`) || !strings.Contains(string(r.Go), "MaxFailures: 3") {
		t.Fatal("Docker policy candidates missing")
	}
	assertGeneratedParity(t, r)
}

func TestUnsupportedMatcherHasOnePreciseRootFinding(t *testing.T) {
	for _, rule := range []string{"HostRegexp(`.*`)", "Host(`api.example`) && Query(`key`, `value`)"} {
		r := audit(t, basicInputs(t, strings.Replace(simpleDynamic, "Host(`api.example`) && Path(`/health`)", rule, 1)))
		requireMigrationGaps(t, r, "routers.rule")
		if strings.Contains(findings(r), "no verified translation") {
			t.Fatal("unsupported matcher obscured by dependency noise")
		}
	}
}

func TestReferencedPluginCannotBecomeEmptyNativeChain(t *testing.T) {
	r := audit(t, policyInputs(t, "plugin: {custom: {enabled: true}}"))
	requireMigrationGaps(t, r, "routers.$middleware:candidate@file")
	if r.Report.GeneratedRoutes != 0 || len(r.Config.Routes) != 0 {
		t.Fatal("route served without its plugin")
	}
}

func TestRepeatedDependencyHasOneRouteImpact(t *testing.T) {
	in := policyInputs(t, "plugin: {custom: {enabled: true}}")
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [candidate, candidate]", 1)
	body += "  middlewares:\n    candidate:\n      plugin: {custom: {enabled: true}}\n"
	in.Dynamic = []string{fixture(t, "repeated.yml", body)}
	r := audit(t, in)
	requireMigrationGaps(t, r, "routers.$middleware:candidate@file")
	count := 0
	for _, o := range r.Report.Objects {
		for _, f := range o.Fields {
			if f.Path == "$middleware:candidate@file" {
				count++
			}
		}
	}
	if count != 1 {
		t.Fatalf("dependency impacts=%d", count)
	}
}
