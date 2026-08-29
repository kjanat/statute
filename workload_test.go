package statute

import (
	"sync"
	"testing"
)

func TestWorkloadZeroValueIsDormant(t *testing.T) {
	t.Parallel()

	var w workload
	if got := w.phaseNow(); got != workloadDormant {
		t.Fatalf("zero value phase = %v, want %v", got, workloadDormant)
	}
	if w.phaseNow().serving() {
		t.Fatal("a dormant workload must not serve")
	}
}

func TestWorkloadOnlyReadyServes(t *testing.T) {
	t.Parallel()

	for _, p := range []workloadPhase{workloadDormant, workloadStarting, workloadStopping, workloadFailed} {
		if p.serving() {
			t.Errorf("%v serves, want not serving", p)
		}
	}
	if !workloadReady.serving() {
		t.Error("ready does not serve, want serving")
	}
}

func TestWorkloadLegalTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path []workloadPhase
	}{
		{"activate and serve", []workloadPhase{workloadStarting, workloadReady}},
		{"idle shutdown", []workloadPhase{workloadStarting, workloadReady, workloadStopping, workloadDormant}},
		{"activation fails then retries", []workloadPhase{workloadStarting, workloadFailed, workloadDormant, workloadStarting, workloadReady}},
		{"external stop while starting", []workloadPhase{workloadStarting, workloadDormant}},
		{"external stop while ready", []workloadPhase{workloadStarting, workloadReady, workloadDormant}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var w workload
			for i, next := range tt.path {
				if !w.to(next) {
					t.Fatalf("step %d: to(%v) from %v rejected, want accepted", i, next, w.phaseNow())
				}
				if got := w.phaseNow(); got != next {
					t.Fatalf("step %d: phase = %v, want %v", i, got, next)
				}
			}
		})
	}
}

func TestWorkloadIllegalTransitionsKeepPhase(t *testing.T) {
	t.Parallel()

	all := []workloadPhase{workloadDormant, workloadStarting, workloadReady, workloadStopping, workloadFailed}

	for _, from := range all {
		for _, to := range all {
			legal := false
			for _, allowed := range workloadTransitions[from] {
				if allowed == to {
					legal = true
				}
			}
			if legal {
				continue
			}

			w := workload{phase: from}
			if w.to(to) {
				t.Errorf("to(%v) from %v accepted, want rejected", to, from)
			}
			if got := w.phaseNow(); got != from {
				t.Errorf("rejected to(%v) moved %v to %v", to, from, got)
			}
		}
	}
}

// A dormant workload must not reach ready without passing through starting,
// which is the invariant separating this model from backend health.
func TestWorkloadCannotSkipStarting(t *testing.T) {
	t.Parallel()

	var w workload
	if w.to(workloadReady) {
		t.Fatal("dormant to ready accepted, want rejected")
	}
	if got := w.phaseNow(); got != workloadDormant {
		t.Fatalf("phase = %v, want %v", got, workloadDormant)
	}
}

func TestWorkloadConcurrentTransitionsElectOneWinner(t *testing.T) {
	t.Parallel()

	var w workload

	const racers = 64

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)

	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()

			if w.to(workloadStarting) {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("accepted transitions = %d, want 1", wins)
	}
	if got := w.phaseNow(); got != workloadStarting {
		t.Fatalf("phase = %v, want %v", got, workloadStarting)
	}
}

func TestWorkloadPhaseStrings(t *testing.T) {
	t.Parallel()

	want := map[workloadPhase]string{
		workloadDormant:  "dormant",
		workloadStarting: "starting",
		workloadReady:    "ready",
		workloadStopping: "stopping",
		workloadFailed:   "failed",
		workloadPhase(9): "unknown",
	}

	for p, s := range want {
		if got := p.String(); got != s {
			t.Errorf("%d.String() = %q, want %q", uint8(p), got, s)
		}
	}
}
