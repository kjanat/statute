package statute

type (
	workloadStop    struct{ issued bool }
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
type dockerProvider struct{}

func (*dockerProvider) recordObservedStopLocked(*workload) {}

func (p *dockerProvider) reconcileRetiredMutationObservationLocked(w *workload, containers []container) bool {
	if w.stop.issued {
		return true
	}
	p.recordObservedStopLocked(w) // want `\[SLC108\].*canonical stopped or missing observation`
	unrelated := []container{}
	for _, c := range unrelated {
		if c.ID == w.binding.containerID && c.Running {
			return true
		}
	}
	p.recordObservedStopLocked(w) // want `\[SLC108\].*canonical stopped or missing observation`
	_ = containers
	return true
}
