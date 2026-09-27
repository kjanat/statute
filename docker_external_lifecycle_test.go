package statute

import (
	"bytes"
	"context"
	"fmt"
	"log"
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

type externalEventStream struct {
	allow      chan struct{}
	connected  chan struct{}
	events     chan string
	disconnect chan struct{}
}

func newExternalEventStream() *externalEventStream {
	return &externalEventStream{
		allow: make(chan struct{}), connected: make(chan struct{}),
		events: make(chan string), disconnect: make(chan struct{}),
	}
}

func (s *externalEventStream) serve(w http.ResponseWriter, r *http.Request) {
	select {
	case <-s.allow:
	case <-r.Context().Done():
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	close(s.connected)
	for {
		select {
		case event := <-s.events:
			_, _ = fmt.Fprintln(w, event)
			w.(http.Flusher).Flush()
		case <-s.disconnect:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func externalWorkloadFixture(t *testing.T, refresh time.Duration) (*dockerProvider, *fakeDaemon, <-chan *externalEventStream) {
	t.Helper()
	policy := testWorkloadPolicy()
	policy.IdleAfter = time.Hour
	return externalWorkloadFixturePolicy(t, refresh, policy)
}

func externalWorkloadFixturePolicy(t *testing.T, refresh time.Duration, policy resolved.Workload) (*dockerProvider, *fakeDaemon, <-chan *externalEventStream) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("external lifecycle backend"))
	}))
	t.Cleanup(backend.Close)
	host, portText, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	p, _, daemon := newFakeProviderDaemon(t, &resolved.Docker{
		Refresh: refresh, Workloads: map[string]resolved.Workload{"wl": policy},
	}, []fakeDaemonContainer{{
		name: "wl-1", ip: host, port: port, stopped: true,
		labels: map[string]string{"statute.enable": "true", "statute.service": "wl", "statute.host": "wl.example.com"},
	}})
	subscriptions := make(chan *externalEventStream)
	daemon.events = func(w http.ResponseWriter, r *http.Request) {
		stream := newExternalEventStream()
		select {
		case subscriptions <- stream:
			stream.serve(w, r)
		case <-r.Context().Done():
		}
	}
	run, err := p.start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.stop)
	return p, daemon, subscriptions
}

func nextExternalSubscription(t *testing.T, subscriptions <-chan *externalEventStream) *externalEventStream {
	t.Helper()
	select {
	case stream := <-subscriptions:
		return stream
	case <-time.After(5 * time.Second):
		t.Fatal("Docker event subscription did not start")
		return nil
	}
}

func waitExternalListing(t *testing.T, daemon *fakeDaemon, after int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if daemon.listCount() > after {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Docker full listing did not start")
}

func externalRunning(daemon *fakeDaemon, running bool) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.find("wl-1").stopped = !running
}

func emitExternalEvent(t *testing.T, stream *externalEventStream, action string) {
	t.Helper()
	select {
	case stream.events <- fmt.Sprintf(`{"Type":"container","Action":%q}`, action):
	case <-time.After(time.Second):
		t.Fatal("event stream did not accept event")
	}
}

