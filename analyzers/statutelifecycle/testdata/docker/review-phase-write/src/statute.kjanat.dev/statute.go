package statute

type workloadPhase uint8

const workloadReady workloadPhase = 2

type workload struct{ phase workloadPhase }

func (w *workload) transitionLocked(next workloadPhase) bool {
	_ = next
	w.phase = workloadReady // want `\[SLC109\].*preserve its requested phase`
	return true
}
