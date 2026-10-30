package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

func TestHealthProbeResolvesURLMethodAndCredentials(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" || r.URL.RequestURI() != "/ready?deep=1" || r.Host != "health.example" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("probe policy changed: method=%s URI=%s Host=%s", r.Method, r.URL, r.Host)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	b := &backendState{backend: &resolved.Backend{Address: strings.TrimPrefix(backend.URL, "http://")}}
	h := newHealthChecker(resolved.HealthCheck{Path: "./ready?deep=1", ResolvePath: true, Method: "HEAD", Headers: map[string]string{"Authorization": "Bearer test-token"}, Healthy: 1, Unhealthy: 1, Timeout: time.Second, Redirects: "none"}, []*backendState{b}, nil, "health.example")
	run := &healthRun{checker: h, successes: map[*backendState]int{}, failures: map[*backendState]int{}}
	run.active.Store(true)
	run.probe(t.Context(), b)
	if !b.isHealthy() {
		t.Fatal("valid probe did not make backend healthy")
	}
}
