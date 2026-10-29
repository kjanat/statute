//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

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
	blob, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	body := workloadClient(ctx, r, "wait", "-url", snapshotURL, "-timeout", "30s", "-spec", string(blob))
	if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
		t.Fatalf("invalid workload snapshot: %v\n%s", err, body)
	}
	return snapshot
}

// workloadClient avoids Compose startup between checks of a short idle window.
func workloadClient(ctx context.Context, r *harness.Run, args ...string) string {
	r.T.Helper()
	id := mustCreateContainer(ctx, r, harness.ContainerSpec{
		Image: os.Getenv("STATUTE_E2E_IMAGE"), Network: r.Compose.Project + "_mesh",
		Entrypoint: []string{"/client"}, Cmd: args,
	}, true)
	defer func() { requireEngine(r.T, r.Engine.Remove(ctx, id)) }()
	code, err := r.Engine.Wait(ctx, id)
	requireEngine(r.T, err)
	out, err := r.Engine.Logs(ctx, id)
	requireEngine(r.T, err)
	if code != 0 {
		r.T.Fatalf("workload client %v exited %d: %s", args, code, out)
	}
	return out
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
	spec := workloadCheckpointSpec(t, want)
	snapshot := awaitWorkloadReport(ctx, t, r, container, spec)
	if !want.matches(snapshot) {
		t.Fatalf("checkpoint %+v: got %+v", want, snapshot)
	}
}

func workloadCheckpointSpec(t *testing.T, want workloadCheckpoint) report.WaitSpec {
	t.Helper()
	spec := workloadOwnerSpec(t, want.service, want.phase)
	spec.JSON = append(spec.JSON,
		jsonEqual(t, "/services/0/idle_stops", want.idleStops),
		jsonEqual(t, "/services/0/external_stops", want.externalStops),
	)
	if want.externalStarts == 0 {
		spec.JSON = append(spec.JSON,
			jsonEqual(t, "/services/0/activation_attempts", want.activations),
			jsonEqual(t, "/services/0/external_starts", 0),
		)
	} else {
		spec.JSON = append(spec.JSON,
			jsonMinimum("/services/0/activation_attempts", fmt.Sprint(want.activations)),
			jsonMinimum("/services/0/external_starts", fmt.Sprint(want.externalStarts)),
		)
	}
	return spec
}

func adoptWorkload(ctx context.Context, t *testing.T, r *harness.Run, container, id, host string, want workloadCheckpoint) {
	t.Helper()
	want.service = r.Compose.Project + "-wl"
	spec := workloadCheckpointSpec(t, want)
	spec.RejectContains = append(spec.RejectContains, container)
	plan := &report.Plan{Name: fmt.Sprintf("adopt-%d", want.externalStarts), Steps: []report.Step{
		{
			Name: "adoption", URL: fmt.Sprintf("http://statute-1:%d/debug/workloads", harness.PortMetrics),
			TargetServer: "statute-1", Proto: "h1", Wait: &report.WaitStep{Timeout: "30s", Spec: spec},
		},
		{
			Name: "serve", URL: fmt.Sprintf("http://%s:%d/echo", host, harness.PortHTTP),
			TargetServer: "statute-1", Proto: "h1", Count: 1,
			Expect: report.Expect{Status: 200, BodyContains: `"origin":"origin-wl"`},
		},
	}}
	client := r.Topology.Clients[0]
	path := r.WritePlan(client, plan)
	name := client + "-" + plan.Name + "-report.json"
	// Start the idle window after the client is running.
	out, err := r.Compose.Output(ctx, "run", "--detach", "--no-deps", client, "run", "-plan", path, "-out", "/reports/"+name)
	requireEngine(t, err)
	actor := strings.TrimSpace(out)
	defer func() { requireEngine(t, r.Engine.Remove(ctx, actor)) }()
	requireEngine(t, r.Engine.Start(ctx, id))
	code, err := r.Engine.Wait(ctx, actor)
	requireEngine(t, err)
	logs, err := r.Engine.Logs(ctx, actor)
	requireEngine(t, err)
	if code != 0 {
		t.Fatalf("adoption client exited %d: %s", code, logs)
	}
	rep := r.ReadReport(name)
	if len(rep.Failures) != 0 || len(rep.Results) != 2 || !rep.Results[0].OK || !rep.Results[1].OK {
		t.Fatalf("adoption plan: %+v", rep)
	}
	var snapshot workloadDiagnosticsReport
	if err := json.Unmarshal([]byte(rep.Results[0].Body), &snapshot); err != nil {
		t.Fatal(err)
	}
	if !want.matches(snapshot) {
		t.Fatalf("adoption checkpoint %+v: got %+v", want, snapshot)
	}
}

func (want workloadCheckpoint) matches(report workloadDiagnosticsReport) bool {
	if !report.hasCurrentOwner(want.service, want.phase) {
		return false
	}
	s := report.Services[0]
	if s.IdleStops != want.idleStops || s.ExternalStops != want.externalStops ||
		s.ActivationAttempts < want.activations || s.ExternalStarts < want.externalStarts {
		return false
	}
	if want.externalStarts == 0 {
		return s.ActivationAttempts == want.activations && s.ExternalStarts == 0
	}
	// Stale adoption increments both counters. Their difference counts demand.
	return s.ActivationAttempts-want.activations == s.ExternalStarts-want.externalStarts
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
