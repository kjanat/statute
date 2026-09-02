package statute

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func TestWorkloadZeroValueIsDormant(t *testing.T) {
	t.Parallel()

	var w workload
	if got := w.phaseNow(); got != workloadDormant {
		t.Fatalf("zero value phase = %v, want %v", got, workloadDormant)
	}
	if w.phaseNow().serving() {
		t.Fatal("a dormant workload must not serve")
	}
}

func TestWorkloadOnlyReadyServes(t *testing.T) {
	t.Parallel()

	for _, p := range []workloadPhase{workloadDormant, workloadStarting, workloadStopPending, workloadStopIssued, workloadStopUnknown, workloadFailed} {
		if p.serving() {
			t.Errorf("%v serves, want not serving", p)
		}
	}
	if !workloadReady.serving() {
		t.Error("ready does not serve, want serving")
	}
}

func TestWorkloadLegalTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path []workloadPhase
	}{
		{"activate and serve", []workloadPhase{workloadStarting, workloadReady}},
		{"idle shutdown", []workloadPhase{workloadStarting, workloadReady, workloadStopPending, workloadStopIssued, workloadDormant}},
		{"revoked idle stop", []workloadPhase{workloadStarting, workloadReady, workloadStopPending, workloadReady}},
		{"failed stop call, container still running", []workloadPhase{workloadStarting, workloadReady, workloadStopPending, workloadStopIssued, workloadReady}},
		{"unknown stop retry succeeds", []workloadPhase{workloadStarting, workloadReady, workloadStopPending, workloadStopIssued, workloadStopUnknown, workloadStopIssued, workloadDormant}},
		{"unknown stop later observed stopped", []workloadPhase{workloadStarting, workloadReady, workloadStopPending, workloadStopIssued, workloadStopUnknown, workloadDormant}},
		{"failed activation cleanup", []workloadPhase{workloadStarting, workloadStopIssued, workloadFailed}},
		{"activation fails then retries", []workloadPhase{workloadStarting, workloadFailed, workloadStarting, workloadReady}},
		{"external start clears a failure", []workloadPhase{workloadStarting, workloadFailed, workloadStarting, workloadReady}},
		{"external stop while starting", []workloadPhase{workloadStarting, workloadDormant}},
		{"external stop while ready", []workloadPhase{workloadStarting, workloadReady, workloadDormant}},
		{"external stop while stop pending", []workloadPhase{workloadStarting, workloadReady, workloadStopPending, workloadDormant}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var w workload
			for i, next := range tt.path {
				if !w.to(next) {
					t.Fatalf("step %d: to(%v) from %v rejected, want accepted", i, next, w.phaseNow())
				}
				if got := w.phaseNow(); got != next {
					t.Fatalf("step %d: phase = %v, want %v", i, got, next)
				}
			}
		})
	}
}

func TestWorkloadIllegalTransitionsKeepPhase(t *testing.T) {
	t.Parallel()

	all := []workloadPhase{workloadDormant, workloadStarting, workloadReady, workloadStopPending, workloadStopIssued, workloadStopUnknown, workloadFailed}

	for _, from := range all {
		for _, to := range all {
			legal := false
			for _, allowed := range workloadTransitions[from] {
				if allowed == to {
					legal = true
				}
			}
			if legal {
				continue
			}

			w := workload{phase: from}
			if w.to(to) {
				t.Errorf("to(%v) from %v accepted, want rejected", to, from)
			}
			if got := w.phaseNow(); got != from {
				t.Errorf("rejected to(%v) moved %v to %v", to, from, got)
			}
		}
	}
}

// A dormant workload must not reach ready without passing through starting,
// which is the invariant separating this model from backend health.
func TestWorkloadCannotSkipStarting(t *testing.T) {
	t.Parallel()

	var w workload
	if w.to(workloadReady) {
		t.Fatal("dormant to ready accepted, want rejected")
	}
	if got := w.phaseNow(); got != workloadDormant {
		t.Fatalf("phase = %v, want %v", got, workloadDormant)
	}
}

func TestWorkloadConcurrentTransitionsElectOneWinner(t *testing.T) {
	t.Parallel()

	var w workload

	const racers = 64

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)

	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()

			if w.to(workloadStarting) {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("accepted transitions = %d, want 1", wins)
	}
	if got := w.phaseNow(); got != workloadStarting {
		t.Fatalf("phase = %v, want %v", got, workloadStarting)
	}
}

func TestWorkloadPhaseStrings(t *testing.T) {
	t.Parallel()

	want := map[workloadPhase]string{
		workloadDormant:     "dormant",
		workloadStarting:    "starting",
		workloadReady:       "ready",
		workloadStopPending: "stop-pending",
		workloadStopIssued:  "stop-issued",
		workloadStopUnknown: "stop-unknown",
		workloadFailed:      "failed",
		workloadPhase(9):    "unknown",
	}

	for p, s := range want {
		if got := p.String(); got != s {
			t.Errorf("%d.String() = %q, want %q", uint8(p), got, s)
		}
	}
}

// --- integration tests against the stateful fake daemon ---

// workloadFixture builds a provider whose fake daemon holds one stopped,
// policy-covered container whose backend address points at the given
// upstream. It returns the provider, the daemon, and the compiled router.
func workloadFixture(t *testing.T, policy resolved.Workload, backendURL string) (*dockerProvider, *fakeDaemon, http.Handler) {
	return workloadFixtureMode(t, policy, backendURL, true)
}

func workloadFixtureMode(t *testing.T, policy resolved.Workload, backendURL string, stopped bool) (*dockerProvider, *fakeDaemon, http.Handler) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(backendURL, "http://"))
	if err != nil {
		t.Fatalf("backend url: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("backend port: %v", err)
	}
	cfg := &resolved.Docker{Workloads: map[string]resolved.Workload{"wl": policy}}
	p, srv, daemon := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{{
		name: "wl-1", ip: host, port: port, stopped: stopped,
		labels: map[string]string{
			"statute.enable":  "true",
			"statute.service": "wl",
			"statute.host":    "wl.example.com",
		},
	}})
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	return p, daemon, srv.buildRouter()
}

// testWorkloadPolicy is a fast-cycling policy for tests.
func testWorkloadPolicy() resolved.Workload {
	return resolved.Workload{
		IdleAfter:    150 * time.Millisecond,
		StartTimeout: 2 * time.Second,
		ReadyTimeout: 3 * time.Second,
		BackoffBase:  200 * time.Millisecond,
		BackoffCap:   time.Second,
	}
}

// waitWorkloadPhase polls the fixture's one workload, "wl", until it
// reaches the wanted phase.
func waitWorkloadPhase(t *testing.T, p *dockerProvider, want workloadPhase) {
	waitWorkloadServicePhase(t, p, "wl", want)
}

func waitWorkloadServicePhase(t *testing.T, p *dockerProvider, service string, want workloadPhase) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w := p.workloadFor(service); w != nil && w.phaseNow() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	w := p.workloadFor(service)
	if w == nil {
		t.Fatalf("no workload entry for %q", service)
	}
	t.Fatalf("workload phase = %v, want %v", w.phaseNow(), want)
}

func waitSignal(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

func TestWaitReadinessProbePacesGenerationChanges(t *testing.T) {
	changed := make(chan struct{})
	close(changed)
	started := time.Now()
	if !waitReadinessProbe(context.Background(), changed) {
		t.Fatal("closed generation edge cancelled readiness pacing")
	}
	if elapsed := time.Since(started); elapsed < workloadProbeInterval {
		t.Fatalf("generation edge resumed readiness after %s, want at least %s", elapsed, workloadProbeInterval)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitReadinessProbe(ctx, changed) {
		t.Fatal("cancelled readiness pacing resumed probing")
	}
}

func waitStatus(t *testing.T, ch <-chan int, timeout time.Duration, message string) int {
	t.Helper()
	select {
	case code := <-ch:
		return code
	case <-time.After(timeout):
		t.Fatal(message)
		return 0
	}
}

func waitError(t *testing.T, ch <-chan error, timeout time.Duration, message string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(timeout):
		t.Fatal(message)
		return nil
	}
}

func assertStatusPending(t *testing.T, ch <-chan int) {
	t.Helper()
	select {
	case code := <-ch:
		t.Fatalf("request completed with %d while Docker stop was in flight", code)
	case <-time.After(100 * time.Millisecond):
	}
}

func waitStartCount(t *testing.T, daemon *fakeDaemon, name string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if daemon.startCount(name) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("start calls for %q = %d, want %d", name, daemon.startCount(name), want)
}

func waitUncertainStopAttempts(t *testing.T, p *dockerProvider, daemon *fakeDaemon, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w := p.workloadFor("wl")
		if w == nil {
			time.Sleep(time.Millisecond)
			continue
		}
		w.mu.Lock()
		stop := w.stop
		settled := w.phase == workloadStopUnknown && stop != nil && stop.uncertain && !stop.issued
		w.mu.Unlock()
		if daemon.stopCount("wl-1") >= want && settled {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stop did not remain uncertain after %d attempts", want)
}

func assertSuccessorActivation(t *testing.T, p *dockerProvider, daemon *fakeDaemon) {
	t.Helper()
	daemon.mu.Lock()
	wantRef := daemon.find("wl-new").id
	daemon.mu.Unlock()
	w := p.workloadFor("wl")
	w.mu.Lock()
	ref := w.containerRefLocked()
	act := w.activation
	w.mu.Unlock()
	if ref != wantRef || act == nil || act.ref != wantRef || !act.observe {
		t.Fatalf("successor binding: ref=%q activation=%+v", ref, act)
	}
	if got := daemon.stopCount("wl-old") + daemon.stopCount("wl-new"); got != 0 {
		t.Fatalf("superseded activation issued %d cleanup stops", got)
	}
	if got := daemon.startCount("wl-new"); got != 0 {
		t.Fatalf("running successor received %d start calls", got)
	}
}

func assertIssuedStopDidNotAffectSuccessor(t *testing.T, daemon *fakeDaemon) {
	t.Helper()
	if got := daemon.stopCount("wl-old"); got != 1 {
		t.Fatalf("old-container stop calls = %d, want 1", got)
	}
	if got := daemon.stopCount("wl-new"); got != 0 {
		t.Fatalf("successor stop calls = %d, want 0", got)
	}
	if got := daemon.startCount("wl-new"); got != 0 {
		t.Fatalf("successor start calls = %d, want 0", got)
	}
}

func TestWorkloadDormantRouteActivatesAndServes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("woken"))
	}))
	t.Cleanup(backend.Close)

	_, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)

	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "woken" {
		t.Fatalf("cold start response: %d %q", rec.Code, rec.Body.String())
	}
	if got := daemon.startCount("wl-1"); got != 1 {
		t.Fatalf("start calls = %d, want 1", got)
	}
}

func TestWorkloadConcurrentRequestsSingleStart(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	_, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)

	const clients = 20
	codes := make(chan int, clients)
	var wg sync.WaitGroup
	wg.Add(clients)
	for range clients {
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
			codes <- rec.Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("concurrent waiter got %d, want 200", code)
		}
	}
	if got := daemon.startCount("wl-1"); got != 1 {
		t.Fatalf("start calls = %d, want 1", got)
	}
}

func TestWorkloadActivationFailureIsTerminalAndBacksOff(t *testing.T) {
	fallbackHit := false
	policy := testWorkloadPolicy()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, _ := workloadFixture(t, policy, backend.URL)
	daemon.mu.Lock()
	daemon.failStart = true
	daemon.mu.Unlock()
	p.srv.cfg.Fallback = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { fallbackHit = true })
	router := p.srv.buildRouter()

	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed activation: %d, want 503", rec.Code)
	}
	if fallbackHit {
		t.Fatal("failed activation reached Config.Fallback")
	}

	// Inside the backoff window: an immediate 503 that names the wait.
	rec = runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("backoff response: %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("backoff response carries no Retry-After")
	}

	// After the window a fresh attempt runs, and a repaired daemon serves.
	daemon.mu.Lock()
	daemon.failStart = false
	daemon.mu.Unlock()
	time.Sleep(policy.BackoffBase + 50*time.Millisecond)
	rec = runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("retry after backoff: %d, want 200", rec.Code)
	}
}

func TestWorkloadFailedActivationOwnsCleanupStop(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	policy := testWorkloadPolicy()
	policy.ReadyTimeout = 50 * time.Millisecond
	policy.Readiness.Mode = resolved.ReadinessDockerHealth
	p, daemon, router := workloadFixture(t, policy, backend.URL)
	daemon.mu.Lock()
	daemon.find("wl-1").health = "starting"
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()

	first := make(chan int)
	go func() {
		first <- runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)).Code
	}()
	waitSignal(t, stopStarted, "failed activation did not issue its cleanup stop")
	if code := waitStatus(t, first, time.Second, "failed activation waiter remained attached to cleanup"); code != http.StatusServiceUnavailable {
		t.Fatalf("failed activation = %d, want 503", code)
	}
	waitWorkloadPhase(t, p, workloadStopIssued)
	mustSync(t, p)
	if got := p.workloadFor("wl").phaseNow(); got != workloadStopIssued {
		t.Fatalf("running observation during cleanup moved phase to %v", got)
	}

	second := make(chan int)
	go func() {
		second <- runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)).Code
	}()
	assertStatusPending(t, second)
	releaseStop()
	if code := waitStatus(t, second, time.Second, "request did not leave settled cleanup"); code != http.StatusServiceUnavailable {
		t.Fatalf("request after cleanup = %d, want 503", code)
	}
	waitWorkloadPhase(t, p, workloadFailed)
}

