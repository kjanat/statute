package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

func TestPassiveHealthSuccessResetAndFullExclusion(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusBadGateway)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(status.Load())) }))
	t.Cleanup(backend.Close)
	clock := &fakeClock{t: time.Unix(100, 0)}
	pool, err := newPoolHandler(&resolved.Pool{Name: "api", RejectUnhealthy: true, Backends: []resolved.Backend{{Address: strings.TrimPrefix(backend.URL, "http://"), Weight: 1}}, PassiveHealthCheck: resolved.PassiveHealthCheck{Enabled: true, MaxFailures: 2, FailureWindow: 10 * time.Second, ResetOnSuccess: true, HoldDown: 10 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	pool.now = clock.Now
	run := pool.startPassive()
	t.Cleanup(run.stop)
	t.Cleanup(pool.transport.CloseIdleConnections)
	for _, tc := range []struct {
		backend int32
		want    int
		advance time.Duration
	}{
		{502, 502, 0}, {204, 204, 0}, {502, 502, 0}, {502, 502, 0}, {204, 503, 0}, {204, 503, 9 * time.Second}, {204, 204, time.Second},
	} {
		status.Store(tc.backend)
		clock.Advance(tc.advance)
		w := runRequest(t, pool, httptest.NewRequest(http.MethodGet, "http://app.example/", nil))
		if w.Code != tc.want {
			t.Fatalf("backend=%d advance=%s status=%d want=%d", tc.backend, tc.advance, w.Code, tc.want)
		}
	}
}

func TestPassiveHealthWithActiveProbesWaitsForRecovery(t *testing.T) {
	clock := &fakeClock{t: time.Unix(100, 0)}
	run := newPassiveRun(resolved.PassiveHealthCheck{Enabled: true, FailureWindow: time.Second, MaxFailures: 1, ResetOnSuccess: true, RecoverByProbe: true}, clock.Now)
	b := &backendState{}
	b.markHealthy(true)
	run.record(b)
	if b.isHealthy() || run.demoted(b) {
		t.Fatal("active-probe recovery did not own backend health")
	}
	clock.Advance(2 * time.Second)
	if b.isHealthy() {
		t.Fatal("passive window expiry bypassed active recovery")
	}
	probes := &healthRun{checker: newHealthChecker(resolved.HealthCheck{Healthy: 1}, nil, nil, ""), successes: map[*backendState]int{}, failures: map[*backendState]int{}}
	probes.active.Store(true)
	probes.recordSuccess(b)
	if !b.isHealthy() {
		t.Fatal("successful active probe did not restore backend")
	}
}
