package statute

import "context"

type workload struct{}
type workloadStop struct{}
type workloadStopAttempt struct{}
type dockerProvider struct{}

func (*dockerProvider) persistOwnedStop(*workload, *workloadStop) error { return nil }
func (*dockerProvider) attemptOwnedStop(context.Context, *workload, *workloadStop) workloadStopAttempt {
	return workloadStopAttempt{}
}

func (p *dockerProvider) good(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	if err := p.persistOwnedStop(w, stop); err != nil {
		return workloadStopAttempt{}
	}
	return p.attemptOwnedStop(ctx, w, stop)
}

func (p *dockerProvider) missing(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	return p.attemptOwnedStop(ctx, w, stop) // want `\[SLC106\].*dominated by successful persistOwnedStop`
}

func (p *dockerProvider) conditional(ctx context.Context, w *workload, stop *workloadStop, persist bool) workloadStopAttempt {
	if persist {
		if err := p.persistOwnedStop(w, stop); err != nil {
			return workloadStopAttempt{}
		}
	}
	return p.attemptOwnedStop(ctx, w, stop) // want `\[SLC106\].*dominated by successful persistOwnedStop`
}

func (p *dockerProvider) failureEdge(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	if err := p.persistOwnedStop(w, stop); err != nil {
		return p.attemptOwnedStop(ctx, w, stop) // want `\[SLC106\].*dominated by successful persistOwnedStop`
	}
	return workloadStopAttempt{}
}

func (p *dockerProvider) detachedContext(_ context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	if err := p.persistOwnedStop(w, stop); err != nil {
		return workloadStopAttempt{}
	}
	return p.attemptOwnedStop(context.Background(), w, stop) // want `\[SLC106\].*dominated by successful persistOwnedStop`
}

func (p *dockerProvider) delayedAttempt(ctx context.Context, w *workload, stop *workloadStop) func() workloadStopAttempt {
	if err := p.persistOwnedStop(w, stop); err != nil {
		return func() workloadStopAttempt { return workloadStopAttempt{} }
	}
	return func() workloadStopAttempt {
		return p.attemptOwnedStop(ctx, w, stop) // want `\[SLC106\].*dominated by successful persistOwnedStop`
	}
}

func (p *dockerProvider) wrongOwner(ctx context.Context, w, other *workload, stop *workloadStop) workloadStopAttempt {
	if err := p.persistOwnedStop(other, stop); err != nil {
		return workloadStopAttempt{}
	}
	return p.attemptOwnedStop(ctx, w, stop) // want `\[SLC106\].*dominated by successful persistOwnedStop`
}

func (p *dockerProvider) escaped() {
	attempt := p.attemptOwnedStop // want `\[SLC106\].*must be called directly`
	_ = attempt
}

func (p *dockerProvider) gotoFailure(ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	if err := p.persistOwnedStop(w, stop); err != nil {
		goto attempt
		return workloadStopAttempt{}
	}
	return workloadStopAttempt{}

attempt:
	return p.attemptOwnedStop(ctx, w, stop) // want `\[SLC106\].*dominated by successful persistOwnedStop`
}

type stopAttemptInterface interface {
	attemptOwnedStop(context.Context, *workload, *workloadStop) workloadStopAttempt
}

func interfaceAttempt(p stopAttemptInterface, ctx context.Context, w *workload, stop *workloadStop) workloadStopAttempt {
	return p.attemptOwnedStop(ctx, w, stop) // want `\[SLC106\].*concrete dockerProvider method`
}
