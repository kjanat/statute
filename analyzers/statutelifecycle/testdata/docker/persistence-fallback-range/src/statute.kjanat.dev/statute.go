package statute

import "errors"

type (
	workloadStopKind uint8
	workloadStop     struct {
		kind      workloadStopKind
		persisted bool
		ref       string
	}
)

type (
	workload              struct{}
	workloadStopOwnership struct{ containerID string }
)

func (*workload) stopOwnershipLocked(*workloadStop) (workloadStopOwnership, bool) {
	return workloadStopOwnership{containerID: "immutable"}, true
}
func (workloadStopOwnership) currentLocked(*workload) bool { return true }

type mutationRecord struct {
	ContainerID string
	Kind        workloadStopKind
	State       string
}

const mutationRecordPrepared = "prepared"

func mutationRecordKindForStop(kind workloadStopKind) workloadStopKind { return kind }

type mutationRegistry struct{}

func (*mutationRegistry) put(mutationRecord) error { return nil }

type dockerProvider struct{ registry *mutationRegistry }

func (p *dockerProvider) currentMutationRegistry() *mutationRegistry { return p.registry }

func (p *dockerProvider) persistOwnedStop(w *workload, stop *workloadStop) error {
	owner, owned := w.stopOwnershipLocked(stop)
	if !owned {
		return errors.New("obsolete")
	}
	alias := stop
	if alias.persisted {
		return nil
	}
	identity := owner.containerID
	alias.ref = identity
	for _, alias.ref = range []string{"mutable"} { // want "\\[SLC105\\].*immutable mutation binding"
	}
	record := mutationRecord{ContainerID: identity, Kind: mutationRecordKindForStop(alias.kind), State: mutationRecordPrepared}
	registry := p.currentMutationRegistry()
	if registry == nil {
		return errors.New("unavailable")
	}
	if err := registry.put(record); err != nil {
		return err
	}
	if !owner.currentLocked(w) {
		return errors.New("obsolete")
	}
	alias.persisted = true
	return nil
}
