package migrate

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	statute "statute.kjanat.dev"
)

func findings(r *Result) string { var b bytes.Buffer; _ = r.Report.WriteHuman(&b); return b.String() }

func TestTLSClientAuthenticationAndCipherPolicy(t *testing.T) {
	in := tlsPolicyInputs(t, `minVersion: VersionTLS12
      maxVersion: VersionTLS13
      cipherSuites: [TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256]
      sniStrict: false
      clientAuth:
        clientAuthType: RequireAndVerifyClientCert
        caFiles: [/not-read/client-ca.pem]`)
	r := audit(t, in)
	requireMigrationGaps(t, r)
	assertGeneratedParity(t, r)
	c, err := statute.Resolve(r.Config)
	if err != nil {
		t.Fatal(err)
	}
	listener := c.Listeners[0]
	if len(listener.ClientAuthPolicies) != 1 || len(listener.ClientAuthPolicies[0].Policy.CAFiles) != 1 {
		t.Fatal("client CA policy was dropped")
	}
	if len(listener.TLSPolicies) != 1 || len(listener.TLSPolicies[0].Policy.CipherSuites) != 1 {
		t.Fatal("cipher policy was dropped")
	}
	if !strings.Contains(string(r.Go), "statute.RequireAndVerifyClientCert") {
		t.Fatal("client authentication mode was lost")
	}
}

func tlsPolicyInputs(t *testing.T, policy string) Inputs {
	t.Helper()
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      tls: {options: selected}", 1)
	body += "tls:\n  stores:\n    default:\n      defaultCertificate: {certFile: /not-read/cert.pem, keyFile: /not-read/key.pem}\n  options:\n    selected:\n      " + policy + "\n"
	in := basicInputs(t, body)
	in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  secure: {address: ':8443', http: {sanitizePath: false}}\n")}
	return in
}

func TestTLSInvalidPolicyCannotBecomeDefaults(t *testing.T) {
	for _, policy := range []string{
		"cipherSuites: [UNRECOGNIZED_CIPHER]",
		"cipherSuites: [TLS_RSA_WITH_3DES_EDE_CBC_SHA]",
		"minVersion: VersionTLS10",
		"clientAuth: {clientAuthType: RequireAndVerifyClientCert}",
		"clientAuth: {clientAuthType: RequireAndVerifyClientCert, caFiles: {bad: shape}}",
	} {
		r := audit(t, tlsPolicyInputs(t, policy))
		if !hasBlockingGaps(r) {
			t.Fatalf("accepted invalid security policy: %s", policy)
		}
	}
}

func TestTLSNoClientCertificateAndEmptyPolicy(t *testing.T) {
	for _, policy := range []string{"clientAuth: {clientAuthType: NoClientCert}", "{}"} {
		r := audit(t, tlsPolicyInputs(t, policy))
		requireMigrationGaps(t, r)
		c, err := statute.Resolve(r.Config)
		if err != nil {
			t.Fatal(err)
		}
		if c.Listeners[0].ClientAuth != nil {
			t.Fatal("invented client authentication")
		}
	}
}

func TestUpstreamTLSFilesProbeAndFlushRemainPoolOwned(t *testing.T) {
	body := strings.Replace(simpleDynamic, "http://127.0.0.1:9000", "https://127.0.0.1:9000", 1)
	body = strings.Replace(body, "      loadBalancer:", `      loadBalancer:
        serversTransport: private
        responseForwarding: {flushInterval: 50ms}
        healthCheck:
          path: /ready
          hostname: health.example.com
          mode: http
          method: GET
          followRedirects: false`, 1)
	body += `  serversTransports:
    private:
      serverName: origin.example.com
      rootCAs: [/not-read/root-ca.pem]
      certificates: [{certFile: /not-read/client.pem, keyFile: /not-read/client.key}]
      disableHTTP2: false
      insecureSkipVerify: false
      forwardingTimeouts: {dialTimeout: 4s, responseHeaderTimeout: 7s, idleConnTimeout: 45s}
`
	r := audit(t, basicInputs(t, body))
	requireMigrationGaps(t, r)
	assertGeneratedParity(t, r)
	c, err := statute.Resolve(r.Config)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Upstreams["api@file"]
	if p.Transport.FlushInterval != 50*time.Millisecond || p.Transport.ResponseHeaderTimeout != 7*time.Second {
		t.Fatal("transport timing changed")
	}
	if p.Transport.ClientCertificate == nil || len(p.Transport.RootCAFiles) != 1 {
		t.Fatal("upstream TLS identity or roots lost")
	}
	if len(p.HealthCheck.Statuses) != 0 || p.HealthCheck.Redirects != "none" || p.HealthCheck.Host != "health.example.com" {
		t.Fatal("probe redirects or Host policy changed")
	}
}

