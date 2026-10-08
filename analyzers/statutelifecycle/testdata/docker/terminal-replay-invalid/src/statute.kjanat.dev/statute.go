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
	if operation != w.stop {
		return 0, false, false
	}
	return operation.result, true, true // want "\\[SLC108\\].*terminal replay requires"
}
