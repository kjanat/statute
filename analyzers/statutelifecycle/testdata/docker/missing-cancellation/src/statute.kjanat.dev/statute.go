package statute

import (
	"context"
	"time"

	"statute.kjanat.dev/internal/docker"
)

const workloadStopTimeout = time.Second

type workloadBindingKey uint64
type workloadPolicy struct{ StartTimeout time.Duration }
type workloadActivation struct {
	policy  workloadPolicy
	binding workloadBindingKey
	ref     string
}
type workloadStop struct {
	binding workloadBindingKey
	ref     string
}
type workloadStopAttempt struct{ err error }
type workload struct{}

func (*workload) callRef(workloadBindingKey, string) string { return "container-id" }

type dockerProvider struct{ client *docker.Client }

func (p *dockerProvider) startActivation(ctx context.Context, w *workload, act *workloadActivation) error {
	sctx, cancel := context.WithTimeout(ctx, act.policy.StartTimeout)
	unused := func() { defer cancel() }
	_ = unused
	return p.client.StartContainer(sctx, w.callRef(act.binding, act.ref)) // want `\[SLC105\].*cancel its bounded context`
}

func (p *dockerProvider) attemptOwnedStop(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	stopRef := w.callRef(stop.binding, stop.ref)
	sctx, cancel := context.WithTimeout(ctx, workloadStopTimeout)
	err := p.client.StopContainer(sctx, stopRef) // want `\[SLC105\].*cancel its bounded context`
	_ = cancel
	return workloadStopAttempt{err: err}
}
