package migrate

import (
	"fmt"
	"strings"
	"testing"
	"time"

	statute "statute.kjanat.dev"
)

func dnsPolicyInputs(t *testing.T, challenge string) Inputs {
	t.Helper()
	in := basicInputs(t, strings.Replace(simpleDynamic, "service: api", "service: api\n      tls: {certResolver: cloud}", 1))
	in.Static = []string{fixture(t, "static.yml", `entryPoints:
  secure: {address: ':8443'}
certificatesResolvers:
  cloud:
    acme:
      email: ops@example.test
      storage: /not-read/acme.json
      keyType: EC256
      dnsChallenge: {provider: cloudflare, `+challenge+`}
`)}
	return in
}

func TestDNSPropagationCandidatePreservesResolverOrderAndDelay(t *testing.T) {
	t.Setenv("CF_DNS_API_TOKEN", "migration-binding")
	for _, delay := range []string{"propagation: {delayBeforeChecks: 3s, disableChecks: false, disableANSChecks: false, requireAllRNS: true}", "delayBeforeCheck: 3s, disablePropagationCheck: false"} {
		r := audit(t, dnsPolicyInputs(t, "resolvers: [1.1.1.1, '2001:db8::1', 'resolver.example:5353', resolver.example], "+delay))
		requireMigrationGaps(t, r, "certificatesresolvers.$dns-propagation")
		c, err := statute.Resolve(r.Config)
		if err != nil {
			t.Fatal(err)
		}
		p := c.Listeners[0].AutoTLSSources[0].DNS01.Propagation
		if p == nil || p.Delay != 3*time.Second || strings.Join(p.Resolvers, ",") != "1.1.1.1:53,[2001:db8::1]:53,resolver.example:5353,resolver.example:53" {
			t.Fatalf("propagation candidate=%+v", p)
		}
		assertGeneratedParity(t, r)
	}
}

func TestInvalidDNSPolicyCannotBecomeAcceptedMigration(t *testing.T) {
	for _, challenge := range []string{
		"resolvers: false", "resolvers: ['https://resolver.example/dns-query']", "resolvers: ['broken:53:extra']",
		"propagation: {delayBeforeChecks: -1s}", "propagation: {delayBeforeChecks: 11m}", "propagation: {delayBeforeChecks: soon}",
		"delayBeforeCheck: 1s, propagation: {delayBeforeChecks: 2s}", "propagation: {disableChecks: invalid}",
	} {
		t.Run(challenge, func(t *testing.T) {
			r := audit(t, dnsPolicyInputs(t, challenge))
			if !r.Report.Blocking || !strings.Contains(string(r.Go), "migration incomplete") {
				t.Fatal("invalid DNS policy accepted")
			}
			if strings.Contains(findings(r), untranslatedReason) {
				t.Fatal("DNS validation did not explain its rejection")
			}
		})
	}
	r := audit(t, dnsPolicyInputs(t, "propagation: {delayBeforeChecks: 0s}"))
	requireMigrationGaps(t, r, "certificatesresolvers.$dns-propagation")
}

func TestIngressSourcePolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		policy string
		gap    bool
	}{
		{"forwardedHeaders: {trustedIPs: [192.0.2.1, '2001:db8::1']}", false},
		{"forwardedHeaders: {trustedIPs: false}", true},
		{"forwardedHeaders: {trustedIPs: [not-an-address]}", true},
		{"forwardedHeaders: {insecure: invalid}", true},
		{"forwardedHeaders: {connection: []}", false},
		{"forwardedHeaders: {connection: [X-Auth]}", true},
		{"forwardedHeaders: {notAppendXForwardedFor: false}", false},
		{"forwardedHeaders: {notAppendXForwardedFor: true}", true},
		{"http: {aliasHeadersStrategy: delete}", false},
		{"http: {aliasHeadersStrategy: reject, underscoreHeadersStrategy: delete}", false},
		{"http: {underscoreHeadersStrategy: delete}", false},
		{"http: {underscoreHeadersStrategy: reject}", false},
		{"http: {aliasHeadersStrategy: invalid}", true},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			in := basicInputs(t, simpleDynamic)
			in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  web:\n    address: ':8080'\n    "+tc.policy+"\n")}
			r := audit(t, in)
			if r.Report.Blocking != tc.gap {
				t.Fatalf("blocking=%t want=%t: %s", r.Report.Blocking, tc.gap, findings(r))
			}
			if strings.Contains(findings(r), untranslatedReason) {
				t.Fatal("ingress validation left an opaque field")
			}
		})
	}
}

