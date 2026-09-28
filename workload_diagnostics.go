package statute

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// workloadDiagnostics accumulates only code-configured service identities. It
// outlives provider runs and retired mutation owners, but is never persisted.
type workloadDiagnostics struct {
	mu       sync.Mutex
	services map[string]workloadServiceCounters
}

type workloadServiceCounters struct {
	ActivationAttempts uint64 `json:"activation_attempts"`
	ActivationFailures uint64 `json:"activation_failures"`
	IdleStops          uint64 `json:"idle_stops"`
	ExternalStarts     uint64 `json:"external_starts"`
	ExternalStops      uint64 `json:"external_stops"`
}

// workloadBindingDiagnostics belongs to the immutable incarnation, unlike the
// service counters. A replacement binding starts with no predecessor details.
type workloadBindingDiagnostics struct {
	lastActivationSeconds float64
	lastFailureReason     string
	lastFailureAt         time.Time
}

type workloadDiagnosticsSnapshot struct {
	Services          []workloadServiceSnapshot `json:"services"`
	OrphanedMutations []workloadOwnerSnapshot   `json:"orphaned_mutations"`
}

type workloadServiceSnapshot struct {
	Service string `json:"service"`
	workloadServiceCounters
	Owners []workloadOwnerSnapshot `json:"owners"`
}

type workloadOwnerSnapshot struct {
	Incarnation           uint64     `json:"incarnation"`
	Current               bool       `json:"current"`
	Phase                 string     `json:"phase"`
	Retired               bool       `json:"retired"`
	ActivationWaiters     int        `json:"activation_waiters"`
	LastActivationSeconds float64    `json:"last_activation_seconds"`
	LastFailureReason     string     `json:"last_failure_reason"`
	LastFailureAt         *time.Time `json:"last_failure_at,omitempty"`
}

type workloadDiagnosticEvent uint8

const (
	workloadActivationAttemptEvent workloadDiagnosticEvent = iota
	workloadActivationFailureEvent
	workloadIdleStopEvent
	workloadExternalStartEvent
	workloadExternalStopEvent
)

func (p *dockerProvider) recordWorkloadEvent(service string, event workloadDiagnosticEvent) {
	if p == nil || p.cfg == nil {
		return
	}
	if _, configured := p.cfg.Workloads[service]; !configured {
		return
	}
	p.diagnostics.mu.Lock()
	defer p.diagnostics.mu.Unlock()
	if p.diagnostics.services == nil {
		p.diagnostics.services = make(map[string]workloadServiceCounters, len(p.cfg.Workloads))
	}
	counters := p.diagnostics.services[service]
	switch event {
	case workloadActivationAttemptEvent:
		counters.ActivationAttempts++
	case workloadActivationFailureEvent:
		counters.ActivationFailures++
	case workloadIdleStopEvent:
		counters.IdleStops++
	case workloadExternalStartEvent:
		counters.ExternalStarts++
	case workloadExternalStopEvent:
		counters.ExternalStops++
	}
	p.diagnostics.services[service] = counters
}

var errWorkloadReadinessTimeout = errors.New("readiness not established")

// workloadActivationFailure retains the error for ordinary logs and errors.Is,
// while diagnostics select only a closed reason assigned at the failing stage.
type workloadActivationFailure struct {
	reason string
	err    error
}

func (e workloadActivationFailure) Error() string { return e.err.Error() }
func (e workloadActivationFailure) Unwrap() error { return e.err }

func classifyWorkloadStartFailure(err error) error {
	reason := "start-failed"
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "start-timeout"
	}
	return workloadActivationFailure{reason: reason, err: err}
}

func classifyWorkloadReadinessFailure(err error) error {
	if err == nil {
		return nil
	}
	reason := "activation-failed"
	switch {
	case errors.Is(err, errWorkloadReadinessTimeout):
		reason = "readiness-timeout"
	case errors.Is(err, errWorkloadStopped):
		reason = "stopped-before-ready"
	}
	return workloadActivationFailure{reason: reason, err: err}
}

