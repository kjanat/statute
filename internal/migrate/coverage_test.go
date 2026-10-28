package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	statute "statute.kjanat.dev"
	"statute.kjanat.dev/resolved"
)

func requireUnblocked(t *testing.T, r *Result) {
	t.Helper()
	if r.Report.Blocking {
		var b bytes.Buffer
		_ = r.Report.WriteHuman(&b)
		t.Fatal(b.String())
	}
}

func TestMixedProviderTLSHealthAndMiddleware(t *testing.T) {
	dynamic := fixture(t, "dynamic.yml", `http:
  routers:
    api:
      entryPoints: [secure]
      rule: Host(`+"`api.example.com`"+`)
      service: api
      tls:
        certResolver: cloud
        options: modern
      middlewares: [headers, prefix, allow]
  services:
    api:
      loadBalancer:
        passHostHeader: false
        serversTransport: internal
        servers: [{url: 'https://127.0.0.1:9443'}]
        healthCheck:
          path: /health
          interval: 8s
          timeout: 3s
          followRedirects: false
          status: 204
  serversTransports:
    internal:
      serverName: backend.example.com
      maxIdleConnsPerHost: 20
      forwardingTimeouts: {dialTimeout: 4s}
  middlewares:
    headers:
      headers:
        customResponseHeaders: {Server: ''}
    prefix:
      addPrefix: {prefix: /api}
    allow:
      ipAllowList: {sourceRange: [127.0.0.0/8]}
tls:
  options:
    modern:
      minVersion: VersionTLS12
      maxVersion: VersionTLS13
`)
	static := fixture(t, "static.yml", fmt.Sprintf(`entryPoints:
  secure: {address: ':8443'}
providers:
  file: {filename: %q, watch: true}
  docker: {exposedByDefault: false, watch: true}
certificatesResolvers:
  cloud:
    acme:
      email: ops@example.com
      storage: /var/lib/traefik/acme.json
      dnsChallenge: {provider: cloudflare}
`, dynamic))
	compose := fixture(t, "compose.yml", `services:
  worker:
    labels:
      traefik.enable: 'true'
      traefik.http.routers.worker.rule: Host(`+"`worker.example.com`"+`)
      traefik.http.routers.worker.entrypoints: secure
      traefik.http.routers.worker.tls.certresolver: cloud
      traefik.http.routers.worker.tls.options: modern@file
      traefik.http.routers.worker.service: worker
      traefik.http.routers.worker.middlewares: headers@file
      traefik.http.services.worker.loadbalancer.server.port: '9000'
      traefik.http.services.worker.loadbalancer.passhostheader: 'false'
      traefik.http.services.worker.loadbalancer.serverstransport: internal@file
      traefik.http.services.worker.loadbalancer.healthcheck.path: /ready
      traefik.http.services.worker.loadbalancer.healthcheck.followredirects: 'false'
      traefik.http.services.worker.loadbalancer.healthcheck.status: '200'
`)
	r := audit(t, Inputs{Static: []string{static}, Dynamic: []string{dynamic}, Compose: []string{compose}})
	requireUnblocked(t, r)
	t.Setenv("CF_DNS_API_TOKEN", "migration-binding")
	assertGeneratedParity(t, r)
	for _, want := range []string{"PoolPolicy", "HealthCheck", "TargetHost", "ServerName", "CloudflareDNS01", "TLSPolicy", "AddPrefix", "Middleware"} {
		if !strings.Contains(string(r.Go), want) {
			t.Errorf("missing %s", want)
		}
	}
	resolved, err := statute.Resolve(r.Config)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Upstreams["api@file"].HealthCheck.Healthy != 1 {
		t.Fatal("source health transition threshold lost")
	}
}

func TestAuthenticationFileBindingIsDeferred(t *testing.T) {
	users := fixture(t, "users.htpasswd", "migration-placeholder:"+validationHash+"\n")
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [auth]", 1)
	body += fmt.Sprintf("  middlewares:\n    auth:\n      basicAuth:\n        usersFile: %q\n", users)
	in := basicInputs(t, body+"tls:\n  stores: {default: {defaultCertificate: {certFile: /cert.pem, keyFile: /key.pem}}}\n")
	in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  web: {address: ':8443', http: {tls: {}}}\n")}
	r := audit(t, in)
	requireUnblocked(t, r)
	assertGeneratedParity(t, r)
	if strings.Contains(string(r.Go), validationHash) {
		t.Fatal("credential copied into generated source")
	}
	deferred := false
	for _, c := range r.Report.Checks {
		deferred = deferred || c.Deferred
	}
	if !deferred {
		t.Fatal("missing deferred validation")
	}
}

func TestListenerIsolationCannotBecomeSharedPolicy(t *testing.T) {
	in := basicInputs(t, strings.Replace(simpleDynamic, "service: api", "service: api\n      entryPoints: [private]", 1))
	in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  public: {address: ':8080'}\n  private: {address: ':8081'}\n")}
	r := audit(t, in)
	if !r.Report.Blocking || !strings.Contains(string(r.Go), "migration incomplete") {
		t.Fatal("listener restriction silently dropped")
	}
}

