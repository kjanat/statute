package statute

import "statute.kjanat.dev/internal/docker"

type (
	workloadBinding struct{ containerID, container string }
	workload        struct{ binding *workloadBinding }
)

func (b *workloadBinding) sameContainer(svc *docker.Service) bool {
	identity := b
	observed := svc
	if identity.containerID == "" || observed.ContainerID == "" {
		return identity.container == observed.Container
	}
	return identity.containerID == observed.ContainerID
}

func (w *workload) sameContainerLocked(svc *docker.Service) bool {
	if w.binding == nil {
		return false
	}
	return w.binding.sameContainer(svc)
}
