package migrate

import (
	"strings"
	"testing"
)

func TestDockerRouterCanSelectFilePoolOrInternalHandler(t *testing.T) {
	for _, target := range []string{"api@file", "noop@internal"} {
		in := dockerServiceInputs(t, "")
		compose := `services:
  api:
    labels:
      traefik.enable: 'true'
      traefik.http.routers.api.rule: Host(` + "`api.example`" + `)
      traefik.http.routers.api.service: ` + target + `
`
		in.Compose = []string{fixture(t, "compose.yml", compose)}
		in.Dynamic = []string{fixture(t, "dynamic.yml", "http:\n  services:\n    api:\n      loadBalancer:\n        servers: [{url: 'http://127.0.0.1:9000'}]\n")}
		r := audit(t, in)
		requireMigrationGaps(t, r)
		if !strings.Contains(string(r.Go), ".Bind") {
			t.Fatal("cross-provider binding missing")
		}
		assertGeneratedParity(t, r)
	}
}

func TestFileRouterCanSelectServiceOnlyDockerBackend(t *testing.T) {
	in := dockerServiceInputs(t, "")
	in.Compose = []string{fixture(t, "compose.yml", `services:
  api:
    labels:
      traefik.enable: 'true'
      traefik.http.services.api.loadbalancer.server.port: '8080'
`)}
	in.Dynamic = []string{fixture(t, "dynamic.yml", strings.ReplaceAll(simpleDynamic, "service: api", "service: api@docker"))}
	r := audit(t, in)
	requireMigrationGaps(t, r)
	if !strings.Contains(string(r.Go), `ProxyToDiscovered("api@traefik")`) {
		t.Fatal("discovered service target missing")
	}
	assertGeneratedParity(t, r)
}
