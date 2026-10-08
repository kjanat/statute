package statute

import "statute.kjanat.dev/internal/docker"

type (
	workloadBinding struct{ containerID, container string }
	workload        struct{ binding *workloadBinding }
)

func (b *workloadBinding) sameContainer(svc *docker.Service) bool {
	return b.container == svc.Container // want "\\[SLC107\\].*container identity helper"
}

func (w *workload) sameContainerLocked(svc *docker.Service) bool {
	if w.binding == nil {
		return false
	}
	return w.binding.sameContainer(svc)
}
