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
	_, owned := w.stopOwnershipLocked(stop)
	if !owned {
		return errors.New("obsolete")
	}
	stop.persisted = true // want "\\[SLC106\\].*persisted flag requires"
	if stop.persisted {
		return nil
	}
	return errors.New("unreachable")
}
