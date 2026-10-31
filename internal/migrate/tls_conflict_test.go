package migrate

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	statute "statute.kjanat.dev"
)

func TestDockerStrictTLSConflictsGuardGeneratedProgram(t *testing.T) {
	for _, tc := range []struct{ strict, conflict bool }{{true, true}, {false, true}, {true, false}} {
		second := "first"
		if tc.conflict {
			second = "second"
		}
		static := fmt.Sprintf("entryPoints:\n  secure: {address: ':8443', http: {sanitizePath: false, tls: {}}}\nproviders:\n  docker: {exposedByDefault: false}\ncore: {strictTLSOptions: %t}\n", tc.strict)
		dynamic := "tls:\n  stores:\n    default:\n      defaultCertificate: {certFile: /not-read/cert.pem, keyFile: /not-read/key.pem}\n  options:\n    first: {minVersion: VersionTLS13}\n    second: {minVersion: VersionTLS12}\n"
		compose := fmt.Sprintf("services:\n  api:\n    labels:\n      traefik.enable: 'true'\n      traefik.http.services.api.loadbalancer.server.port: '8080'\n      traefik.http.routers.first.rule: Host(`api.example`)\n      traefik.http.routers.first.entrypoints: secure\n      traefik.http.routers.first.tls: 'true'\n      traefik.http.routers.first.tls.options: first@file\n      traefik.http.routers.first.service: api\n      traefik.http.routers.second.rule: Host(`api.example`)\n      traefik.http.routers.second.entrypoints: secure\n      traefik.http.routers.second.tls: 'true'\n      traefik.http.routers.second.tls.options: %s@file\n      traefik.http.routers.second.service: api\n", second)
		r := audit(t, Inputs{Static: []string{fixture(t, "static.yml", static)}, Dynamic: []string{fixture(t, "dynamic.yml", dynamic)}, Compose: []string{fixture(t, "compose.yml", compose)}})
		want := tc.strict && tc.conflict
		if r.Report.Blocking != want || strings.Contains(string(r.Go), "migration incomplete") != want {
			t.Fatalf("strict=%t conflict=%t blocking=%t findings=%s", tc.strict, tc.conflict, r.Report.Blocking, findings(r))
		}
		if want {
			assertTLSConflictStartupBlocked(t, r)
		}
		assertGeneratedParity(t, r)
	}
}

func assertTLSConflictStartupBlocked(t *testing.T, r *Result) {
	t.Helper()
	binary := compileGenerated(t, r)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// #nosec G204 -- Execute only the generated fixture built in this test.
	cmd := exec.CommandContext(ctx, binary)
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "migration incomplete") {
		t.Fatalf("generated program did not refuse startup: %v\n%s", err, output)
	}
}

func TestTLSConflictUsesSourceDefaultOrDisablesRouters(t *testing.T) {
	for _, strict := range []bool{false, true} {
		body := `http:
  routers:
    first:
      rule: Host(` + "`api.example`" + `)
      service: api
      priority: 10
      tls: {options: first}
    second:
      rule: Host(` + "`api.example`" + `)
      service: api
      priority: 20
      tls: {options: second}
  services:
    api:
      loadBalancer: {servers: [{url: 'http://127.0.0.1:9000'}]}
tls:
  stores:
    default:
      defaultCertificate: {certFile: /not-read/cert.pem, keyFile: /not-read/key.pem}
  options:
    first: {minVersion: VersionTLS13}
    second: {minVersion: VersionTLS12}
`
		in := basicInputs(t, body)
		in.Static = []string{fixture(t, "static.yml", fmt.Sprintf("entryPoints:\n  secure: {address: ':8443', http: {sanitizePath: false, tls: {}}}\ncore: {strictTLSOptions: %t}\n", strict))}
		r := audit(t, in)
		requireMigrationGaps(t, r)
		c, err := statute.Resolve(r.Config)
		if err != nil {
			t.Fatal(err)
		}
		if strict {
			if len(c.Routes) != 0 {
				t.Fatal("strict TLS conflict left routes serving")
			}
		} else if len(c.Routes) == 0 || len(c.Listeners[0].TLSPolicies) != 0 {
			t.Fatal("TLS conflict did not select the default policy")
		}
		assertGeneratedParity(t, r)
	}
}
