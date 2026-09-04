package statute

import "errors"

type workloadBindingKey uint64
type workloadStopResult uint8
type workloadStopApply uint8
type workloadStopAttempt struct{ result workloadStopResult }

type workloadStop struct {
	binding workloadBindingKey
	done    chan struct{}
}

type workloadStopOwnership struct {
	containerID string
	service     string
	bindingKey  workloadBindingKey
}

func (workloadStopOwnership) currentLocked(*workload) bool { return true }

type workload struct{ stop *workloadStop }

func (*workload) stopOwnershipLocked(stop *workloadStop) (workloadStopOwnership, bool) {
	return workloadStopOwnership{containerID: "container-id", service: "service", bindingKey: stop.binding}, true
}

func (w *workload) settleStopLocked(*dockerProvider, *workloadStop, workloadStopResult) {
	stop := w.stop
	w.stop = nil
	close(stop.done)
}

func (*workload) sameContainerLocked(*service) bool { return true }

func (w *workload) supersedeBindingLocked() {
	stop := w.stop
	w.stop = nil
	close(stop.done)
}

type mutationRegistry struct{}

func (*mutationRegistry) delete(string) error { return nil }

type dockerProvider struct{ registry *mutationRegistry }

func (p *dockerProvider) currentMutationRegistry() *mutationRegistry   { return p.registry }
func (*dockerProvider) invalidateWorkloadObservationsLocked(*workload) {}
func (*dockerProvider) invalidateStoppedGeneration(string, workloadBindingKey, workloadStopResult) {
}
func (*dockerProvider) scheduleReconcile() {}

func (w *workload) applyStopAttempt(p *dockerProvider, stop *workloadStop, attempt workloadStopAttempt) workloadStopApply {
	owner, owned := w.stopOwnershipLocked(stop)
	if !owned {
		return 0
	}
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
	w.settleStopLocked(p, stop, result) // want `\[SLC107\].*reconcile scheduling`
	return 1
}

func rawDelete(registry *mutationRegistry) {
	_ = registry.delete("container-id") // want `\[SLC107\].*only be deleted by.*applyStopAttempt`
}

func rawSettle(w *workload, p *dockerProvider, stop *workloadStop) {
	w.settleStopLocked(p, stop, 0) // want `\[SLC107\].*only settle through.*applyStopAttempt`
}

func rawRelease(w *workload, stop *workloadStop) {
	w.stop = nil     // want `\[SLC107\].*workload.stop may only be cleared`
	close(stop.done) // want `\[SLC107\].*waiters may only be released`
}

func aliasedRelease(w *workload, stop *workloadStop) {
	done := stop.done
	close(done)                   // want `\[SLC107\].*waiters may only be released`
	w.stop = (*workloadStop)(nil) // want `\[SLC107\].*workload.stop may only be cleared`
}

func declaredAliasRelease(w *workload, stop *workloadStop) {
	var done = stop.done
	close(done) // want `\[SLC107\].*waiters may only be released`
	var zero *workloadStop
	w.stop = zero   // want `\[SLC107\].*workload.stop may only be cleared`
	*w = workload{} // want `\[SLC107\].*workload values may not be replaced`
}

func escapedRelease(w *workload, registry *mutationRegistry) {
	remove := registry.delete    // want `\[SLC107\].*deletion must remain a direct canonical settlement call`
	settle := w.settleStopLocked // want `\[SLC107\].*release must remain a direct canonical call`
	_, _ = remove, settle
}

type service struct{}

func badSupersession(w *workload) {
	w.supersedeBindingLocked() // want `\[SLC107\].*only be superseded after sameContainerLocked`
}

func goodSupersession(w *workload, svc *service) {
	if !w.sameContainerLocked(svc) {
		w.supersedeBindingLocked()
	}
}

type mutationDeleter interface{ delete(string) error }

func interfaceDelete(registry mutationDeleter) {
	_ = registry.delete("container-id") // want `\[SLC107\].*concrete mutationRegistry method`
}

type workloadSettler interface {
	settleStopLocked(*dockerProvider, *workloadStop, workloadStopResult)
}

func interfaceSettle(w workloadSettler, p *dockerProvider, stop *workloadStop) {
	w.settleStopLocked(p, stop, 0) // want `\[SLC107\].*concrete workload method`
}
