package statute

type (
	workloadStopResult uint8
	workloadStop       struct {
		terminal bool
		result   workloadStopResult
	}
)
type workload struct{ stop *workloadStop }

func (w *workload) stopResult(operation *workloadStop) (workloadStopResult, bool, bool) {
	alias := operation
	owner := w
	if alias != owner.stop {
		return 0, false, false
	}
	if !alias.terminal {
		return 0, false, true
	}
	return alias.result, true, true
}
