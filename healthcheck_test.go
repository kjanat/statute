package statute

import (
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

// TestHealthCheck_RecordTransitions exercises the threshold logic: a
// backend is only demoted after Unhealthy consecutive failures and only
// promoted after Healthy consecutive successes.
func TestHealthCheck_RecordTransitions(t *testing.T) {
	t.Parallel()
	cfg := resolved.HealthCheck{
		Enabled:   true,
		Interval:  100 * time.Millisecond,
		Timeout:   50 * time.Millisecond,
		Healthy:   2,
		Unhealthy: 3,
	}
	b := &backendState{backend: &resolved.Backend{Address: "x:1"}}
	b.markHealthy(true)
	hc := newHealthChecker(cfg, []*backendState{b}, nil, "")

	// Two failures: still healthy (under threshold).
	hc.recordFailure(b)
	hc.recordFailure(b)
	if !b.isHealthy() {
		t.Errorf("backend demoted before threshold")
	}
	hc.recordFailure(b)
	if b.isHealthy() {
		t.Errorf("backend should be unhealthy after 3 failures")
	}

	// Two successes flip it back.
	hc.recordSuccess(b)
	if b.isHealthy() {
		t.Errorf("backend promoted before threshold (1 success)")
	}
	hc.recordSuccess(b)
	if !b.isHealthy() {
		t.Errorf("backend should be healthy after 2 successes")
	}
}

// TestHealthCheck_DisabledStartsNoGoroutine — a disabled config must not
// spawn the prober goroutine. We can't directly assert this, but start()
// returns quickly and stop() doesn't block on a nil done channel.
func TestHealthCheck_DisabledIsInert(t *testing.T) {
	t.Parallel()
	cfg := resolved.HealthCheck{Enabled: false}
	hc := newHealthChecker(cfg, nil, nil, "")
	hc.start() // must not panic
	hc.stop()  // must not block; cancel is nil
}
