package statute

import "context"

type (
	workload            struct{}
	workloadStop        struct{}
	workloadStopAttempt struct{}
	workloadStopApply   uint8
	dockerProvider      struct{}
	workloadPhase       uint8
)

type evidenceApplier interface {
	applyStopAttempt(*dockerProvider, *workloadStop, workloadStopAttempt) workloadStopApply
}
type observationRecorder interface {
	recordObservedStopLocked(*workload)
}
type contextRunner interface {
	runOwnedStop(context.Context, *workload, *workloadStop)
}
type rawTransition interface {
	transitionLocked(workloadPhase) bool
}

func applyThroughInterface(applier evidenceApplier, p *dockerProvider, stop *workloadStop) {
	applier.applyStopAttempt(p, stop, workloadStopAttempt{}) // want `\[SLC108\].*evidence requires concrete typed boundaries`
}

func captureEvidence(applier evidenceApplier) {
	_ = applier.applyStopAttempt // want `\[SLC108\].*evidence requires concrete typed boundaries`
}

func observeThroughInterface(recorder observationRecorder, w *workload) {
	recorder.recordObservedStopLocked(w) // want `\[SLC108\].*evidence requires concrete typed boundaries`
}

func contextThroughInterface(runner contextRunner, ctx context.Context, w *workload, stop *workloadStop) {
	runner.runOwnedStop(ctx, w, stop) // want `\[SLC106\].*context wrappers cannot use interface dispatch or capture`
}

func captureContext(runner contextRunner) {
	_ = runner.runOwnedStop // want `\[SLC106\].*context wrappers cannot use interface dispatch or capture`
}

func captureTransition(w rawTransition) {
	_ = w.transitionLocked // want `\[SLC109\].*phase transition may not escape through an interface`
}