func TestWorkloadRejectedCleanupPreservesBackoffAcrossSettlementReconcile(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	policy := testWorkloadPolicy()
	policy.ReadyTimeout = 50 * time.Millisecond
	policy.BackoffBase = 2 * time.Second
	policy.BackoffCap = 2 * time.Second
	policy.Readiness.Mode = resolved.ReadinessDockerHealth
	p, daemon, router := workloadFixture(t, policy, backend.URL)
	daemon.mu.Lock()
	daemon.find("wl-1").health = "starting"
	daemon.rejectStop = true
	daemon.blockRejectedStop = true
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()

	first := make(chan int)
	go func() {
		first <- runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)).Code
	}()
	waitSignal(t, stopStarted, "failed activation did not issue its cleanup stop")
	if code := waitStatus(t, first, time.Second, "failed activation waiter remained attached to cleanup"); code != http.StatusServiceUnavailable {
		t.Fatalf("failed activation = %d, want 503", code)
	}
	w := p.workloadFor("wl")
	w.mu.Lock()
	failures, failedUntil := w.failures, w.failedUntil
	w.mu.Unlock()

	daemon.mu.Lock()
	daemon.find("wl-1").stopped = true
	daemon.find("wl-1").health = "healthy"
	daemon.listStarted = make(chan struct{})
	daemon.listRelease = make(chan struct{})
	listStarted := daemon.listStarted
	listRelease := daemon.listRelease
	daemon.mu.Unlock()
	var releaseListOnce sync.Once
	releaseList := func() { releaseListOnce.Do(func() { close(listRelease) }) }
	defer releaseList()
	p.generationMu.Lock()
	settled := p.generationChanged
	p.generationMu.Unlock()
	p.scheduleReconcile()
	waitSignal(t, listStarted, "listing did not capture stopped state before cleanup rejection")
	daemon.mu.Lock()
	daemon.find("wl-1").stopped = false
	daemon.mu.Unlock()
	releaseStop()
	waitWorkloadPhase(t, p, workloadFailed)
	releaseList()
	waitSignal(t, settled, "cleanup settlement did not reconcile the running container")

	rec := runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("request after rejected cleanup: %d (Retry-After %q), want backed-off 503", rec.Code, rec.Header().Get("Retry-After"))
	}
	w.mu.Lock()
	phase, gotFailures, gotFailedUntil, activation := w.phase, w.failures, w.failedUntil, w.activation
	w.mu.Unlock()
	if phase != workloadFailed || activation != nil {
		t.Fatalf("settlement reconcile state: phase=%v activation=%v, want failed without activation", phase, activation)
	}
	if gotFailures != failures || !gotFailedUntil.Equal(failedUntil) {
		t.Fatalf("settlement reconcile backoff: failures=%d until=%v, want %d until %v", gotFailures, gotFailedUntil, failures, failedUntil)
	}

	w.mu.Lock()
	w.failedUntil = time.Now().Add(-time.Second)
	w.mu.Unlock()
	rec = runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readiness retry after backoff = %d, want 200", rec.Code)
	}
	if got := daemon.startCount("wl-1"); got != 1 {
		t.Fatalf("start calls = %d, want original activation only", got)
	}
}

func TestWorkloadStaleStoppedSnapshotCannotInventExternalRepair(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	policy := testWorkloadPolicy()
	policy.ReadyTimeout = 50 * time.Millisecond
	policy.BackoffBase = 2 * time.Second
	policy.BackoffCap = 2 * time.Second
	policy.Readiness.Mode = resolved.ReadinessDockerHealth
	p, daemon, router := workloadFixture(t, policy, backend.URL)
	daemon.mu.Lock()
	daemon.find("wl-1").health = "starting"
	daemon.rejectStop = true
	daemon.listStarted = make(chan struct{})
	daemon.listRelease = make(chan struct{})
	listStarted := daemon.listStarted
	listRelease := daemon.listRelease
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseList := func() { releaseOnce.Do(func() { close(listRelease) }) }
	defer releaseList()

	p.generationMu.Lock()
	reconciled := p.generationChanged
	p.generationMu.Unlock()
	p.scheduleReconcile()
	waitSignal(t, listStarted, "stale stopped listing did not start")

	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed activation = %d, want 503", rec.Code)
	}
	w := p.workloadFor("wl")
	waitWorkloadPhase(t, p, workloadFailed)
	w.mu.Lock()
	failures, failedUntil := w.failures, w.failedUntil
	w.mu.Unlock()
	daemon.mu.Lock()
	daemon.find("wl-1").health = "healthy"
	daemon.mu.Unlock()
	releaseList()
	waitSignal(t, reconciled, "stale stopped listing did not trigger a fresh publication")

	rec = runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("request after stale stopped listing: %d (Retry-After %q), want backed-off 503", rec.Code, rec.Header().Get("Retry-After"))
	}
	w.mu.Lock()
	phase, gotFailures, gotFailedUntil, activation := w.phase, w.failures, w.failedUntil, w.activation
	w.mu.Unlock()
	if phase != workloadFailed || activation != nil || gotFailures != failures || !gotFailedUntil.Equal(failedUntil) {
		t.Fatalf("state after stale stopped listing: phase=%v activation=%v failures=%d until=%v, want failed without activation and %d until %v", phase, activation, gotFailures, gotFailedUntil, failures, failedUntil)
	}
}

func TestWorkloadRunningFailureNeedsExternalRestartToClearBackoff(t *testing.T) {
	var healthy atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	policy := testWorkloadPolicy()
	policy.ReadyTimeout = 500 * time.Millisecond
	policy.BackoffBase = 2 * time.Second
	policy.BackoffCap = 2 * time.Second
	policy.Readiness = resolved.WorkloadReadiness{Mode: resolved.ReadinessHTTP, Path: "/ready"}
	p, daemon, _ := workloadFixtureMode(t, policy, backend.URL, false)
	waitWorkloadPhase(t, p, workloadFailed)
	w := p.workloadFor("wl")
	w.mu.Lock()
	failures, failedUntil := w.failures, w.failedUntil
	w.mu.Unlock()

	healthy.Store(true)
	mustSync(t, p)
	rec := runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("unchanged running observation: %d (Retry-After %q), want backed-off 503", rec.Code, rec.Header().Get("Retry-After"))
	}
	w.mu.Lock()
	phase, gotFailures, gotFailedUntil := w.phase, w.failures, w.failedUntil
	w.mu.Unlock()
	if phase != workloadFailed || gotFailures != failures || !gotFailedUntil.Equal(failedUntil) {
		t.Fatalf("unchanged running state: phase=%v failures=%d until=%v, want failed with %d until %v", phase, gotFailures, gotFailedUntil, failures, failedUntil)
	}

	daemon.mu.Lock()
	daemon.find("wl-1").stopped = true
	daemon.mu.Unlock()
	mustSync(t, p)
	daemon.mu.Lock()
	daemon.find("wl-1").stopped = false
	daemon.mu.Unlock()
	mustSync(t, p)
	waitWorkloadPhase(t, p, workloadReady)
	if got := daemon.startCount("wl-1"); got != 0 {
		t.Fatalf("external restart issued %d start calls, want observe-only readiness", got)
	}
}

func TestWorkloadIdleStopAndReactivation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)

	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cold start: %d", rec.Code)
	}
	waitWorkloadPhase(t, p, workloadDormant)
	if got := daemon.stopCount("wl-1"); got != 1 {
		t.Fatalf("stop calls = %d, want 1", got)
	}

	rec = runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reactivation: %d", rec.Code)
	}
	if got := daemon.startCount("wl-1"); got != 2 {
		t.Fatalf("start calls = %d, want 2", got)
	}
}

func TestWorkloadInFlightRequestHoldsIdleStop(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte("slow"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)

	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
		done <- rec.Code
	}()

	waitWorkloadPhase(t, p, workloadReady)
	// Twice the idle window passes while the request is in flight.
	time.Sleep(400 * time.Millisecond)
	if got := daemon.stopCount("wl-1"); got != 0 {
		t.Fatalf("stop fired with a request in flight (%d calls)", got)
	}
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("held request: %d, want 200", code)
	}
	// The idle timer starts at completion, and only then may the stop fire.
	waitWorkloadPhase(t, p, workloadDormant)
	if got := daemon.stopCount("wl-1"); got != 1 {
		t.Fatalf("stop calls after idle = %d, want 1", got)
	}
}

func TestWorkloadRevokedStopServesWithoutColdStart(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)

	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cold start: %d", rec.Code)
	}
	w := p.workloadFor("wl")

	// Take the stop decision by hand, before any Docker call, and revoke
	// it with a request: the workload keeps serving and no stop happens.
	w.mu.Lock()
	w.stopIdleLocked()
	w.toLocked(workloadStopPending)
	binding := w.binding.key
	epoch := w.idleEpoch
	w.mu.Unlock()

	rec = runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoked stop: %d, want 200", rec.Code)
	}
	if got := w.phaseNow(); got != workloadReady {
		t.Fatalf("phase after revoke = %v, want ready", got)
	}
	p.performStop(context.Background(), w, binding, epoch, p.currentRun())
	if got := daemon.stopCount("wl-1"); got != 0 {
		t.Fatalf("revoked stop still issued %d stop calls", got)
	}
	if got := daemon.startCount("wl-1"); got != 1 {
		t.Fatalf("start calls = %d, want 1 (no cold start)", got)
	}
}

func TestWorkloadRequestWaitsForIssuedStopAndReactivates(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	daemon.mu.Lock()
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()

	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cold start: %d", rec.Code)
	}
	waitSignal(t, stopStarted, "idle stop was not issued")
	waitWorkloadPhase(t, p, workloadStopIssued)

	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
		done <- rec.Code
	}()
	assertStatusPending(t, done)
	releaseStop()
	if code := waitStatus(t, done, 3*time.Second, "request did not reactivate after issued stop"); code != http.StatusOK {
		t.Fatalf("request after issued stop = %d, want 200", code)
	}
	if got := daemon.stopCount("wl-1"); got != 1 {
		t.Fatalf("stop calls = %d, want 1", got)
	}
	if got := daemon.startCount("wl-1"); got != 2 {
		t.Fatalf("start calls = %d, want 2", got)
	}
}

func TestWorkloadRequestWaitsForRejectedStopAndServes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	daemon.mu.Lock()
	daemon.rejectStop = true
	daemon.blockRejectedStop = true
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	daemon.mu.Unlock()
	releaseStop := sync.OnceFunc(func() { close(stopRelease) })
	defer releaseStop()

	if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("cold start = %d, want 200", rec.Code)
	}
	waitSignal(t, stopStarted, "idle stop was not issued")
	waitWorkloadPhase(t, p, workloadStopIssued)

	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
		done <- rec.Code
	}()
	assertStatusPending(t, done)
	releaseStop()
	if code := waitStatus(t, done, time.Second, "request did not resume after rejected stop"); code != http.StatusOK {
		t.Fatalf("request after rejected stop = %d, want 200", code)
	}
	if got := daemon.startCount("wl-1"); got != 1 {
		t.Fatalf("start calls = %d, want 1", got)
	}
}

func TestWorkloadContainerReplacementDoesNotInheritIssuedStop(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	labels := func(enabled string) map[string]string {
		return map[string]string{
			"statute.enable":  enabled,
			"statute.service": "wl",
			"statute.host":    "wl.example.com",
		}
	}
	p, srv, daemon := newFakeProviderDaemon(t, &resolved.Docker{
		Workloads: map[string]resolved.Workload{"wl": testWorkloadPolicy()},
	}, []fakeDaemonContainer{{
		name: "wl-old", ip: host, port: port, labels: labels("true"),
	}})
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	router := srv.buildRouter()
	if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("initial request = %d, want 200", rec.Code)
	}
	waitSignal(t, stopStarted, "idle stop was not issued")
	waitWorkloadPhase(t, p, workloadStopIssued)
	w := p.workloadFor("wl")

	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
		done <- rec.Code
	}()
	assertStatusPending(t, done)
	daemon.swap([]fakeDaemonContainer{{
		name: "wl-new", ip: host, port: port, health: "starting", labels: labels("true"),
	}})
	mustSync(t, p)
	releaseStop()
	if code := waitStatus(t, done, time.Second, "missing predecessor did not release its waiter"); code != http.StatusServiceUnavailable {
		t.Fatalf("missing predecessor response = %d, want 503", code)
	}
	successor := requireFreshStartingSuccessor(t, p, w)
	successorDone := make(chan int)
	go func() {
		successorDone <- runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)).Code
	}()
	assertStatusPending(t, successorDone)

	daemon.mu.Lock()
	wantRef := daemon.find("wl-new").id
	daemon.find("wl-new").health = "healthy"
	daemon.mu.Unlock()
	if code := waitStatus(t, successorDone, 2*time.Second, "successor readiness did not release its waiter"); code != http.StatusOK {
		t.Fatalf("successor readiness response = %d, want 200", code)
	}
	waitWorkloadPhase(t, p, workloadReady)
	requireReadySuccessor(t, successor, wantRef)

	releaseStop()
	assertIssuedStopDidNotAffectSuccessor(t, daemon)
	if rec := runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("successor request = %d, want 200", rec.Code)
	}
}

func requireFreshStartingSuccessor(t *testing.T, p *dockerProvider, predecessor *workload) *workload {
	t.Helper()
	successor := p.workloadFor("wl")
	if successor == nil || successor == predecessor {
		t.Fatal("successor did not receive a fresh lifecycle grant")
	}
	successor.mu.Lock()
	ref := successor.containerRefLocked()
	act := successor.activation
	successor.mu.Unlock()
	if ref == "" || act == nil || !act.observe {
		t.Fatalf("successor readiness state: ref=%q activation=%+v", ref, act)
	}
	return successor
}