func TestHeaderPolicyValidationAndDisabledFeatures(t *testing.T) {
	for _, policy := range []string{
		"headers: {contentTypeNosniff: invalid}", "headers: {stsSeconds: -1}", "headers: {stsSeconds: 3, stsPreload: invalid}",
		"headers: {unknownConditionalFeature: true}",
	} {
		r := audit(t, policyInputs(t, policy))
		if !r.Report.Blocking || strings.Contains(findings(r), untranslatedReason) || r.Report.GeneratedRoutes != 0 {
			t.Fatalf("invalid header policy not isolated: %s", findings(r))
		}
	}
	r := audit(t, policyInputs(t, `headers:
  stsSeconds: 5
  forceSTSHeader: true
  stsIncludeSubdomains: false
  stsPreload: false
  referrerPolicy: ''
  contentSecurityPolicy: ''
  permissionsPolicy: ''
  allowedHosts: []
  sslProxyHeaders: {}
  sslHost: ''
  accessControlAllowCredentials: false
  accessControlMaxAge: 0
`))
	requireMigrationGaps(t, r)
	c, err := statute.Resolve(r.Config)
	if err != nil || len(c.Routes[0].Middleware) != 1 || c.Routes[0].Middleware[0].HeaderHTTPSOnly {
		t.Fatal("forced HSTS lost HTTP emission")
	}
}

func TestRateSourceAdmissionAndKeyDifferencesStayExplicit(t *testing.T) {
	for _, policy := range []string{
		"rateLimit: {average: 0}", "rateLimit: {average: 1, period: -1s}", "rateLimit: {average: 1, period: invalid}",
		"rateLimit: {average: 1, sourceCriterion: {requestHeaderName: X-Client}}",
		"rateLimit: {average: 1, distributed: true}",
	} {
		r := audit(t, policyInputs(t, policy))
		if !r.Report.Blocking || strings.Contains(findings(r), untranslatedReason) {
			t.Fatalf("rate admission not explained: %s", findings(r))
		}
	}
	for _, criterion := range []string{"{}", "{requestHost: false}", "{requestHost: true}"} {
		r := audit(t, policyInputs(t, fmt.Sprintf("rateLimit: {average: 1, burst: 2, period: 0s, distributed: false, sourceCriterion: %s}", criterion)))
		requireMigrationGaps(t, r, "middlewares.$rate-capacity", "middlewares.$rate-delay", "routers.$middleware:candidate@file")
		if !strings.Contains(string(r.Go), `statute.RateLimit("1/s")`) {
			t.Fatal("source zero period lost its one-second default")
		}
	}
}

func TestPassiveHealthValidationKeepsInvalidPolicyBlocked(t *testing.T) {
	for _, policy := range []string{"{}", "{failureWindow: -1s}", "{failureWindow: invalid}", "{maxFailedAttempts: 0}", "{maxFailedAttempts: invalid}"} {
		body := strings.Replace(simpleDynamic, "      loadBalancer:", "      loadBalancer:\n        passiveHealthCheck: "+policy, 1)
		r := audit(t, basicInputs(t, body))
		if !r.Report.Blocking || !strings.Contains(findings(r), "$health-passive") {
			t.Fatal("passive policy lost its explicit behavior decision")
		}
		if policy == "{}" && !strings.Contains(string(r.Go), "MaxFailures: 1") {
			t.Fatal("source passive defaults missing")
		}
	}
}
