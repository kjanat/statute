package statute

import (
	"context"
	"errors"
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
	done    chan struct{}
}

type workloadStopResult uint8
type workloadStopApply uint8

type workloadStopAttempt struct {
	result workloadStopResult
	err    error
}

type workloadStopOwnership struct {
	containerID string
	service     string
	bindingKey  workloadBindingKey
}

func (o workloadStopOwnership) currentLocked(*workload) bool { return true }

type workload struct {
	stop *workloadStop
}

func (*workload) callRef(workloadBindingKey, string) string { return "container-id" }
func (*workload) stopOwnershipLocked(stop *workloadStop) (workloadStopOwnership, bool) {
	return workloadStopOwnership{containerID: "container-id", service: "service", bindingKey: stop.binding}, true
}

func (w *workload) settleStopLocked(*dockerProvider, *workloadStop, workloadStopResult) {
	stop := w.stop
	w.stop = nil
	close(stop.done)
}

type mutationRegistry struct{}

func (*mutationRegistry) delete(string) error { return nil }

type dockerProvider struct {
	client   *docker.Client
	registry *mutationRegistry
}

func (p *dockerProvider) currentMutationRegistry() *mutationRegistry { return p.registry }
func (*dockerProvider) persistOwnedStop(*workload, *workloadStop) error {
	return nil
}
func (*dockerProvider) invalidateWorkloadObservationsLocked(*workload) {}
func (*dockerProvider) invalidateStoppedGeneration(string, workloadBindingKey, workloadStopResult) {
}
func (*dockerProvider) scheduleReconcile() {}

func (p *dockerProvider) startActivation(ctx context.Context, w *workload, act *workloadActivation) error {
	sctx, cancel := context.WithTimeout(ctx, act.policy.StartTimeout)
	defer cancel()
	return p.client.StartContainer(sctx, w.callRef(act.binding, act.ref))
}

func (p *dockerProvider) attemptOwnedStop(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	stopRef := w.callRef(stop.binding, stop.ref)
	sctx, cancel := context.WithTimeout(ctx, workloadStopTimeout)
	err := p.client.StopContainer(sctx, stopRef)
	cancel()
	return workloadStopAttempt{err: err}
}

func (p *dockerProvider) executeOwnedStopAttempt(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	if err := p.persistOwnedStop(w, stop); err != nil {
		return workloadStopAttempt{err: err}
	}
	return p.attemptOwnedStop(ctx, w, stop)
}

func (w *workload) applyStopAttempt(p *dockerProvider, stop *workloadStop, attempt workloadStopAttempt) workloadStopApply {
	owner, owned := w.stopOwnershipLocked(stop)
	if !owned {
		return 0
	}
	result := attempt.result
	registry := p.currentMutationRegistry()
	var persistErr error
	if registry == nil {
		persistErr = errors.New("mutation registry unavailable")
	} else {
		persistErr = registry.delete(owner.containerID)
	}
	if !owner.currentLocked(w) {
		return 0
	}
	if persistErr != nil {
		return 0
	}
	p.invalidateWorkloadObservationsLocked(w)
	p.invalidateStoppedGeneration(owner.service, owner.bindingKey, result)
	w.settleStopLocked(p, stop, result)
	p.scheduleReconcile()
	return 1
}
