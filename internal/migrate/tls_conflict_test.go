package migrate

import (
	"fmt"
	"testing"

	statute "statute.kjanat.dev"
)

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