func requireReadySuccessor(t *testing.T, successor *workload, wantRef string) {
	t.Helper()
	successor.mu.Lock()
	ref := successor.containerRefLocked()
	act := successor.activation
	successor.mu.Unlock()
	if ref != wantRef || act != nil {
		t.Fatalf("successor binding: ref=%q activation=%+v", ref, act)
	}
}

func TestWorkloadStopInspectFailureHoldsUnknownState(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("initial request = %d, want 200", rec.Code)
	}
	daemon.mu.Lock()
	daemon.failStop = true
	daemon.stallInspect = true
	daemon.inspectStarted = make(chan struct{})
	inspectStarted := daemon.inspectStarted
	daemon.mu.Unlock()

	waitSignal(t, inspectStarted, "stop fallback did not inspect the container")
	waitWorkloadPhase(t, p, workloadStopUnknown)
	if rec := runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request while stop outcome is unknown = %d, want 503", rec.Code)
	}
	if got := daemon.startCount("wl-1"); got != 1 {
		t.Fatalf("unknown stop outcome changed start count to %d, want 1", got)
	}

	daemon.mu.Lock()
	daemon.failStop = false
	daemon.stallInspect = false
	daemon.mu.Unlock()
	waitWorkloadPhase(t, p, workloadDormant)
	if got := daemon.stopCount("wl-1"); got < 2 {
		t.Fatalf("unknown stop converged after %d stop calls, want a bounded retry", got)
	}
}

func TestWorkloadStopConvergenceRestartsWithProviderRun(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("initial request = %d, want 200", rec.Code)
	}
	daemon.mu.Lock()
	daemon.failStop = true
	daemon.stallInspect = true
	daemon.inspectStarted = make(chan struct{})
	inspectStarted := daemon.inspectStarted
	daemon.mu.Unlock()
	waitSignal(t, inspectStarted, "stop fallback did not inspect the container")
	waitWorkloadPhase(t, p, workloadStopUnknown)

	p.lifecycleMu.Lock()
	firstRun := p.current
	p.lifecycleMu.Unlock()
	stopped := make(chan struct{})
	go func() {
		firstRun.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("provider shutdown did not own stop convergence")
	}

	daemon.mu.Lock()
	daemon.failStop = false
	daemon.stallInspect = false
	daemon.mu.Unlock()
	secondRun, err := p.start()
	if err != nil {
		t.Fatalf("provider restart: %v", err)
	}
	t.Cleanup(secondRun.stop)
	waitWorkloadPhase(t, p, workloadDormant)
}

func TestWorkloadIdleTimerRestartsWithProviderRun(t *testing.T) {
	policy := testWorkloadPolicy()
	policy.IdleAfter = 500 * time.Millisecond
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, policy, backend.URL)
	if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("cold start = %d, want 200", rec.Code)
	}
	waitWorkloadPhase(t, p, workloadReady)

	p.lifecycleMu.Lock()
	firstRun := p.current
	p.lifecycleMu.Unlock()
	firstRun.stop()
	daemon.mu.Lock()
	daemon.stopStarted = make(chan struct{})
	stopStarted := daemon.stopStarted
	daemon.mu.Unlock()

	secondRun, err := p.start()
	if err != nil {
		t.Fatalf("provider restart: %v", err)
	}
	t.Cleanup(secondRun.stop)
	waitSignal(t, stopStarted, "provider restart did not restore the idle timer")
}

func TestWorkloadLostStopResponseNeverReopensServing(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	daemon.mu.Lock()
	daemon.loseStopReply = true
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()

	if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("initial request = %d, want 200", rec.Code)
	}
	waitSignal(t, stopStarted, "idle stop was not accepted by the daemon")
	waitWorkloadPhase(t, p, workloadStopUnknown)
	if rec := runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request after lost stop response = %d, want 503", rec.Code)
	}
	if got := p.workloadFor("wl").phaseNow(); got == workloadReady {
		t.Fatal("ambiguous stop reopened serving after an immediate running inspect")
	}

	releaseStop()
	waitWorkloadPhase(t, p, workloadDormant)
}

func TestWorkloadStop500AfterSideEffectNeverReopensServing(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	daemon.mu.Lock()
	daemon.stopFailsAfterSide = true
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()

	if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("initial request = %d, want 200", rec.Code)
	}
	waitSignal(t, stopStarted, "idle stop side effect was not issued")
	waitWorkloadPhase(t, p, workloadStopUnknown)
	if rec := runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request after ambiguous 500 = %d, want 503", rec.Code)
	}

	releaseStop()
	waitWorkloadPhase(t, p, workloadDormant)
}

func TestWorkloadRejectedRetryDoesNotEraseStopUncertainty(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	daemon.mu.Lock()
	daemon.loseStopReply = true
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()

	if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("initial request = %d, want 200", rec.Code)
	}
	waitSignal(t, stopStarted, "idle stop was not accepted by the daemon")
	daemon.mu.Lock()
	daemon.loseStopReply = false
	daemon.rejectStop = true
	daemon.mu.Unlock()
	waitUncertainStopAttempts(t, p, daemon, 2)
	if rec := runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request after rejected retry = %d, want 503", rec.Code)
	}

	releaseStop()
	waitWorkloadPhase(t, p, workloadDormant)
}

func TestWorkloadExternalStopReconcilesAndReactivates(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)

	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cold start: %d", rec.Code)
	}

	// Someone stops the container outside statute.
	daemon.mu.Lock()
	daemon.find("wl-1").stopped = true
	daemon.mu.Unlock()
	mustSync(t, p)
	waitWorkloadPhase(t, p, workloadDormant)

	rec = runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reactivation after external stop: %d", rec.Code)
	}
	if got := daemon.startCount("wl-1"); got != 2 {
		t.Fatalf("start calls = %d, want 2", got)
	}
}

func TestRecoveredMutationQuarantinesOnlyRecordedContainer(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	labels := map[string]string{"statute.enable": "true", "statute.service": "wl", "statute.host": "wl.example.com"}
	cfg := &resolved.Docker{
		Workloads: map[string]resolved.Workload{"wl": testWorkloadPolicy()},
	}
	p, srv, daemon := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{
		{id: "container-c", name: "wl-c", ip: host, port: port, labels: labels},
		{id: "container-d", name: "wl-d", ip: host, port: port, labels: labels},
	})
	registry, err := openMutationRegistry(cfg.Storage, cfg.Endpoint)
	if err != nil {
		t.Fatalf("open mutation registry: %v", err)
	}
	if err := registry.put(mutationRecord{ContainerID: "container-c", ContainerName: "wl-c", Service: "wl", Kind: mutationRecordIdleStop, State: mutationRecordPrepared}); err != nil {
		t.Fatalf("seed mutation registry: %v", err)
	}
	daemon.mu.Lock()
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	run, err := p.start()
	if err != nil {
		t.Fatalf("recovery provider start: %v", err)
	}
	t.Cleanup(func() {
		releaseStop()
		run.stop()
	})
	waitSignal(t, stopStarted, "recovered mutation did not resume")
	if !run.registry.contains("container-c") {
		t.Fatal("recovered mutation disappeared before Docker confirmed settlement")
	}
	if rec := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("healthy joined contributor route = %d, want 200", rec.Code)
	}
	if got := daemon.stopCount("wl-c"); got != 1 {
		t.Fatalf("recorded container stops = %d, want 1", got)
	}
	if got := daemon.stopCount("wl-d"); got != 0 {
		t.Fatalf("joined container stops = %d, want 0", got)
	}
}

func TestRecoveredMutationSurvivesContainerRelabel(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	cfg := &resolved.Docker{Workloads: map[string]resolved.Workload{"wl": testWorkloadPolicy()}}
	p, srv, daemon := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{
		{id: "container-c", name: "wl-c", ip: host, port: port, labels: map[string]string{"statute.enable": "true", "statute.service": "renamed", "statute.host": "old.example.com"}},
		{id: "container-d", name: "wl-d", ip: host, port: port, labels: map[string]string{"statute.enable": "true", "statute.service": "wl", "statute.host": "live.example.com"}},
	})
	registry, err := openMutationRegistry(cfg.Storage, cfg.Endpoint)
	if err != nil {
		t.Fatalf("open mutation registry: %v", err)
	}
	if err := registry.put(mutationRecord{ContainerID: "container-c", ContainerName: "wl-c", Service: "wl", Kind: mutationRecordIdleStop, State: mutationRecordPrepared}); err != nil {
		t.Fatalf("seed mutation registry: %v", err)
	}
	daemon.mu.Lock()
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	run, err := p.start()
	if err != nil {
		releaseStop()
		t.Fatalf("recovery provider start: %v", err)
	}
	t.Cleanup(func() {
		releaseStop()
		run.stop()
	})
	waitSignal(t, stopStarted, "relabelled mutation did not resume")
	if rec := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://old.example.com/", nil)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("recorded relabelled contribution = %d, want 503", rec.Code)
	}
	if rec := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://live.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("independent live contribution = %d, want 200", rec.Code)
	}
}

func TestWorkloadPersistsStopBeforeDockerMutation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	daemon.mu.Lock()
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	t.Cleanup(releaseStop)

	if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("activation = %d, want 200", rec.Code)
	}
	waitSignal(t, stopStarted, "idle stop did not reach Docker")
	registry, err := openMutationRegistry(p.cfg.Storage, p.cfg.Endpoint)
	if err != nil {
		t.Fatalf("reopen mutation registry: %v", err)
	}
	if !registry.contains("id-0") {
		t.Fatal("Docker received stop before durable mutation intent existed")
	}
	releaseStop()
	waitWorkloadPhase(t, p, workloadDormant)
	if registry, err = openMutationRegistry(p.cfg.Storage, p.cfg.Endpoint); err != nil {
		t.Fatalf("reopen settled registry: %v", err)
	}
	if registry.contains("id-0") {
		t.Fatal("terminal stop left durable mutation intent behind")
	}
}

func TestWorkloadIssuedStopOwnsSettlementObservation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	daemon.mu.Lock()
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	t.Cleanup(releaseStop)

	if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("activation = %d, want 200", rec.Code)
	}
	waitSignal(t, stopStarted, "idle stop did not reach Docker")
	daemon.mu.Lock()
	daemon.find("id-0").stopped = true
	daemon.mu.Unlock()
	mustSync(t, p)
	if !p.currentMutationRegistry().contains("id-0") {
		t.Fatal("discovery settled an in-flight stop before its Docker call returned")
	}
	w := p.workloadFor("wl")
	w.mu.Lock()
	owned := w.stop != nil && w.stop.issued
	w.mu.Unlock()
	if !owned {
		t.Fatal("discovery cleared in-flight mutation ownership")
	}
	daemon.mu.Lock()
	daemon.find("id-0").stopped = false
	daemon.mu.Unlock()
	releaseStop()
	waitWorkloadPhase(t, p, workloadDormant)
}

func TestWorkloadRegistryDeleteFailureRemainsQuarantined(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	daemon.mu.Lock()
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	t.Cleanup(releaseStop)

	if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("activation = %d, want 200", rec.Code)
	}
	waitSignal(t, stopStarted, "idle stop did not reach Docker")
	root := p.cfg.Storage
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove registry root: %v", err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block registry root: %v", err)
	}
	releaseStop()
	waitWorkloadPhase(t, p, workloadStopUnknown)
	if !p.currentMutationRegistry().contains("id-0") {
		t.Fatal("failed registry deletion cleared in-memory ownership")
	}
	if err := os.Remove(root); err != nil {
		t.Fatalf("remove registry blocker: %v", err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("restore registry root: %v", err)
	}
	waitWorkloadPhase(t, p, workloadDormant)
	if p.currentMutationRegistry().contains("id-0") {
		t.Fatal("registry ownership remained after persistence recovered")
	}
}

func TestRecoveredMutationDoesNotAttachToSameNameSuccessor(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	policy := testWorkloadPolicy()
	policy.IdleAfter = time.Hour
	cfg := &resolved.Docker{Workloads: map[string]resolved.Workload{"wl": policy}}
	p, srv, daemon := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{{
		id: "new-id", name: "wl-1", ip: host, port: port,
		labels: map[string]string{"statute.enable": "true", "statute.service": "wl", "statute.host": "wl.example.com"},
	}})
	registry, err := openMutationRegistry(cfg.Storage, cfg.Endpoint)
	if err != nil {
		t.Fatalf("open mutation registry: %v", err)
	}
	if err := registry.put(mutationRecord{ContainerID: "old-id", ContainerName: "wl-1", Service: "wl", Kind: mutationRecordIdleStop, State: mutationRecordUncertain}); err != nil {
		t.Fatalf("seed mutation registry: %v", err)
	}
	run, err := p.start()
	if err != nil {
		t.Fatalf("recovery provider start: %v", err)
	}
	t.Cleanup(run.stop)
	if rec := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("successor route = %d, want 200", rec.Code)
	}
	if got := daemon.stopCount("wl-1"); got != 0 {
		t.Fatalf("same-name successor inherited %d stops", got)
	}
	if run.registry.contains("old-id") {
		t.Fatal("missing historical immutable ID remained outstanding")
	}
}

func TestEstablishedProviderAdoptsExternallyStartedContainer(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, _ := workloadFixtureMode(t, testWorkloadPolicy(), backend.URL, false)
	waitWorkloadPhase(t, p, workloadReady)
	if got := daemon.startCount("wl-1"); got != 0 {
		t.Fatalf("same-process adoption issued %d starts, want 0", got)
	}
}

