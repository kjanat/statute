package statute

type workloadStopResult uint8

const (
	workloadStopSucceeded workloadStopResult = iota
	workloadStopRejected
	workloadStopAmbiguous
)

type workloadStop struct {
	issued, terminal, uncertain bool
	result                      workloadStopResult
}
type (
	workloadBinding struct{ containerID string }
	workload        struct {
		stop    *workloadStop
		binding *workloadBinding
	}
)

type container struct {
	ID      string
	Running bool
}
type (
	service        struct{ Running bool }
	dockerProvider struct{}
)

func (p *dockerProvider) recordObservedStopLocked(w *workload) {
	stop := w.stop
	stop.terminal = true
	stop.result = workloadStopSucceeded
}

func (p *dockerProvider) observeStoppedWorkloadLocked(w *workload) {
	if w.stop.issued {
		return
	}
	p.recordObservedStopLocked(w)
}

func (p *dockerProvider) observeWorkloadLocked(w *workload, svc *service) {
	if svc.Running {
		return
	}
	p.observeStoppedWorkloadLocked(w)
}

func (p *dockerProvider) reconcileRetiredMutationObservationLocked(w *workload, containers []container) {
	if w.stop.issued {
		return
	}
	for _, candidate := range containers {
		if candidate.ID == w.binding.containerID {
			if candidate.Running {
				return
			}
			p.recordObservedStopLocked(w)
			return
		}
	}
	p.recordObservedStopLocked(w)
}
func badRetirement(p *dockerProvider, w *workload) { p.recordObservedStopLocked(w) } // want "\\[SLC108\\].*canonical stopped or missing"
