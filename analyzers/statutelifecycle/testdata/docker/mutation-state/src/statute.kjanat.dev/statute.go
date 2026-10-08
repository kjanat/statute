package statute

import (
	"context"
	"errors"
	"time"

	"statute.kjanat.dev/internal/docker"
)

const (
	workloadStopTimeout  = time.Second
	workloadProbeTimeout = time.Second
)

type workloadBindingKey uint64

type workloadPolicy struct {
	StartTimeout time.Duration
}

type workloadActivation struct {
	policy  workloadPolicy
	binding workloadBindingKey
	ref     string
}

type workloadStop struct {
	uncertain bool
	terminal  bool
	result    workloadStopResult
	binding   workloadBindingKey
	ref       string
	done      chan struct{}
}

type workloadStopResult uint8

const (
	workloadStopSucceeded workloadStopResult = iota
	workloadStopRejected
	workloadStopAmbiguous
)

const (
	workloadStopObsolete workloadStopApply = iota
	workloadStopUnsettled
	workloadStopSettled
)

type workloadStopApply uint8

type workloadStopAttempt struct {
	stopErr, inspectErr error
	result              workloadStopResult
	err                 error
}

type workloadStopOwnership struct {
	containerID string
	service     string
	bindingKey  workloadBindingKey
}

func (o workloadStopOwnership) currentLocked(*workload) bool { return true }

type workload struct {
	stop    *workloadStop
	phase   workloadPhase
	retired bool
}

func (*workload) callRef(workloadBindingKey, string) string { return "container-id" }
func (*workload) stopOwnershipLocked(stop *workloadStop) (workloadStopOwnership, bool) {
	return workloadStopOwnership{containerID: "container-id", service: "service", bindingKey: stop.binding}, true
}

func (w *workload) settleStopLocked(*dockerProvider, *workloadStop, workloadStopResult) {
	stop := w.stop
	w.stop = nil
	close(stop.done)
}

type mutationRegistry struct{}

func (*mutationRegistry) delete(string) error { return nil }

type dockerProvider struct {
	client   *docker.Client
	registry *mutationRegistry
}

func (p *dockerProvider) currentMutationRegistry() *mutationRegistry { return p.registry }
func (*dockerProvider) persistOwnedStop(*workload, *workloadStop) error {
	return nil
}
func (*dockerProvider) invalidateWorkloadObservationsLocked(*workload) {}
func (*dockerProvider) invalidateStoppedGeneration(string, workloadBindingKey, workloadStopResult) {
}
func (*dockerProvider) scheduleReconcile() {}

func (p *dockerProvider) startActivation(ctx context.Context, w *workload, act *workloadActivation) error {
	sctx, cancel := context.WithTimeout(ctx, act.policy.StartTimeout)
	defer cancel()
	return p.client.StartContainer(sctx, w.callRef(act.binding, act.ref))
}

func (p *dockerProvider) runActivation(ctx context.Context, w *workload, act *workloadActivation) error {
	return p.startActivation(ctx, w, act)
}

func (p *dockerProvider) activate(ctx context.Context, w *workload, act *workloadActivation) {
	_ = p.runActivation(ctx, w, act)
	p.finishActivation(ctx, w, act)
}

func (p *dockerProvider) finishActivation(ctx context.Context, w *workload, _ *workloadActivation) {
	p.runOwnedStop(ctx, w, w.stop)
}

func (p *dockerProvider) attemptOwnedStop(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	stopRef := w.callRef(stop.binding, stop.ref)
	sctx, cancel := context.WithTimeout(ctx, workloadStopTimeout)
	err := p.client.StopContainer(sctx, stopRef)
	cancel()
	if err == nil {
		return workloadStopAttempt{result: workloadStopSucceeded}
	}
	if docker.LifecycleContainerMissing(err) {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err}
	}
	if !docker.LifecycleOutcomeAmbiguous(err) {
		return workloadStopAttempt{result: workloadStopRejected, stopErr: err}
	}

	inspectRef := w.callRef(stop.binding, stop.ref)
	ictx, icancel := context.WithTimeout(ctx, workloadProbeTimeout)
	insp, inspectErr := p.client.InspectContainer(ictx, inspectRef)
	icancel()
	if inspectErr == nil && !insp.Running {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err}
	}
	if docker.LifecycleContainerMissing(inspectErr) {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err, inspectErr: inspectErr}
	}
	return workloadStopAttempt{result: workloadStopAmbiguous, stopErr: err, inspectErr: inspectErr}
}

