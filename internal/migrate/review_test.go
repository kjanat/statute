package migrate

import (
	"fmt"
	"strings"
	"testing"
)

func TestResponseHeaderOrderIncludesEntrypointDefaults(t *testing.T) {
	for _, provider := range []string{providerFile, providerDocker} {
		body := "http:\n"
		if provider == providerFile {
			body = strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [set@file]", 1)
		}
		body += `  middlewares:
    remove:
      headers: {customResponseHeaders: {Server: ''}}
    set:
      headers: {customResponseHeaders: {server: '${SERVER_HEADER}'}}
`
		in := basicInputs(t, body)
		in.Static = []string{fixture(t, "static.yml", `entryPoints:
  web:
    address: ':8080'
    http: {sanitizePath: false, middlewares: [remove@file]}
providers:
  docker: {exposedByDefault: false}
`)}
		if provider == providerDocker {
			in.Compose = []string{fixture(t, "compose.yml", `services:
  api:
    labels:
      traefik.enable: 'true'
      traefik.http.routers.api.rule: Host(`+"`api.example`"+`)
      traefik.http.routers.api.service: api
      traefik.http.routers.api.middlewares: set@file
      traefik.http.services.api.loadbalancer.server.port: '8080'
`)}
		}
		r := audit(t, in)
		requireMigrationGaps(t, r)
		t.Setenv("SERVER_HEADER", "migration-binding")
		assertGeneratedParity(t, r)
	}
}

func TestIndependentHeaderOperationsRemainTranslatable(t *testing.T) {
	body := strings.Replace(simpleDynamic, "service: api", "service: api\n      middlewares: [first, second, first]", 1)
	body += `  middlewares:
    first:
      headers:
        customResponseHeaders: {Server: ''}
        customRequestHeaders: {X-Input: '${FIRST_HEADER}'}
    second:
      headers:
        customResponseHeaders: {X-Powered-By: ''}
        customRequestHeaders: {X-Input: '${SECOND_HEADER}'}
`
	r := audit(t, basicInputs(t, body))
	requireMigrationGaps(t, r)
	t.Setenv("FIRST_HEADER", "migration-binding")
	t.Setenv("SECOND_HEADER", "migration-binding")
	assertGeneratedParity(t, r)
}

func TestEntrypointPathSanitizationRequiresExplicitDecision(t *testing.T) {
	for _, setting := range []string{"", "true", "false", "null", "'false'"} {
		t.Run(setting, func(t *testing.T) {
			static := "entryPoints:\n  web:\n    address: ':8080'\n"
			if setting != "" {
				static += "    http: {sanitizePath: " + setting + "}\n"
			}
			in := Inputs{Static: []string{fixture(t, "static.yml", static)}, Dynamic: []string{fixture(t, "dynamic.yml", simpleDynamic)}}
			r := audit(t, in)
			if setting == "false" || setting == "true" || setting == "" {
				requireMigrationGaps(t, r)
				return
			}
			if !hasBlockingGaps(r) || !strings.Contains(findings(r), "sanitizePath") || !strings.Contains(string(r.Go), "panic(") {
				t.Fatal("path sanitization difference permits serving")
			}
		})
	}
}

func TestTLSResourcesAcrossFilesRetainIdentityAndProvenance(t *testing.T) {
	in := tlsPolicyInputs(t, "minVersion: VersionTLS13")
	combined := audit(t, in)
	routes := fixture(t, "routes.yml", strings.Replace(simpleDynamic, "service: api", "service: api\n      tls: {options: selected}", 1))
	store := fixture(t, "store.yml", "tls:\n  stores:\n    default:\n      defaultCertificate: {certFile: /not-read/cert.pem, keyFile: /not-read/key.pem}\n")
	policy := fixture(t, "policy.yml", "tls:\n  options:\n    selected: {minVersion: VersionTLS13}\n")
	in.Dynamic = []string{routes, store, policy}
	r := audit(t, in)
	requireMigrationGaps(t, r)
	if string(combined.Go) != string(r.Go) {
		t.Fatal("splitting TLS resources across files changed generated configuration")
	}
	for _, want := range []struct{ name, source, path string }{
		{"stores.default", store, "stores.default.defaultCertificate.certFile"},
		{"options.selected", policy, "options.selected.minVersion"},
	} {
		found := false
		for _, o := range r.Report.Objects {
			if o.Kind == kindTLS && o.Name == want.name && o.Source == want.source {
				found = o.Generated && o.value(want.path) != nil
			}
		}
		if !found {
			t.Fatalf("TLS resource provenance lost: %s", want.name)
		}
	}
	assertGeneratedParity(t, r)
}

func TestDuplicateTLSResourcesRemainBlocking(t *testing.T) {
	for _, resource := range []string{"options:\n    selected: {minVersion: VersionTLS13}", "stores:\n    default:\n      defaultCertificate: {certFile: /not-read/cert.pem, keyFile: /not-read/key.pem}"} {
		in := tlsPolicyInputs(t, "minVersion: VersionTLS13")
		in.Dynamic = append(in.Dynamic, fixture(t, "duplicate.yml", fmt.Sprintf("tls:\n  %s\n", resource)))
		r := audit(t, in)
		if !hasBlockingGaps(r) || !strings.Contains(findings(r), "$identity") {
			t.Fatal("duplicate TLS resource accepted")
		}
	}
}
