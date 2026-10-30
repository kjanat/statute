package migrate

import (
	"strings"
	"testing"

	statute "statute.kjanat.dev"
)

const boundHeaderPolicies = `  middlewares:
    security:
      headers:
        contentTypeNosniff: true
        customFrameOptionsValue: SAMEORIGIN
        customResponseHeaders: {X-XSS-Protection: '0'}
    robots:
      headers: {customResponseHeaders: {X-Robots-Tag: 'noindex, nofollow'}}
`

func compositionInput(t *testing.T, refs, extra string) Inputs {
	t.Helper()
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: ["+refs+"]", 1)
	return basicInputs(t, body+boundHeaderPolicies+extra)
}

func TestInlineBindingCandidatesKeepCompleteRoutePolicy(t *testing.T) {
	r := audit(t, compositionInput(t, "security, robots", ""))
	requireMigrationGaps(t, r)
	if r.Report.GeneratedRoutes != 2 {
		t.Fatalf("candidate routes=%d, want both host spellings with their full policy", r.Report.GeneratedRoutes)
	}
	c, err := statute.Resolve(r.Config)
	if err != nil || len(testMiddlewareLeaves(c.Routes[0].Middleware)) != 4 {
		t.Fatalf("candidate header chain: %v %+v", err, c)
	}
	for _, value := range []string{"SAMEORIGIN", "noindex, nofollow"} {
		if !strings.Contains(string(r.Go), value) {
			t.Fatal("public header policy missing")
		}
	}
	bindCandidateHeaders(t, r)
	assertGeneratedParity(t, r)
}

func TestRedirectCompositionKeepsHeadersAndSkipsUnusedService(t *testing.T) {
	for _, refs := range []string{"security, robots, canonical", "outer"} {
		t.Run(refs, func(t *testing.T) {
			extra := `    canonical:
      redirectRegex: {regex: '^https?://[^/]+/(.*)', replacement: 'https://new.example/${1}', permanent: true}
`
			if refs == "outer" {
				extra += `    outer:
      chain: {middlewares: [security, inner]}
    inner:
      chain: {middlewares: [robots, canonical]}
`
			}
			in := compositionInput(t, refs, extra)
			body := strings.Replace(simpleDynamic, "service: api", "service: noop@internal\n      middlewares: ["+refs+"]", 1)
			in.Dynamic = []string{fixture(t, "dynamic.yml", body+boundHeaderPolicies+extra)}
			r := audit(t, in)
			requireMigrationGaps(t, r)
			if r.Report.GeneratedRoutes != 2 {
				t.Fatalf("redirect candidates=%d", r.Report.GeneratedRoutes)
			}
			c, err := statute.Resolve(r.Config)
			if err != nil || len(testMiddlewareLeaves(c.Routes[0].Middleware)) != 5 {
				t.Fatalf("redirect chain lost policy: %v %+v", err, c)
			}
			bindCandidateHeaders(t, r)
			assertGeneratedParity(t, r)
		})
	}
}

func bindCandidateHeaders(t *testing.T, r *Result) {
	t.Helper()
	for _, check := range r.Report.Checks {
		if name, ok := strings.CutPrefix(check.Code, "environment-required:"); ok {
			t.Setenv(name, "migration-binding")
		}
	}
}

func TestBindingCandidateCannotHideUnsupportedHeaderPolicy(t *testing.T) {
	in := compositionInput(t, "security, robots", "")
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [security, robots]", 1)
	headers := strings.Replace(boundHeaderPolicies, "contentTypeNosniff: true", "contentTypeNosniff: true\n        accessControlAllowCredentials: true", 1)
	in.Dynamic = []string{fixture(t, "cors.yml", body+headers)}
	r := audit(t, in)
	if r.Report.GeneratedRoutes != 0 || !strings.Contains(findings(r), "$middleware:security@file") {
		t.Fatal("unsupported CORS policy disappeared from candidate route")
	}
}

func TestRedirectCannotOmitConflictingEntrypointPolicies(t *testing.T) {
	in := compositionInput(t, "canonical", `    canonical:
      redirectScheme: {scheme: https}
`)
	in.Static = []string{fixture(t, "static.yml", `entryPoints:
  web: {address: ':8080', http: {middlewares: [security@file]}}
  other: {address: ':8081', http: {middlewares: [robots@file]}}
`)}
	r := audit(t, in)
	requireMigrationGaps(t, r)
	if !strings.Contains(string(r.Go), "OnListeners") {
		t.Fatal("listener-specific policy omitted")
	}
}
