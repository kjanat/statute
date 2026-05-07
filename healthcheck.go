package statute

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/kjanat/statute/resolved"
)

// healthChecker probes each backend on an interval and toggles its healthy
// flag according to consecutive success/failure thresholds.
type healthChecker struct {
	cfg      resolved.HealthCheck
	backends []*backendState
	client   *http.Client

	mu        sync.Mutex
	successes map[*backendState]int
	failures  map[*backendState]int

	cancel context.CancelFunc
	done   chan struct{}
}

func newHealthChecker(cfg resolved.HealthCheck, backends []*backendState) *healthChecker {
	return &healthChecker{
		cfg:      cfg,
		backends: backends,
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
		successes: make(map[*backendState]int),
		failures:  make(map[*backendState]int),
	}
}

func (h *healthChecker) start() {
	if !h.cfg.Enabled || len(h.backends) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})

	go func() {
		defer close(h.done)
		// Run an immediate probe so a backend whose first probe fails does
		// not absorb traffic during the first interval.
		h.probeAll(ctx)
		t := time.NewTicker(h.cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.probeAll(ctx)
			}
		}
	}()
}

func (h *healthChecker) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	<-h.done
}

func (h *healthChecker) probeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range h.backends {
		wg.Add(1)
		go func(b *backendState) {
			defer wg.Done()
			h.probe(ctx, b)
		}(b)
	}
	wg.Wait()
}

func (h *healthChecker) probe(ctx context.Context, b *backendState) {
	target, err := backendURL(b.backend)
	if err != nil {
		h.recordFailure(b)
		return
	}
	target.Path = h.cfg.Path

	pCtx, cancel := context.WithTimeout(ctx, h.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		h.recordFailure(b)
		return
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.recordFailure(b)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		h.recordSuccess(b)
		return
	}
	h.recordFailure(b)
}

func (h *healthChecker) recordSuccess(b *backendState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures[b] = 0
	h.successes[b]++
	if h.successes[b] >= h.cfg.Healthy {
		b.markHealthy(true)
	}
}

func (h *healthChecker) recordFailure(b *backendState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.successes[b] = 0
	h.failures[b]++
	if h.failures[b] >= h.cfg.Unhealthy {
		b.markHealthy(false)
	}
}