func TestWorkloadMultiContributorServiceIsNotGated(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	labels := func() map[string]string {
		return map[string]string{
			"statute.enable":  "true",
			"statute.service": "wl",
			"statute.host":    "wl.example.com",
		}
	}
	cfg := &resolved.Docker{Workloads: map[string]resolved.Workload{"wl": testWorkloadPolicy()}}
	p, srv, daemon := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{
		{name: "wl-1", ip: host, port: port, labels: labels()},
		{name: "wl-2", ip: host, port: port, labels: labels()},
	})
	run, err := p.start()
	if err != nil {
		t.Fatalf("fresh provider start: %v", err)
	}
	t.Cleanup(run.stop)

	if w := p.workloadFor("wl"); w != nil {
		t.Fatal("multi-contributor service got a workload entry")
	}
	rec := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ungated multi-contributor service: %d, want 200", rec.Code)
	}
	if got := daemon.startCount("wl-1") + daemon.startCount("wl-2"); got != 0 {
		t.Fatalf("provider issued %d start calls without authority", got)
	}
	if got := daemon.stopCount("wl-1") + daemon.stopCount("wl-2"); got != 0 {
		t.Fatalf("provider issued %d stop calls without authority", got)
	}
}

func TestWorkloadUnextractableCandidatePreventsLifecycleAuthority(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	labels := func() map[string]string {
		return map[string]string{
			"statute.enable":  "true",
			"statute.service": "wl",
			"statute.host":    "wl.example.com",
		}
	}
	p, srv, daemon := newFakeProviderDaemon(t, &resolved.Docker{
		Workloads: map[string]resolved.Workload{"wl": testWorkloadPolicy()},
	}, []fakeDaemonContainer{
		{name: "wl-1", ip: host, port: port, labels: labels()},
		{name: "wl-2", port: port, labels: labels()},
	})
	run, err := p.start()
	if err != nil {
		t.Fatalf("fresh provider start: %v", err)
	}
	t.Cleanup(run.stop)

	if w := p.workloadFor("wl"); w != nil {
		t.Fatal("unextractable second candidate collapsed into one-to-one lifecycle authority")
	}
	if rec := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("valid contributor route = %d, want 200", rec.Code)
	}
	if got := daemon.startCount("wl-1") + daemon.startCount("wl-2") + daemon.stopCount("wl-1") + daemon.stopCount("wl-2"); got != 0 {
		t.Fatalf("provider issued %d lifecycle calls for ambiguous candidate topology", got)
	}
}

func TestWorkloadGrantRemovalLeavesContainerRunning(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)

	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cold start: %d", rec.Code)
	}
	w := p.workloadFor("wl")

	// The service disappears from discovery while ready; the entry
	// retires and its container is left as it is.
	daemon.swap(nil)
	mustSync(t, p)
	if p.workloadFor("wl") != nil {
		t.Fatal("retired workload still registered")
	}
	time.Sleep(300 * time.Millisecond)
	if got := daemon.stopCount("wl-1"); got != 0 {
		t.Fatalf("retired workload was stopped (%d calls)", got)
	}
	if got := w.phaseNow(); got != workloadReady {
		t.Fatalf("retired workload phase = %v, want ready untouched", got)
	}
}

func TestWorkloadReadyRequestExcludesIdleStop(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cold start: %d", rec.Code)
	}
	w := p.workloadFor("wl")

	// The ready decision and the active-count registration share one
	// critical section; an idle expiry in between must find the count.
	w.mu.Lock()
	binding := w.binding.key
	epoch := w.idleEpoch
	w.mu.Unlock()
	lease, err := w.ensureReady(context.Background(), p, binding)
	if err != nil {
		t.Fatalf("ensureReady: %v", err)
	}
	w.mu.Lock()
	active := w.binding.activity.active
	w.mu.Unlock()
	if active != 1 {
		t.Fatalf("active after ensureReady = %d, want 1", active)
	}
	stopsBefore := daemon.stopCount("wl-1")
	p.idleExpire(w, binding, epoch, p.currentRun())
	if got := w.phaseNow(); got != workloadReady {
		t.Fatalf("idle expiry with a counted request moved phase to %v", got)
	}
	if got := daemon.stopCount("wl-1"); got != stopsBefore {
		t.Fatalf("idle expiry with a counted request issued a stop")
	}
	w.end(p, lease)
}

func TestWorkloadRetryHoldsOneOuterLease(t *testing.T) {
	binding := &workloadBinding{key: 1, container: "wl-1", containerID: "id-1"}
	w := &workload{service: "wl", phase: workloadReady, binding: binding, policy: resolved.Workload{IdleAfter: time.Hour}}
	revision := workloadRoutingRevision("routes")
	p := &dockerProvider{srv: &server{}}
	p.srv.dynamic.Store(&dynamicTable{
		workloadBindings:  map[string]workloadBindingKey{"wl": binding.key},
		workloadRevisions: map[string]workloadRoutingRevision{"wl": revision},
	})
	gate := &workloadGate{p: p, w: w, binding: binding.key, revision: revision}
	attempts := 0
	inner := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		attempts++
		if _, release, err := gate.requestLease(r); err != nil || release {
			t.Fatalf("attempt %d lease: release=%v err=%v", attempts, release, err)
		}
		w.mu.Lock()
		active := w.binding.activity.active
		w.mu.Unlock()
		if active != 1 {
			t.Fatalf("attempt %d workload activity = %d, want one outer lease", attempts, active)
		}
		if attempts == 1 {
			rw.WriteHeader(http.StatusBadGateway)
			return
		}
		rw.WriteHeader(http.StatusOK)
	})
	retry := retryHandler(resolved.Middleware{Type: resolved.MWRetry, RetryMax: 2, RetryOnStatuses: []int{http.StatusBadGateway}}, inner)
	scope := &workloadRequestScope{p: p, w: w, next: retry}
	rec := runRequest(t, scope, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK || attempts != 2 {
		t.Fatalf("retry response = %d after %d attempts, want 200 after 2", rec.Code, attempts)
	}
	w.mu.Lock()
	active := binding.activity.active
	w.stopIdleLocked()
	w.mu.Unlock()
	if active != 0 {
		t.Fatalf("activity after retry = %d, want 0", active)
	}
}

func TestWorkloadTimeoutHoldsLeaseUntilInnerHandlerReturns(t *testing.T) {
	binding := &workloadBinding{key: 1, container: "wl-1", containerID: "id-1"}
	w := &workload{service: "wl", phase: workloadReady, binding: binding, policy: resolved.Workload{IdleAfter: time.Hour}}
	p := &dockerProvider{srv: &server{}}
	gate := &workloadGate{p: p, w: w, binding: binding.key}
	started := make(chan struct{})
	release := make(chan struct{})
	innerDone := make(chan struct{})
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		defer close(innerDone)
		state, ok := r.Context().Value(workloadRequestStateKey{}).(*workloadRequestState)
		if !ok || !state.beginGate() {
			return
		}
		defer state.endGate()
		if _, _, err := gate.requestLease(r); err != nil {
			return
		}
		close(started)
		<-release
	})
	scope := &workloadRequestScope{p: p, w: w, next: http.TimeoutHandler(inner, 20*time.Millisecond, "timed out")}
	done := make(chan struct{})
	go func() {
		scope.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
		close(done)
	}()
	waitSignal(t, started, "inner handler did not acquire workload lease")
	waitSignal(t, done, "timeout did not finish outer request scope")
	w.mu.Lock()
	active := binding.activity.active
	w.mu.Unlock()
	if active != 1 {
		t.Fatalf("activity after outer timeout = %d, want 1", active)
	}
	close(release)
	waitSignal(t, innerDone, "inner handler did not finish")
	w.mu.Lock()
	active = binding.activity.active
	w.stopIdleLocked()
	w.mu.Unlock()
	if active != 0 {
		t.Fatalf("activity after inner handler = %d, want 0", active)
	}
}

func TestCurrentPoolRejectsPreMutationGeneration(t *testing.T) {
	binding := workloadBindingKey(1)
	pool := &runningPool{}
	otherPool := &runningPool{}
	p := &dockerProvider{srv: &server{}}
	p.srv.dynamic.Store(&dynamicTable{
		pools:            map[string]*runningPool{"wl": pool, "other": otherPool},
		workloadBindings: map[string]workloadBindingKey{"wl": binding, "other": binding},
	})
	if got := p.currentPool("wl", binding); got != pool {
		t.Fatal("current generation pool was not available")
	}
	p.markMutationSettled("wl")
	if got := p.currentPool("wl", binding); got != nil {
		t.Fatal("pre-mutation generation pool remained available")
	}
	if got := p.currentPool("other", binding); got != otherPool {
		t.Fatal("one workload mutation invalidated an unrelated service pool")
	}
}

func TestWorkloadHTTPReadinessUsesExplicitHostOnBackupBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Host != "api.internal" {
			http.Error(rw, "wrong host", http.StatusNotFound)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	pool := &resolved.Pool{
		Backends:     []resolved.Backend{{Address: strings.TrimPrefix(backend.URL, "http://"), Backup: true}},
		UpstreamHost: resolved.HostExplicit,
		HostValue:    "api.internal",
	}
	handler, err := newPoolHandler(pool)
	if err != nil {
		t.Fatalf("newPoolHandler: %v", err)
	}
	t.Cleanup(handler.transport.CloseIdleConnections)
	binding := workloadBindingKey(1)
	p := &dockerProvider{srv: &server{}}
	p.srv.dynamic.Store(&dynamicTable{
		pools:            map[string]*runningPool{"wl": {handler: handler}},
		workloadBindings: map[string]workloadBindingKey{"wl": binding},
	})
	w := &workload{service: "wl"}
	act := &workloadActivation{binding: binding, policy: resolved.Workload{Readiness: resolved.WorkloadReadiness{Mode: resolved.ReadinessHTTP, Path: "/ready"}}}
	if !p.probeHTTP(context.Background(), w, act) {
		t.Fatal("HTTP readiness rejected explicit-Host backup backend")
	}
}

func TestWorkloadDefinitiveStopRejectionSkipsInspect(t *testing.T) {
	p, _, daemon := newFakeProviderDaemon(t, &resolved.Docker{}, []fakeDaemonContainer{{name: "wl-1"}})
	daemon.mu.Lock()
	daemon.rejectStop = true
	daemon.stallInspect = true
	daemon.inspectStarted = make(chan struct{})
	inspectStarted := daemon.inspectStarted
	id := daemon.find("wl-1").id
	daemon.mu.Unlock()
	binding := &workloadBinding{key: 1, container: "wl-1", containerID: id}
	w := &workload{binding: binding}
	stop := &workloadStop{binding: binding.key, ref: id}
	attempt := p.attemptOwnedStop(context.Background(), w, stop)
	if attempt.result != workloadStopRejected {
		t.Fatalf("definitive stop result = %v, want rejected", attempt.result)
	}
	select {
	case <-inspectStarted:
		t.Fatal("definitive stop rejection performed fallback inspect")
	default:
	}
}

