package statute

import "errors"

type workloadStopKind uint8
type workloadStop struct {
	kind      workloadStopKind
	persisted bool
}
type workload struct{}
type workloadStopOwnership struct{ containerID string }

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
	record := mutationRecord{ContainerID: identity, Kind: mutationRecordKindForStop(alias.kind), State: mutationRecordPrepared}
	registry := p.currentMutationRegistry()
	if registry == nil {
		return errors.New("unavailable")
	}
	if !owner.currentLocked(w) {
		return errors.New("obsolete")
	}
	if err := (registry.put)(record); err != nil {
		return err
	}
	alias.persisted = true // want `\[SLC106\].*persisted flag requires`
	return nil             // want `\[SLC106\].*persistence success requires`
}
