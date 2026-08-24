package statute

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

// fakeClock is a mutable clock for driving passive-window aging in tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newPassivePoolHandler builds a poolHandler over the given backend
// addresses with passive health enabled and the given clock installed.
func newPassivePoolHandler(t *testing.T, clock *fakeClock, window time.Duration, maxFailures int, addrs ...string) *poolHandler {
	t.Helper()
	rp := &resolved.Pool{Name: "p"}
	for _, a := range addrs {
		rp.Backends = append(rp.Backends, resolved.Backend{Address: a, Weight: 1})
	}
	rp.PassiveHealthCheck = resolved.PassiveHealthCheck{
		Enabled:       true,
		FailureWindow: window,
		MaxFailures:   maxFailures,
	}
	ph, err := newPoolHandler(rp)
	if err != nil {
		t.Fatalf("newPoolHandler: %v", err)
	}
	if clock != nil {
		ph.now = clock.Now
	}
	t.Cleanup(ph.transport.CloseIdleConnections)
	return ph
}

// TestPassiveWindowedNotConsecutive — demotion counts failures inside the
// sliding window, not consecutively: an interleaved success neither clears
// nor extends the window, failures spread wider than the window never reach
// the threshold, and aged-out failures re-admit the backend.
func TestPassiveWindowedNotConsecutive(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: time.Now()}
	ph := newPassivePoolHandler(t, clock, 10*time.Second, 3, "127.0.0.1:1", "127.0.0.1:2")
	running := ph.start()
	t.Cleanup(running.shutdown)
	run := ph.passive.Load()
	b, other := ph.primary[0], ph.primary[1]

	// fail, success (not recorded — success never clears), fail, fail
	// inside one window: demoted.
	run.record(b)
	clock.Advance(time.Second)
	clock.Advance(time.Second)
	run.record(b)
	clock.Advance(time.Second)
	run.record(b)
	if !run.demoted(b) {
		t.Fatal("three in-window failures with an interleaved success did not demote")
	}
	if c := ph.candidates(); len(c) != 1 || c[0] != other {
		t.Fatalf("candidates: got %d backends, want only the undemoted one", len(c))
	}

	// Aging out re-admits: the whole window slides past the failures.
	clock.Advance(11 * time.Second)
	if run.demoted(b) {
		t.Fatal("aged-out failures still demote")
	}
	if c := ph.candidates(); len(c) != 2 {
		t.Fatalf("candidates after aging: got %d backends, want 2", len(c))
	}

	// The same number of failures spread wider than the window never
	// demotes: at most two of them share a window.
	for range 3 {
		run.record(b)
		clock.Advance(6 * time.Second)
	}
	if run.demoted(b) {
		t.Fatal("failures spread beyond the window demoted the backend")
	}
}

// TestPassiveCountsBackendAttemptsUnderRetry — passive failures count per
// backend attempt, not per client-visible outcome: with Retry recovering
// every request via the good backend, the bad backend still accumulates its
// per-attempt failures and demotes.
func TestPassiveCountsBackendAttemptsUnderRetry(t *testing.T) {
	t.Parallel()
	var badHits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(bad.Close)
	good := newEchoBackend(t)

	ph := newPassivePoolHandler(t, nil, time.Minute, 2,
		strings.TrimPrefix(bad.URL, "http://"),
		strings.TrimPrefix(good.URL, "http://"),
	)
	running := ph.start()
	t.Cleanup(running.shutdown)
	run := ph.passive.Load()
	badState, goodState := ph.primary[0], ph.primary[1]

	h := retryHandler(resolved.Middleware{Type: resolved.MWRetry, RetryMax: 2, RetryOnStatuses: []int{503}}, ph)
	for i := range 3 {
		rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200 via retry", i, rec.Code)
		}
	}

	if !run.demoted(badState) {
		t.Error("failing backend not demoted despite every request succeeding")
	}
	if run.demoted(goodState) {
		t.Error("succeeding backend demoted")
	}
	// Round-robin alternates A,B per attempt: requests 1 and 2 each burn
	// one attempt on the bad backend before recovering; request 3 finds it
	// demoted and never touches it.
	if got := badHits.Load(); got != 2 {
		t.Errorf("bad backend attempts: got %d, want 2", got)
	}
}

// TestPassiveSingleBackendDegradedModeKeepsServing — degraded mode is
// unchanged: when passive demotion excludes a pool's only backend, the
// degraded floor keeps routing to it rather than manufacturing 503s.
func TestPassiveSingleBackendDegradedModeKeepsServing(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "backend says no", http.StatusServiceUnavailable)
	}))
	t.Cleanup(backend.Close)

	ph := newPassivePoolHandler(t, nil, time.Minute, 1, strings.TrimPrefix(backend.URL, "http://"))
	running := ph.start()
	t.Cleanup(running.shutdown)

	first := runRequest(t, ph, httptest.NewRequest("GET", "/", nil))
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("first request: status %d", first.Code)
	}
	if !ph.passive.Load().demoted(ph.primary[0]) {
		t.Fatal("backend not demoted after its failure")
	}

	second := runRequest(t, ph, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(second.Body.String(), "backend says no") {
		t.Errorf("demoted single backend no longer serves: status %d body %q", second.Code, second.Body.String())
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("backend hits: got %d, want 2", got)
	}
}

