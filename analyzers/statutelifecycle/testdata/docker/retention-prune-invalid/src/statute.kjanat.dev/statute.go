package statute

import "sync"

type (
	workloadStop struct{}
	workload     struct {
		mu   sync.Mutex
		stop *workloadStop
	}
)

type dockerProvider struct {
	retiredMutations []*workload
	workloadEntries  map[string]*workload
}
type service struct{ Name string }

func (p *dockerProvider) prepareWorkloadObservationLocked(svc *service) {
	if p.workloadEntries == nil {
		p.workloadEntries = map[string]*workload{}
	}
	if p.workloadEntries[svc.Name] != nil {
		panic("existing owner")
	}
	p.workloadEntries[svc.Name] = &workload{}
}

func (p *dockerProvider) detachMutationOwnerHeldLocked(old *workload, service string) {
	if p.workloadEntries[service] != old {
		panic("different owner")
	}
	p.retiredMutations = append(p.retiredMutations, old)
	p.workloadEntries[service] = &workload{}
}

func (p *dockerProvider) retiredMutationContainerRefsLocked() {
	kept := p.retiredMutations[:0]
	for _, w := range p.retiredMutations {
		w.mu.Lock()
		if w.stop == nil {
			kept = append(kept, w)
		}
		w.mu.Unlock()
	}
	p.retiredMutations = kept // want `\[SLC107\].*registry changes must preserve`
}
