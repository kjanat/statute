package statute

type workloadBindingKey uint64
type workloadStopResult uint8
type workloadStopApply uint8
type workloadStopAttempt struct{ result workloadStopResult }
type workloadStop struct{ binding workloadBindingKey }

type workloadStopOwnership struct {
	containerID string
	service     string
	bindingKey  workloadBindingKey
}

func (workloadStopOwnership) currentLocked(*workload) bool { return true }

type workload struct{}

func (*workload) settleStopLocked(*dockerProvider, *workloadStop, workloadStopResult) {}

type mutationRegistry struct{}

func (*mutationRegistry) delete(string) error { return nil }

type dockerProvider struct{ registry *mutationRegistry }

func (p *dockerProvider) currentMutationRegistry() *mutationRegistry   { return p.registry }
func (*dockerProvider) invalidateWorkloadObservationsLocked(*workload) {}
func (*dockerProvider) invalidateStoppedGeneration(string, workloadBindingKey, workloadStopResult) {
}
func (*dockerProvider) scheduleReconcile() {}

func (w *workload) applyStopAttempt(p *dockerProvider, stop *workloadStop, attempt workloadStopAttempt) workloadStopApply {
	owner := workloadStopOwnership{containerID: "container-id", service: "service", bindingKey: stop.binding}
	result := attempt.result
	registry := p.currentMutationRegistry()
	persistErr := registry.delete(owner.containerID)
	if !owner.currentLocked(w) {
		return 0
	}
	if persistErr != nil {
		return 0
	}
	p.invalidateWorkloadObservationsLocked(w)
	p.invalidateStoppedGeneration(owner.service, owner.bindingKey, result)
	w.settleStopLocked(p, stop, result) // want `\[SLC107\].*owner revalidation`
	p.scheduleReconcile()
	return 1
}
