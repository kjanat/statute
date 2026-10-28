package statute

import (
	"testing"
	"time"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func TestDockerExternalStopInvalidatesPoolBeforePublication(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode resolved.ReadinessMode
	}{
		{"tcp", resolved.ReadinessTCP},
		{"http", resolved.ReadinessHTTP},
		{"docker-health", resolved.ReadinessDockerHealth},
	} {
		for _, phase := range []workloadPhase{workloadReady, workloadStopPending} {
			t.Run(phase.String()+"/"+tc.name, func(t *testing.T) {
				policy := testWorkloadPolicy()
				policy.IdleAfter = time.Hour
				policy.Readiness = resolved.WorkloadReadiness{Mode: tc.mode, Path: "/"}
				p, daemon, _ := externalWorkloadFixturePolicy(t, 0, policy)
				daemon.mu.Lock()
				daemon.find("wl-1").health = "healthy"
				daemon.mu.Unlock()
				assertExternalWorkloadServes(t, p)
				assertExternalStopInvalidatesPool(t, p, daemon, phase, policy)
				assertExternalWorkloadServes(t, p)
				if got := daemon.startCount("wl-1"); got != 2 {
					t.Fatalf("start calls = %d, want 2", got)
				}
			})
		}
	}
}

func assertExternalStopInvalidatesPool(t *testing.T, p *dockerProvider, daemon *fakeDaemon, phase workloadPhase, policy resolved.Workload) {
	t.Helper()
	// Keep the old backend published while testing readiness after an external stop.
	p.syncMu.Lock()
	defer p.syncMu.Unlock()
	externalRunning(daemon, false)
	w := p.workloadFor("wl")
	binding := w.currentBinding()
	revision := p.srv.dynamic.Load().workloadRevisions["wl"]
	w.mu.Lock()
	if phase == workloadStopPending {
		w.toLocked(workloadStopPending)
	}
	p.observeStoppedWorkloadLocked(w)
	w.mu.Unlock()
	act := &workloadActivation{binding: binding, policy: policy}
	ready, changed := p.probeReady(t.Context(), w, act, docker.InspectState{Running: true, Health: "healthy"})
	if ready || changed == nil {
		t.Errorf("readiness used the pool from before the external stop: ready=%t publication wait=%t", ready, changed != nil)
	}
	if p.currentRoutePool("wl", binding, revision) != nil {
		t.Error("request can still use the pool from before the external stop")
	}
}
