//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/e2e/harness"
)

// The wire shape is intentionally independent of Statute's internal snapshot.
type workloadDiagnosticsReport struct {
	Services          []workloadServiceReport `json:"services"`
	OrphanedMutations []json.RawMessage       `json:"orphaned_mutations"`
}

type workloadServiceReport struct {
	Service            string                `json:"service"`
	ActivationAttempts uint64                `json:"activation_attempts"`
	ActivationFailures uint64                `json:"activation_failures"`
	IdleStops          uint64                `json:"idle_stops"`
	ExternalStarts     uint64                `json:"external_starts"`
	ExternalStops      uint64                `json:"external_stops"`
	Owners             []workloadOwnerReport `json:"owners"`
}

type workloadOwnerReport struct {
	Incarnation           uint64  `json:"incarnation"`
	Current               bool    `json:"current"`
	Phase                 string  `json:"phase"`
	Retired               bool    `json:"retired"`
	ActivationWaiters     int     `json:"activation_waiters"`
	LastActivationSeconds float64 `json:"last_activation_seconds"`
	LastFailureReason     string  `json:"last_failure_reason"`
}

func readSettledWorkloadReport(ctx context.Context, t *testing.T, r *harness.Run, container string) workloadDiagnosticsReport {
	t.Helper()
	return awaitWorkloadReport(ctx, t, r, container, "settled lifecycle diagnostics", func(report workloadDiagnosticsReport) bool {
		return report.settled(r.Compose.Project + "-wl")
	})
}

func awaitWorkloadReport(ctx context.Context, t *testing.T, r *harness.Run, container, what string, accept func(workloadDiagnosticsReport) bool) workloadDiagnosticsReport {
	t.Helper()
	var report workloadDiagnosticsReport
	snapshotURL := fmt.Sprintf("http://statute-1:%d/debug/workloads", harness.PortMetrics)
	pollUntil(t, 30*time.Second, what, func() (bool, string) {
		body, err := clientGet(ctx, r, snapshotURL)
		if err != nil {
			return false, err.Error()
		}
		if strings.Contains(body, container) {
			t.Fatal("snapshot exposes the Docker container name")
		}
		if err := json.Unmarshal([]byte(body), &report); err != nil {
			t.Fatalf("invalid workload snapshot: %v\n%s", err, body)
		}
		return accept(report), body
	})
	return report
}

// workloadCheckpoint describes the wire-visible result of one explicit action.
type workloadCheckpoint struct {
	service        string
	phase          string
	activations    uint64
	idleStops      uint64
	externalStarts uint64
	externalStops  uint64
}

func awaitWorkloadCheckpoint(ctx context.Context, t *testing.T, r *harness.Run, container string, want workloadCheckpoint) {
	t.Helper()
	want.service = r.Compose.Project + "-wl"
	awaitWorkloadReport(ctx, t, r, container, fmt.Sprintf("workload checkpoint %+v", want), want.matches)
}

func (want workloadCheckpoint) matches(report workloadDiagnosticsReport) bool {
	if !report.hasCurrentOwner(want.service, want.phase) {
		return false
	}
	s := report.Services[0]
	return s.ActivationAttempts == want.activations && s.IdleStops == want.idleStops &&
		s.ExternalStarts == want.externalStarts && s.ExternalStops == want.externalStops
}

func (r workloadDiagnosticsReport) hasCurrentOwner(service, phase string) bool {
	if len(r.Services) != 1 || len(r.Services[0].Owners) != 1 || len(r.OrphanedMutations) != 0 {
		return false
	}
	s := r.Services[0]
	return s.Service == service && s.ActivationFailures == 0 && s.Owners[0].atCheckpoint(phase)
}

func (o workloadOwnerReport) atCheckpoint(phase string) bool {
	return o.Current && !o.Retired && o.Phase == phase && o.Incarnation > 0 &&
		o.ActivationWaiters == 0 && o.LastFailureReason == ""
}

func (r workloadDiagnosticsReport) settled(service string) bool {
	if len(r.Services) != 1 || len(r.Services[0].Owners) != 1 || len(r.OrphanedMutations) != 0 {
		return false
	}
	return r.Services[0].settled(service) && r.Services[0].Owners[0].settled()
}

func (s workloadServiceReport) settled(service string) bool {
	return s.Service == service && s.ActivationAttempts >= 4 && s.ActivationFailures == 0 &&
		s.IdleStops >= 3 && s.ExternalStarts >= 2 && s.ExternalStops >= 1
}

func (o workloadOwnerReport) settled() bool {
	return o.Current && !o.Retired && o.Phase == "dormant" && o.Incarnation > 0 &&
		o.ActivationWaiters == 0 && o.LastActivationSeconds > 0 && o.LastFailureReason == ""
}

func assertWorkloadDiagnostics(ctx context.Context, t *testing.T, r *harness.Run, container string) {
	t.Helper()
	report := readSettledWorkloadReport(ctx, t, r, container)
	service := report.Services[0]
	metrics := mustClientGet(ctx, r, fmt.Sprintf("http://statute-1:%d/metrics", harness.PortMetrics))
	for name, value := range map[string]uint64{
		"activations_total": service.ActivationAttempts, "activation_failures_total": service.ActivationFailures,
		"idle_stops_total": service.IdleStops, "external_starts_total": service.ExternalStarts,
		"external_stops_total": service.ExternalStops,
	} {
		line := fmt.Sprintf("statute_docker_workload_%s{service=%q} %d\n", name, service.Service, value)
		if !strings.Contains(metrics, line) {
			t.Errorf("missing metric %q in %s", line, metrics)
		}
	}
	health, err := clientGet(ctx, r, fmt.Sprintf("http://statute-1:%d/debug/workloads", harness.PortHealth))
	if err == nil || strings.Contains(health, "orphaned_mutations") {
		t.Fatalf("health listener exposes diagnostics: %s, %v", health, err)
	}
	content := mustClientGet(ctx, r, fmt.Sprintf("http://%s.test:%d/debug/workloads", r.Compose.Project, harness.PortHTTP))
	if strings.Contains(content, "orphaned_mutations") || !strings.Contains(content, `"origin":"origin-wl"`) {
		t.Fatalf("public workload path must still reach its origin, not diagnostics: %s", content)
	}
}
