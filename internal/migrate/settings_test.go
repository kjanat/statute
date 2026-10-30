package migrate

import (
	"fmt"
	"strings"
	"testing"
)

func TestProcessSettingsTranslateWithoutStartingServices(t *testing.T) {
	for _, setting := range []string{
		"accessLog: {}",
		"accessLog: {format: common}",
		"accessLog: {format: json, filePath: /not-created/access.log, bufferingSize: 2, filters: {statusCodes: [200-299, '503']}}",
		"ping: {}",
		"ping: {entryPoint: web, terminatingStatusCode: 202}",
		"ping: {manualRouting: true}",
		"global: {checkNewVersion: false, sendAnonymousUsage: false}",
		"core: {strictTLSOptions: true, defaultRuleSyntax: v3}",
		"core: {strictTLSOptions: false, defaultRuleSyntax: v2}",
		"log: {level: DEBUG}",
		"api: {dashboard: true}",
	} {
		t.Run(setting, func(t *testing.T) {
			in := basicInputs(t, simpleDynamic)
			in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  web: {address: ':8080', http: {sanitizePath: false}}\n  traefik: {address: ':8082', http: {sanitizePath: false}}\n"+setting+"\n")}
			r := audit(t, in)
			requireMigrationGaps(t, r)
			assertGeneratedParity(t, r)
		})
	}
}

func TestProcessSettingsRejectInvalidValues(t *testing.T) {
	for _, setting := range []string{
		"accessLog: false", "ping: false", "accessLog: []",
		"accessLog: {format: xml}", "accessLog: {filePath: '${LOG_PATH}'}", "accessLog: {bufferingSize: -1}", "accessLog: {bufferingSize: bad}", "accessLog: {filters: {statusCodes: false}}",
		"ping: {manualRouting: bad}", "ping: {terminatingStatusCode: bad}", "ping: {terminatingStatusCode: 100}", "ping: {entryPoint: missing}",
	} {
		in := basicInputs(t, simpleDynamic)
		in.Static = []string{fixture(t, "static.yml", fmt.Sprintf("entryPoints:\n  web: {address: ':8080', http: {sanitizePath: false}}\n%s\n", setting))}
		r := audit(t, in)
		if !r.Report.Blocking || !strings.Contains(string(r.Go), "migration incomplete") {
			t.Fatalf("invalid setting accepted: %s", setting)
		}
	}
}
