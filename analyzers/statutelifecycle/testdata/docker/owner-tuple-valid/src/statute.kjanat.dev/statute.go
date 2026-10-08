package statute

type (
	workloadBindingKey uint64
	workloadBinding    struct {
		key                    workloadBindingKey
		containerID, container string
	}
)

type (
	workloadStop struct{ binding workloadBindingKey }
	workload     struct {
		stop    *workloadStop
		binding *workloadBinding
		service string
	}
)

type workloadStopOwnership struct {
	stop                                *workloadStop
	binding                             *workloadBinding
	bindingKey                          workloadBindingKey
	containerID, containerName, service string
}

func (w *workload) stopOwnershipLocked(stop *workloadStop) (workloadStopOwnership, bool) {
	if stop == nil || w.stop != stop || w.binding == nil || w.binding.key != stop.binding || w.binding.containerID == "" {
		return workloadStopOwnership{}, false
	}
	return workloadStopOwnership{
		stop:          stop,
		binding:       w.binding,
		bindingKey:    w.binding.key,
		containerID:   w.binding.containerID,
		containerName: w.binding.container,
		service:       w.service,
	}, true
}

func (o workloadStopOwnership) currentLocked(w *workload) bool {
	return w.stop == o.stop && w.binding == o.binding && o.binding.key == o.bindingKey &&
		o.stop.binding == o.bindingKey && o.binding.containerID == o.containerID
}
