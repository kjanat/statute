package statute

import "sync"

type workloadStop struct{}
type workload struct {
	mu   sync.Mutex
	stop *workloadStop
}
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
		if w.stop != nil {
			kept = append(kept, w)
		}
		w.mu.Unlock()
	}
	p.retiredMutations = kept
}
func discard(p *dockerProvider, w *workload) {
	p.retiredMutations = nil // want "\\[SLC107\\].*registry changes must preserve"
	p.workloadEntries = nil  // want "\\[SLC107\\].*registry changes must preserve"
	alias := p.retiredMutations
	clear(alias) // want "\\[SLC107\\].*registry may not be cleared"
	alias[0] = w // want "\\[SLC107\\].*registry changes must preserve"
	entries := p.workloadEntries
	delete(entries, "key") // want "\\[SLC107\\].*registry may not be cleared"
}

func reboundProvider(p, other *dockerProvider) {
	p = other
	p.retiredMutations = nil // want `\[SLC107\].*registry changes must preserve`
}

func compositeEscape(p *dockerProvider) {
	holder := struct{ owners []*workload }{p.retiredMutations} // want `\[SLC107\].*composite storage`
	holder.owners[0] = nil
}

func channelEscape(p *dockerProvider, ch chan []*workload) {
	ch <- p.retiredMutations // want `\[SLC107\].*through a channel`
}

func appendAlias(p *dockerProvider, extra *workload) {
	alias := append(p.retiredMutations, extra)
	alias[0] = nil // want `\[SLC107\].*registry changes must preserve`
}

func rangeOverwrite(p *dockerProvider) {
	for _, p.retiredMutations = range [][]*workload{nil} { // want `\[SLC107\].*range assignment`
	}
}

func rangeProviderOverwrite(p *dockerProvider) {
	for _, *p = range []dockerProvider{{}} { // want `\[SLC107\].*provider values may not be replaced by range`
	}
}
