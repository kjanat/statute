package migrate

import (
	"bytes"
	"flag"
	"os"
	"strconv"
	"strings"
	"testing"
)

const goldenPath = "testdata/compact.golden"

var updateGolden = flag.Bool("update", false, "rewrite testdata/*.golden from the current generator")

const goldenStatic = `entryPoints:
  web:
    address: ':8080'
    http: {sanitizePath: false, middlewares: [edge@file]}
`

const goldenDynamic = `http:
  middlewares:
    edge:
      rateLimit: {average: 50, burst: 100}
    limit:
      rateLimit: {average: 20, burst: 40}
    twin:
      rateLimit: {average: 20, burst: 40}
    by-host:
      rateLimit: {average: 5, burst: 10, sourceCriterion: {requestHost: true}}
    by-header:
      rateLimit: {average: 5, burst: 10, sourceCriterion: {requestHeaderName: X-Client}}
    tag:
      headers:
        customResponseHeaders: {X-Policy: '${GOLDEN_POLICY}'}
    stack:
      chain: {middlewares: [tag, limit]}
  routers:
    app:
      rule: Host(` + "`a.example`" + `) || Host(` + "`b.example`" + `)
      service: app
      middlewares: [stack, twin]
    admin:
      rule: Host(` + "`admin.example`" + `) && PathPrefix(` + "`/api`" + `)
      service: stream
      middlewares: [by-host, by-header]
  services:
    app:
      loadBalancer:
        servers:
          - url: http://127.0.0.1:9000
    spare:
      loadBalancer:
        servers:
          - url: http://127.0.0.1:9001
    stream:
      loadBalancer:
        servers:
          - url: http://127.0.0.1:9002
        responseForwarding: {flushInterval: 50ms}
`

func TestGoldenMigrationOutput(t *testing.T) {
	in := Inputs{Static: []string{fixture(t, "static.yml", goldenStatic)}, Dynamic: []string{fixture(t, "dynamic.yml", goldenDynamic)}}
	result := audit(t, in)
	if again := audit(t, in); !bytes.Equal(result.Go, again.Go) {
		t.Fatal("nondeterministic generated source")
	}
	if *updateGolden {
		if err := os.WriteFile(goldenPath, result.Go, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Go, want) {
		t.Fatalf("generated source differs from %s; rerun with -update and review the diff\n%s", goldenPath, firstDifference(string(want), string(result.Go)))
	}
	t.Setenv("GOLDEN_POLICY", "migration-binding")
	assertGeneratedParity(t, result)
}

func firstDifference(want, got string) string {
	wantLines, gotLines := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range max(len(wantLines), len(gotLines)) {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			return "line " + strconv.Itoa(i+1) + ":\n- " + w + "\n+ " + g
		}
	}
	return ""
}
