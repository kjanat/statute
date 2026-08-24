package statute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	run := &healthRun{checker: hc, successes: make(map[*backendState]int), failures: make(map[*backendState]int)}
	run.active.Store(true)

	// Two failures: still healthy (under threshold).
	run.recordFailure(b)
	run.recordFailure(b)
	if !b.isHealthy() {
		t.Errorf("backend demoted before threshold")
	}
	run.recordFailure(b)
	if b.isHealthy() {
		t.Errorf("backend should be unhealthy after 3 failures")
	}

	// Two successes flip it back.
	run.recordSuccess(b)
	if b.isHealthy() {
		t.Errorf("backend promoted before threshold (1 success)")
	}
	run.recordSuccess(b)
	if !b.isHealthy() {
		t.Errorf("backend should be healthy after 2 successes")
	}
}

// TestHealthCheckStatuses — configured Statuses replace the 200-399
// default exactly: a 304 that the default accepts demotes under [200,204],
// and an empty list keeps today's range.
func TestHealthCheckStatuses(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(srv.Close)

	probeOnce := func(statuses []int) bool {
		t.Helper()
		cfg := resolved.HealthCheck{
			Enabled: true, Path: "/healthz",
			Interval: time.Hour, Timeout: 10 * time.Second,
			Healthy: 1, Unhealthy: 1,
			Statuses: statuses,
		}
		b := &backendState{backend: &resolved.Backend{Address: strings.TrimPrefix(srv.URL, "http://")}}
		b.markHealthy(true)
		hc := newHealthChecker(cfg, []*backendState{b}, nil, "")
		run := &healthRun{checker: hc, successes: make(map[*backendState]int), failures: make(map[*backendState]int)}
		run.active.Store(true)
		run.probe(context.Background(), b)
		return b.isHealthy()
	}

	if !probeOnce(nil) {
		t.Error("default statuses rejected a 304")
	}
	if probeOnce([]int{200, 204}) {
		t.Error("Statuses [200,204] accepted a 304")
	}
	if !probeOnce([]int{200, 304}) {
		t.Error("Statuses [200,304] rejected a 304")
	}
}

// TestHealthCheckStatusesRedirect — an explicit probe policy judges the
// health endpoint's own response: Statuses accept or reject the 3xx
// itself instead of whatever the redirect lands on, and a probe Host
// never follows a redirect elsewhere. Default probes keep following
// redirects (200-399 on the final response), today's behavior.
func TestHealthCheckStatusesRedirect(t *testing.T) {
	t.Parallel()
	var followed atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			followed.Add(1)
			w.WriteHeader(http.StatusOK)
		case "/bad":
			followed.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		case "/redirect-to-ok":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			http.Redirect(w, r, "/bad", http.StatusFound)
		}
	}))
	t.Cleanup(srv.Close)

	probeOnce := func(path, host string, statuses []int) bool {
		t.Helper()
		cfg := resolved.HealthCheck{
			Enabled: true, Path: path,
			Interval: time.Hour, Timeout: 10 * time.Second,
			Healthy: 1, Unhealthy: 1,
			Host: host, Statuses: statuses,
		}
		b := &backendState{backend: &resolved.Backend{Address: strings.TrimPrefix(srv.URL, "http://")}}
		b.markHealthy(true)
		hc := newHealthChecker(cfg, []*backendState{b}, nil, host)
		run := &healthRun{checker: hc, successes: make(map[*backendState]int), failures: make(map[*backendState]int)}
		run.active.Store(true)
		run.probe(context.Background(), b)
		return b.isHealthy()
	}

	cases := []struct {
		name     string
		path     string
		host     string
		statuses []int
		healthy  bool
		follows  int64
	}{
		{"Statuses [200] rejects the 302 without following it", "/redirect-to-ok", "", []int{200}, false, 0},
		{"Statuses [302] accepts the 302 itself, not the 500 behind it", "/redirect-to-bad", "", []int{302}, true, 0},
		{"default statuses follow the redirect to the 200", "/redirect-to-ok", "", nil, true, 1},
		{"Host alone stops following; the 302 passes the default range", "/redirect-to-bad", "probe.example.test", nil, true, 0},
		{"Host with Statuses [302] accepts without following", "/redirect-to-bad", "probe.example.test", []int{302}, true, 0},
	}
	for _, tc := range cases {
		followed.Store(0)
		if got := probeOnce(tc.path, tc.host, tc.statuses); got != tc.healthy {
			t.Errorf("%s: healthy = %v, want %v", tc.name, got, tc.healthy)
		}
		if got := followed.Load(); got != tc.follows {
			t.Errorf("%s: followed %d redirect(s), want %d", tc.name, got, tc.follows)
		}
	}
}

// TestHealthCheck_DisabledStartsNoGoroutine — a disabled config must not
// spawn the prober goroutine. We can't directly assert this, but start()
// returns quickly and stop() doesn't block on a nil done channel.
func TestHealthCheck_DisabledIsInert(t *testing.T) {
	t.Parallel()
	cfg := resolved.HealthCheck{Enabled: false}
	hc := newHealthChecker(cfg, nil, nil, "")
	run := hc.start() // must not panic
	run.stop()        // must not block; cancel is nil
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

	run := hc.start()
	<-reached  // the probe is in flight, blocked at the backend
	run.stop() // cancels it mid-request
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

	first := hc.start()
	first.stop()
	b.markHealthy(false) // what a genuine probe failure during the attempt leaves
	second := hc.start()
	defer second.stop()
	if !b.isHealthy() {
		t.Fatal("restarted checker inherited the previous attempt's demotion")
	}
}

// TestHealthRun_StoppedGenerationCannotAffectRestart proves ownership is on
// the returned run handle: invoking a stale handle after a restart neither
// cancels the new loop nor records a verdict into its backend state.
func TestHealthRun_StoppedGenerationCannotAffectRestart(t *testing.T) {
	t.Parallel()
	b := &backendState{backend: &resolved.Backend{Address: "127.0.0.1:1"}}
	b.markHealthy(true)
	hc := newHealthChecker(resolved.HealthCheck{
		Enabled: false, Path: "/healthz", Interval: time.Hour,
		Timeout: time.Second, Healthy: 1, Unhealthy: 1,
	}, []*backendState{b}, nil, "")

	first := hc.start()
	first.stop()
	second := hc.start()
	defer second.stop()

	first.stop()
	first.recordFailure(b)
	if !second.active.Load() {
		t.Fatal("stale stop cancelled the later health run")
	}
	if !b.isHealthy() {
		t.Fatal("stale health run mutated backend state after restart")
	}
}