func (p *dockerProvider) executeOwnedStopAttempt(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	if err := p.persistOwnedStop(w, stop); err != nil {
		return workloadStopAttempt{err: err}
	}
	return p.attemptOwnedStop(ctx, w, stop)
}

func (p *dockerProvider) runOwnedStop(ctx context.Context, w *workload, stop *workloadStop) {
	_ = p.executeOwnedStopAttempt(ctx, w, stop)
}

type dockerRun struct{}

func (*dockerRun) track(fn func(context.Context)) bool {
	fn(context.Background())
	return true
}

func (p *dockerProvider) scheduleStop(w *workload, stop *workloadStop) {
	run := &dockerRun{}
	run.track(func(ctx context.Context) { p.performStop(ctx, w, stop) })
}

func (p *dockerProvider) performStop(ctx context.Context, w *workload, stop *workloadStop) {
	p.runOwnedStop(ctx, w, stop)
}

func (w *workload) applyStopAttempt(p *dockerProvider, stop *workloadStop, attempt workloadStopAttempt) workloadStopApply {
	owner, owned := w.stopOwnershipLocked(stop)
	if !owned {
		return 0
	}
	if attempt.result == workloadStopAmbiguous {
		stop.uncertain = true
		return workloadStopUnsettled
	}
	if attempt.result == workloadStopRejected && stop.uncertain {
		return workloadStopUnsettled
	}
	stop.terminal = true
	stop.result = attempt.result
	result := attempt.result
	registry := p.currentMutationRegistry()
	var persistErr error
	if registry == nil {
		persistErr = errors.New("mutation registry unavailable")
	} else {
		persistErr = registry.delete(owner.containerID)
	}
	if !owner.currentLocked(w) {
		return 0
	}
	if persistErr != nil {
		return 0
	}
	p.invalidateWorkloadObservationsLocked(w)
	p.invalidateStoppedGeneration(owner.service, owner.bindingKey, result)
	w.settleStopLocked(p, stop, result)
	p.scheduleReconcile()
	return 1
}

type workloadPhase uint8

const (
	workloadDormant workloadPhase = iota
	workloadStarting
	workloadReady
	workloadStopPending
	workloadStopIssued
	workloadStopUnknown
	workloadFailed
)

func (w *workload) toLocked(next workloadPhase) bool {
	alias := w
	if (alias.stop != nil) && next != workloadStopIssued && next != workloadStopUnknown {
		return false
	}
	return w.transitionLocked(next)
}
func (w *workload) transitionLocked(next workloadPhase) bool { w.phase = next; return true }
func retireGrant(w *workload)                                { w.retired = true }
func acceptedMonotonicAlias(stop *workloadStop)              { alias := stop; more := alias; more.uncertain = true }

func badRetirement(w *workload) {
	w.retired = true
	w.stop = &workloadStop{} // want "\\[SLC107\\].*installing a stop"
}

func badUncertainty(stop *workloadStop) {
	alias := stop
	alias.uncertain = false // want "\\[SLC108\\].*uncertainty is monotonic"
}
func badTerminal(stop *workloadStop) { stop.terminal = true } // want "\\[SLC108\\].*terminal evidence"
func badRetiredPhase(w *workload) {
	w.retired = true
	w.phase = workloadReady // want "\\[SLC109\\].*phase writes"
}
func badDirectTransition(w *workload)        { w.transitionLocked(workloadDormant) } // want "\\[SLC109\\].*raw phase transition"
func badOperationReplace(stop *workloadStop) { *stop = workloadStop{} }              // want "\\[SLC108\\].*values may not be replaced"
func clearBool(v *bool)                      { *v = false }
func escapedUncertainty(stop *workloadStop)  { clearBool(&stop.uncertain) } // want "\\[SLC107\\].*address may not escape"
func chainedEscape(w *workload) {
	ptr := &w.stop
	alias := ptr
	clearOwner(alias) // want "\\[SLC107\\].*address alias may not escape"
}
func clearOwner(v **workloadStop)                    { *v = nil }
func goodAddressRead(stop *workloadStop) bool        { ptr := &stop.uncertain; return *ptr }
func badAddressWrite(stop *workloadStop)             { ptr := &stop.uncertain; *ptr = false } // want "\\[SLC108\\].*uncertainty is monotonic"
func parenthesizedDelete(registry *mutationRegistry) { _ = registry.delete("id") }            // want "\\[SLC107\\].*only be deleted"
func arbitraryContext(ctx context.Context, p *dockerProvider, w *workload) {
	p.runOwnedStop(ctx, w, w.stop) // want "\\[SLC106\\].*provider-derived tracked context"
}