func TestWorkloadStaleIdleCallbackCannotUseSuccessorRun(t *testing.T) {
	p := &dockerProvider{}
	oldRun := &dockerRun{provider: p, ctx: context.Background()}
	newRun := &dockerRun{provider: p, ctx: context.Background()}
	p.current = oldRun
	w := &workload{
		phase: workloadReady,
		binding: &workloadBinding{
			key:       1,
			container: "old",
		},
		nextBinding: 1,
		idleEpoch:   1,
	}

	oldRun.trackMu.Lock()
	done := make(chan struct{})
	go func() {
		p.idleExpire(w, 1, 1, oldRun)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for w.phaseNow() != workloadStopPending {
		if time.Now().After(deadline) {
			oldRun.trackMu.Unlock()
			t.Fatal("idle callback did not reach stop-pending")
		}
		time.Sleep(time.Millisecond)
	}

	w.mu.Lock()
	w.stopIdleLocked()
	w.bindContainerLocked(&docker.Service{Container: "new", ContainerID: "new-id"})
	w.phase = workloadReady
	w.mu.Unlock()
	p.current = newRun
	oldRun.stopping = true
	oldRun.trackMu.Unlock()
	waitSignal(t, done, "stale idle callback did not return")

	w.mu.Lock()
	phase, ref := w.phase, w.binding.ref()
	w.mu.Unlock()
	if phase != workloadReady || ref != "new-id" {
		t.Fatalf("successor after stale idle callback: phase=%v ref=%q", phase, ref)
	}
}

func TestWorkloadStaleCompletionDoesNotChangeCurrentActivity(t *testing.T) {
	oldBinding := &workloadBinding{key: 1, activity: workloadActivity{active: 1}}
	currentBinding := &workloadBinding{key: 2, containerID: "new-id", activity: workloadActivity{active: 1}}
	w := workload{
		phase:   workloadReady,
		binding: currentBinding,
	}
	w.end(nil, workloadLease{binding: oldBinding.key, activity: &oldBinding.activity})
	w.mu.Lock()
	active, idle := w.binding.activity.active, w.idle
	w.mu.Unlock()
	if oldBinding.activity.active != 0 || active != 1 || idle != nil {
		t.Fatalf("activity after stale completion: old=%d current=%d idle=%v", oldBinding.activity.active, active, idle)
	}
}

func TestWorkloadSuccessfulActivationReservesWaitingRequestActivity(t *testing.T) {
	binding := &workloadBinding{key: 1}
	act := &workloadActivation{binding: binding.key}
	act.done = make(chan struct{})
	act.waiting = 1
	w := &workload{
		phase:      workloadStarting,
		policy:     resolved.Workload{IdleAfter: time.Millisecond},
		binding:    binding,
		activation: act,
	}

	w.settleActivation(nil, act, nil)
	time.Sleep(5 * time.Millisecond)
	w.mu.Lock()
	phase, active, idle := w.phase, binding.activity.active, w.idle
	w.mu.Unlock()
	if phase != workloadReady || active != 1 || idle != nil {
		t.Fatalf("settled waiter: phase=%v active=%d idle=%v, want ready with reserved activity", phase, active, idle)
	}

	lease, failed := w.finishWaiting(nil, &act.workloadWait, true)
	if failed || lease.binding != binding.key {
		t.Fatalf("reserved waiter claim: lease=%+v failed=%v", lease, failed)
	}
	w.end(nil, lease)
	w.mu.Lock()
	active, idle = binding.activity.active, w.idle
	w.stopIdleLocked()
	w.mu.Unlock()
	if active != 0 || idle == nil {
		t.Fatalf("completed waiter: active=%d idle=%v, want idle countdown", active, idle)
	}
}

func TestWorkloadRejectedIdleStopReservesWaitingRequestActivity(t *testing.T) {
	binding := &workloadBinding{key: 1}
	stop := &workloadStop{kind: workloadIdleStop, binding: binding.key}
	stop.done = make(chan struct{})
	stop.waiting = 1
	w := &workload{
		phase:   workloadStopIssued,
		policy:  resolved.Workload{IdleAfter: time.Millisecond},
		binding: binding,
		stop:    stop,
	}

	w.mu.Lock()
	w.settleStopLocked(nil, stop, workloadStopRejected)
	w.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	w.mu.Lock()
	phase, active, idle := w.phase, binding.activity.active, w.idle
	w.mu.Unlock()
	if phase != workloadReady || active != 1 || idle != nil {
		t.Fatalf("settled stop waiter: phase=%v active=%d idle=%v, want ready with reserved activity", phase, active, idle)
	}

	lease, failed := w.finishWaiting(nil, &stop.workloadWait, true)
	if failed || lease.binding != binding.key {
		t.Fatalf("reserved stop waiter claim: lease=%+v failed=%v", lease, failed)
	}
	w.end(nil, lease)
	w.mu.Lock()
	active, idle = binding.activity.active, w.idle
	w.stopIdleLocked()
	w.mu.Unlock()
	if active != 0 || idle == nil {
		t.Fatalf("completed stop waiter: active=%d idle=%v, want idle countdown", active, idle)
	}
}

func TestWorkloadContainerIDRefinesExistingBinding(t *testing.T) {
	binding := &workloadBinding{key: 1, container: "wl-1", activity: workloadActivity{active: 1}}
	w := workload{
		phase:       workloadReady,
		policy:      resolved.Workload{IdleAfter: time.Hour},
		binding:     binding,
		nextBinding: binding.key,
	}
	w.mu.Lock()
	changed := w.bindContainerLocked(&docker.Service{Container: "wl-1", ContainerID: "new-id"})
	w.mu.Unlock()
	if changed || w.binding != binding {
		t.Fatal("a name-to-ID refinement replaced the container binding")
	}
	if got := binding.ref(); got != "new-id" {
		t.Fatalf("refined Docker reference = %q, want new-id", got)
	}
	pool := &runningPool{}
	p := &dockerProvider{srv: &server{}}
	p.srv.dynamic.Store(&dynamicTable{
		pools:            map[string]*runningPool{"wl": pool},
		workloadBindings: map[string]workloadBindingKey{"wl": binding.key},
	})
	if got := p.currentPool("wl", binding.key); got != pool {
		t.Fatal("a name-to-ID refinement detached the generation pool")
	}
	w.end(nil, workloadLease{binding: binding.key, activity: &binding.activity})
	w.mu.Lock()
	active, idle := w.binding.activity.active, w.idle
	w.stopIdleLocked()
	w.mu.Unlock()
	if active != 0 || idle == nil {
		t.Fatalf("activity after refined completion: active=%d idle=%v", active, idle)
	}
}

func TestWorkloadOperationRefRefinesToContainerID(t *testing.T) {
	w := &workload{}
	w.mu.Lock()
	w.bindContainerLocked(&docker.Service{Container: "wl-1"})
	binding := w.binding.key
	stop := w.newStopLocked(nil, workloadIdleStop, binding, "wl-1")
	w.bindContainerLocked(&docker.Service{Container: "wl-1", ContainerID: "immutable-id"})
	w.mu.Unlock()

	if got := w.callRef(stop.binding, stop.ref); got != "immutable-id" {
		t.Fatalf("refined operation ref = %q, want immutable ID", got)
	}
}

func TestWorkloadStopSettlementVersionsBeforeStateChange(t *testing.T) {
	registry, err := openMutationRegistry(t.TempDir(), "test-endpoint")
	if err != nil {
		t.Fatalf("open mutation registry: %v", err)
	}
	p := &dockerProvider{current: &dockerRun{registry: registry}}
	binding := &workloadBinding{key: 1, container: "wl-1", containerID: "immutable-id"}
	stop := &workloadStop{binding: binding.key, issued: true}
	stop.done = make(chan struct{})
	w := &workload{service: "wl", phase: workloadStopIssued, binding: binding, stop: stop}
	p.generationMu.Lock()
	settled := make(chan workloadStopApply, 1)
	go func() {
		settled <- w.applyStopAttempt(p, stop, workloadStopAttempt{result: workloadStopSucceeded})
	}()
	deadline := time.Now().Add(time.Second)
	for w.mu.TryLock() {
		w.mu.Unlock()
		if time.Now().After(deadline) {
			p.generationMu.Unlock()
			t.Fatal("stop settlement did not acquire workload lock")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-settled:
		p.generationMu.Unlock()
		t.Fatal("stop state changed before mutation version could advance")
	default:
	}
	p.generationMu.Unlock()
	if got := <-settled; got != workloadStopSettled {
		t.Fatalf("stop settlement = %v, want settled", got)
	}
	if p.currentMutationVersion("wl") != 1 || w.phaseNow() != workloadDormant {
		t.Fatalf("settled state: version=%d phase=%v", p.currentMutationVersion("wl"), w.phaseNow())
	}
}

func TestWorkloadFreshRejectedStopSettlesPreparedMutation(t *testing.T) {
	registry, err := openMutationRegistry(t.TempDir(), "test-endpoint")
	if err != nil {
		t.Fatalf("open mutation registry: %v", err)
	}
	record := mutationRecord{ContainerID: "immutable-id", ContainerName: "wl-1", Service: "wl", Kind: mutationRecordIdleStop, State: mutationRecordPrepared}
	if err := registry.put(record); err != nil {
		t.Fatalf("persist prepared mutation: %v", err)
	}
	p := &dockerProvider{
		current:          &dockerRun{registry: registry, kick: make(chan struct{}, 1)},
		mutationVersions: map[string]uint64{},
	}
	binding := &workloadBinding{key: 1, container: "wl-1", containerID: "immutable-id"}
	stop := &workloadStop{kind: workloadIdleStop, binding: binding.key, issued: true, persisted: true}
	stop.done = make(chan struct{})
	w := &workload{
		service: "wl",
		policy:  resolved.Workload{IdleAfter: time.Hour},
		phase:   workloadStopIssued,
		binding: binding,
		stop:    stop,
	}
	t.Cleanup(func() {
		w.mu.Lock()
		w.stopIdleLocked()
		w.mu.Unlock()
	})

	got := w.applyStopAttempt(p, stop, workloadStopAttempt{result: workloadStopRejected, stopErr: fmt.Errorf("definitive rejection")})
	if got != workloadStopSettled {
		t.Fatalf("fresh rejected stop = %v, want settled", got)
	}
	if registry.contains("immutable-id") {
		t.Fatal("fresh definitive rejection retained prepared mutation")
	}
	if phase := w.phaseNow(); phase != workloadReady {
		t.Fatalf("workload phase = %v, want ready", phase)
	}
}

func TestWorkloadContainerIDNeverDowngradesToName(t *testing.T) {
	binding := &workloadBinding{key: 1, container: "wl-1", containerID: "id-a"}
	w := workload{binding: binding, nextBinding: binding.key}
	w.mu.Lock()
	changed := w.bindContainerLocked(&docker.Service{Container: "wl-1"})
	w.mu.Unlock()
	if changed || w.binding.key != binding.key || w.binding.ref() != "id-a" {
		t.Fatalf("missing ID weakened binding: changed=%v key=%d ref=%q", changed, w.binding.key, w.binding.ref())
	}

	w.mu.Lock()
	changed = w.bindContainerLocked(&docker.Service{Container: "wl-1", ContainerID: "id-b"})
	w.mu.Unlock()
	if !changed || w.binding.key == binding.key || w.binding.ref() != "id-b" {
		t.Fatalf("same-name recreation was absorbed: changed=%v key=%d ref=%q", changed, w.binding.key, w.binding.ref())
	}
}

func TestWorkloadWaitersBelongToTheirOutcome(t *testing.T) {
	oldBinding := &workloadBinding{key: 1, container: "wl-1", containerID: "id-a"}
	oldActivation := &workloadActivation{binding: oldBinding.key}
	oldActivation.done = make(chan struct{})
	w := workload{
		phase:       workloadStarting,
		binding:     oldBinding,
		nextBinding: oldBinding.key,
		activation:  oldActivation,
	}
	w.mu.Lock()
	_, oldWait, err := w.serveCurrentStateLocked(nil)
	w.mu.Unlock()
	if err != nil || oldWait.waiting != 1 {
		t.Fatalf("old wait registration: waiters=%d err=%v", oldWait.waiting, err)
	}

	w.mu.Lock()
	w.bindContainerLocked(&docker.Service{Container: "wl-1", ContainerID: "id-b"})
	newActivation := &workloadActivation{observe: true, binding: w.binding.key}
	newActivation.done = make(chan struct{})
	w.toLocked(workloadStarting)
	w.activation = newActivation
	_, newWait, err := w.serveCurrentStateLocked(nil)
	w.mu.Unlock()
	if err != nil || newWait.waiting != 1 {
		t.Fatalf("new wait registration: waiters=%d err=%v", newWait.waiting, err)
	}

	out := w.settleActivation(nil, newActivation, errWorkloadStopped)
	if out.waiters != 1 || oldWait.waiting != 1 {
		t.Fatalf("successor settlement: logged=%d old=%d, want 1 and 1", out.waiters, oldWait.waiting)
	}
	_, _ = w.finishWaiting(nil, oldWait, false)
	_, _ = w.finishWaiting(nil, newWait, false)
}

func TestWorkloadShutdownDuringActivationIssuesNoStop(t *testing.T) {
	policy := testWorkloadPolicy()
	policy.ReadyTimeout = 30 * time.Second

	// Port 1 answers nothing; the activation is still in flight when
	// the provider run stops.
	p, daemon, router := workloadFixture(t, policy, "http://127.0.0.1:1")

	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
		done <- rec.Code
	}()
	waitWorkloadPhase(t, p, workloadStarting)
	w := p.workloadFor("wl")

	p.lifecycleMu.Lock()
	run := p.current
	p.lifecycleMu.Unlock()
	run.stop()

	if code := <-done; code != http.StatusServiceUnavailable {
		t.Fatalf("waiter during shutdown: %d, want 503", code)
	}
	if got := daemon.stopCount("wl-1"); got != 0 {
		t.Fatalf("shutdown issued %d stop calls, want 0", got)
	}
	w.mu.Lock()
	phase, failedUntil := w.phase, w.failedUntil
	w.mu.Unlock()
	if phase != workloadDormant {
		t.Fatalf("phase after abandoned activation = %v, want dormant", phase)
	}
	if !failedUntil.IsZero() {
		t.Fatal("abandoned activation left a backoff window")
	}
}

func TestWorkloadShutdownQuiescesIdleStopsBeforeRequestDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := &dockerProvider{workloadEntries: make(map[string]*workload)}
	run := &dockerRun{provider: p, ctx: ctx, cancel: cancel}
	p.current = run
	binding := &workloadBinding{key: 1, activity: workloadActivity{active: 1}}
	w := &workload{
		service: "wl",
		phase:   workloadReady,
		policy:  resolved.Workload{IdleAfter: time.Millisecond},
		binding: binding,
	}
	p.workloadEntries[w.service] = w

	run.quiesceWorkloads()
	w.end(p, workloadLease{binding: binding.key, activity: &binding.activity})
	time.Sleep(10 * time.Millisecond)
	w.mu.Lock()
	phase, idle := w.phase, w.idle
	w.mu.Unlock()
	if phase != workloadReady || idle != nil {
		t.Fatalf("drained request armed shutdown idle stop: phase=%v idle=%v", phase, idle)
	}
}

func TestWorkloadShutdownRevokesFiredIdleCallbackBeforeIssuance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := &dockerProvider{workloadEntries: make(map[string]*workload)}
	run := &dockerRun{provider: p, ctx: ctx, cancel: cancel}
	p.current = run
	binding := &workloadBinding{key: 1}
	w := &workload{service: "wl", phase: workloadReady, binding: binding, idleEpoch: 1}
	p.workloadEntries[w.service] = w

	run.trackMu.Lock()
	done := make(chan struct{})
	go func() {
		p.idleExpire(w, binding.key, 1, run)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for w.phaseNow() != workloadStopPending {
		if time.Now().After(deadline) {
			run.trackMu.Unlock()
			t.Fatal("fired idle callback did not become stop-pending")
		}
		time.Sleep(time.Millisecond)
	}
	run.quiesceWorkloads()
	run.trackMu.Unlock()
	waitSignal(t, done, "fired idle callback did not return")
	run.wg.Wait()
	w.mu.Lock()
	phase, stop := w.phase, w.stop
	w.mu.Unlock()
	if phase != workloadReady || stop != nil {
		t.Fatalf("shutdown allowed fired callback to issue stop: phase=%v stop=%v", phase, stop)
	}
}

