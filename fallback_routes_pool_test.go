package statute

import (
	"bufio"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFallbackRoutesSharedPoolRetryPolicy(t *testing.T) {
	t.Parallel()
	var attempts, fallbackCalls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.URL.Path != "/rewritten/item" || !slices.Equal(r.Header.Values("X-Once"), []string{"once"}) {
			t.Errorf("backend request path=%q headers=%v", r.URL.Path, r.Header.Values("X-Once"))
		}
		w.Header().Set("X-Policy-Seen", r.Header.Get("X-Policy"))
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(backend.Close)
	router := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{"shared": Pool{Backends: []Backend{{Address: backend.URL}}}},
		Routes: Routes{Match("/ordinary/*").ProxyTo("shared").With(
			SetRequestHeader("X-Policy", "ordinary"), AddRequestHeader("X-Once", "once"), ReplacePath("/rewritten/item"),
		)},
		FallbackRoutes: Routes{Match("/terminal/*").ProxyTo("shared").With(
			Retry(2, OnStatus(http.StatusBadGateway)), SetRequestHeader("X-Policy", "terminal"),
			AddRequestHeader("X-Once", "once"), StripPrefix("/terminal"), AddPrefix("/rewritten"),
		)},
		Fallback: countingFallback(&fallbackCalls),
	})
	for _, tc := range []struct {
		path, policy string
		attempts     int64
	}{
		{"/ordinary/item", "ordinary", 1},
		{"/terminal/item", "terminal", 2},
		{"/ordinary/item", "ordinary", 1},
	} {
		before := attempts.Load()
		req := httptest.NewRequest(http.MethodGet, "http://client"+tc.path, nil)
		rec := runRequest(t, router, req)
		if rec.Code != http.StatusBadGateway || rec.Header().Get("X-Policy-Seen") != tc.policy || attempts.Load()-before != tc.attempts {
			t.Fatalf("%s: status=%d policy=%q attempts=%d", tc.path, rec.Code, rec.Header().Get("X-Policy-Seen"), attempts.Load()-before)
		}
		if req.URL.Path != tc.path {
			t.Fatalf("mutated original path: %q", req.URL.Path)
		}
	}
	if fallbackCalls.Load() != 0 {
		t.Fatal("exhausted proxy retries fell through to the application fallback")
	}
}

func TestFallbackRoutesSharePassiveHealth(t *testing.T) {
	t.Parallel()
	var badHits, fallbackCalls atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(bad.Close)
	good := httptest.NewServer(noContentHandler)
	t.Cleanup(good.Close)
	srv, err := newServer(mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{"shared": Pool{
			Backends:           []Backend{{Address: bad.URL}, {Address: good.URL}},
			PassiveHealthCheck: PassiveHealthCheck{FailureWindow: "1m", MaxFailures: 1},
		}},
		Routes:         Routes{Match("/ordinary").ProxyTo("shared")},
		FallbackRoutes: Routes{Match("/*").ProxyTo("shared")},
		Fallback:       countingFallback(&fallbackCalls),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.pools) != 1 {
		t.Fatalf("pool count=%d, want one shared pool", len(srv.pools))
	}
	pool := srv.pools["shared"]
	run := pool.start()
	t.Cleanup(run.shutdown)
	router := srv.buildRouter()
	// The first ordinary request demotes the failing backend. Both route
	// tables must subsequently select the same surviving backend.
	if got := runRequest(t, router, httptest.NewRequest(http.MethodGet, "/ordinary", nil)).Code; got != http.StatusServiceUnavailable {
		t.Fatalf("first ordinary response=%d", got)
	}
	for _, path := range []string{"/terminal", "/ordinary", "/terminal"} {
		if got := runRequest(t, router, httptest.NewRequest(http.MethodGet, path, nil)).Code; got != http.StatusNoContent {
			t.Fatalf("%s ignored shared demotion: %d", path, got)
		}
	}
	if badHits.Load() != 1 || fallbackCalls.Load() != 0 {
		t.Fatalf("bad attempts=%d application fallback=%d", badHits.Load(), fallbackCalls.Load())
	}
}

