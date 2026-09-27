package statute

import "statute.kjanat.dev/internal/docker"

type workloadBindingKey uint64
type workloadBinding struct {
	key                    workloadBindingKey
	containerID, container string
}
type workloadStop struct {
	binding workloadBindingKey
	ref     string
}
type workload struct{ binding *workloadBinding }
type dockerProvider struct{}

func (b *workloadBinding) sameContainer(svc *docker.Service) bool {
	if b.containerID != "" && svc.ContainerID != "" {
		return b.containerID == svc.ContainerID
	}
	return b.container == svc.Container
}
func (w *workload) sameContainerLocked(svc *docker.Service) bool {
	return w.binding != nil && w.binding.sameContainer(svc)
}
func (w *workload) supersedeBindingLocked()                           {}
func (*dockerProvider) nextWorkloadBindingLocked() workloadBindingKey { return 1 }
func (p *dockerProvider) newWorkloadBindingLocked(w *workload, svc *docker.Service) {
	w.binding = &workloadBinding{key: p.nextWorkloadBindingLocked(), container: svc.Container, containerID: svc.ContainerID}
}
func (b *workloadBinding) observe(svc *docker.Service) {
	b.container = svc.Container
	id := svc.ContainerID
	if id == "" {
		return
	}
	b.containerID = id
}
func (p *dockerProvider) bindWorkloadContainerLocked(w *workload, svc *docker.Service) bool {
	if w.binding == nil {
		p.newWorkloadBindingLocked(w, svc)
		return false
	}
	if w.sameContainerLocked(svc) {
		w.binding.observe(svc)
		return false
	}
	w.supersedeBindingLocked()
	p.newWorkloadBindingLocked(w, svc)
	return true
}
