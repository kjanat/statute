package statute

type workloadBindingKey uint64
type workloadBinding struct {
	key                    workloadBindingKey
	containerID, container string
}
type workloadStop struct{ binding workloadBindingKey }
type workload struct {
	stop    *workloadStop
	binding *workloadBinding
	service string
}
type workloadStopOwnership struct {
	stop                                *workloadStop
	binding                             *workloadBinding
	bindingKey                          workloadBindingKey
	containerID, containerName, service string
}

func (w *workload) stopOwnershipLocked(stop *workloadStop) (workloadStopOwnership, bool) {
	if stop == nil || w.stop != stop || w.binding == nil || w.binding.key != stop.binding {
		return workloadStopOwnership{}, false
	}
	return workloadStopOwnership{ // want `\[SLC107\].*complete stop, binding, key, and immutable ID tuple`
		stop:          stop,
		binding:       w.binding,
		bindingKey:    w.binding.key,
		containerID:   w.binding.containerID,
		containerName: w.binding.container,
		service:       w.service,
	}, true
}
func (o workloadStopOwnership) currentLocked(w *workload) bool {
	return w.stop == o.stop && w.binding == o.binding && o.binding.key == o.bindingKey && o.binding.containerID == o.containerID // want `\[SLC107\].*complete stop, binding, key, and immutable ID tuple`
}
