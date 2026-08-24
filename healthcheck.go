package statute

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"statute.kjanat.dev/resolved"
)

// healthChecker probes each backend on an interval and toggles its healthy
// flag according to consecutive success/failure thresholds.
type healthChecker struct {
	cfg      resolved.HealthCheck
	host     string
	backends []*backendState
	client   *http.Client
}

// healthRun owns one generation of a health checker's cancellation,
// completion, and threshold counters. Keeping those fields off the reusable
// healthChecker prevents a stopped Start attempt from cancelling or updating a
// later attempt.
type healthRun struct {
	checker *healthChecker

	mu        sync.Mutex
	successes map[*backendState]int
	failures  map[*backendState]int

	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	active   atomic.Bool
}

// newHealthChecker builds a prober whose client rides the given transport —
// the pool hands over its proxy transport, so probes verify backend TLS under
// exactly the policy proxied requests use. A nil transport means Go's
// default. A non-empty host is the probe Host newPoolHandler derived —
// HealthCheck.Host when set, else the pool's explicit Host policy — carried
// on every probe; empty leaves probes on each backend's own host.
func newHealthChecker(cfg resolved.HealthCheck, backends []*backendState, transport http.RoundTripper, host string) *healthChecker {
	return &healthChecker{
		cfg:      cfg,
		host:     host,
		backends: backends,
		client: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: transport,
		},
	}
}

func (h *healthChecker) start() *healthRun {
	r := &healthRun{
		checker:   h,
		successes: make(map[*backendState]int),
		failures:  make(map[*backendState]int),
	}
	r.active.Store(true)
	if !h.cfg.Enabled || len(h.backends) == 0 {
		return r
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	// Reset counters and health bits: a restart follows a failed Start,
	// whose backends never served, so nothing it observed may survive.
	for _, b := range h.backends {
		b.markHealthy(true)
	}

	go func() {
		defer close(r.done)
		// Run an immediate probe so a backend whose first probe fails does
		// not absorb traffic during the first interval.
		r.probeAll(ctx)
		t := time.NewTicker(h.cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.probeAll(ctx)
			}
		}
	}()
	return r
}

func (r *healthRun) stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		r.active.Store(false)
		if r.cancel == nil {
			return
		}
		r.cancel()
		<-r.done
	})
}

func (r *healthRun) probeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range r.checker.backends {
		wg.Add(1)
		go func(b *backendState) {
			defer wg.Done()
			r.probe(ctx, b)
		}(b)
	}
	wg.Wait()
}

// isCheckerStopped reports whether ctx, not the shorter per-probe pCtx
// derived from it, is what ended the request: a probe cancelled by the
// checker's own stop is lifecycle, not a backend verdict.
func isCheckerStopped(ctx context.Context) bool {
	return ctx.Err() != nil
}

func (r *healthRun) probe(ctx context.Context, b *backendState) {
	h := r.checker
	target, err := backendURL(b.backend)
	if err != nil {
		r.recordFailure(b)
		return
	}
	target.Path = h.cfg.Path

	pCtx, cancel := context.WithTimeout(ctx, h.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		r.recordFailure(b)
		return
	}
	if h.host != "" {
		req.Host = h.host
	}
	resp, err := h.client.Do(req)
	if err != nil {
		if isCheckerStopped(ctx) {
			return
		}
		r.recordFailure(b)
		return
	}
	_ = resp.Body.Close()
	if h.acceptsStatus(resp.StatusCode) {
		r.recordSuccess(b)
		return
	}
	r.recordFailure(b)
}

// acceptsStatus reports whether a probe response status counts as healthy:
// the configured Statuses when any are set, else the 200-399 default.
func (h *healthChecker) acceptsStatus(code int) bool {
	if len(h.cfg.Statuses) > 0 {
		return slices.Contains(h.cfg.Statuses, code)
	}
	return code >= 200 && code < 400
}

func (r *healthRun) recordSuccess(b *backendState) {
	if !r.active.Load() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active.Load() {
		return
	}
	r.failures[b] = 0
	r.successes[b]++
	if r.successes[b] >= r.checker.cfg.Healthy {
		b.markHealthy(true)
	}
}

func (r *healthRun) recordFailure(b *backendState) {
	if !r.active.Load() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active.Load() {
		return
	}
	r.successes[b] = 0
	r.failures[b]++
	if r.failures[b] >= r.checker.cfg.Unhealthy {
		b.markHealthy(false)
	}
}
