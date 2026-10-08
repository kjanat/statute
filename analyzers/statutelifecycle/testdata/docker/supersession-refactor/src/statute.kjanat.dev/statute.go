package statute

type (
	service        struct{}
	workloadStop   struct{}
	workload       struct{ stop *workloadStop }
	dockerProvider struct{}
)

func (*workload) sameContainerLocked(*service) bool { return true }
func (w *workload) supersedeBindingLocked()         { w.stop = nil }
func (p *dockerProvider) bindWorkloadContainerLocked(w *workload, svc *service) {
	alias := w
	if alias.sameContainerLocked(svc) {
		return
	}
	w.supersedeBindingLocked()
}
