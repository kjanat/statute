package statute

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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
		{"unknown stop later observed running", []workloadPhase{workloadStarting, workloadReady, workloadStopPending, workloadStopIssued, workloadStopUnknown, workloadStarting, workloadReady}},
		{"unknown stop later observed stopped", []workloadPhase{workloadStarting, workloadReady, workloadStopPending, workloadStopIssued, workloadStopUnknown, workloadDormant}},
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
func workloadFixture(t *testing.T, policy resolved.Workload, backendURL string, stopped bool) (*dockerProvider, *fakeDaemon, http.Handler) {
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

func assertStatusPending(t *testing.T, ch <-chan int, wait time.Duration) {
	t.Helper()
	select {
	case code := <-ch:
		t.Fatalf("request completed with %d while Docker stop was in flight", code)
	case <-time.After(wait):
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

func assertSuccessorActivation(t *testing.T, p *dockerProvider, daemon *fakeDaemon) {
	t.Helper()
	w := p.workloadFor("wl")
	w.mu.Lock()
	ref := w.containerRefLocked()
	act := w.activation
	w.mu.Unlock()
	if ref != "id-1" || act == nil || act.ref != "id-1" || !act.observe {
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

	_, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)

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

	_, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)

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

	p, daemon, _ := workloadFixture(t, policy, backend.URL, true)
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

func TestWorkloadIdleStopAndReactivation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)

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

	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)

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

	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)

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
	w.mu.Unlock()

	rec = runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoked stop: %d, want 200", rec.Code)
	}
	if got := w.phaseNow(); got != workloadReady {
		t.Fatalf("phase after revoke = %v, want ready", got)
	}
	p.performStop(context.Background(), w)
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
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)
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
	assertStatusPending(t, done, 100*time.Millisecond)
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

func TestWorkloadContainerReplacementSupersedesIssuedStop(t *testing.T) {
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

	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
		done <- rec.Code
	}()
	assertStatusPending(t, done, 100*time.Millisecond)
	daemon.swap([]fakeDaemonContainer{{
		name: "wl-new", ip: host, port: port, health: "starting", labels: labels("true"),
	}})
	mustSync(t, p)
	if code := waitStatus(t, done, time.Second, "superseded stop did not release its waiter"); code != http.StatusServiceUnavailable {
		t.Fatalf("superseded stop response = %d, want 503", code)
	}
	w := p.workloadFor("wl")
	w.mu.Lock()
	ref := w.containerRefLocked()
	act := w.activation
	w.mu.Unlock()
	if ref != "id-1" || act == nil || act.ref != "id-1" || !act.observe {
		t.Fatalf("successor binding: ref=%q activation=%+v", ref, act)
	}

	releaseStop()
	daemon.mu.Lock()
	daemon.find("wl-new").health = "healthy"
	daemon.mu.Unlock()
	waitWorkloadPhase(t, p, workloadReady)
	assertIssuedStopDidNotAffectSuccessor(t, daemon)
	if rec := runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("successor request = %d, want 200", rec.Code)
	}
}

func TestWorkloadStopInspectFailureHoldsUnknownState(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, false)
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
	if got := daemon.startCount("wl-1"); got != 0 {
		t.Fatalf("unknown stop outcome issued %d starts", got)
	}

	daemon.mu.Lock()
	daemon.stallInspect = false
	daemon.mu.Unlock()
	mustSync(t, p)
	waitWorkloadPhase(t, p, workloadReady)
	if rec := runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("request after running observation = %d, want 200", rec.Code)
	}
}

func TestWorkloadExternalStopReconcilesAndReactivates(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)

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

func TestWorkloadAdoptsExternallyStartedContainer(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	// The container is already running at boot: readiness is established
	// observe-only, without a start call, and the idle policy applies.
	p, daemon, _ := workloadFixture(t, testWorkloadPolicy(), backend.URL, false)

	waitWorkloadPhase(t, p, workloadReady)
	if got := daemon.startCount("wl-1"); got != 0 {
		t.Fatalf("adoption issued %d start calls, want 0", got)
	}
	waitWorkloadPhase(t, p, workloadDormant)
	if got := daemon.stopCount("wl-1"); got != 1 {
		t.Fatalf("idle stop after adoption = %d calls, want 1", got)
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
	mustSync(t, p)

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

func TestWorkloadGrantRemovalLeavesContainerRunning(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)

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

	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)
	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cold start: %d", rec.Code)
	}
	w := p.workloadFor("wl")

	// The ready decision and the active-count registration share one
	// critical section; an idle expiry in between must find the count.
	w.mu.Lock()
	binding := w.binding.key
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
	p.idleExpire(w)
	if got := w.phaseNow(); got != workloadReady {
		t.Fatalf("idle expiry with a counted request moved phase to %v", got)
	}
	if got := daemon.stopCount("wl-1"); got != stopsBefore {
		t.Fatalf("idle expiry with a counted request issued a stop")
	}
	w.end(p, lease)
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
	w.end(p, workloadLease{binding: binding.key, activity: &binding.activity})
	w.mu.Lock()
	active, idle := w.binding.activity.active, w.idle
	w.stopIdleLocked()
	w.mu.Unlock()
	if active != 0 || idle == nil {
		t.Fatalf("activity after refined completion: active=%d idle=%v", active, idle)
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
	w.finishWaiting(oldWait)
	w.finishWaiting(newWait)
}

func TestWorkloadShutdownDuringActivationIssuesNoStop(t *testing.T) {
	policy := testWorkloadPolicy()
	policy.ReadyTimeout = 30 * time.Second

	// Port 1 answers nothing; the activation is still in flight when
	// the provider run stops.
	p, daemon, router := workloadFixture(t, policy, "http://127.0.0.1:1", true)

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

	p, daemon, router := workloadFixture(t, policy, backend.URL, true)
	daemon.mu.Lock()
	daemon.failStart = true
	containers := daemon.containers
	oldID := containers[0].id
	daemon.mu.Unlock()

	rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed activation: %d", rec.Code)
	}

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
}

func TestWorkloadStaleObserveLeavesNoBackoff(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	p, daemon, _ := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)
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

	p, daemon, router := workloadFixture(t, policy, backend.URL, true)
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
	mustSync(t, p)

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
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)
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
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)
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
	p, daemon, router := workloadFixture(t, testWorkloadPolicy(), backend.URL, true)
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
