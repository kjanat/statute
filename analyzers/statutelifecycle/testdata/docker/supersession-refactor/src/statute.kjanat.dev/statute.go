package statute

type service struct{}
type workloadStop struct{}
type workload struct{ stop *workloadStop }
type dockerProvider struct{}

func (*workload) sameContainerLocked(*service) bool { return true }
func (w *workload) supersedeBindingLocked()         { w.stop = nil }
func (p *dockerProvider) bindWorkloadContainerLocked(w *workload, svc *service) {
	alias := w
	if alias.sameContainerLocked(svc) {
		return
	}
	(w.supersedeBindingLocked)()
}
