package migrate

import (
	"fmt"
	"strings"
	"testing"
	"time"

	statute "statute.kjanat.dev"
)

func dockerServiceInputs(t *testing.T, labels string) Inputs {
	t.Helper()
	static := "entryPoints:\n  web: {address: ':8080', http: {sanitizePath: false}}\nproviders:\n  docker: {exposedByDefault: false}\n"
	compose := `services:
  api:
    labels:
      traefik.enable: 'true'
      traefik.http.routers.api.rule: Host(` + "`api.example`" + `)
      traefik.http.routers.api.service: api
      traefik.http.services.api.loadbalancer.server.port: '8080'
` + labels
	return Inputs{Static: []string{fixture(t, "static.yml", static)}, Compose: []string{fixture(t, "compose.yml", compose)}}
}

func TestDockerBuiltinTransportHasNoInventoryObject(t *testing.T) {
	in := dockerServiceInputs(t, "      traefik.http.services.api.loadbalancer.serverstransport: default@internal\n")
	in.Static = append(in.Static, fixture(t, "transport.yml", "serversTransport:\n  forwardingTimeouts: {dialTimeout: 9s}\n"))
	r := audit(t, in)
	requireForwardedHeaderGap(t, r)
	c, err := statute.Resolve(r.Config)
	if err != nil {
		t.Fatal(err)
	}
	if c.Docker.PoolPolicy["api@traefik"].Transport.DialTimeout != 9*time.Second {
		t.Fatal("explicit built-in reference lost static transport defaults")
	}
	assertGeneratedParity(t, r)
}

func TestHealthURLSemanticsCannotBecomeLiteralPath(t *testing.T) {
	for _, provider := range []string{providerFile, providerDocker} {
		for _, path := range []string{"/ready?deep=true", "/ready%2Fdeep", "/ready#fragment", "/x/../ready", "//other.example/ready", "/ready"} {
			t.Run(provider+path, func(t *testing.T) {
				body := strings.Replace(simpleDynamic, "      loadBalancer:", fmt.Sprintf("      loadBalancer:\n        healthCheck: {path: %q, followRedirects: false}", path), 1)
				in := basicInputs(t, body)
				if provider == providerDocker {
					in = dockerServiceInputs(t, fmt.Sprintf("      traefik.http.services.api.loadbalancer.healthcheck.path: %q\n      traefik.http.services.api.loadbalancer.healthcheck.followredirects: 'false'\n", path))
				}
				r := audit(t, in)
				if path == "/ready" {
					requireForwardedHeaderGap(t, r, "services.$health-degraded")
					return
				}
				if !hasOtherBlockingGaps(r) || !strings.Contains(findings(r), "$health-path") || !strings.Contains(string(r.Go), "panic(") {
					t.Fatal("health URL semantics changed without blocking")
				}
			})
		}
	}
}

func TestRouterTLSOverridesWholeEntrypointObject(t *testing.T) {
	t.Setenv("CF_DNS_API_TOKEN", "migration-binding")
	type selection struct {
		clientAuth, policy bool
		sources            int
		domain             string
	}
	for _, tc := range []struct {
		routerTLS string
		want      selection
	}{
		{"", selection{true, true, 1, "inherited.example"}},
		{"\n      tls: {}", selection{}},
		{"\n      tls: {certResolver: own}", selection{false, false, 1, "api.example"}},
	} {
		in := basicInputs(t, strings.Replace(simpleDynamic, "service: api", "service: api"+tc.routerTLS, 1)+`tls:
  stores:
    default:
      defaultCertificate: {certFile: /not-read/cert.pem, keyFile: /not-read/key.pem}
  options:
    strict:
      minVersion: VersionTLS13
      clientAuth: {clientAuthType: RequireAnyClientCert}
`)
		in.Static = []string{fixture(t, "static.yml", `entryPoints:
  secure:
    address: ':8443'
    http:
      sanitizePath: false
      tls:
        options: strict@file
        certResolver: inherited
        domains: [{main: inherited.example}]
certificatesResolvers:
  inherited:
    acme: {email: ops@example.com, storage: /acme/inherited.json, keyType: EC256, dnsChallenge: {provider: cloudflare}}
  own:
    acme: {email: ops@example.com, storage: /acme/own.json, keyType: EC256, dnsChallenge: {provider: cloudflare}}
`)}
		r := audit(t, in)
		c, err := statute.Resolve(r.Config)
		if err != nil {
			t.Fatal(err)
		}
		l := c.Listeners[0]
		got := selection{clientAuth: l.ClientAuth != nil, policy: l.TLSPolicy != nil, sources: len(l.AutoTLSSources)}
		if len(l.AutoTLSSources) > 0 {
			got.domain = l.AutoTLSSources[0].Domains[0]
		}
		if got != tc.want {
			t.Fatalf("TLS selection for %q: got %+v, want %+v", tc.routerTLS, got, tc.want)
		}
		assertGeneratedParity(t, r)
	}
}

func TestDockerExplicitTLSDoesNotInheritEntrypointOptions(t *testing.T) {
	in := dockerServiceInputs(t, "      traefik.http.routers.api.tls: 'true'\n")
	in.Static = []string{fixture(t, "static.yml", `entryPoints:
  secure:
    address: ':8443'
    http: {sanitizePath: false, tls: {options: strict@file}}
providers:
  docker: {exposedByDefault: false}
`)}
	in.Dynamic = []string{fixture(t, "tls.yml", `tls:
  stores:
    default:
      defaultCertificate: {certFile: /not-read/cert.pem, keyFile: /not-read/key.pem}
  options:
    strict:
      minVersion: VersionTLS13
      clientAuth: {clientAuthType: RequireAnyClientCert}
`)}
	r := audit(t, in)
	c, err := statute.Resolve(r.Config)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listeners[0].ClientAuth != nil || c.Listeners[0].TLSPolicy != nil {
		t.Fatal("Docker router with explicit TLS inherited entrypoint options")
	}
	assertGeneratedParity(t, r)
}

func TestACMEDefaultKeyTypeRequiresMigrationDecision(t *testing.T) {
	for _, keyType := range []string{"", "RSA4096", "EC384", "EC256"} {
		in := basicInputs(t, strings.Replace(simpleDynamic, "service: api", "service: api\n      tls: {certResolver: cloud}", 1))
		setting := ""
		if keyType != "" {
			setting = "      keyType: " + keyType + "\n"
		}
		in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  secure: {address: ':8443', http: {sanitizePath: false}}\ncertificatesResolvers:\n  cloud:\n    acme:\n      email: ops@example.com\n      storage: /acme/state.json\n      dnsChallenge: {provider: cloudflare}\n"+setting)}
		r := audit(t, in)
		if keyType == "EC256" {
			requireForwardedHeaderGap(t, r)
			continue
		}
		if !hasOtherBlockingGaps(r) || !strings.Contains(findings(r), "keyType") || !strings.Contains(string(r.Go), "panic(") {
			t.Fatalf("incompatible ACME key type accepted: %q", keyType)
		}
	}
}