func TestWorkloadStoppedCoveredContainerKeepsRefusalEnvelope(t *testing.T) {
	fallbackHit := false
	cfg := &resolved.Docker{
		TraefikLabels: true,
		Workloads:     map[string]resolved.Workload{"api@traefik": testWorkloadPolicy()},
	}
	p, srv, _ := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{
		{name: "api-1", stopped: true, labels: map[string]string{
			"traefik.enable":                                     "true",
			"traefik.http.routers.r1.rule":                       "Host(`covered.example.com`) && Header(`X-K`, `v`)",
			"traefik.http.routers.r1.service":                    "api",
			"traefik.http.services.api.loadbalancer.server.port": "7000",
		}},
		{name: "other-1", stopped: true, labels: map[string]string{
			"traefik.enable":               "true",
			"traefik.http.routers.r2.rule": "Host(`other.example.com`) && Header(`X-K`, `v`)",
		}},
	})
	srv.cfg.Fallback = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHit = true
		w.WriteHeader(http.StatusTeapot)
	})
	mustSync(t, p)
	router := srv.buildRouter()

	// The covered registration's unparseable rule keeps refusing while
	// its container is stopped.
	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://covered.example.com/", nil))
	if rec.Code != http.StatusNotFound || fallbackHit {
		t.Fatalf("covered refusal: %d (fallback hit: %v), want tombstoned 404", rec.Code, fallbackHit)
	}
	// The uncovered stopped container stays invisible: its traffic
	// reaches the fallback like any unmatched request.
	rec = runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://other.example.com/", nil))
	if rec.Code != http.StatusTeapot || !fallbackHit {
		t.Fatalf("uncovered stopped container: %d, want fallback", rec.Code)
	}
}

func TestWorkloadBackoffSurvivesRecreation(t *testing.T) {
	policy := testWorkloadPolicy()
	policy.BackoffBase = 5 * time.Second
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, router := workloadFixture(t, policy, backend.URL)
	daemon.mu.Lock()
	daemon.failStart = true
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	containers := daemon.containers
	oldID := containers[0].id
	daemon.mu.Unlock()
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()

	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed activation: %d", rec.Code)
	}
	waitSignal(t, stopStarted, "failed activation did not issue cleanup stop")

	// The container is recreated: it leaves one listing entirely and
	// returns under the same name. The backoff window must survive.
	daemon.swap(nil)
	mustSync(t, p)
	containers[0] = daemon.recreate(containers[0])
	if containers[0].id == oldID {
		t.Fatal("recreated container retained its old ID")
	}
	daemon.swap(containers)
	mustSync(t, p)

	rec = runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("recreated crash-looper: %d (Retry-After %q), want immediate backoff 503", rec.Code, rec.Header().Get("Retry-After"))
	}
	if got := daemon.startCount("wl-1"); got != 1 {
		t.Fatalf("start calls = %d, want 1: recreation shed the backoff", got)
	}
	releaseStop()
}

func TestWorkloadStaleObserveLeavesNoBackoff(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, _ := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	w := p.workloadFor("wl")

	// An observe-only activation whose container turns out stopped, the
	// stale-listing race, corrects to dormant without a backoff window.
	w.mu.Lock()
	act, err := p.beginActivationLocked(w, true)
	w.mu.Unlock()
	if err != nil {
		t.Fatalf("beginActivation: %v", err)
	}
	<-act.done
	w.mu.Lock()
	phase, failedUntil := w.phase, w.failedUntil
	w.mu.Unlock()
	if phase != workloadDormant || !failedUntil.IsZero() {
		t.Fatalf("stale observe: phase %v, backoff until %v; want dormant without backoff", phase, failedUntil)
	}
	if got := daemon.stopCount("wl-1"); got != 0 {
		t.Fatalf("stale observe issued %d stop calls", got)
	}
}

func TestWorkloadRecreationRequiresFreshReadiness(t *testing.T) {
	policy := testWorkloadPolicy()
	policy.IdleAfter = 5 * time.Second
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, router := workloadFixture(t, policy, backend.URL)
	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cold start: %d", rec.Code)
	}
	w := p.workloadFor("wl")

	// The container is removed while the workload is ready; the retained
	// entry keeps its phase.
	daemon.mu.Lock()
	containers := daemon.containers
	oldID := containers[0].id
	daemon.mu.Unlock()
	daemon.swap(nil)
	mustSync(t, p)
	if got := w.phaseNow(); got != workloadReady {
		t.Fatalf("retained phase = %v, want the stale ready this test exercises", got)
	}

	// Recreated and running, HEALTHCHECK still "starting": the entry
	// must re-enter the readiness gate.
	recreated := make([]fakeDaemonContainer, len(containers))
	copy(recreated, containers)
	recreated[0] = daemon.recreate(recreated[0])
	if recreated[0].id == oldID {
		t.Fatal("recreated container retained its old ID")
	}
	recreated[0].health = "starting"
	daemon.swap(recreated)
	mustSync(t, p)
	waitWorkloadPhase(t, p, workloadStarting)
	if got := daemon.startCount("wl-1"); got != 1 {
		t.Fatalf("observe adoption issued %d start calls, want the original 1", got)
	}

	daemon.mu.Lock()
	daemon.find("wl-1").health = "healthy"
	daemon.mu.Unlock()
	waitWorkloadPhase(t, p, workloadReady)
	w.mu.Lock()
	idleArmed := w.idle != nil
	w.mu.Unlock()
	if !idleArmed {
		t.Fatal("re-proven readiness left no idle timer armed")
	}
}

func TestWorkloadMultiServiceContainerIsNotGated(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	// One container contributes two Traefik services; a stop would act
	// on both, so the policy naming one of them must fail closed.
	cfg := &resolved.Docker{
		TraefikLabels: true,
		Workloads:     map[string]resolved.Workload{"a@traefik": testWorkloadPolicy()},
	}
	p, srv, daemon := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{{
		name: "combo-1", ip: host, port: port,
		labels: map[string]string{
			"traefik.enable":                                   "true",
			"traefik.http.routers.ra.rule":                     "Host(`a.example.com`)",
			"traefik.http.routers.ra.service":                  "a",
			"traefik.http.routers.rb.rule":                     "Host(`b.example.com`)",
			"traefik.http.routers.rb.service":                  "b",
			"traefik.http.services.a.loadbalancer.server.port": portStr,
			"traefik.http.services.b.loadbalancer.server.port": portStr,
		},
	}})
	run, err := p.start()
	if err != nil {
		t.Fatalf("fresh provider start: %v", err)
	}
	t.Cleanup(run.stop)

	if w := p.workloadFor("a@traefik"); w != nil {
		t.Fatal("multi-service container got a workload entry")
	}
	router := srv.buildRouter()
	for _, h := range []string{"a.example.com", "b.example.com"} {
		rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://"+h+"/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("ungated route %s: %d, want 200", h, rec.Code)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := daemon.stopCount("combo-1") + daemon.startCount("combo-1"); got != 0 {
		t.Fatalf("provider issued %d lifecycle calls without a single-service owner", got)
	}
}

func workloadTopologyLabels(port string, sibling bool) map[string]string {
	labels := map[string]string{
		"traefik.enable":                                   "true",
		"traefik.http.routers.ra.middlewares":              "policy@file",
		"traefik.http.routers.ra.rule":                     "Host(`a.example.com`)",
		"traefik.http.routers.ra.service":                  "a",
		"traefik.http.services.a.loadbalancer.server.port": port,
	}
	if sibling {
		labels["traefik.http.routers.rb.middlewares"] = "policy@file"
		labels["traefik.http.routers.rb.rule"] = "Host(`b.example.com`)"
		labels["traefik.http.routers.rb.service"] = "b"
		labels["traefik.http.services.b.loadbalancer.server.port"] = port
	}
	return labels
}

func assertWorkloadTopologyRoutes(t *testing.T, router http.Handler, status int, policyHeader string) {
	t.Helper()
	for _, host := range []string{"a.example.com", "b.example.com"} {
		rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil))
		if rec.Code != status {
			t.Fatalf("route %s = %d, want %d", host, rec.Code, status)
		}
		if got := rec.Header().Get("X-Policy"); got != policyHeader {
			t.Fatalf("route %s: X-Policy=%q, want %q", host, got, policyHeader)
		}
	}
}

func assertDynamicTableShape(t *testing.T, table *dynamicTable, routes, quarantines, tombstones, pools int) {
	t.Helper()
	if len(table.routes) != routes || len(table.quarantines) != quarantines || len(table.tombstones) != tombstones || len(table.pools) != pools {
		t.Fatalf("dynamic table routes/quarantines/tombstones/pools = %d/%d/%d/%d, want %d/%d/%d/%d", len(table.routes), len(table.quarantines), len(table.tombstones), len(table.pools), routes, quarantines, tombstones, pools)
	}
}

func assertRouteResponse(t *testing.T, router http.Handler, target string, status int, body string) {
	t.Helper()
	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != status || (body != "" && rec.Body.String() != body) {
		t.Fatalf("route %s = %d %q, want %d %q", target, rec.Code, rec.Body.String(), status, body)
	}
}

func waitRetiredWorkloadDormant(t *testing.T, w *workload) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if w.phaseNow() == workloadDormant {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("retired workload phase = %v, want dormant", w.phaseNow())
}

func TestWorkloadRetiredIssuedStopQuarantinesSiblingRoutes(t *testing.T) {
	backendHits := make(chan struct{}, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits <- struct{}{}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	cfg := &resolved.Docker{
		TraefikLabels: true,
		Middleware: map[string][]resolved.Middleware{
			"policy@file": {mustResolveMW(t, SetResponseHeader("X-Policy", "applied"))},
		},
		Workloads: map[string]resolved.Workload{"a@traefik": testWorkloadPolicy()},
	}
	p, srv, daemon := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{{
		name: "combo-1", ip: host, port: port, labels: workloadTopologyLabels(portStr, false),
	}})
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	w := p.workloadFor("a@traefik")
	if w == nil {
		t.Fatal("initial one-to-one service has no workload")
	}
	waitSignal(t, stopStarted, "idle stop was not issued")
	if got := w.phaseNow(); got != workloadStopIssued {
		t.Fatalf("phase before topology change = %v, want stop-issued", got)
	}

	daemon.swap([]fakeDaemonContainer{{
		name: "combo-1", ip: host, port: port, labels: workloadTopologyLabels(portStr, true),
	}})
	mustSync(t, p)
	if got := p.workloadFor("a@traefik"); got != nil {
		t.Fatal("multi-service topology retained lifecycle authority")
	}
	assertWorkloadTopologyRoutes(t, srv.buildRouter(), http.StatusServiceUnavailable, "")
	if got := len(backendHits); got != 0 {
		t.Fatalf("quarantined routes reached backend %d times", got)
	}

	releaseStop()
	waitRetiredWorkloadDormant(t, w)
	daemon.mu.Lock()
	daemon.find("combo-1").stopped = false
	daemon.mu.Unlock()
	mustSync(t, p)
	assertWorkloadTopologyRoutes(t, srv.buildRouter(), http.StatusOK, "applied")
	if got := daemon.stopCount("combo-1"); got != 1 {
		t.Fatalf("stop calls = %d, want 1", got)
	}
	if got := daemon.startCount("combo-1"); got != 0 {
		t.Fatalf("retired workload issued %d start calls", got)
	}
}

func TestWorkloadRetiredStopSettlementRepublishesRoutes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	cfg := &resolved.Docker{
		TraefikLabels: true,
		Middleware: map[string][]resolved.Middleware{
			"policy@file": {mustResolveMW(t, SetResponseHeader("X-Policy", "applied"))},
		},
		Workloads: map[string]resolved.Workload{"a@traefik": testWorkloadPolicy()},
	}
	p, srv, daemon := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{{
		name: "combo-1", ip: host, port: port, labels: workloadTopologyLabels(portStr, false),
	}})
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	w := p.workloadFor("a@traefik")
	if w == nil {
		t.Fatal("initial one-to-one service has no workload")
	}
	waitSignal(t, stopStarted, "idle stop was not issued")

	daemon.swap([]fakeDaemonContainer{{
		name: "combo-1", ip: host, port: port, labels: workloadTopologyLabels(portStr, true),
	}})
	mustSync(t, p)
	assertWorkloadTopologyRoutes(t, srv.buildRouter(), http.StatusServiceUnavailable, "")
	p.generationMu.Lock()
	republished := p.generationChanged
	p.generationMu.Unlock()

	releaseStop()
	waitRetiredWorkloadDormant(t, w)
	waitSignal(t, republished, "settled retired stop did not republish routes")
	daemon.mu.Lock()
	daemon.find("combo-1").stopped = false
	daemon.mu.Unlock()
	mustSync(t, p)
	assertWorkloadTopologyRoutes(t, srv.buildRouter(), http.StatusOK, "applied")
	if got := daemon.stopCount("combo-1"); got != 1 {
		t.Fatalf("stop calls = %d, want 1", got)
	}
	if got := daemon.startCount("combo-1"); got != 0 {
		t.Fatalf("retired workload issued %d start calls", got)
	}
}

