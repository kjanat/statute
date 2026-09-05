package statute

import (
	"context"
	"time"

	"statute.kjanat.dev/internal/docker"
)

const workloadStopTimeout = time.Second

type workloadBindingKey uint64

type workloadPolicy struct {
	StartTimeout time.Duration
}

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
	sctx, cancel := context.WithTimeout(context.Background(), act.policy.StartTimeout)
	defer cancel()
	return p.client.StartContainer(sctx, w.callRef(act.binding, act.ref)) // want `\[SLC105\].*derive its timeout from the provider context`
}

func (p *dockerProvider) runActivation(_ context.Context, w *workload, act *workloadActivation) error {
	return p.startActivation(context.Background(), w, act) // want `\[SLC105\].*provider-derived tracked context`
}

func (*dockerProvider) activate(context.Context, *workload, *workloadActivation) {}

func detachedActivation(p *dockerProvider, w *workload, act *workloadActivation) {
	p.activate(context.Background(), w, act) // want `\[SLC105\].*provider-derived tracked context`
}

func (p *dockerProvider) attemptOwnedStop(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	sctx, cancel := context.WithTimeout(ctx, workloadStopTimeout)
	err := p.client.StopContainer(sctx, stop.ref) // want `\[SLC105\].*target workload.callRef`
	cancel()
	return workloadStopAttempt{err: err}
}

func (*dockerProvider) executeOwnedStopAttempt(context.Context, *workload, *workloadStop) workloadStopAttempt {
	return workloadStopAttempt{}
}

func (p *dockerProvider) runOwnedStop(_ context.Context, w *workload, stop *workloadStop) {
	_ = p.executeOwnedStopAttempt(context.Background(), w, stop) // want `\[SLC106\].*provider-derived tracked context`
}

func detachedStop(p *dockerProvider, w *workload, stop *workloadStop) {
	p.runOwnedStop(context.Background(), w, stop) // want `\[SLC106\].*provider-derived tracked context`
}

func (*dockerProvider) finishActivation(context.Context, *workload, *workloadActivation) {}
func (*dockerProvider) performStop(context.Context, *workload, *workloadStop)            {}

func detachedBridges(p *dockerProvider, w *workload, act *workloadActivation, stop *workloadStop) {
	p.finishActivation(context.Background(), w, act) // want `\[SLC106\].*provider-derived tracked context`
	p.performStop(context.Background(), w, stop)     // want `\[SLC106\].*provider-derived tracked context`
}