func TestHeaderBindingsDoNotCopyCredentials(t *testing.T) {
	for _, value := range []string{"${API_SECRET}", "literal-private-token"} {
		body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [headers]", 1)
		body += "  middlewares:\n    headers:\n      headers:\n        customRequestHeaders:\n          X-Token: '" + value + "'\n"
		r := audit(t, basicInputs(t, body))
		report, _ := json.Marshal(r.Report)
		if strings.Contains(string(r.Go), "literal-private-token") || strings.Contains(string(report), "literal-private-token") {
			t.Fatal("secret leak")
		}
		if value == "${API_SECRET}" {
			requireUnblocked(t, r)
			t.Setenv("API_SECRET", "migration-binding")
			assertGeneratedParity(t, r)
		} else if !r.Report.Blocking {
			t.Fatal("inline credential relocation must block")
		}
	}
}

func TestHoistedHeaderAfterAuthenticationBlocks(t *testing.T) {
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [auth, headers]", 1)
	body += "  middlewares:\n    auth:\n      basicAuth: {usersFile: /not/read/by/audit}\n    headers:\n      headers:\n        customResponseHeaders: {Server: ''}\n"
	r := audit(t, basicInputs(t, body))
	if !r.Report.Blocking {
		t.Fatal("header middleware moved before auth without a finding")
	}
}

func TestEntrypointMiddlewareAndSharedDefaults(t *testing.T) {
	body := simpleDynamic + "  middlewares:\n    edge:\n      headers:\n        customRequestHeaders: {X-Untrusted: ''}\n"
	in := basicInputs(t, body)
	in.Static = []string{fixture(t, "static.yml", `entryPoints:
  web:
    address: ':8080'
    http:
      middlewares: [edge@file]
      maxHeaderBytes: 8192
    transport:
      respondingTimeouts: {readTimeout: 15s, writeTimeout: 20s, idleTimeout: 90s}
serversTransport:
  forwardingTimeouts: {dialTimeout: 9s}
`)}
	result := audit(t, in)
	requireUnblocked(t, result)
	assertGeneratedParity(t, result)
	c, err := statute.Resolve(result.Config)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Routes[0].Middleware) != 1 || c.Defaults.MaxHeaderBytes != 8192 || c.Upstreams["api@file"].Transport.MaxIdleConnsPerHost != 200 {
		t.Fatal("entrypoint policy or source defaults lost")
	}
}

func TestGuardedMiddlewareCandidatesCompile(t *testing.T) {
	for _, definition := range []string{
		"buffering: {maxRequestBodyBytes: 1048576, memRequestBodyBytes: 65536, maxResponseBodyBytes: 1048576}",
		"retry: {attempts: 3}",
		"replacePath: {path: /new}",
		"stripPrefix: {prefixes: [/old]}",
		"compress: {}",
	} {
		body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [candidate]", 1)
		body += "  middlewares:\n    candidate:\n      " + definition + "\n"
		r := audit(t, basicInputs(t, body))
		if !r.Report.Blocking || !strings.Contains(string(r.Go), "func migrationCandidate") {
			t.Fatalf("missing guarded native candidate for %s", definition)
		}
		compileGenerated(t, r)
	}
}

func TestIncompleteFileProviderInventoryBlocks(t *testing.T) {
	in := basicInputs(t, simpleDynamic)
	in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  web: {address: ':8080'}\nproviders:\n  file: {directory: /not-supplied-to-audit}\n")}
	r := audit(t, in)
	if !r.Report.Blocking {
		t.Fatal("unsupplied provider inputs accepted")
	}
}

func TestMissingNamedTLSResolverBlocks(t *testing.T) {
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      tls: {certResolver: missing}", 1)
	r := audit(t, basicInputs(t, body))
	if !r.Report.Blocking {
		t.Fatal("missing certificate resolver accepted")
	}
}

func TestSourceTopologyRejectsSameCountWrongAction(t *testing.T) {
	result := audit(t, basicInputs(t, simpleDynamic))
	c, err := statute.Resolve(result.Config)
	if err != nil {
		t.Fatal(err)
	}
	g := &generator{r: result, objects: map[string]*Object{}, chains: map[string]middleware{}, entrypoints: map[string]*Object{}}
	g.indexObjects()
	c.Routes[0].Upstream = nil
	c.Routes[0].Redirect = &resolved.Redirect{Target: "https://wrong.example", Status: 302}
	g.checkSourceTopology(c)
	check := result.Report.Checks[len(result.Report.Checks)-1]
	if !check.Blocking {
		t.Fatal("same route count hid an incorrect route action")
	}
}

func TestInlineAuthenticationRelocationRemainsBlocking(t *testing.T) {
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [auth]", 1)
	body += "  middlewares:\n    auth:\n      basicAuth:\n        users: ['alice:" + validationHash + "', 'bob:${AUTH_HASH}']\n"
	r := audit(t, basicInputs(t, body))
	if !r.Report.Blocking || strings.Contains(string(r.Go), validationHash) {
		t.Fatal("inline credential was copied or its relocation was accepted")
	}
	compileGenerated(t, r)
}

func TestACMEDirectoryCredentialsAreWithheld(t *testing.T) {
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      tls: {certResolver: cloud}", 1)
	in := basicInputs(t, body)
	in.Static = []string{fixture(t, "static.yml", `entryPoints:
  secure: {address: ':8443'}
certificatesResolvers:
  cloud:
    acme:
      caServer: https://secret-user:secret-password@ca.example/directory
      storage: /var/lib/traefik/acme.json
      dnsChallenge: {provider: cloudflare}
`)}
	r := audit(t, in)
	report, _ := json.Marshal(r.Report)
	if !r.Report.Blocking || strings.Contains(string(r.Go), "secret-password") || strings.Contains(string(report), "secret-password") {
		t.Fatal("ACME directory credential was accepted or exposed")
	}
}
