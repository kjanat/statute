package migrate

import (
	"fmt"
	"strings"
	"testing"

	statute "statute.kjanat.dev"
)

func TestEntrypointRedirectTrustRequiresExplicitSchemeDecision(t *testing.T) {
	for _, forwarded := range []string{"{trustedIPs: [192.0.2.0/24]}", "{insecure: true}", "{insecure: false}"} {
		t.Run(forwarded, func(t *testing.T) {
			in := basicInputs(t, simpleDynamic)
			in.Static = []string{fixture(t, "static.yml", fmt.Sprintf(`entryPoints:
  web:
    address: ':8080'
    forwardedHeaders: %s
    http: {redirections: {entryPoint: {to: ':443', scheme: https}}}
  content:
    address: ':8081'
`, forwarded))}
			r := audit(t, in)
			if forwarded == "{insecure: false}" {
				requireMigrationGaps(t, r)
				return
			}
			requireMigrationGaps(t, r, "entrypoints.$redirect-trust")
		})
	}
}

func TestRedirectSourceStatusAndCaptureValidation(t *testing.T) {
	for _, tc := range []struct {
		policy string
		status int
	}{
		{"redirectScheme: {scheme: https, port: '8443', forcePermanentRedirect: true}", 308},
		{`redirectRegex: {regex: '^https?://[^/]+(.*)$', replacement: 'https://new.example$1', statusCode: 303}`, 303},
		{`redirectRegex: {regex: '^http://api\.example/(.*)$', replacement: 'https://new.example/${1}', permanent: true}`, 301},
	} {
		r := audit(t, policyInputs(t, tc.policy))
		c, err := statute.Resolve(r.Config)
		if err != nil || len(c.Routes) != 2 || c.Routes[0].Redirect.Status != tc.status {
			t.Fatalf("redirect candidate status=%d: %v %+v", tc.status, err, c)
		}
		assertGeneratedParity(t, r)
	}
	for _, policy := range []string{
		"redirectScheme: {scheme: ftp}", "redirectScheme: {scheme: https, port: invalid}",
		"redirectScheme: {scheme: https, permanent: invalid}", "redirectScheme: {scheme: https, forcePermanentRedirect: invalid}",
		`redirectRegex: {regex: '^https?://[^/]+/(.*)', replacement: 'https://new.example/${1}', statusCode: 200}`,
		`redirectRegex: {regex: '^https?://[^/]+/(.+)', replacement: 'https://new.example/${1}'}`,
		`redirectRegex: {regex: '^https?://[^/]+/(.*)', replacement: 'https://new.example/${2}'}`,
		`redirectRegex: {regex: '^https?://[^/]+/(.*)', replacement: 'https://new.example${1}'}`,
		`redirectRegex: {regex: '^http://api\.example/prefix/(.*)', replacement: 'https://new.example/${1}'}`,
		`redirectRegex: {regex: '${PATTERN}', replacement: 'https://new.example/${1}'}`,
	} {
		t.Run(policy, func(t *testing.T) {
			r := audit(t, policyInputs(t, policy))
			if !r.Report.Blocking || r.Report.GeneratedRoutes != 0 || strings.Contains(findings(r), untranslatedReason) {
				t.Fatalf("redirect mismatch not isolated: %s", findings(r))
			}
		})
	}
}

func TestRedirectContinuationAndMissingPolicyCannotDisappear(t *testing.T) {
	for _, refs := range []string{"canonical, robots", "missing, canonical", "pair"} {
		in := compositionInput(t, refs, `    canonical:
      redirectScheme: {scheme: https}
    pair:
      chain: {middlewares: [missing, canonical]}
`)
		r := audit(t, in)
		if !r.Report.Blocking || r.Report.GeneratedRoutes != 0 {
			t.Fatalf("incomplete redirect composition accepted: %s", refs)
		}
	}
}
