package statute

import (
	"testing"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func TestWorkloadRegistryDuplicateObservationPreservesOwner(t *testing.T) {
	t.Parallel()
	stop := &workloadStop{binding: 1, uncertain: true}
	w := &workload{
		service: "service", retired: true, phase: workloadStopUnknown, stop: stop,
		binding: &workloadBinding{key: 1, containerID: "immutable"},
	}
	p := &dockerProvider{workloadEntries: map[string]*workload{"service": w}}
	svc := &docker.Service{Name: "service", ContainerID: "immutable"}
	if !p.prepareWorkloadObservationLocked(svc, nil) {
		t.Fatal("current observation was rejected")
	}
	if p.workloadEntries["service"] != w || w.stop != stop || !stop.uncertain {
		t.Fatal("duplicate registration replaced the outstanding owner")
	}
}

func TestWorkloadRegistryDetachRejectsWrongOwner(t *testing.T) {
	t.Parallel()
	registered := &workload{stop: &workloadStop{uncertain: true}}
	other := &workload{stop: &workloadStop{uncertain: true}}
	p := &dockerProvider{workloadEntries: map[string]*workload{"service": registered}}
	other.mu.Lock()
	defer other.mu.Unlock()
	defer func() {
		if recover() == nil {
			t.Error("detaching a different owner did not panic")
		}
		if p.workloadEntries["service"] != registered || len(p.retiredMutations) != 0 || other.retired {
			t.Error("failed detach changed registry or retirement state")
		}
		if !registered.stop.uncertain || !other.stop.uncertain {
			t.Error("failed detach changed mutation uncertainty")
		}
	}()
	p.detachMutationOwnerHeldLocked(other, "service")
}

func TestWorkloadRegistryDetachPreservesExactPredecessor(t *testing.T) {
	t.Parallel()
	stop := &workloadStop{uncertain: true}
	old := &workload{stop: stop}
	p := &dockerProvider{cfg: &resolved.Docker{}, workloadEntries: map[string]*workload{"service": old}}
	old.mu.Lock()
	fresh := p.detachMutationOwnerHeldLocked(old, "service")
	if fresh == old || p.workloadEntries["service"] != fresh {
		t.Fatal("detach did not install a fresh registry owner")
	}
	if len(p.retiredMutations) != 1 || p.retiredMutations[0] != old || old.stop != stop || !stop.uncertain {
		t.Fatal("detach discarded the exact predecessor mutation")
	}
}
