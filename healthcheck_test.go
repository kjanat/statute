package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// TestHealthCheck_StopCancellationIsNotFailure — a probe aborted by the
// checker's own stop (what the Start rollback does to a probe in flight)
// is lifecycle, not a backend verdict. With Unhealthy: 1 a single
// recorded failure demotes, so the healthy bit is the whole assertion.
func TestHealthCheck_StopCancellationIsNotFailure(t *testing.T) {
	t.Parallel()
	reached := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(reached) })
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	b := &backendState{backend: &resolved.Backend{Address: strings.TrimPrefix(srv.URL, "http://")}}
	b.markHealthy(true)
	hc := newHealthChecker(resolved.HealthCheck{
		Enabled:   true,
		Path:      "/healthz",
		Interval:  time.Hour, // only the immediate probe matters
		Timeout:   time.Minute,
		Healthy:   2,
		Unhealthy: 1,
	}, []*backendState{b}, nil, "")

	hc.start()
	<-reached // the probe is in flight, blocked at the backend
	hc.stop() // cancels it mid-request
	releaseOnce()
	if !b.isHealthy() {
		t.Fatal("stop-cancelled probe was recorded as a backend failure")
	}
}

// TestHealthCheck_RestartResetsState — a restarted checker (a Start retried
// after a rollback) begins from the initial all-healthy state instead of
// inheriting whatever a rolled-back attempt observed.
func TestHealthCheck_RestartResetsState(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	b := &backendState{backend: &resolved.Backend{Address: strings.TrimPrefix(srv.URL, "http://")}}
	b.markHealthy(true)
	hc := newHealthChecker(resolved.HealthCheck{
		Enabled:   true,
		Path:      "/healthz",
		Interval:  time.Hour,
		Timeout:   time.Minute,
		Healthy:   2,
		Unhealthy: 1,
	}, []*backendState{b}, nil, "")

	hc.start()
	hc.stop()
	b.markHealthy(false) // what a genuine probe failure during the attempt leaves
	hc.start()
	defer hc.stop()
	if !b.isHealthy() {
		t.Fatal("restarted checker inherited the previous attempt's demotion")
	}
}
