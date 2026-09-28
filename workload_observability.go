package statute

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const workloadSnapshotPath = "/debug/workloads"

// newMetricsMux validates the configurable pattern alongside reserved handlers.
// Resolve and runtime construction use the same registration rules.
func newMetricsMux(pattern string, metrics, workloads http.Handler) (handler http.Handler, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			handler = nil
			err = fmt.Errorf("metrics: invalid or conflicting path: %v", recovered)
		}
	}()
	fields := strings.Fields(pattern)
	if len(fields) > 0 {
		path := fields[len(fields)-1]
		if slash := strings.IndexByte(path, '/'); slash >= 0 {
			path = path[slash:]
		}
		if decoded, decodeErr := url.PathUnescape(path); decodeErr == nil && decoded == workloadSnapshotPath {
			return nil, fmt.Errorf("metrics: %s is reserved for workload diagnostics", workloadSnapshotPath)
		}
	}
	mux := http.NewServeMux()
	mux.Handle(pattern, metrics)
	mux.Handle("GET "+workloadSnapshotPath, workloads)
	registerPprof(mux)
	// Exact diagnostics take precedence over host-specific metrics patterns.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == workloadSnapshotPath {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			workloads.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}

func workloadSnapshotHandler(p *dockerProvider) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		snapshot := p.snapshotWorkloads()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(snapshot)
	})
}

// Prometheus permits only these three label escapes, unlike Go string quoting.
var workloadLabelEscaper = strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"")

func writeWorkloadPrometheus(w io.Writer, snapshot workloadDiagnosticsSnapshot) {
	const (
		counterKind = "counter"
		gaugeKind   = "gauge"
	)
	pf := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	for _, metric := range []struct {
		name, kind, help string
	}{
		{"activations_total", counterKind, "Accepted activation and observe-only readiness attempts."},
		{"activation_failures_total", counterKind, "Failed activation or readiness attempts, excluding cancellation and supersession."},
		{"idle_stops_total", counterKind, "Successfully settled idle shutdowns."},
		{"external_starts_total", counterKind, "Observed running transitions adopted through readiness."},
		{"external_stops_total", counterKind, "Observed external stops of ready or stop-pending workloads."},
		{"phase", gaugeKind, "Current owner phase: dormant=0 starting=1 ready=2 stop-pending=3 stop-issued=4 stop-unknown=5 failed=6."},
		{"waiters", gaugeKind, "Requests waiting for the current activation, excluding stop waiters."},
		{"last_activation_seconds", gaugeKind, "Duration of the current incarnation's last completed activation or readiness attempt."},
		{"retired", gaugeKind, "Whether the current owner has lost lifecycle authority."},
		{"retired_owners", gaugeKind, "Retained retired owners, including predecessor mutations."},
		{"orphaned_mutations", gaugeKind, "Recovered mutation owners with no currently configured service authority."},
	} {
		pf("# HELP statute_docker_workload_%s %s\n", metric.name, metric.help)
		pf("# TYPE statute_docker_workload_%s %s\n", metric.name, metric.kind)
	}
	pf("statute_docker_workload_orphaned_mutations %d\n", len(snapshot.OrphanedMutations))
	for _, service := range snapshot.Services {
		label := workloadLabelEscaper.Replace(service.Service)
		counter := func(name string, value uint64) {
			pf("statute_docker_workload_%s{service=\"%s\"} %d\n", name, label, value)
		}
		counter("activations_total", service.ActivationAttempts)
		counter("activation_failures_total", service.ActivationFailures)
		counter("idle_stops_total", service.IdleStops)
		counter("external_starts_total", service.ExternalStarts)
		counter("external_stops_total", service.ExternalStops)
		retired := 0
		for _, owner := range service.Owners {
			if owner.Retired {
				retired++
			}
			if owner.Current {
				writeCurrentWorkloadPrometheus(w, label, owner)
			}
		}
		pf("statute_docker_workload_retired_owners{service=\"%s\"} %d\n", label, retired)
	}
}

func writeCurrentWorkloadPrometheus(w io.Writer, label string, owner workloadOwnerSnapshot) {
	pf := func(name string, value any) {
		_, _ = fmt.Fprintf(w, "statute_docker_workload_%s{service=\"%s\"} %v\n", name, label, value)
	}
	for phase := workloadDormant; phase <= workloadFailed; phase++ {
		if phase.String() == owner.Phase {
			pf("phase", uint8(phase))
			break
		}
	}
	pf("waiters", owner.ActivationWaiters)
	pf("last_activation_seconds", owner.LastActivationSeconds)
	retired := 0
	if owner.Retired {
		retired = 1
	}
	pf("retired", retired)
}