// TestPassiveAndActiveInterplay — the two health signals are independent: an
// active probe success does not clear a passive window (the backend stays
// excluded until failures age out), and passive demotion works with active
// health checks disabled entirely.
func TestPassiveAndActiveInterplay(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: time.Now()}
	ph := newPassivePoolHandler(t, clock, time.Minute, 2, "127.0.0.1:1", "127.0.0.1:2")
	running := ph.start()
	t.Cleanup(running.shutdown)
	run := ph.passive.Load()
	b := ph.primary[0]

	run.record(b)
	run.record(b)
	if !run.demoted(b) {
		t.Fatal("backend not demoted")
	}

	// An active probe success marks the backend healthy — and changes
	// nothing about the passive window.
	hr := &healthRun{checker: ph.hc, successes: make(map[*backendState]int), failures: make(map[*backendState]int)}
	hr.active.Store(true)
	hr.recordSuccess(b)
	hr.recordSuccess(b)
	if !b.isHealthy() {
		t.Fatal("active successes did not mark the backend healthy")
	}
	if !run.demoted(b) {
		t.Error("active probe success cleared the passive window")
	}
	if c := ph.candidates(); len(c) != 1 || c[0] == b {
		t.Errorf("probe-healthy but passively demoted backend still a candidate")
	}

	// The pool above has no active health check at all — passive demotion
	// worked regardless — and the config shape resolves too.
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{"api": Pool{
			Backends:           []Backend{{Address: "127.0.0.1:1"}, {Address: "127.0.0.1:2"}},
			PassiveHealthCheck: PassiveHealthCheck{FailureWindow: "30s", MaxFailures: 3},
		}},
		Routes: Routes{Match("/*").ProxyTo("api")},
	})
	pool := r.Upstreams["api"]
	if pool.HealthCheck.Enabled || !pool.PassiveHealthCheck.Enabled {
		t.Errorf("passive without active: resolved %+v / %+v", pool.HealthCheck, pool.PassiveHealthCheck)
	}
}

// TestPoolRestartResetsPassiveWindow — a restart after shutdown (the failed
// Start retry path) begins from empty windows, and a stopped generation's
// late recording is inert against both its own and the successor's state.
func TestPoolRestartResetsPassiveWindow(t *testing.T) {
	t.Parallel()
	ph := newPassivePoolHandler(t, nil, time.Minute, 1, "127.0.0.1:1")
	b := ph.primary[0]

	first := ph.start()
	run1 := ph.passive.Load()
	run1.record(b)
	if !run1.demoted(b) {
		t.Fatal("first generation did not demote")
	}
	first.shutdown()
	run1.record(b)

	second := ph.start()
	t.Cleanup(second.shutdown)
	run2 := ph.passive.Load()
	if run2 == run1 {
		t.Fatal("restart reused the stopped passive generation")
	}
	if run2.demoted(b) {
		t.Fatal("restarted pool inherited the previous generation's failures")
	}
	run1.record(b)
	if run2.demoted(b) {
		t.Fatal("stopped generation's late recording reached the live generation")
	}
	if c := ph.candidates(); len(c) != 1 {
		t.Fatalf("candidates after restart: got %d, want 1", len(c))
	}
}

// TestPassiveClientCancelDoesNotRecord — a client abort surfaces through
// the proxy's ErrorHandler, but it is not a backend fault: a client that
// cancels-and-retries inside the window must not demote a healthy backend
// pool-wide. A genuine transport error on the same path must still record,
// so the guard cannot silently widen into swallowing real failures.
func TestPassiveClientCancelDoesNotRecord(t *testing.T) {
	t.Parallel()
	reached := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	var once sync.Once
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(reached) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(backend.Close)

	ph := newPassivePoolHandler(t, nil, time.Minute, 1, strings.TrimPrefix(backend.URL, "http://"))
	running := ph.start()
	t.Cleanup(running.shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ph.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-reached // the attempt is in flight, held open by the backend
	cancel()
	<-done
	releaseOnce()
	if ph.passive.Load().demoted(ph.primary[0]) {
		t.Error("client cancellation was recorded as a passive backend failure")
	}

	// The companion: an actual transport error — nothing listens on the
	// address — takes the same ErrorHandler path and must still record.
	dead := newPassivePoolHandler(t, nil, time.Minute, 1, "127.0.0.1:1")
	deadRunning := dead.start()
	t.Cleanup(deadRunning.shutdown)
	rec := runRequest(t, dead, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("transport error status: %d, want 502", rec.Code)
	}
	if !dead.passive.Load().demoted(dead.primary[0]) {
		t.Error("genuine transport error was not recorded as a passive failure")
	}
}

// TestPassivePoolPathPreservesStreaming — passive recording hooks into the
// reverse proxy's outcome paths, not a response-writer wrapper, so the pool
// path must still stream: a flushed SSE chunk reaches the client while the
// backend is still holding the response open.
func TestPassivePoolPathPreservesStreaming(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(backend.Close)

	ph := newPassivePoolHandler(t, nil, time.Minute, 3, strings.TrimPrefix(backend.URL, "http://"))
	running := ph.start()
	t.Cleanup(running.shutdown)
	front := httptest.NewServer(ph)
	t.Cleanup(front.Close)

	resp, err := http.Get(front.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		lines <- line
	}()
	select {
	case line := <-lines:
		if !strings.Contains(line, "data: first") {
			t.Errorf("streamed line: %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Error("flushed chunk never reached the client while the backend held the stream open")
	}
	releaseOnce()
}
