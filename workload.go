package statute

import (
	"slices"
	"sync"
)

// workloadPhase is the lifecycle of one on-demand Docker workload, the
// per-container contribution beneath a discovered service.
//
// It is deliberately not [backendState]. A backend begins healthy and demotes on
// evidence of failure; a workload begins unavailable and becomes eligible for
// proxying only once readiness is established.
type workloadPhase uint8

const (
	// workloadDormant is a container that exists and is not running.
	workloadDormant workloadPhase = iota
	// workloadStarting is an activation in progress, readiness not yet proven.
	workloadStarting
	// workloadReady is a container whose readiness has been established.
	workloadReady
	// workloadStopping is an idle shutdown in progress.
	workloadStopping
	// workloadFailed is an activation that timed out or failed.
	workloadFailed
)

func (p workloadPhase) String() string {
	switch p {
	case workloadDormant:
		return "dormant"
	case workloadStarting:
		return "starting"
	case workloadReady:
		return "ready"
	case workloadStopping:
		return "stopping"
	case workloadFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// serving reports whether a workload in this phase may receive proxied traffic.
func (p workloadPhase) serving() bool { return p == workloadReady }

// workloadTransitions is the complete legal transition set.
//
// Docker may report a stop statute did not initiate, so `starting` and `ready`
// both reach `dormant` without passing through `stopping`. A request racing a
// stop, and any retry semantics beyond returning `failed` to `dormant`, are not
// expressed here.
var workloadTransitions = map[workloadPhase][]workloadPhase{
	workloadDormant:  {workloadStarting},
	workloadStarting: {workloadReady, workloadFailed, workloadDormant},
	workloadReady:    {workloadStopping, workloadDormant},
	workloadStopping: {workloadDormant},
	workloadFailed:   {workloadDormant},
}

// workload carries the lifecycle state of one on-demand container. The zero
// value is a dormant workload.
type workload struct {
	mu    sync.Mutex
	phase workloadPhase
}

// phaseNow returns the current phase.
func (w *workload) phaseNow() workloadPhase {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.phase
}

// to moves the workload to next and reports whether the transition was legal. An
// illegal transition leaves the phase untouched.
func (w *workload) to(next workloadPhase) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !slices.Contains(workloadTransitions[w.phase], next) {
		return false
	}
	w.phase = next

	return true
}