func assertExternalWorkloadServes(t *testing.T, p *dockerProvider) {
	t.Helper()
	rec := runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "external lifecycle backend" {
		t.Fatalf("workload response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestDockerInitialSubscriptionResyncClosesListingGap(t *testing.T) {
	p, daemon, subscriptions := externalWorkloadFixture(t, 0)
	stream := nextExternalSubscription(t, subscriptions)
	if p.workloadFor("wl").phaseNow() != workloadDormant {
		t.Fatal("startup listing did not observe dormant container")
	}
	// The change follows startup's listing but precedes the event subscription.
	// No event is delivered for it; the successful subscription must resync.
	externalRunning(daemon, true)
	close(stream.allow)
	waitWorkloadPhase(t, p, workloadReady)
	assertExternalWorkloadServes(t, p)
	if daemon.startCount("wl-1") != 0 {
		t.Fatal("adopting the gap's running observation issued a start")
	}
}

func TestDockerReconnectResyncClosesListingGap(t *testing.T) {
	p, daemon, subscriptions := externalWorkloadFixture(t, 0)
	first := nextExternalSubscription(t, subscriptions)
	before := daemon.listCount()
	close(first.allow)
	waitExternalListing(t, daemon, before)
	before = daemon.listCount()
	close(first.disconnect)
	second := nextExternalSubscription(t, subscriptions)
	// Capture the existing pre-reconnect snapshot while subscription is delayed.
	waitExternalListing(t, daemon, before)
	externalRunning(daemon, true)
	close(second.allow)
	waitWorkloadPhase(t, p, workloadReady)
	assertExternalWorkloadServes(t, p)
	if daemon.startCount("wl-1") != 0 {
		t.Fatal("reconnect adoption issued a start")
	}
}

func TestDockerEventOnlyExternalStopAndStart(t *testing.T) {
	p, daemon, subscriptions := externalWorkloadFixture(t, 0)
	stream := nextExternalSubscription(t, subscriptions)
	close(stream.allow)
	waitSignal(t, stream.connected, "event stream did not connect")
	assertExternalWorkloadServes(t, p)
	externalRunning(daemon, false)
	emitExternalEvent(t, stream, "die")
	waitWorkloadPhase(t, p, workloadDormant)
	assertExternalWorkloadServes(t, p)
	if daemon.startCount("wl-1") != 2 {
		t.Fatal("external stop did not retain demand-driven activation authority")
	}
	externalRunning(daemon, false)
	emitExternalEvent(t, stream, "stop")
	waitWorkloadPhase(t, p, workloadDormant)
	externalRunning(daemon, true)
	emitExternalEvent(t, stream, "start")
	waitWorkloadPhase(t, p, workloadReady)
	assertExternalWorkloadServes(t, p)
	if daemon.startCount("wl-1") != 2 {
		t.Fatal("external start adoption issued another start")
	}
}

func TestDockerPeriodicRefreshDiscoversExternalStopWithoutEvent(t *testing.T) {
	p, daemon, subscriptions := externalWorkloadFixture(t, 20*time.Millisecond)
	stream := nextExternalSubscription(t, subscriptions)
	// Hold the event subscription throughout: only startup and polling can list.
	assertExternalWorkloadServes(t, p)
	externalRunning(daemon, false)
	waitWorkloadPhase(t, p, workloadDormant)
	assertExternalWorkloadServes(t, p)
	if daemon.startCount("wl-1") != 2 {
		t.Fatal("poll-discovered stop did not reactivate on demand")
	}
	select {
	case <-stream.connected:
		t.Fatal("polling test unexpectedly established an event subscription")
	default:
	}
}

func TestDockerExternalStartPreservesOwnedStop(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		name := "issued"
		if unknown {
			name = "unknown"
		}
		t.Run(name, func(t *testing.T) { testExternalStartPreservesOwnedStop(t, unknown) })
	}
}

func testExternalStartPreservesOwnedStop(t *testing.T, unknown bool) {
	t.Helper()
	policy := testWorkloadPolicy()
	policy.IdleAfter = 20 * time.Millisecond
	p, daemon, subscriptions := externalWorkloadFixturePolicy(t, 0, policy)
	stream := nextExternalSubscription(t, subscriptions)
	close(stream.allow)
	stopStarted, stopRelease := make(chan struct{}), make(chan struct{})
	daemon.mu.Lock()
	daemon.stopStarted, daemon.stopRelease = stopStarted, stopRelease
	daemon.loseStopReply = unknown
	daemon.mu.Unlock()
	defer close(stopRelease)
	assertExternalWorkloadServes(t, p)
	waitSignal(t, stopStarted, "owned stop did not reach Docker")
	phase := workloadStopIssued
	if unknown {
		phase = workloadStopUnknown
	}
	waitWorkloadPhase(t, p, phase)
	w := p.workloadFor("wl")
	w.mu.Lock()
	owner := w.stop
	w.mu.Unlock()
	before := daemon.listCount()
	externalRunning(daemon, true)
	emitExternalEvent(t, stream, "start")
	waitExternalListing(t, daemon, before)
	p.syncMu.Lock()
	w.mu.Lock()
	retained := w.stop == owner && !w.phase.serving() && (!unknown || owner.uncertain)
	w.mu.Unlock()
	p.syncMu.Unlock()
	if !retained || !p.currentMutationRegistry().contains("id-0") {
		t.Fatal("external running observation erased mutation ownership or quarantine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil).WithContext(ctx)
	if rec := runRequest(t, p.srv.buildRouter(), req); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request during owned stop = %d, want 503", rec.Code)
	}
	if daemon.startCount("wl-1") != 1 {
		t.Fatal("external running observation issued an extra start")
	}
}

func TestDockerExternalStopDuringActivation(t *testing.T) {
	for _, observe := range []bool{false, true} {
		name := "start-owned"
		if observe {
			name = "observe-only"
		}
		t.Run(name, func(t *testing.T) { testExternalStopDuringActivation(t, observe) })
	}
}

func testExternalStopDuringActivation(t *testing.T, observe bool) {
	t.Helper()
	policy := testWorkloadPolicy()
	policy.IdleAfter = time.Hour
	policy.Readiness.Mode = resolved.ReadinessDockerHealth
	p, daemon, subscriptions := externalWorkloadFixturePolicy(t, 0, policy)
	stream := nextExternalSubscription(t, subscriptions)
	close(stream.allow)
	daemon.mu.Lock()
	daemon.find("wl-1").health = "starting"
	daemon.mu.Unlock()
	if observe {
		externalRunning(daemon, true)
		emitExternalEvent(t, stream, "start")
		waitWorkloadPhase(t, p, workloadStarting)
	}
	response := make(chan int, 1)
	go func() {
		rec := runRequest(t, p.srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil))
		response <- rec.Code
	}()
	waitWorkloadPhase(t, p, workloadStarting)
	w := p.workloadFor("wl")
	w.mu.Lock()
	act := w.activation
	w.mu.Unlock()
	if act == nil || act.observe != observe {
		t.Fatal("activation owner differs from test scenario")
	}
	waitExternalActivationWaiter(t, w, act)
	// A start-owned attempt must have completed its start before the external stop.
	if !observe {
		waitExternalStart(t, daemon)
	}
	externalRunning(daemon, false)
	emitExternalEvent(t, stream, "die")
	if code := waitStatus(t, response, 5*time.Second, "stopped activation did not answer waiter"); code != http.StatusServiceUnavailable {
		t.Fatalf("stopped activation response = %d, want 503", code)
	}
	want := workloadFailed
	if observe {
		want = workloadDormant
	}
	waitWorkloadPhase(t, p, want)
	assertExternalStoppedActivation(t, w, daemon, observe)
}

func assertExternalStoppedActivation(t *testing.T, w *workload, daemon *fakeDaemon, observe bool) {
	t.Helper()
	w.mu.Lock()
	backoff := !w.failedUntil.IsZero()
	w.mu.Unlock()
	if observe {
		if backoff || daemon.startCount("wl-1") != 0 || daemon.stopCount("wl-1") != 0 {
			t.Fatal("observe-only stale activation acquired mutation or backoff ownership")
		}
	} else if !backoff || daemon.stopCount("wl-1") != 1 {
		t.Fatal("start-owned failure did not preserve backoff and canonical cleanup")
	}
}

func waitExternalStart(t *testing.T, daemon *fakeDaemon) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if daemon.startCount("wl-1") != 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("activation did not reach Docker start")
}

