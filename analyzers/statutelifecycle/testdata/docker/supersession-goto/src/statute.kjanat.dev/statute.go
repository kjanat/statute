package statute

type service struct{}
type workloadStop struct{}
type workload struct{ stop *workloadStop }
type dockerProvider struct{}

func (*workload) sameContainerLocked(*service) bool { return true }
func (w *workload) supersedeBindingLocked()         { w.stop = nil }
func (p *dockerProvider) bindWorkloadContainerLocked(w *workload, svc *service) {
	if w.sameContainerLocked(svc) {
		goto bypass
	}
	return
bypass:
	w.supersedeBindingLocked() // want `\[SLC107\].*only be superseded`
}
