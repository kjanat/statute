package statute

import (
	"context"
	"time"

	"statute.kjanat.dev/internal/docker"
)

const (
	workloadStopTimeout  = time.Second
	workloadProbeTimeout = time.Second
)

type (
	workloadBindingKey uint64
	workloadStop       struct {
		binding workloadBindingKey
		ref     string
	}
)
type workload struct{}

func (*workload) callRef(workloadBindingKey, string) string { return "immutable" }

type (
	dockerProvider     struct{ client *docker.Client }
	workloadStopResult uint8
)

const (
	workloadStopSucceeded workloadStopResult = iota
	workloadStopRejected
	workloadStopAmbiguous
)

type workloadStopAttempt struct {
	result              workloadStopResult
	stopErr, inspectErr error
}

func (p *dockerProvider) attemptOwnedStop(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	stopRef := w.callRef(stop.binding, stop.ref)
	sctx, cancel := context.WithTimeout(ctx, workloadStopTimeout)
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

	ictx, icancel := context.WithTimeout(ctx, workloadProbeTimeout)
	insp, inspectErr := p.client.InspectContainer(ictx, "different-container") // want `\[SLC108\].*same provider, immutable operation target`
	icancel()
	if inspectErr == nil && !insp.Running {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err}
	}
	if docker.LifecycleContainerMissing(inspectErr) {
		return workloadStopAttempt{result: workloadStopSucceeded, stopErr: err, inspectErr: inspectErr}
	}
	return workloadStopAttempt{result: workloadStopAmbiguous, stopErr: err, inspectErr: inspectErr}
}
