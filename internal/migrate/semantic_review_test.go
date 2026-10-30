package migrate

import (
	"strings"
	"testing"

	statute "statute.kjanat.dev"
)

func TestHostlessRouterDoesNotAssignNamedSNIPolicy(t *testing.T) {
	in := tlsPolicyInputs(t, "minVersion: VersionTLS13\n      clientAuth: {clientAuthType: RequireAnyClientCert}")
	body := strings.Replace(simpleDynamic, "Host(`api.example`) && Path(`/health`)", "Path(`/health`)", 1)
	body = strings.Replace(body, "service: api", "service: api\n      tls: {options: selected}", 1)
	body += `tls:
  stores:
    default:
      defaultCertificate: {certFile: /not-read/cert.pem, keyFile: /not-read/key.pem}
  options:
    selected:
      minVersion: VersionTLS13
      clientAuth: {clientAuthType: RequireAnyClientCert}
`
	in.Dynamic = []string{fixture(t, "hostless.yml", body)}
	r := audit(t, in)
	requireMigrationGaps(t, r)
	c, err := statute.Resolve(r.Config)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listeners[0].ClientAuth != nil || len(c.Listeners[0].ClientAuthPolicies) > 0 || len(c.Listeners[0].TLSPolicies) > 0 {
		t.Fatal("hostless router assigned a named SNI policy")
	}
}

func TestAllUnhealthyBehaviorRequiresReview(t *testing.T) {
	for _, provider := range []string{providerFile, providerDocker} {
		body := strings.Replace(simpleDynamic, "      loadBalancer:", "      loadBalancer:\n        healthCheck: {path: /ready, followRedirects: false}", 1)
		in := basicInputs(t, body)
		if provider == providerDocker {
			in = dockerServiceInputs(t, "      traefik.http.services.api.loadbalancer.healthcheck.path: /ready\n      traefik.http.services.api.loadbalancer.healthcheck.followredirects: 'false'\n")
		}
		r := audit(t, in)
		requireMigrationGaps(t, r)
		c, err := statute.Resolve(r.Config)
		if err != nil {
			t.Fatal(err)
		}
		var path string
		if provider == providerDocker {
			path = c.Docker.PoolPolicy["api@traefik"].HealthCheck.Path
		} else {
			path = c.Upstreams["api@file"].HealthCheck.Path
		}
		if path != "/ready" {
			t.Fatal("guarded candidate lost its health configuration")
		}
		assertGeneratedParity(t, r)
	}
}

func TestForwardedHeaderDefaultsGenerateExplicitIngress(t *testing.T) {
	for _, forwarded := range []string{"", "    forwardedHeaders: {}\n", "    forwardedHeaders: {insecure: false}\n", "    forwardedHeaders: {insecure: true}\n", "    forwardedHeaders: {trustedIPs: [192.0.2.0/24]}\n"} {
		in := basicInputs(t, simpleDynamic)
		in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  web:\n    address: ':8080'\n    http: {sanitizePath: false}\n"+forwarded)}
		r := audit(t, in)
		requireMigrationGaps(t, r)
		c, err := statute.Resolve(r.Config)
		if err != nil || c.Listeners[0].Ingress == nil || !c.Listeners[0].Ingress.ForwardedHeaders {
			t.Fatalf("ingress policy missing: %v", err)
		}
		assertGeneratedParity(t, r)
	}
}