func TestWorkloadMutationQuarantineBypassesServingCompilation(t *testing.T) {
	backendHits := make(chan struct{}, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits <- struct{}{}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	cfg := &resolved.Docker{
		TraefikLabels: true,
		Middleware: map[string][]resolved.Middleware{
			"policy@file": {mustResolveMW(t, SetResponseHeader("X-Policy", "applied"))},
		},
		Workloads: map[string]resolved.Workload{"a@traefik": testWorkloadPolicy()},
	}
	p, srv, daemon := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{{
		name: "combo-1", ip: host, port: port, labels: workloadTopologyLabels(portStr, false),
	}})
	fallbackCalls := fallbackServer(t, srv, nil)
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	w := p.workloadFor("a@traefik")
	if w == nil {
		t.Fatal("initial one-to-one service has no workload")
	}
	waitSignal(t, stopStarted, "idle stop was not issued")

	labels := workloadTopologyLabels(portStr, true)
	labels["traefik.http.routers.ra.middlewares"] = "unknown@file"
	labels["traefik.http.routers.ra-copy.rule"] = "Host(`a.example.com`)"
	labels["traefik.http.routers.ra-copy.service"] = "a"
	labels["traefik.http.routers.ra-copy.middlewares"] = "unknown@file"
	labels["traefik.http.services.b.loadbalancer.healthcheck.path"] = "/ready"
	labels["traefik.http.services.b.loadbalancer.healthcheck.interval"] = "not-a-duration"
	daemon.swap([]fakeDaemonContainer{{name: "combo-1", ip: host, port: port, labels: labels}})
	mustSync(t, p)

	tab := srv.dynamic.Load()
	if len(tab.routes) != 0 || len(tab.quarantines) != 3 || len(tab.pools) != 0 || len(tab.tombstones) != 0 {
		t.Fatalf("quarantine table routes/quarantines/pools/tombstones = %d/%d/%d/%d, want 0/3/0/0", len(tab.routes), len(tab.quarantines), len(tab.pools), len(tab.tombstones))
	}
	assertWorkloadTopologyRoutes(t, srv.buildRouter(), http.StatusServiceUnavailable, "")
	if got := len(backendHits); got != 0 {
		t.Fatalf("quarantine reached backend %d times", got)
	}
	p.generationMu.Lock()
	republished := p.generationChanged
	p.generationMu.Unlock()

	daemon.mu.Lock()
	daemon.failList = true
	daemon.listStarted = make(chan struct{})
	daemon.listRelease = make(chan struct{})
	listStarted := daemon.listStarted
	listRelease := daemon.listRelease
	daemon.mu.Unlock()
	releaseStop()
	waitRetiredWorkloadDormant(t, w)
	waitSignal(t, listStarted, "settlement did not trigger quarantine publication")
	daemon.mu.Lock()
	daemon.failList = false
	daemon.listRelease = nil
	daemon.mu.Unlock()
	close(listRelease)
	waitSignal(t, republished, "settled quarantine did not publish normal refusal semantics")
	assertWorkloadTopologyRoutes(t, srv.buildRouter(), http.StatusNotFound, "")
	if got := fallbackCalls.Load(); got != 0 {
		t.Fatalf("normal refusal called fallback %d times", got)
	}
	if got := len(backendHits); got != 0 {
		t.Fatalf("invalid serving configuration reached backend %d times", got)
	}
}

func TestWorkloadMutationQuarantineSurvivesExtractionRefusal(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("unexpected"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	cfg := &resolved.Docker{
		TraefikLabels: true,
		Middleware: map[string][]resolved.Middleware{
			"policy@file": {mustResolveMW(t, SetResponseHeader("X-Policy", "applied"))},
		},
		Workloads: map[string]resolved.Workload{"a@traefik": testWorkloadPolicy()},
	}
	p, srv, daemon := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{{
		name: "combo-1", ip: host, port: port, labels: workloadTopologyLabels(portStr, false),
	}})
	fallbackCalls := fallbackServer(t, srv, nil)
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	w := p.workloadFor("a@traefik")
	waitSignal(t, stopStarted, "idle stop was not issued")

	daemon.swap([]fakeDaemonContainer{{
		name: "combo-1", ip: host, port: port,
		labels: map[string]string{
			"traefik.enable":                                   "true",
			"traefik.http.routers.ra.rule":                     "HostRegexp(`{subdomain:[a-z]+}.example.com`)",
			"traefik.http.routers.ra.service":                  "a",
			"traefik.http.services.a.loadbalancer.server.port": portStr,
		},
	}})
	mustSync(t, p)
	tab := srv.dynamic.Load()
	assertDynamicTableShape(t, tab, 0, 1, 0, 0)
	if dockerWarningContains(p, `router "ra"`, "dropping its routes") {
		t.Fatal("quarantined extraction announced an ordinary route refusal")
	}
	assertRouteResponse(t, srv.buildRouter(), "http://invalid.example.com/", http.StatusServiceUnavailable, "")
	p.generationMu.Lock()
	republished := p.generationChanged
	p.generationMu.Unlock()

	releaseStop()
	waitRetiredWorkloadDormant(t, w)
	waitSignal(t, republished, "settlement did not republish extraction refusal")
	assertRouteResponse(t, srv.buildRouter(), "http://invalid.example.com/", http.StatusNotFound, "404 page not found\n")
	if !dockerWarningContains(p, `router "ra"`, "dropping its routes") {
		t.Fatal("settled extraction refusal was not announced")
	}
	if got := fallbackCalls.Load(); got != 0 {
		t.Fatalf("extraction refusal reached fallback %d times", got)
	}
}

func TestWorkloadMutationQuarantineSurvivesServiceKeyReplacement(t *testing.T) {
	backendC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("container-c"))
	}))
	t.Cleanup(backendC.Close)
	backendD := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("container-d"))
	}))
	t.Cleanup(backendD.Close)
	hostC, portCStr, _ := net.SplitHostPort(strings.TrimPrefix(backendC.URL, "http://"))
	portC, _ := strconv.Atoi(portCStr)
	hostD, portDStr, _ := net.SplitHostPort(strings.TrimPrefix(backendD.URL, "http://"))
	portD, _ := strconv.Atoi(portDStr)
	cfg := &resolved.Docker{
		TraefikLabels: true,
		Middleware: map[string][]resolved.Middleware{
			"policy@file": {mustResolveMW(t, SetResponseHeader("X-Policy", "applied"))},
		},
		Workloads: map[string]resolved.Workload{"a@traefik": testWorkloadPolicy()},
	}
	p, srv, daemon := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{{
		name: "container-c", ip: hostC, port: portC, labels: workloadTopologyLabels(portCStr, false),
	}})
	fallbackCalls := fallbackServer(t, srv, nil)
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	w := p.workloadFor("a@traefik")
	waitSignal(t, stopStarted, "idle stop was not issued")

	invalidC := map[string]string{
		"traefik.enable":                                   "true",
		"traefik.http.routers.ra.rule":                     "HostRegexp(`{subdomain:[a-z]+}.example.com`)",
		"traefik.http.routers.ra.service":                  "a",
		"traefik.http.services.a.loadbalancer.server.port": portCStr,
	}
	validD := map[string]string{
		"traefik.enable":                                   "true",
		"traefik.http.routers.rd.rule":                     "Host(`d.example.com`)",
		"traefik.http.routers.rd.service":                  "a",
		"traefik.http.services.a.loadbalancer.server.port": portDStr,
	}
	successorPolicy := testWorkloadPolicy()
	successorPolicy.IdleAfter = time.Hour
	p.cfg.Workloads["a@traefik"] = successorPolicy
	daemon.swap([]fakeDaemonContainer{
		{name: "container-c", ip: hostC, port: portC, labels: invalidC},
		{name: "container-d", ip: hostD, port: portD, labels: validD},
	})
	mustSync(t, p)
	if got := p.workloadFor("a@traefik"); got == nil || got == w {
		t.Fatal("service-key successor did not receive independent lifecycle authority")
	}
	assertDynamicTableShape(t, srv.dynamic.Load(), 1, 1, 0, 1)
	assertRouteResponse(t, srv.buildRouter(), "http://d.example.com/", http.StatusOK, "container-d")
	assertRouteResponse(t, srv.buildRouter(), "http://quarantined.example.com/", http.StatusServiceUnavailable, "")
	p.generationMu.Lock()
	republished := p.generationChanged
	p.generationMu.Unlock()

	releaseStop()
	waitRetiredWorkloadDormant(t, w)
	waitSignal(t, republished, "settlement did not transfer the service grant")
	assertRouteResponse(t, srv.buildRouter(), "http://d.example.com/", http.StatusOK, "container-d")
	assertRouteResponse(t, srv.buildRouter(), "http://quarantined.example.com/", http.StatusNotFound, "404 page not found\n")
	if got := fallbackCalls.Load(); got != 0 {
		t.Fatalf("replacement refusal reached fallback %d times", got)
	}
}

func TestWorkloadRenamedMutationOwnerDoesNotBlockSuccessorAuthority(t *testing.T) {
	backendC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("container-c"))
	}))
	t.Cleanup(backendC.Close)
	backendD := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("container-d"))
	}))
	t.Cleanup(backendD.Close)
	hostC, portCStr, _ := net.SplitHostPort(strings.TrimPrefix(backendC.URL, "http://"))
	portC, _ := strconv.Atoi(portCStr)
	hostD, portDStr, _ := net.SplitHostPort(strings.TrimPrefix(backendD.URL, "http://"))
	portD, _ := strconv.Atoi(portDStr)
	labels := func(service, host string) map[string]string {
		return map[string]string{
			"statute.enable":  "true",
			"statute.service": service,
			"statute.host":    host,
		}
	}
	p, srv, daemon := newFakeProviderDaemon(t, &resolved.Docker{
		Workloads: map[string]resolved.Workload{
			"a": testWorkloadPolicy(),
			"b": testWorkloadPolicy(),
		},
	}, []fakeDaemonContainer{{name: "container-c", ip: hostC, port: portC, labels: labels("a", "a.example.com")}})
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	w := p.workloadFor("a")
	waitSignal(t, stopStarted, "old-service idle stop was not issued")

	daemon.swap([]fakeDaemonContainer{
		{name: "container-c", port: portC, labels: labels("b", "old-b.example.com")},
		{name: "container-d", ip: hostD, port: portD, labels: labels("b", "new-b.example.com")},
	})
	mustSync(t, p)
	if got := p.workloadFor("b"); got == nil || got == w {
		t.Fatal("renamed mutation owner blocked independent successor authority")
	}
	assertRouteResponse(t, srv.buildRouter(), "http://new-b.example.com/", http.StatusOK, "container-d")
	assertRouteResponse(t, srv.buildRouter(), "http://old-b.example.com/", http.StatusServiceUnavailable, "")
}

func TestWorkloadMutationQuarantineDoesNotCrossContributors(t *testing.T) {
	backendC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("container-c"))
	}))
	t.Cleanup(backendC.Close)
	backendD := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("container-d"))
	}))
	t.Cleanup(backendD.Close)
	hostC, portCStr, _ := net.SplitHostPort(strings.TrimPrefix(backendC.URL, "http://"))
	portC, _ := strconv.Atoi(portCStr)
	hostD, portDStr, _ := net.SplitHostPort(strings.TrimPrefix(backendD.URL, "http://"))
	portD, _ := strconv.Atoi(portDStr)
	labels := func(port string) map[string]string {
		return map[string]string{
			"traefik.enable":                                        "true",
			"traefik.http.routers.shared.rule":                      "Host(`shared.example.com`)",
			"traefik.http.routers.shared.service":                   "shared",
			"traefik.http.services.shared.loadbalancer.server.port": port,
		}
	}
	p, srv, daemon := newFakeProviderDaemon(t, &resolved.Docker{
		TraefikLabels: true,
		Workloads:     map[string]resolved.Workload{"shared@traefik": testWorkloadPolicy()},
	}, []fakeDaemonContainer{{name: "container-c", ip: hostC, port: portC, labels: labels(portCStr)}})
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	waitSignal(t, stopStarted, "idle stop was not issued")

	daemon.swap([]fakeDaemonContainer{
		{name: "container-c", ip: hostC, port: portC, labels: labels(portCStr)},
		{name: "container-d", ip: hostD, port: portD, labels: labels(portDStr)},
	})
	mustSync(t, p)
	if got := p.workloadFor("shared@traefik"); got != nil {
		t.Fatal("multi-contributor service retained lifecycle authority")
	}
	tab := srv.dynamic.Load()
	if len(tab.routes) != 1 || len(tab.quarantines) != 1 || len(tab.pools) != 1 {
		t.Fatalf("contributor table routes/quarantines/pools = %d/%d/%d, want 1/1/1", len(tab.routes), len(tab.quarantines), len(tab.pools))
	}
	rec := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://shared.example.com/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "container-d" {
		t.Fatalf("independent contributor response = %d %q, want 200 container-d", rec.Code, rec.Body.String())
	}

	releaseStop()
}

func TestWorkloadSpecificQuarantineBlocksBroadHealthyContributor(t *testing.T) {
	backendC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("container-c"))
	}))
	t.Cleanup(backendC.Close)
	var backendDHits atomic.Int32
	backendD := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendDHits.Add(1)
		_, _ = w.Write([]byte("container-d"))
	}))
	t.Cleanup(backendD.Close)
	hostC, portCStr, _ := net.SplitHostPort(strings.TrimPrefix(backendC.URL, "http://"))
	portC, _ := strconv.Atoi(portCStr)
	hostD, portDStr, _ := net.SplitHostPort(strings.TrimPrefix(backendD.URL, "http://"))
	portD, _ := strconv.Atoi(portDStr)
	labels := func(path string) map[string]string {
		return map[string]string{
			"statute.enable":  "true",
			"statute.service": "shared",
			"statute.host":    "app.example.com",
			"statute.path":    path,
		}
	}
	p, srv, daemon := newFakeProviderDaemon(t, &resolved.Docker{
		Workloads: map[string]resolved.Workload{"shared": testWorkloadPolicy()},
	}, []fakeDaemonContainer{{name: "container-c", ip: hostC, port: portC, labels: labels("/admin/*")}})
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	waitSignal(t, stopStarted, "idle stop was not issued")

	daemon.swap([]fakeDaemonContainer{
		{name: "container-c", ip: hostC, port: portC, labels: labels("/admin/*")},
		{name: "container-d", ip: hostD, port: portD, labels: labels("/*")},
	})
	mustSync(t, p)
	assertDynamicTableShape(t, srv.dynamic.Load(), 1, 1, 0, 1)
	router := srv.buildRouter()
	assertRouteResponse(t, router, "http://app.example.com/admin/users", http.StatusServiceUnavailable, "")
	if got := backendDHits.Load(); got != 0 {
		t.Fatalf("specific quarantine reached broad backend %d times", got)
	}
	assertRouteResponse(t, router, "http://app.example.com/public", http.StatusOK, "container-d")
	if got := backendDHits.Load(); got != 1 {
		t.Fatalf("broad healthy route hits = %d, want 1", got)
	}

	releaseStop()
}