func TestFallbackRoutesTLSHostAndActiveHealth(t *testing.T) {
	t.Parallel()
	var probes atomic.Int64
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "assets.internal" || r.TLS.ServerName != "example.com" {
			t.Errorf("Host=%q SNI=%q", r.Host, r.TLS.ServerName)
		}
		if r.URL.Path == "/health" {
			probes.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	dir := t.TempDir()
	writeFile(t, dir, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw})))
	cfg := Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{"assets": Pool{
			Backends:     []Backend{{Address: backend.URL}},
			UpstreamHost: HostValue("assets.internal"),
			Transport:    Transport{ServerName: "example.com", RootCAFiles: []string{dir + "/ca.pem"}},
			HealthCheck:  HealthCheck{Path: "/health", Interval: "1h", Timeout: "1s", Healthy: 1, Unhealthy: 1},
		}},
		FallbackRoutes: Routes{Match("/*").ProxyTo("assets")},
	}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	ph := srv.pools["assets"]
	t.Cleanup(ph.transport.CloseIdleConnections)
	assertTerminalHealthTransport(t, ph, &probes)
	if got := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://client/default", nil)).Code; got != http.StatusNoContent {
		t.Fatalf("terminal proxy=%d", got)
	}
	pool := cfg.Upstreams["assets"]
	pool.Transport.RootCAFiles = nil
	cfg.Upstreams["assets"] = pool
	var fallbackCalls atomic.Int64
	cfg.Fallback = countingFallback(&fallbackCalls)
	if got := proxyThrough(t, cfg).Code; got != http.StatusBadGateway || fallbackCalls.Load() != 0 {
		t.Fatalf("untrusted terminal backend response=%d fallback=%d", got, fallbackCalls.Load())
	}
}

func assertTerminalHealthTransport(t *testing.T, ph *poolHandler, probes *atomic.Int64) {
	t.Helper()
	if ph.hc.client.Transport != http.RoundTripper(ph.transport) {
		t.Fatal("health and terminal proxy do not share a transport")
	}
	b := ph.primary[0]
	b.markHealthy(false)
	run := &healthRun{checker: ph.hc, successes: map[*backendState]int{}, failures: map[*backendState]int{}}
	run.active.Store(true)
	run.probe(context.Background(), b)
	if !b.isHealthy() || probes.Load() != 1 {
		t.Fatal("TLS/Host configured health probe failed")
	}
}

