//go:build e2e

package e2e

import (
	"encoding/json"
	"testing"
)

func TestRegression_WorkloadCheckpoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*workloadDiagnosticsReport)
		accept bool
	}{
		{"adopted ready", func(*workloadDiagnosticsReport) {}, true},
		{"another project", func(r *workloadDiagnosticsReport) { r.Services[0].Service = "other-wl" }, false},
		{"demand won adoption race", func(r *workloadDiagnosticsReport) { r.Services[0].ExternalStarts = 0 }, false},
		{"issued stop", func(r *workloadDiagnosticsReport) { r.Services[0].Owners[0].Phase = "stop-issued" }, false},
		{"unknown stop", func(r *workloadDiagnosticsReport) { r.Services[0].Owners[0].Phase = "stop-unknown" }, false},
		{"retired owner", func(r *workloadDiagnosticsReport) { r.Services[0].Owners[0].Retired = true }, false},
		{"stale owner", func(r *workloadDiagnosticsReport) { r.Services[0].Owners[0].Current = false }, false},
		{"active waiters", func(r *workloadDiagnosticsReport) { r.Services[0].Owners[0].ActivationWaiters = 1 }, false},
		{"activation failed", func(r *workloadDiagnosticsReport) { r.Services[0].ActivationFailures = 1 }, false},
		{"extra activation", func(r *workloadDiagnosticsReport) { r.Services[0].ActivationAttempts++ }, false},
		{"missing idle settlement", func(r *workloadDiagnosticsReport) { r.Services[0].IdleStops-- }, false},
		{"unobserved external stop", func(r *workloadDiagnosticsReport) { r.Services[0].ExternalStops = 0 }, false},
		{"orphan mutation", func(r *workloadDiagnosticsReport) { r.OrphanedMutations = []json.RawMessage{json.RawMessage(`{}`)} }, false},
		{"missing owner", func(r *workloadDiagnosticsReport) { r.Services[0].Owners = nil }, false},
		{"missing service", func(r *workloadDiagnosticsReport) { r.Services = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := workloadDiagnosticsReport{Services: []workloadServiceReport{{
				Service: "wl", ActivationAttempts: 4, IdleStops: 2, ExternalStarts: 2, ExternalStops: 1,
				Owners: []workloadOwnerReport{{Current: true, Phase: "ready", Incarnation: 1}},
			}}}
			tc.change(&report)
			want := workloadCheckpoint{service: "wl", phase: "ready", activations: 4, idleStops: 2, externalStarts: 2, externalStops: 1}
			if got := want.matches(report); got != tc.accept {
				t.Fatalf("matches = %v, want %v: %+v", got, tc.accept, report)
			}
		})
	}
}

func TestRegression_WorkloadDormantCheckpoint(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"dormant", "stop-issued", "stop-unknown", "ready"} {
		t.Run(phase, func(t *testing.T) {
			report := workloadDiagnosticsReport{Services: []workloadServiceReport{{
				Service: "wl", ActivationAttempts: 1, IdleStops: 1, ExternalStarts: 1,
				Owners: []workloadOwnerReport{{Current: true, Phase: phase, Incarnation: 1}},
			}}}
			want := workloadCheckpoint{service: "wl", phase: "dormant", activations: 1, idleStops: 1, externalStarts: 1}
			if got := want.matches(report); got != (phase == "dormant") {
				t.Fatalf("settlement checkpoint accepted phase %q: %v", phase, got)
			}
		})
	}
}
