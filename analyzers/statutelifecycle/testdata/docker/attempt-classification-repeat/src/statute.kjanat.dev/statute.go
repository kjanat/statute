package statute

import (
	"context"
	"time"

	"statute.kjanat.dev/internal/docker"
)

const workloadStopTimeout = time.Second
const workloadProbeTimeout = time.Second

type workloadBindingKey uint64
type workloadStop struct {
	binding workloadBindingKey
	ref     string
}
type workload struct{}

func (*workload) callRef(workloadBindingKey, string) string { return "immutable" }

type dockerProvider struct{ client *docker.Client }
type workloadStopResult uint8

const (
	workloadStopSucceeded workloadStopResult = iota
	workloadStopRejected
	workloadStopAmbiguous
)

type workloadStopAttempt struct {
	result              workloadStopResult
	stopErr, inspectErr error
}

func (p *dockerProvider) attemptOwnedStop(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt { // want `\[SLC108\].*exactly one Docker mutation`
	stopRef := w.callRef(stop.binding, stop.ref)
	sctx, cancel := context.WithTimeout(ctx, workloadStopTimeout)
	_ = p.client.StopContainer(sctx, stopRef)
	err := p.client.StopContainer(sctx, stopRef)
	cancel()
	if err == nil {
		return workloadStopAttempt{result: workloadStopSucceeded}
	}
	if docker.LifecycleContainerMissing(err) {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err}
	}
	if !docker.LifecycleOutcomeAmbiguous(err) {
		return workloadStopAttempt{result: workloadStopRejected, stopErr: err}
	}

	inspectRef := w.callRef(stop.binding, stop.ref)
	ictx, icancel := context.WithTimeout(ctx, workloadProbeTimeout)
	insp, inspectErr := p.client.InspectContainer(ictx, inspectRef)
	icancel()
	if inspectErr == nil && !insp.Running {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err}
	}
	if docker.LifecycleContainerMissing(inspectErr) {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err, inspectErr: inspectErr}
	}
	return workloadStopAttempt{result: workloadStopAmbiguous, stopErr: err, inspectErr: inspectErr}
}
