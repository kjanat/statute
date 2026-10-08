package statute

type workloadPhase uint8

const workloadReady workloadPhase = 2

type (
	workloadStop struct{ uncertain bool }
	workload     struct {
		phase workloadPhase
		stop  *workloadStop
	}
)

func decrement(w *workload) {
	w.phase-- // want `\[SLC109\].*phase writes`
}

func decrementAlias(w *workload) {
	phase := &w.phase
	(*phase)-- // want `\[SLC109\].*phase writes`
}

func rangeWrite(w *workload) {
	for _, w.phase = range []workloadPhase{workloadReady} { // want `\[SLC109\].*phase writes`
	}
}

func reboundClear(w, replacement *workload) {
	w = replacement
	w.stop = nil // want `\[SLC107\].*only be cleared`
}

func elementReplacement(stops []workloadStop) {
	stops[0] = workloadStop{} // want `\[SLC108\].*values may not be replaced`
}

func acceptedAddressRead(w *workload) workloadPhase {
	phase := &w.phase
	return *phase
}

func acceptedRetirementMarker(w *workloadStop) {
	alias := w
	alias.uncertain = true
}