func TestWorkloadStoppedObservationSettlesRenamedMutation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("unexpected"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	p, srv, daemon := newFakeProviderDaemon(t, &resolved.Docker{
		Workloads: map[string]resolved.Workload{"a": testWorkloadPolicy()},
	}, []fakeDaemonContainer{{
		name: "container-c", ip: host, port: port,
		labels: map[string]string{"statute.enable": "true", "statute.service": "a", "statute.host": "a.example.com"},
	}})
	fallbackCalls := fallbackServer(t, srv, nil)
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	stopStarted := daemon.stopStarted
	stopRelease := daemon.stopRelease
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopRelease) }) }
	defer releaseStop()
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	w := p.workloadFor("a")
	waitSignal(t, stopStarted, "idle stop was not issued")

	daemon.swap([]fakeDaemonContainer{{
		name: "container-c", stopped: true,
		labels: map[string]string{"statute.enable": "true", "statute.service": "b", "statute.host": "b.example.com"},
	}})
	mustSync(t, p)
	if rec := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://b.example.com/", nil)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("in-flight renamed contribution = %d, want 503", rec.Code)
	}
	if got := w.phaseNow(); got != workloadStopIssued {
		t.Fatalf("in-flight renamed mutation phase = %v, want stop-issued", got)
	}
	p.generationMu.Lock()
	republished := p.generationChanged
	p.generationMu.Unlock()
	releaseStop()
	waitRetiredWorkloadDormant(t, w)
	waitSignal(t, republished, "settled renamed mutation did not republish routes")
	if rec := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://b.example.com/", nil)); rec.Code != http.StatusTeapot {
		t.Fatalf("settled renamed contribution = %d, want fallback 418", rec.Code)
	}
	if got := fallbackCalls.Load(); got != 1 {
		t.Fatalf("fallback calls = %d, want 1", got)
	}
}

func TestWorkloadDisabledSchemaDoesNotMakeContainerMultiService(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	cfg := &resolved.Docker{
		TraefikLabels: true,
		Workloads:     map[string]resolved.Workload{"wl": testWorkloadPolicy()},
	}
	p, srv, _ := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{{
		name: "wl-1", ip: host, port: port,
		labels: map[string]string{
			"statute.enable":                      "true",
			"statute.service":                     "wl",
			"statute.host":                        "wl.example.com",
			"traefik.enable":                      "false",
			"traefik.http.routers.shadow.rule":    "Host(`shadow.example.com`)",
			"traefik.http.routers.shadow.service": "shadow",
			"traefik.http.services.shadow.loadbalancer.server.port": portStr,
		},
	}})
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)

	if w := p.workloadFor("wl"); w == nil {
		t.Fatal("active service lost its workload gate to an explicitly disabled schema")
	}
	rec := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("active route: code=%d body=%q, want 200 ok", rec.Code, rec.Body.String())
	}
}

func TestWorkloadContainerReplacementSupersedesActivation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	labels := func(enabled string) map[string]string {
		return map[string]string{
			"statute.enable":  enabled,
			"statute.service": "wl",
			"statute.host":    "wl.example.com",
		}
	}
	policy := testWorkloadPolicy()
	policy.IdleAfter = 5 * time.Second
	policy.ReadyTimeout = 2 * time.Second
	policy.Readiness.Mode = resolved.ReadinessDockerHealth
	p, srv, daemon := newFakeProviderDaemon(t, &resolved.Docker{
		Workloads: map[string]resolved.Workload{"wl": policy},
	}, []fakeDaemonContainer{{
		name: "wl-old", ip: host, port: port, stopped: true, health: "starting", labels: labels("true"),
	}})
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	oldReq := httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)
	oldHandler := findHandler(srv.dynamic.Load().routes, "wl.example.com", oldReq)
	if oldHandler == nil {
		t.Fatal("initial workload route is missing")
	}

	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		oldHandler.ServeHTTP(rec, oldReq)
		done <- rec.Code
	}()
	waitWorkloadPhase(t, p, workloadStarting)
	waitStartCount(t, daemon, "wl-old", 1)

	daemon.swap([]fakeDaemonContainer{
		{name: "wl-old", ip: host, port: port, health: "starting", labels: labels("false")},
		{name: "wl-new", ip: host, port: port, health: "starting", labels: labels("true")},
	})
	mustSync(t, p)
	if code := waitStatus(t, done, time.Second, "superseded activation did not release its waiter"); code != http.StatusServiceUnavailable {
		t.Fatalf("superseded activation response = %d, want 503", code)
	}

	assertSuccessorActivation(t, p, daemon)

	daemon.mu.Lock()
	daemon.find("wl-new").health = "healthy"
	daemon.mu.Unlock()
	waitWorkloadPhase(t, p, workloadReady)
	stale := runRequest(t, oldHandler, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if stale.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale route reached successor: code=%d, want 503", stale.Code)
	}
	rec := runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("successor route: code=%d body=%q, want 200 ok", rec.Code, rec.Body.String())
	}
}

func TestWorkloadRoutingRevisionSupersedesQueuedRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	labels := func(middleware string) map[string]string {
		return map[string]string{
			"traefik.enable":                                    "true",
			"traefik.http.routers.wl.rule":                      "Host(`wl.example.com`)",
			"traefik.http.routers.wl.service":                   "wl",
			"traefik.http.routers.wl.middlewares":               middleware,
			"traefik.http.services.wl.loadbalancer.server.port": portStr,
		}
	}
	policy := testWorkloadPolicy()
	policy.IdleAfter = 5 * time.Second
	policy.Readiness.Mode = resolved.ReadinessDockerHealth
	cfg := &resolved.Docker{
		TraefikLabels: true,
		Middleware: map[string][]resolved.Middleware{
			"old@file": {mustResolveMW(t, SetResponseHeader("X-Policy", "old"))},
			"new@file": {mustResolveMW(t, SetResponseHeader("X-Policy", "new"))},
		},
		Workloads: map[string]resolved.Workload{"wl@traefik": policy},
	}
	p, srv, daemon := newFakeProviderDaemon(t, cfg, []fakeDaemonContainer{{
		name: "wl-1", ip: host, port: port, stopped: true, health: "starting", labels: labels("old@file"),
	}})
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	oldTable := srv.dynamic.Load()
	oldHandler := findHandler(oldTable.routes, "wl.example.com", httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if oldHandler == nil {
		t.Fatal("initial workload route is missing")
	}

	done := make(chan *httptest.ResponseRecorder)
	go func() {
		rec := httptest.NewRecorder()
		oldHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
		done <- rec
	}()
	waitWorkloadServicePhase(t, p, "wl@traefik", workloadStarting)

	daemon.mu.Lock()
	container := daemon.containers[0]
	container.labels = labels("new@file")
	daemon.mu.Unlock()
	daemon.swap([]fakeDaemonContainer{container})
	mustSync(t, p)
	newTable := srv.dynamic.Load()
	if oldTable.workloadBindings["wl@traefik"] != newTable.workloadBindings["wl@traefik"] {
		t.Fatal("label-only change replaced the container binding")
	}
	if oldTable.workloadRevisions["wl@traefik"] == newTable.workloadRevisions["wl@traefik"] {
		t.Fatal("middleware change retained the routing revision")
	}

	daemon.mu.Lock()
	daemon.find("wl-1").health = "healthy"
	daemon.mu.Unlock()
	rec := <-done
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale queued request = %d, want 503", rec.Code)
	}
	waitWorkloadServicePhase(t, p, "wl@traefik", workloadReady)
	current := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if current.Code != http.StatusOK || current.Header().Get("X-Policy") != "new" {
		t.Fatalf("current route: code=%d policy=%q, want 200 new", current.Code, current.Header().Get("X-Policy"))
	}
}

func TestWorkloadReplacementStartsFreshPoolRuntime(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("initial request = %d, want 200", rec.Code)
	}
	oldTable := p.srv.dynamic.Load()
	oldPool := oldTable.pools["wl"]
	oldBinding := oldTable.workloadBindings["wl"]
	oldPool.handler.primary[0].markHealthy(false)

	daemon.mu.Lock()
	container := daemon.containers[0]
	daemon.mu.Unlock()
	replacement := daemon.recreate(container)
	daemon.swap([]fakeDaemonContainer{replacement})
	mustSync(t, p)
	newTable := p.srv.dynamic.Load()
	newPool := newTable.pools["wl"]
	if newTable.workloadBindings["wl"] == oldBinding {
		t.Fatal("same-name recreation retained the binding key")
	}
	if newPool == oldPool {
		t.Fatal("successor reused its predecessor's pool runtime")
	}
	if oldPool.isLive() {
		t.Fatal("predecessor pool remained live after replacement")
	}
	if !newPool.handler.primary[0].isHealthy() {
		t.Fatal("successor inherited predecessor health state")
	}
}

func TestWorkloadStreamingResponseHoldsIdleStop(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseStream := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseStream()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		close(started)
		<-release
		_, _ = w.Write([]byte("b"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	proxy := httptest.NewServer(router)
	t.Cleanup(proxy.Close)

	done := make(chan error)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, proxy.URL, nil)
		req.Host = "wl.example.com"
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- err
	}()
	waitSignal(t, started, "streaming response did not start")
	time.Sleep(400 * time.Millisecond)
	if got := daemon.stopCount("wl-1"); got != 0 {
		t.Fatalf("streaming response allowed %d idle stops", got)
	}
	releaseStream()
	if err := waitError(t, done, time.Second, "streaming response did not finish"); err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	waitWorkloadPhase(t, p, workloadDormant)
	if got := daemon.stopCount("wl-1"); got != 1 {
		t.Fatalf("stop calls after stream = %d, want 1", got)
	}
}

func TestWorkloadReplacementDetachesStreamingActivity(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseStream := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseStream()
	oldBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		close(started)
		<-release
		_, _ = w.Write([]byte("b"))
	}))
	t.Cleanup(oldBackend.Close)
	newBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("new"))
	}))
	t.Cleanup(newBackend.Close)
	oldHost, oldPortStr, _ := net.SplitHostPort(strings.TrimPrefix(oldBackend.URL, "http://"))
	oldPort, _ := strconv.Atoi(oldPortStr)
	newHost, newPortStr, _ := net.SplitHostPort(strings.TrimPrefix(newBackend.URL, "http://"))
	newPort, _ := strconv.Atoi(newPortStr)
	labels := map[string]string{
		"statute.enable":  "true",
		"statute.service": "wl",
		"statute.host":    "wl.example.com",
	}
	p, srv, daemon := newFakeProviderDaemon(t, &resolved.Docker{
		Workloads: map[string]resolved.Workload{"wl": testWorkloadPolicy()},
	}, []fakeDaemonContainer{{
		name: "wl-1", ip: oldHost, port: oldPort, labels: labels,
	}})
	run, err := p.start()
	if err != nil {
		t.Fatalf("provider start: %v", err)
	}
	t.Cleanup(run.stop)
	proxy := httptest.NewServer(srv.buildRouter())
	t.Cleanup(proxy.Close)

	done := make(chan error)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, proxy.URL, nil)
		req.Host = "wl.example.com"
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- err
	}()
	waitSignal(t, started, "old streaming response did not start")
	daemon.mu.Lock()
	oldID := daemon.containers[0].id
	daemon.mu.Unlock()
	replacement := daemon.recreate(fakeDaemonContainer{
		name: "wl-1", ip: newHost, port: newPort, labels: labels,
	})
	if replacement.id == oldID {
		t.Fatal("same-name replacement retained its old ID")
	}
	daemon.swap([]fakeDaemonContainer{replacement})
	mustSync(t, p)
	waitWorkloadPhase(t, p, workloadReady)
	waitWorkloadPhase(t, p, workloadDormant)

	releaseStream()
	if err := waitError(t, done, time.Second, "old streaming response did not finish"); err != nil {
		t.Fatalf("old streaming request: %v", err)
	}
	if got := p.workloadFor("wl").phaseNow(); got != workloadDormant {
		t.Fatalf("stale completion changed successor phase to %v", got)
	}
}

func TestWorkloadWebSocketHoldsIdleStop(t *testing.T) {
	opened := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWebSocket := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseWebSocket()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack backend: %v", err)
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		close(opened)
		<-release
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL)
	proxy := httptest.NewServer(router)
	t.Cleanup(proxy.Close)

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, _ = fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: wl.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read upgrade: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101", resp.StatusCode)
	}
	waitSignal(t, opened, "WebSocket did not open")
	time.Sleep(400 * time.Millisecond)
	if got := daemon.stopCount("wl-1"); got != 0 {
		t.Fatalf("open WebSocket allowed %d idle stops", got)
	}
	releaseWebSocket()
	_ = conn.Close()
	waitWorkloadPhase(t, p, workloadDormant)
	if got := daemon.stopCount("wl-1"); got != 1 {
		t.Fatalf("stop calls after WebSocket = %d, want 1", got)
	}
}
