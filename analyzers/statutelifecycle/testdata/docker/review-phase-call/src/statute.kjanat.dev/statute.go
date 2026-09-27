package statute

type workloadPhase uint8

const (
	workloadDormant workloadPhase = iota
	workloadStarting
	workloadReady
	workloadStopPending
	workloadStopIssued
	workloadStopUnknown
	workloadFailed
)

type workloadStop struct{}
type workload struct {
	stop  *workloadStop
	phase workloadPhase
	other *workload
}

func (w *workload) transitionLocked(next workloadPhase) bool {
	w.phase = next
	return true
}

func (w *workload) toLocked(next workloadPhase) bool {
	alias := w
	if alias.stop != nil && next != workloadStopIssued && next != workloadStopUnknown {
		return false
	}
	if w.other != nil {
		return w.other.transitionLocked(next) // want `\[SLC109\].*raw phase transition`
	}
	return w.transitionLocked(workloadReady) // want `\[SLC109\].*raw phase transition`
}