func (w *workload) recordActivationCompletionLocked(p *dockerProvider, act *workloadActivation, err error, outcome activationOutcome) {
	if w.binding == nil || w.binding.key != act.binding {
		return
	}
	details := &w.binding.diagnostics
	details.lastActivationSeconds = time.Since(act.started).Seconds()
	if err == nil || outcome.abandoned || outcome.stale {
		return
	}
	reason := "activation-failed"
	if failure, ok := errors.AsType[workloadActivationFailure](err); ok {
		reason = failure.reason
	}
	details.lastFailureReason = reason
	details.lastFailureAt = time.Now().UTC()
	p.recordWorkloadEvent(w.service, workloadActivationFailureEvent)
}

// snapshotWorkloads copies membership, all owners, and service counters under
// workloadMu -> owner mutexes -> diagnostics.mu. Writers already holding an
// owner mutex take only diagnostics.mu. No response contains mutable state.
func (p *dockerProvider) snapshotWorkloads() workloadDiagnosticsSnapshot {
	out := workloadDiagnosticsSnapshot{
		Services: []workloadServiceSnapshot{}, OrphanedMutations: []workloadOwnerSnapshot{},
	}
	if p == nil || p.cfg == nil {
		return out
	}
	p.workloadMu.Lock()
	defer p.workloadMu.Unlock()
	owners, current := p.diagnosticOwnersLocked()
	for _, w := range owners {
		w.mu.Lock()
	}
	defer func() {
		for _, owner := range slices.Backward(owners) {
			owner.mu.Unlock()
		}
	}()
	p.diagnostics.mu.Lock()
	defer p.diagnostics.mu.Unlock()
	indices := make(map[string]int, len(p.cfg.Workloads))
	for _, service := range sortedWorkloadServices(p.cfg.Workloads) {
		indices[service] = len(out.Services)
		out.Services = append(out.Services, workloadServiceSnapshot{
			Service: service, workloadServiceCounters: p.diagnostics.services[service], Owners: []workloadOwnerSnapshot{},
		})
	}
	for _, w := range owners {
		owner := w.diagnosticSnapshotLocked(current[w])
		if i, configured := indices[w.service]; configured {
			out.Services[i].Owners = append(out.Services[i].Owners, owner)
		} else if w.stop != nil {
			out.OrphanedMutations = append(out.OrphanedMutations, owner)
		}
	}
	return out
}

func sortedWorkloadServices[V any](services map[string]V) []string {
	names := make([]string, 0, len(services))
	for service := range services {
		names = append(names, service)
	}
	slices.Sort(names)
	return names
}

// Registry membership fixes this lock order: sorted current entries followed
// by retained mutation insertion order. Duplicate pointers are locked once.
func (p *dockerProvider) diagnosticOwnersLocked() ([]*workload, map[*workload]bool) {
	owners := make([]*workload, 0, len(p.workloadEntries)+len(p.retiredMutations))
	current := make(map[*workload]bool, len(p.workloadEntries))
	services := make([]string, 0, len(p.workloadEntries))
	for service := range p.workloadEntries {
		services = append(services, service)
	}
	slices.Sort(services)
	for _, service := range services {
		w := p.workloadEntries[service]
		if _, seen := current[w]; !seen {
			owners = append(owners, w)
		}
		current[w] = true
	}
	for _, w := range p.retiredMutations {
		if _, seen := current[w]; !seen {
			owners = append(owners, w)
			current[w] = false
		}
	}
	return owners, current
}

func (w *workload) diagnosticSnapshotLocked(current bool) workloadOwnerSnapshot {
	out := workloadOwnerSnapshot{Current: current, Phase: w.phase.String(), Retired: w.retired}
	if w.activation != nil {
		out.ActivationWaiters = w.activation.waiting
	}
	if w.binding != nil {
		out.Incarnation = uint64(w.binding.key)
		out.LastActivationSeconds = w.binding.diagnostics.lastActivationSeconds
		out.LastFailureReason = w.binding.diagnostics.lastFailureReason
		if !w.binding.diagnostics.lastFailureAt.IsZero() {
			at := w.binding.diagnostics.lastFailureAt
			out.LastFailureAt = &at
		}
	}
	return out
}