func TestFallbackRoutesPoolStartupRollbackRetry(t *testing.T) {
	srv, busy, probes, cancelledProbe := newTerminalRollbackFixture(t)
	oldPassive := assertTerminalPoolRollback(t, srv, cancelledProbe)
	addr := busy.Addr().String()
	if err := busy.Close(); err != nil {
		t.Fatal(err)
	}
	before := probes.Load()
	if err := srv.Start(); err != nil {
		t.Fatalf("retry Start: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	waitForProbes(t, probes, before)
	mustServeProxyOK(t, addr)
	if current := srv.pools["terminal"].passive.Load(); current == oldPassive || !current.active.Load() {
		t.Fatal("retry did not acquire a fresh live passive-health run")
	}
	assertTerminalPoolShutdown(t, srv)
}

// newTerminalRollbackFixture holds the content port occupied and the first
// health request in flight so failed startup must actually cancel owned work.
func newTerminalRollbackFixture(t *testing.T) (*server, net.Listener, *atomic.Int64, <-chan struct{}) {
	t.Helper()
	firstProbe, cancelledProbe := make(chan struct{}), make(chan struct{})
	var probes atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && probes.Add(1) == 1 {
			close(firstProbe)
			<-r.Context().Done()
			close(cancelledProbe)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(backend.Close)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	addr := busy.Addr().String()
	srv, err := newServer(mustResolve(t, Config{
		Listeners: Listeners{HTTP(addr)},
		Upstreams: Upstreams{"terminal": Pool{
			Backends:           []Backend{{Address: backend.URL}},
			HealthCheck:        HealthCheck{Path: "/healthz", Interval: "20ms", Timeout: "1s"},
			PassiveHealthCheck: PassiveHealthCheck{FailureWindow: "1m", MaxFailures: 1},
		}},
		FallbackRoutes: Routes{Match("/*").ProxyTo("terminal")},
		Shutdown:       Shutdown{GracePeriod: "2s"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	srv.listenTCP = func(ctx context.Context, network, addr string) (net.Listener, error) {
		// Bind cannot fail before the first run actually owns an in-flight
		// probe. Rollback must cancel that request before the retry begins.
		select {
		case <-firstProbe:
		case <-time.After(5 * time.Second):
			return nil, fmt.Errorf("health probe did not start")
		}
		return lc.Listen(ctx, network, addr)
	}
	return srv, busy, &probes, cancelledProbe
}

func assertTerminalPoolRollback(t *testing.T, srv *server, cancelledProbe <-chan struct{}) *passiveRun {
	t.Helper()
	if err := srv.Start(); err == nil {
		t.Fatal("Start succeeded on an occupied port")
	}
	select {
	case <-cancelledProbe:
	case <-time.After(5 * time.Second):
		t.Fatal("failed Start did not cancel its terminal pool's health request")
	}
	// Rollback must retire the passive run before another attempt can own it.
	oldPassive := srv.pools["terminal"].passive.Load()
	if oldPassive == nil || oldPassive.active.Load() {
		t.Fatal("rolled-back pool did not retire its passive-health run")
	}
	return oldPassive
}

func assertTerminalPoolShutdown(t *testing.T, srv *server) {
	t.Helper()
	running := srv.run.pools[0]
	if err := srv.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if running.isLive() || running.health.active.Load() || running.passive.active.Load() {
		t.Fatal("Shutdown left terminal pool resources live")
	}
	select {
	case <-running.health.done:
	default:
		t.Fatal("Shutdown returned before the terminal health worker joined")
	}
}

func TestFallbackRoutesBackupAndDegraded(t *testing.T) {
	t.Parallel()
	var fallbackCalls atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Backend", "primary")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(primary.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Backend", "backup")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backup.Close)
	srv, err := newServer(mustResolve(t, Config{
		Listeners:      Listeners{HTTP(":0")},
		Upstreams:      Upstreams{"shared": Pool{Backends: []Backend{{Address: primary.URL}, {Address: backup.URL, Backup: true}}}},
		Routes:         Routes{Match("/ordinary").ProxyTo("shared")},
		FallbackRoutes: Routes{Match("/*").ProxyTo("shared").With(Retry(2, OnStatus(http.StatusServiceUnavailable)))},
		Fallback:       countingFallback(&fallbackCalls),
	}))
	if err != nil {
		t.Fatal(err)
	}
	ph := srv.pools["shared"]
	t.Cleanup(ph.transport.CloseIdleConnections)
	router := srv.buildRouter()
	for _, tc := range []struct {
		name            string
		primary, backup bool
		want            string
		status          int
	}{
		{"healthy primary", true, true, "primary", http.StatusServiceUnavailable},
		{"backup", false, true, "backup", http.StatusNoContent},
		{"degraded primary", false, false, "primary", http.StatusServiceUnavailable},
	} {
		ph.primary[0].markHealthy(tc.primary)
		ph.backup[0].markHealthy(tc.backup)
		for _, path := range []string{"/ordinary", "/terminal"} {
			rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != tc.status || rec.Header().Get("X-Backend") != tc.want {
				t.Fatalf("%s %s: status=%d backend=%q", tc.name, path, rec.Code, rec.Header().Get("X-Backend"))
			}
		}
	}
	if fallbackCalls.Load() != 0 {
		t.Fatal("backend health or exhausted retries escaped to application fallback")
	}
}

func TestFallbackRoutesResponseHeaderTimeout(t *testing.T) {
	t.Parallel()
	var calls, fallbackCalls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}))
	t.Cleanup(backend.Close)
	cfg := responseHeaderTimeoutConfig(backend.URL, "30ms")
	cfg.FallbackRoutes = Routes{Match("/*").ProxyTo("api").With(Retry(2, OnStatus(http.StatusBadGateway)))}
	cfg.Routes = nil
	cfg.Fallback = countingFallback(&fallbackCalls)
	if got := proxyThrough(t, cfg).Code; got != http.StatusBadGateway || calls.Load() != 2 || fallbackCalls.Load() != 0 {
		t.Fatalf("header timeout status=%d attempts=%d application fallback=%d", got, calls.Load(), fallbackCalls.Load())
	}
}

func TestFallbackRoutesUpgrade(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("backend hijack: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		payload := make([]byte, 4)
		if _, err := io.ReadFull(rw, payload); err != nil {
			t.Errorf("upgraded backend read: %v", err)
			return
		}
		_, _ = rw.Write(payload)
		_ = rw.Flush()
	}))
	t.Cleanup(backend.Close)
	router := fallbackRouter(t, Config{
		Listeners:      Listeners{HTTP(":0")},
		Upstreams:      Upstreams{"terminal": Pool{Backends: []Backend{{Address: backend.URL}}}},
		FallbackRoutes: Routes{Match("/*").ProxyTo("terminal").With(SetResponseHeader("X-Terminal", "yes"))},
	})
	proxy := httptest.NewServer(router)
	t.Cleanup(proxy.Close)
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxy.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprint(conn, "GET /socket HTTP/1.1\r\nHost: client\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	rd := bufio.NewReader(conn)
	resp, err := http.ReadResponse(rd, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status=%d", resp.StatusCode)
	}
	_, _ = io.WriteString(conn, "ping")
	readExactWithin(t, rd, "ping", 5*time.Second)
}

func TestFallbackRoutesStreamDrainsThroughShutdown(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "first;")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "last")
	}))
	t.Cleanup(backend.Close)
	t.Cleanup(releaseOnce)
	srv, err := newServer(mustResolve(t, Config{
		Listeners:      Listeners{HTTP("127.0.0.1:0")},
		Upstreams:      Upstreams{"stream": Pool{Backends: []Backend{{Address: backend.URL}}, Transport: Transport{FlushInterval: "1ms"}}},
		FallbackRoutes: Routes{Match("/*").ProxyTo("stream").With(SetResponseHeader("X-Terminal", "yes"))},
		Observability:  Observability{AccessLog: JSONLog(LogWriter{w: io.Discard, name: "discard"})},
		Shutdown:       Shutdown{GracePeriod: "5s"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	addr := srv.run.listeners.http[0].listener.Addr().String()
	t.Cleanup(func() { releaseOnce(); _ = srv.Shutdown() })
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	resp, err := client.Get("http://" + addr + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Terminal") != "yes" {
		t.Fatal("terminal response middleware missing")
	}
	readExactWithin(t, resp.Body, "first;", 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown() }()
	waitForRefused(t, addr, done)
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned before terminal stream drained: %v", err)
	default:
	}
	releaseOnce()
	assertTerminalStreamDrained(t, resp.Body, done)
}

func assertTerminalStreamDrained(t *testing.T, bodyReader io.Reader, done <-chan error) {
	t.Helper()
	body, err := io.ReadAll(bodyReader)
	if err != nil || string(body) != "last" {
		t.Fatalf("stream tail=%q error=%v", body, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not finish after terminal stream drained")
	}
}

func TestFallbackRoutesRequestCancellation(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	t.Cleanup(backend.Close)
	router := fallbackRouter(t, Config{
		Listeners:      Listeners{HTTP(":0")},
		Upstreams:      Upstreams{"terminal": Pool{Backends: []Backend{{Address: backend.URL}}}},
		FallbackRoutes: Routes{Match("/*").ProxyTo("terminal")},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
		close(done)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("backend did not receive terminal request")
	}
	cancel()
	for name, ch := range map[string]<-chan struct{}{"backend cancellation": cancelled, "proxy return": done} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("missing %s", name)
		}
	}
}
