package statute

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

	for _, p := range []workloadPhase{workloadDormant, workloadStarting, workloadStopPending, workloadStopIssued, workloadFailed} {
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

	all := []workloadPhase{workloadDormant, workloadStarting, workloadReady, workloadStopPending, workloadStopIssued, workloadFailed}

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
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w := p.workloadFor("wl"); w != nil && w.phaseNow() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	w := p.workloadFor("wl")
	if w == nil {
		t.Fatal("no workload entry for \"wl\"")
	}
	t.Fatalf("workload phase = %v, want %v", w.phaseNow(), want)
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
