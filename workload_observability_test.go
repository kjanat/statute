package statute

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestWorkloadDiagnosticsMetricsPaths(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"/debug/workloads", "GET /debug/workloads", "POST /debug/workloads",
		"example.test/debug/workloads", "/%64ebug/workloads", "HEAD /%64ebug/workloads",
		"/debug/pprof/", "HEAD /debug/{name}", "not a valid pattern",
	} {
		t.Run(path, func(t *testing.T) {
			if _, err := resolveMetrics(Prometheus("127.0.0.1:0", path)); err == nil {
				t.Fatalf("conflicting path %q accepted", path)
			}
			s := &server{stats: newStats()}
			if _, err := s.buildMetricsServer(resolved.Metrics{Enabled: true, Path: path}); err == nil {
				t.Fatalf("runtime accepted conflicting path %q", path)
			}
		})
	}
}

func TestWorkloadDiagnosticsAllowedMetricsPaths(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/metrics", "/", "/debug/", "/debug/workloads/", "GET /debug/{name}", "example.test/", "GET example.test/debug/{name}"} {
		t.Run("allowed "+path, func(t *testing.T) {
			m, err := resolveMetrics(Prometheus("127.0.0.1:0", path))
			if err != nil {
				t.Fatal(err)
			}
			s := &server{stats: newStats()}
			hs, err := s.buildMetricsServer(m)
			if err != nil {
				t.Fatal(err)
			}
			assertEmptyWorkloadEndpoint(t, hs.Handler)
			post := httptest.NewRecorder()
			hs.Handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "http://example.test"+workloadSnapshotPath, nil))
			if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
				t.Fatal("reserved snapshot path accepted a write method")
			}
			pprof := httptest.NewRecorder()
			hs.Handler.ServeHTTP(pprof, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
			if pprof.Code != http.StatusOK {
				t.Fatalf("pprof status %d", pprof.Code)
			}
		})
	}
}

func assertEmptyWorkloadEndpoint(t *testing.T, handler http.Handler) {
	t.Helper()
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "http://example.test"+workloadSnapshotPath, nil))
	if out.Code != http.StatusOK || out.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("snapshot: status %d, type %q", out.Code, out.Header().Get("Content-Type"))
	}
	var snapshot workloadDiagnosticsSnapshot
	if err := json.Unmarshal(out.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Services == nil || snapshot.OrphanedMutations == nil || len(snapshot.Services)+len(snapshot.OrphanedMutations) != 0 {
		t.Fatalf("absent Docker must produce empty arrays: %s", out.Body.String())
	}
	if out.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("snapshot must not be cached")
	}
}

func TestWorkloadDiagnosticsPrometheusProjection(t *testing.T) {
	t.Parallel()
	snapshot := workloadDiagnosticsSnapshot{
		Services: []workloadServiceSnapshot{{
			Service:            "quote\"slash\\line\nend",
			ActivationAttempts: 7, ActivationFailures: 2, IdleStops: 3, ExternalStarts: 4, ExternalStops: 5,
			Owners: []workloadOwnerSnapshot{
				{Current: true, Incarnation: 9, Phase: "starting", ActivationWaiters: 6, LastActivationSeconds: 0.25},
				{Incarnation: 8, Phase: "stop-unknown", Retired: true, LastFailureReason: "start-failed"},
			},
		}},
		OrphanedMutations: []workloadOwnerSnapshot{{Incarnation: 3, Phase: "stop-unknown", Retired: true}},
	}
	var output bytes.Buffer
	writeWorkloadPrometheus(&output, snapshot)
	const label = `{service="quote\"slash\\line\nend"}`
	for _, sample := range []string{
		"activations_total" + label + " 7", "activation_failures_total" + label + " 2",
		"idle_stops_total" + label + " 3", "external_starts_total" + label + " 4",
		"external_stops_total" + label + " 5", "phase" + label + " 1",
		"waiters" + label + " 6", "last_activation_seconds" + label + " 0.25",
		"retired" + label + " 0", "retired_owners" + label + " 1", "orphaned_mutations 1",
	} {
		if !strings.Contains(output.String(), "statute_docker_workload_"+sample+"\n") {
			t.Errorf("missing sample %s in %s", sample, output.String())
		}
	}
	if strings.Count(output.String(), "statute_docker_workload_phase{") != 1 {
		t.Fatal("predecessor must not emit a duplicate current-state sample")
	}
	for _, forbidden := range []string{"incarnation=", "reason=", "start-failed"} {
		if strings.Contains(output.String(), forbidden) {
			t.Errorf("unbounded or sensitive metric label: %s", forbidden)
		}
	}
}

func TestWorkloadDiagnosticsListenerIsolation(t *testing.T) {
	t.Parallel()
	content, metrics, health := reserveAddr(t), reserveAddr(t), reserveAddr(t)
	cfg := Config{
		Listeners:     Listeners{HTTP(content)},
		Routes:        Routes{Match("/*").Handle(http.NotFoundHandler())},
		Observability: Observability{Metrics: Prometheus(metrics, "/metrics"), Health: Health(health, "/healthz")},
	}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	for _, tc := range []struct {
		addr string
		want int
	}{{content, http.StatusNotFound}, {health, http.StatusNotFound}, {metrics, http.StatusOK}} {
		waitForListen(t, tc.addr)
		resp, err := http.Get("http://" + tc.addr + workloadSnapshotPath)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != tc.want {
			t.Fatalf("listener %s: status %d body %s err %v", tc.addr, resp.StatusCode, body, readErr)
		}
		if tc.addr != metrics && strings.Contains(string(body), "orphaned_mutations") {
			t.Fatalf("workload diagnostics escaped onto listener %s", tc.addr)
		}
	}
	mustServeMetrics(t, metrics)
	mustServeHealth(t, health)
}