func waitExternalActivationWaiter(t *testing.T, w *workload, act *workloadActivation) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		attached := w.activation == act && act.waiting > 0
		w.mu.Unlock()
		if attached {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("request did not attach to activation")
}

type externalLifecycleLogs struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *externalLifecycleLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *externalLifecycleLogs) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

func TestDockerExternalLifecycleDiagnosticsRepeatOnTransitions(t *testing.T) {
	var logs externalLifecycleLogs
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	p, daemon, subscriptions := externalWorkloadFixture(t, 0)
	stream := nextExternalSubscription(t, subscriptions)
	close(stream.allow)
	assertExternalWorkloadServes(t, p)
	for cycle := 1; cycle <= 2; cycle++ {
		externalRunning(daemon, false)
		emitExternalEvent(t, stream, "die")
		waitWorkloadPhase(t, p, workloadDormant)
		if got := logs.count(`container "id-0"; dormant, routing demand may reactivate`); got != cycle {
			t.Fatalf("stop diagnostics = %d, want %d", got, cycle)
		}
		mustSync(t, p)
		if got := logs.count("stopped outside statute"); got != cycle {
			t.Fatal("unchanged stopped observation repeated transition diagnostic")
		}
		externalRunning(daemon, true)
		emitExternalEvent(t, stream, "start")
		waitWorkloadPhase(t, p, workloadReady)
		assertExternalWorkloadServes(t, p)
		if got := logs.count(`container "id-0"; observe-only adoption`); got != cycle {
			t.Fatalf("adoption diagnostics = %d, want %d", got, cycle)
		}
	}
}