func TestEntrypointRedirectWithHTTP01AndDeclaredDomains(t *testing.T) {
	body := strings.Replace(simpleDynamic, "service: api", `service: api
      entryPoints: [secure]
      tls:
        certResolver: lets
        domains: [{main: api.example.com, sans: [cdn.example.com]}]`, 1)
	in := basicInputs(t, body)
	in.Static = []string{fixture(t, "static.yml", `entryPoints:
  web:
    address: ':80'
    http:
      sanitizePath: false
      redirections:
        entryPoint: {to: secure, scheme: https, permanent: true, priority: 42}
  secure: {address: ':443', http: {sanitizePath: false}}
certificatesResolvers:
  lets:
    acme:
      email: ops@example.com
      caServer: https://ca.example.com/directory
      storage: /var/lib/traefik/acme.json
      keyType: EC256
      httpChallenge: {entryPoint: web}
`)}
	r := audit(t, in)
	requireMigrationGaps(t, r)
	assertGeneratedParity(t, r)
	c, err := statute.Resolve(r.Config)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Routes) < 1 || !strings.Contains(string(r.Go), "statute.RedirectScheme(") {
		t.Fatal("conditional listener redirect lost")
	}
	if len(c.Listeners[0].AutoTLSSources[0].Domains) != 2 {
		t.Fatal("explicit ACME domain coverage lost")
	}
}

func TestRedirectTargetAndChallengeGapsBlock(t *testing.T) {
	for _, target := range []string{"secure", ":443", "missing"} {
		in := basicInputs(t, simpleDynamic)
		in.Static = []string{fixture(t, "static.yml", fmt.Sprintf("entryPoints:\n  web: {address: ':8080', http: {sanitizePath: false, redirections: {entryPoint: {to: %q}}}}\n  secure: {address: ':8443', http: {sanitizePath: false}}\n", target))}
		r := audit(t, in)
		if target != "missing" {
			requireMigrationGaps(t, r)
			continue
		}
		if !hasBlockingGaps(r) {
			t.Fatal("redirect gap was accepted or candidate lost")
		}
	}
}

func TestDynamicDirectoryCoverageAndSymlinkRefusal(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "routes.yml"), []byte(simpleDynamic), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("not configuration"), 0o600); err != nil {
		t.Fatal(err)
	}
	static := fixture(t, "static.yml", fmt.Sprintf("entryPoints:\n  web: {address: ':8080', http: {sanitizePath: false}}\nproviders:\n  file: {directory: %q}\n", dir))
	in := Inputs{Static: []string{static}, Dynamic: []string{dir}}
	r := audit(t, in)
	requireMigrationGaps(t, r)
	if r.Report.Routes != 1 {
		t.Fatal("nested source omitted")
	}
	if err := os.Symlink(nested, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := Audit(in); err == nil {
		t.Fatal("directory symlink accepted")
	}
}

func TestDockerProviderPreservesEndpointNetworkAndEntrypointPolicy(t *testing.T) {
	in := basicInputs(t, simpleDynamic+`  middlewares:
    allow:
      ipAllowList: {sourceRange: [192.0.2.0/24]}
`)
	in.Static = []string{fixture(t, "static.yml", `entryPoints:
  web:
    address: ':8080'
    http: {sanitizePath: false, middlewares: [allow@file]}
providers:
  docker:
    endpoint: unix:///not-contacted/docker.sock
    network: private
    exposedByDefault: false
    watch: true
`)}
	r := audit(t, in)
	requireMigrationGaps(t, r)
	assertGeneratedParity(t, r)
	c, err := statute.Resolve(r.Config)
	if err != nil {
		t.Fatal(err)
	}
	if c.Docker.Endpoint != "unix:///not-contacted/docker.sock" || c.Docker.Network != "private" || len(c.Docker.DefaultMiddleware) != 1 {
		t.Fatal("Docker discovery configuration or default middleware lost")
	}
}

func TestEntrypointPolicyCannotDisappearOrLeakAcrossListeners(t *testing.T) {
	for _, policy := range []string{"[missing@file]", "[allow]", "{bad: shape}"} {
		in := basicInputs(t, simpleDynamic)
		in.Static = []string{fixture(t, "static.yml", fmt.Sprintf("entryPoints:\n  web: {address: ':8080', http: {sanitizePath: false, middlewares: %s}}\n", policy))}
		r := audit(t, in)
		if !hasBlockingGaps(r) || !strings.Contains(string(r.Go), "panic(") {
			t.Fatal("unresolved entrypoint policy permits serving")
		}
	}
	in := basicInputs(t, simpleDynamic+`  middlewares:
    allow:
      ipAllowList: {sourceRange: [192.0.2.0/24]}
`)
	in.Static = []string{fixture(t, "static.yml", `entryPoints:
  web:
    address: ':8080'
    http: {sanitizePath: false, middlewares: [allow@file]}
    transport: {respondingTimeouts: {readTimeout: 5s}}
  other: {address: ':8081', http: {sanitizePath: false}}
`)}
	r := audit(t, in)
	requireMigrationGaps(t, r, "entrypoints.transport.respondingTimeouts.readTimeout")
	if !strings.Contains(string(r.Go), "OnListeners") {
		t.Fatal("listener-specific policy missing")
	}
	if r.Config.Defaults.ReadTimeout == "5s" {
		t.Fatal("listener timeout leaked into global defaults")
	}
}

func TestBackendURLFeaturesCannotBeSilentlyRemoved(t *testing.T) {
	for _, endpoint := range []string{"http://user:password@example.com", "http://example.com/base", "http://example.com?token=private", "http://example.com#fragment", "ftp://example.com", "http://%invalid"} {
		body := strings.Replace(simpleDynamic, "http://127.0.0.1:9000", endpoint, 1)
		r := audit(t, basicInputs(t, body))
		if !hasBlockingGaps(r) || len(r.Config.Upstreams) != 0 || !strings.Contains(string(r.Go), "panic(") {
			t.Fatalf("unsafe backend conversion accepted: %s", endpoint)
		}
	}
}
