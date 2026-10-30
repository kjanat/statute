package migrate

import (
	"encoding/json"
	"strings"
	"testing"

	statute "statute.kjanat.dev"
)

func TestHealthHeaderCredentialsRequireBindings(t *testing.T) {
	for _, value := range []string{"Bearer private-token", "${PROBE_TOKEN}"} {
		for _, provider := range []string{providerFile, providerDocker} {
			body := strings.Replace(simpleDynamic, "      loadBalancer:", "      loadBalancer:\n        healthCheck:\n          path: /ready\n          headers: {Authorization: '"+value+"'}", 1)
			in := basicInputs(t, body)
			if provider == providerDocker {
				in = dockerServiceInputs(t, "      traefik.http.services.api.loadbalancer.healthcheck.path: /ready\n      traefik.http.services.api.loadbalancer.healthcheck.headers.Authorization: '"+value+"'\n")
			}
			r := audit(t, in)
			assertProbeSecretsWithheld(t, r)
			c, err := statute.Resolve(r.Config)
			if err != nil {
				t.Fatal(err)
			}
			var headers map[string]string
			if provider == providerDocker {
				headers = c.Docker.PoolPolicy["api@traefik"].HealthCheck.Headers
			} else {
				headers = c.Upstreams["api@file"].HealthCheck.Headers
			}
			if !probeAuthBindingPresent(headers) {
				t.Fatal("probe policy missing binding placeholder")
			}

			bindings := 0
			for _, check := range r.Report.Checks {
				if name, ok := strings.CutPrefix(check.Code, "environment-required:"); ok {
					bindings++
					t.Setenv(name, "migration-binding")
				}
			}
			if bindings != 1 {
				t.Fatalf("required bindings=%d", bindings)
			}
			assertGeneratedParity(t, r)
		}
	}
}

func assertProbeSecretsWithheld(t *testing.T, r *Result) {
	t.Helper()
	report, err := json.Marshal(r.Report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(r.Go), "private-token") || strings.Contains(string(report), "private-token") {
		t.Fatal("probe credential exposed")
	}
}

func probeAuthBindingPresent(headers map[string]string) bool {
	for name, value := range headers {
		if strings.EqualFold(name, "Authorization") && value == "migration-binding" {
			return true
		}
	}
	return false
}
