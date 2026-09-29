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
	"statute.kjanat.dev/e2e/report"
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
	spec := workloadOwnerSpec(t, r.Compose.Project+"-wl", "dormant")
	spec.JSON = append(spec.JSON,
		jsonMinimum("/services/0/activation_attempts", "4"),
		jsonMinimum("/services/0/idle_stops", "3"),
		jsonMinimum("/services/0/external_starts", "2"),
		jsonMinimum("/services/0/external_stops", "1"),
		jsonPositive("/services/0/owners/0/last_activation_seconds"),
	)
	snapshot := awaitWorkloadReport(ctx, t, r, container, spec)
	if !snapshot.settled(r.Compose.Project + "-wl") {
		t.Fatalf("unsettled workload diagnostics: %+v", snapshot)
	}
	return snapshot
}

func awaitWorkloadReport(ctx context.Context, t *testing.T, r *harness.Run, container string, spec report.WaitSpec) workloadDiagnosticsReport {
	t.Helper()
	var snapshot workloadDiagnosticsReport
	snapshotURL := fmt.Sprintf("http://statute-1:%d/debug/workloads", harness.PortMetrics)
	spec.RejectContains = append(spec.RejectContains, container)
	body := awaitClient(ctx, r, snapshotURL, 30*time.Second, spec)
	if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
		t.Fatalf("invalid workload snapshot: %v\n%s", err, body)
	}
	return snapshot
}

func workloadOwnerSpec(t *testing.T, service, phase string) report.WaitSpec {
	t.Helper()
	return report.WaitSpec{JSON: []report.JSONCondition{
		jsonLength("/services", 1), jsonLength("/services/0/owners", 1), jsonLength("/orphaned_mutations", 0),
		jsonEqual(t, "/services/0/service", service), jsonEqual(t, "/services/0/activation_failures", 0),
		jsonEqual(t, "/services/0/owners/0/current", true), jsonEqual(t, "/services/0/owners/0/retired", false),
		jsonEqual(t, "/services/0/owners/0/phase", phase), jsonPositive("/services/0/owners/0/incarnation"),
		jsonEqual(t, "/services/0/owners/0/activation_waiters", 0), jsonEqual(t, "/services/0/owners/0/last_failure_reason", ""),
	}}
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
	spec := workloadOwnerSpec(t, want.service, want.phase)
	spec.JSON = append(spec.JSON,
		jsonEqual(t, "/services/0/activation_attempts", want.activations),
		jsonEqual(t, "/services/0/idle_stops", want.idleStops),
		jsonEqual(t, "/services/0/external_starts", want.externalStarts),
		jsonEqual(t, "/services/0/external_stops", want.externalStops),
	)
	snapshot := awaitWorkloadReport(ctx, t, r, container, spec)
	if !want.matches(snapshot) {
		t.Fatalf("checkpoint %+v: got %+v", want, snapshot)
	}
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
