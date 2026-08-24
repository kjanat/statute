package statute

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// TestRouterHostAndPath walks the host-and-path matching matrix. The router
// matches in declaration order; the first hit wins.
func TestRouterHostAndPath(t *testing.T) {
	t.Parallel()
	backend := newEchoBackend(t)

	cfg := Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"api": Pool{Backends: []Backend{{Address: strings.TrimPrefix(backend.URL, "http://")}}},
		},
		Routes: Routes{
			Match("/api/v1/*").Host("api.example.com").ProxyTo("api"),
			Match("/admin").Host("admin.example.com").ProxyTo("api"),
			Match("/*").ProxyTo("api"),
		},
	}
	r := mustResolve(t, cfg)
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		for _, ph := range srv.pools {
			ph.shutdown()
		}
	})
	h := srv.buildRouter()

	cases := []struct {
		name, host, path string
		wantStatus       int
		wantUpstreamPath string
	}{
		{"host+path scoped match", "api.example.com", "/api/v1/users", http.StatusOK, "/api/v1/users"},
		{"host scoped, path miss falls through to catch-all", "api.example.com", "/other", http.StatusOK, "/other"},
		{"different host catches all", "client.example.com", "/anything", http.StatusOK, "/anything"},
		{"admin exact match", "admin.example.com", "/admin", http.StatusOK, "/admin"},
		{"admin exact pattern does not match suffix", "admin.example.com", "/admin/users", http.StatusOK, "/admin/users"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://"+c.host+c.path, nil)
			rec := runRequest(t, h, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, c.wantStatus)
			}
			echo := decodeEcho(t, rec.Body)
			if echo.Path != c.wantUpstreamPath {
				t.Errorf("upstream saw path %q, want %q", echo.Path, c.wantUpstreamPath)
			}
		})
	}
}

// TestRouterNotFound — request reaches no route → 404.
func TestRouterNotFound(t *testing.T) {
	t.Parallel()
	backend := newEchoBackend(t)
	cfg := Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"api": Pool{Backends: []Backend{{Address: strings.TrimPrefix(backend.URL, "http://")}}},
		},
		Routes: Routes{
			Match("/api/*").Host("api.example.com").ProxyTo("api"),
		},
	}
	r := mustResolve(t, cfg)
	srv, err := newServer(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, ph := range srv.pools {
			ph.shutdown()
		}
	})

	req := httptest.NewRequest("GET", "http://other.example.com/api/users", nil)
	rec := runRequest(t, srv.buildRouter(), req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

// TestRedirectHandler — redirect listener emits 301 to the configured scheme,
// preserving path, query, and host.
func TestRedirectHandler(t *testing.T) {
	t.Parallel()
	h := redirectHandler("https")
	req := httptest.NewRequest("GET", "http://example.com/path?x=1&y=2", nil)
	rec := runRequest(t, h, req)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status: got %d, want 301", rec.Code)
	}
	want := "https://example.com/path?x=1&y=2"
	if got := rec.Header().Get("Location"); got != want {
		t.Errorf("Location: got %q, want %q", got, want)
	}
}

// TestServerStartShutdown — bind on :0, hit it, shut down within the grace
// period. Confirms the full lifecycle end-to-end including signal of completion.
func TestServerStartShutdown(t *testing.T) {
	backend := newEchoBackend(t)

	cfg := Config{
		Listeners: Listeners{HTTP("127.0.0.1:0")},
		Upstreams: Upstreams{
			"api": Pool{Backends: []Backend{{Address: strings.TrimPrefix(backend.URL, "http://")}}},
		},
		Routes: Routes{Match("/*").ProxyTo("api")},
		Defaults: Defaults{
			ReadHeaderTimeout: "1s",
		},
		Shutdown: Shutdown{GracePeriod: "2s"},
	}
	// Patch the resolved Addr before newServer so every derived copy agrees.
	r := mustResolve(t, cfg)
	addr := reserveAddr(t)
	r.Listeners[0].Addr = addr
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := srv.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()

	// Wait briefly for goroutines to bind before issuing a request.
	waitForListen(t, addr)

	resp, err := http.Get("http://" + addr + "/test")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"path":"/test"`) {
		t.Errorf("upstream body unexpected: %s", body)
	}
}

// TestStartFailureReleasesEarlierListeners — when a later listener fails
// to bind, every listener Start already opened must close again: a
// failed Start leaves no socket serving.
func TestStartFailureReleasesEarlierListeners(t *testing.T) {
	// Hold a port so the second listener's bind must fail.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })

	cfg := Config{
		Listeners: Listeners{HTTP("127.0.0.1:0"), HTTP("127.0.0.1:0")},
		Routes:    Routes{Match("/*").RedirectTo("https://example.com", http.StatusMovedPermanently)},
		Shutdown:  Shutdown{GracePeriod: "2s"},
	}
	r := mustResolve(t, cfg)
	firstAddr := reserveAddr(t)
	r.Listeners[0].Addr = firstAddr
	r.Listeners[1].Addr = busy.Addr().String()
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}

	if err := srv.Start(); err == nil {
		t.Fatal("Start succeeded despite a conflicting listener address")
	}
	mustBindNow(t, firstAddr)

	// The retry must actually serve: a poisoned unwind still returns nil
	// while every listener sits closed.
	secondAddr := busy.Addr().String()
	if err := busy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("retried Start: %v", err)
	}
	defer func() {
		if err := srv.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()
	for _, addr := range []string{firstAddr, secondAddr} {
		mustServeRedirect(t, addr)
	}
}

// TestStartMetricsFailureReleasesListeners — a metrics bind failure after
// the content listeners opened must close them again, through the same
// rollback that guards listener-vs-listener conflicts.
func TestStartMetricsFailureReleasesListeners(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })

	cfg := Config{
		Listeners: Listeners{HTTP("127.0.0.1:0")},
		Routes:    Routes{Match("/*").RedirectTo("https://example.com", http.StatusMovedPermanently)},
		Observability: Observability{
			Metrics: Prometheus(busy.Addr().String(), "/metrics"),
		},
		Shutdown: Shutdown{GracePeriod: "2s"},
	}
	r := mustResolve(t, cfg)
	contentAddr := reserveAddr(t)
	r.Listeners[0].Addr = contentAddr
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}

	if err := srv.Start(); err == nil {
		t.Fatal("Start succeeded despite a conflicting metrics address")
	}
	mustBindNow(t, contentAddr)

	// Retry after freeing the metrics port: content and metrics listeners
	// must both come up serving.
	metricsAddr := busy.Addr().String()
	if err := busy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("retried Start: %v", err)
	}
	defer func() {
		if err := srv.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()
	mustServeRedirect(t, contentAddr)
	mustServeMetrics(t, metricsAddr)
}

// TestStartFailureStopsHealthCheckers — the rollback owes more than
// sockets: startPrerequisites started the static pools' health checkers,
// so a failed Start must stop them instead of leaving probe goroutines
// hammering backends for a server that serves nothing. A retry must start
// them again.
func TestStartFailureStopsHealthCheckers(t *testing.T) {
	backend, probes := newProbeCountingBackend(t, "/healthz")

	// Hold the listener's port so Start fails in the bind phase — after
	// startPrerequisites has already brought the checkers up.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	addr := busy.Addr().String()

	cfg := Config{
		Listeners: Listeners{HTTP("127.0.0.1:0")},
		Upstreams: Upstreams{
			"api": Pool{
				Backends:    []Backend{{Address: strings.TrimPrefix(backend.URL, "http://")}},
				HealthCheck: HealthCheck{Path: "/healthz", Interval: "20ms", Timeout: "1s"},
			},
		},
		Routes:   Routes{Match("/*").ProxyTo("api")},
		Shutdown: Shutdown{GracePeriod: "2s"},
	}
	r := mustResolve(t, cfg)
	r.Listeners[0].Addr = addr
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}

	if err := srv.Start(); err == nil {
		t.Fatal("Start succeeded despite a conflicting listener address")
	}
	assertHealthCheckerStopped(t, srv.pools["api"])
	// Settle before sampling: stop waits only the probe's client side,
	// so the backend can count one more hit just after Start returns.
	time.Sleep(50 * time.Millisecond)
	stopped := probes.Load()
	time.Sleep(150 * time.Millisecond) // several probe intervals
	if got := probes.Load(); got != stopped {
		t.Fatalf("probes continued after the failed Start: %d, want %d", got, stopped)
	}

	if err := busy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("retried Start: %v", err)
	}
	defer func() {
		if err := srv.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()
	waitForProbes(t, probes, stopped)
	mustServeProxyOK(t, addr)
}

// TestStartRetryResetsBackendHealth — health state a rolled-back attempt
// left behind must not leak into the successful retry. A primary demoted
// during a failed Start (a genuinely down backend, or a mis-scored probe)
// would otherwise stay unhealthy into the commit, and candidates() would
// route every request to the backups until Healthy consecutive probes
// undo it. The retried Start's checker restart resets backends to the
// initial healthy state, because none of them has ever served.
func TestStartRetryResetsBackendHealth(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("primary"))
	}))
	t.Cleanup(primary.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("backup"))
	}))
	t.Cleanup(backup.Close)

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	addr := busy.Addr().String()

	cfg := Config{
		Listeners: Listeners{HTTP("127.0.0.1:0")},
		Upstreams: Upstreams{
			"api": Pool{
				Backends: []Backend{
					{Address: strings.TrimPrefix(primary.URL, "http://")},
					{Address: strings.TrimPrefix(backup.URL, "http://"), Backup: true},
				},
				HealthCheck: HealthCheck{Path: "/", Interval: "20ms", Timeout: "1s"},
			},
		},
		Routes:   Routes{Match("/*").ProxyTo("api")},
		Shutdown: Shutdown{GracePeriod: "2s"},
	}
	r := mustResolve(t, cfg)
	r.Listeners[0].Addr = addr
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}

	if err := srv.Start(); err == nil {
		t.Fatal("Start succeeded despite a conflicting listener address")
	}
	// What a genuine probe failure during the rolled-back attempt leaves.
	srv.pools["api"].primary[0].markHealthy(false)

	if err := busy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("retried Start: %v", err)
	}
	defer func() {
		if err := srv.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()
	if !srv.pools["api"].primary[0].isHealthy() {
		t.Fatal("retried Start inherited the failed attempt's demotion")
	}
	waitForListen(t, addr)
	resp, err := http.Get("http://" + addr + "/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "primary" {
		t.Fatalf("request served by %q, want the reset primary", body)
	}
}

// assertHealthCheckerStopped fails unless the pool's prober goroutine has
// already exited. The rollback stops it synchronously, so the release owes
// no polling grace and a closed done channel is the exact invariant.
func assertHealthCheckerStopped(t *testing.T, ph *poolHandler) {
	t.Helper()
	select {
	case <-ph.hc.done:
	default:
		t.Fatal("health checker goroutine still running after the failed Start")
	}
}

// newProbeCountingBackend returns a backend answering everything 200 that
// counts the health-check probes it sees on path.
func newProbeCountingBackend(t *testing.T, path string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var probes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == path {
			probes.Add(1)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &probes
}

// waitForProbes polls until the backend has seen a probe past baseline —
// proof a restarted health checker is running again. The deadline is
// generous: a ticker interval on a loaded machine is not a schedule.
func waitForProbes(t *testing.T, probes *atomic.Int64, baseline int64) {
	t.Helper()
	const deadline = 3 * time.Second
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if probes.Load() > baseline {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("health checker did not resume probing within %s", deadline)
}

// mustServeProxyOK asserts the proxied route answers 200 on addr.
func mustServeProxyOK(t *testing.T, addr string) {
	t.Helper()
	waitForListen(t, addr)
	resp, err := http.Get("http://" + addr + "/x")
	if err != nil {
		t.Fatalf("GET %s: %v", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("%s: status %d, want 200", addr, resp.StatusCode)
	}
}

// mustServeMetrics asserts the metrics endpoint answers 200 on addr.
func mustServeMetrics(t *testing.T, addr string) {
	t.Helper()
	waitForListen(t, addr)
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("metrics status: %d, want 200", resp.StatusCode)
	}
}

// mustServeRedirect asserts the redirect-route config used by the Start
// rollback tests actually answers on addr — proof the listener behind it
// is alive, not a bound-then-instantly-closed socket.
func mustServeRedirect(t *testing.T, addr string) {
	t.Helper()
	waitForListen(t, addr)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get("http://" + addr + "/x")
	if err != nil {
		t.Fatalf("GET %s: %v", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("%s: status %d, want 301", addr, resp.StatusCode)
	}
}

// mustBindNow asserts addr is bindable the moment it is called: the
// rollback (and Shutdown's socket close) run synchronously before their
// caller returns, so the release owes no polling grace.
func mustBindNow(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("%s still bound: %v", addr, err)
	}
	_ = ln.Close()
}

// mustBindNowUDP is mustBindNow for a UDP address.
func mustBindNowUDP(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatalf("UDP %s still bound: %v", addr, err)
	}
	_ = conn.Close()
}

// newHTTP3TestServer builds a server with one HTTPS listener on tcpAddr
// serving a self-signed cert for h3.example, with HTTP/3 on udpAddr and,
// when metricsAddr is non-empty, a metrics listener on it. The resolved
// config is patched before newServer so the cert router, the Alt-Svc
// header, and serveListener's scheme dispatch all agree on the real
// addresses; the metrics address goes in unpatched because nothing is
// keyed by it.
func newHTTP3TestServer(t *testing.T, tcpAddr, udpAddr, metricsAddr string) *server {
	t.Helper()
	certFile, keyFile := writeSelfSignedCert(t, "h3.example")
	cfg := Config{
		Listeners: Listeners{
			HTTPS("127.0.0.1:0", StaticTLS(certFile, keyFile), HTTP3("127.0.0.1:0")),
		},
		Routes:   Routes{Match("/*").RedirectTo("https://example.com", http.StatusMovedPermanently)},
		Shutdown: Shutdown{GracePeriod: "2s"},
	}
	if metricsAddr != "" {
		cfg.Observability = Observability{Metrics: Prometheus(metricsAddr, "/metrics")}
	}
	r := mustResolve(t, cfg)
	r.Listeners[0].Addr = tcpAddr
	r.Listeners[0].HTTP3Addr = udpAddr
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return srv
}

// TestStartShutdownReleasesHTTP3Socket — Start binds the HTTP/3 UDP
// socket and quic-go never closes a caller-provided conn, so a normal
// Shutdown must close it itself or the port leaks for the process's
// lifetime. Holding the port is asserted with a real HTTP/3 request, not
// just a failed rebind — a dead serve loop pins the socket too.
func TestStartShutdownReleasesHTTP3Socket(t *testing.T) {
	udpAddr := reserveUDPAddr(t)
	srv := newHTTP3TestServer(t, reserveAddr(t), udpAddr, "")

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The socket is held while serving — by a QUIC server that answers.
	if conn, err := net.ListenPacket("udp", udpAddr); err == nil {
		_ = conn.Close()
		t.Fatal("HTTP/3 UDP socket not bound while serving")
	}
	mustServeHTTP3(t, udpAddr)
	if err := srv.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// ...and released once Shutdown returns: the conn close happens
	// inside Shutdown, after the drain.
	mustBindNowUDP(t, udpAddr)
}

// TestStartHTTP3BindFailureReleasesListeners — an HTTP/3 UDP bind
// conflict must fail Start, release the TCP socket bound before it
// without ever having exposed a route on it, and leave the server
// retryable with both transports actually serving afterwards.
func TestStartHTTP3BindFailureReleasesListeners(t *testing.T) {
	busy, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	udpAddr := busy.LocalAddr().String()
	tcpAddr := reserveAddr(t)
	srv := newHTTP3TestServer(t, tcpAddr, udpAddr, "")

	if err := srv.Start(); err == nil {
		t.Fatal("Start succeeded despite a conflicting HTTP/3 UDP address")
	}
	// Two-phase Start accepted no connection: the address must refuse and
	// already be free.
	if conn, err := net.DialTimeout("tcp", tcpAddr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("TCP address still accepting after failed Start")
	}
	mustBindNow(t, tcpAddr)

	if err := busy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("retried Start: %v", err)
	}
	defer func() {
		if err := srv.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()
	mustServeTLSRedirect(t, tcpAddr)
	mustServeHTTP3(t, udpAddr)
}

// TestStartMetricsFailureReleasesHTTP3Socket — the metrics socket binds
// last, so a metrics conflict is the one rollback path that has to give
// back a UDP socket it already bound successfully. Both transports of the
// HTTP/3 listener must be free again the moment Start returns, and the
// retry must bring all three up serving.
func TestStartMetricsFailureReleasesHTTP3Socket(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	metricsAddr := busy.Addr().String()

	tcpAddr := reserveAddr(t)
	udpAddr := reserveUDPAddr(t)
	srv := newHTTP3TestServer(t, tcpAddr, udpAddr, metricsAddr)

	if err := srv.Start(); err == nil {
		t.Fatal("Start succeeded despite a conflicting metrics address")
	}
	mustBindNow(t, tcpAddr)
	mustBindNowUDP(t, udpAddr)

	if err := busy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("retried Start: %v", err)
	}
	defer func() {
		if err := srv.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()
	mustServeTLSRedirect(t, tcpAddr)
	mustServeHTTP3(t, udpAddr)
	mustServeMetrics(t, metricsAddr)
}

// mustServeHTTP3 asserts the HTTP/3 endpoint answers a real request on
// udpAddr — proof the QUIC serve loop is alive, not merely that some
// process holds the socket.
func mustServeHTTP3(t *testing.T, udpAddr string) {
	t.Helper()
	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{
			ServerName:         "h3.example",
			InsecureSkipVerify: true, //nolint:gosec // G402: hermetic test against the self-signed fixture cert
		},
	}
	defer func() { _ = tr.Close() }()
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       5 * time.Second,
	}
	resp, err := client.Get("https://" + udpAddr + "/x")
	if err != nil {
		t.Fatalf("HTTP/3 GET %s: %v", udpAddr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("HTTP/3 %s: status %d, want 301", udpAddr, resp.StatusCode)
	}
}

// mustServeTLSRedirect is mustServeRedirect over TLS, for the HTTPS
// listener the HTTP/3 tests configure.
func mustServeTLSRedirect(t *testing.T, addr string) {
	t.Helper()
	waitForListen(t, addr)
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				ServerName:         "h3.example",
				InsecureSkipVerify: true, //nolint:gosec // G402: hermetic test against the self-signed fixture cert
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       5 * time.Second,
	}
	resp, err := client.Get("https://" + addr + "/x")
	if err != nil {
		t.Fatalf("GET %s: %v", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("%s: status %d, want 301", addr, resp.StatusCode)
	}
}

// TestAltSvcDropsWhenHTTP3Dies — with ma=86400, an Alt-Svc header for a
// dead endpoint sends every compatible client through a failed QUIC
// attempt first. The header must track the serve loop's liveness.
func TestAltSvcDropsWhenHTTP3Dies(t *testing.T) {
	tcpAddr := reserveAddr(t)
	srv := newHTTP3TestServer(t, tcpAddr, reserveUDPAddr(t), "")

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := srv.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()
	waitForAltSvc(t, tcpAddr, true)
	// Kill the serve loop out from under the server: its socket going
	// away is the unexpected-death path.
	if err := srv.http3Servers[0].conn.Close(); err != nil {
		t.Fatal(err)
	}
	waitForAltSvc(t, tcpAddr, false)
}

// waitForAltSvc polls the HTTPS listener until the Alt-Svc header's
// presence matches want.
func waitForAltSvc(t *testing.T, addr string, want bool) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				ServerName:         "h3.example",
				InsecureSkipVerify: true, //nolint:gosec // G402: hermetic test against the self-signed fixture cert
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       5 * time.Second,
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("https://" + addr + "/x")
		if err == nil {
			has := resp.Header.Get("Alt-Svc") != ""
			_ = resp.Body.Close()
			if has == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Alt-Svc presence on %s never became %v", addr, want)
}

// TestShutdownErrorChannelHoldsAllProducers — every shutdown path erroring
// at once must not deadlock the error channel: the listeners, metrics, and
// the tracing flush all send before anything drains. Regression for the
// capacity being one producer short.
func TestShutdownErrorChannelHoldsAllProducers(t *testing.T) {
	cfg := Config{
		Listeners: Listeners{HTTP("127.0.0.1:0")},
		Routes:    Routes{Match("/*").RedirectTo("https://example.com", http.StatusMovedPermanently)},
		Observability: Observability{
			Metrics: Prometheus("127.0.0.1:0", "/metrics"),
		},
		Shutdown: Shutdown{GracePeriod: "100ms"},
	}
	r := mustResolve(t, cfg)
	contentAddr := reserveAddr(t)
	metricsAddr := reserveAddr(t)
	r.Listeners[0].Addr = contentAddr
	r.Observability.Metrics.Addr = metricsAddr
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	srv.tracingShutdown = func(context.Context) error { return errors.New("tracing flush failed") }

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForListen(t, contentAddr)
	waitForListen(t, metricsAddr)
	// Park one half-written request on each server so its drain runs out
	// the grace period and returns an error.
	for _, addr := range []string{contentAddr, metricsAddr} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan error, 1)
	go func() { done <- srv.Shutdown() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Shutdown reported success with every producer failing")
		}
		if !strings.Contains(err.Error(), "tracing flush failed") {
			t.Fatalf("joined error %v lost the tracing failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown deadlocked on its error channel")
	}
}

// reserveUDPAddr is reserveAddr for a UDP port: bind ephemeral, close,
// return the address for the HTTP/3 listener under test to claim.
func reserveUDPAddr(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := conn.LocalAddr().String()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestNewServer_HTTPSWithoutTLSMaterial — building a server with an HTTPS
// listener that has no TLS material is an error from newServer.
func TestNewServer_HTTPSWithoutTLSMaterial(t *testing.T) {
	t.Parallel()
	// Construct a resolved config by hand so we bypass Resolve's own
	// rejection of this state (Resolve catches it earlier).
	cfg := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"a": Pool{Backends: []Backend{{Address: "127.0.0.1:1"}}},
		},
		Routes: Routes{Match("/*").ProxyTo("a")},
	})
	cfg.Listeners[0].Scheme = "https"
	cfg.Listeners[0].Redirect = ""
	cfg.Listeners[0].AutoTLS = nil
	cfg.Listeners[0].StaticTLS = nil
	_, err := newServer(cfg)
	if err == nil {
		t.Fatal("want error for HTTPS listener with no TLS material")
	}
	if !strings.Contains(err.Error(), "TLS material") {
		t.Errorf("error: %v", err)
	}
}

// TestRedirectListener_AlsoServesACMEChallenge — when AutoTLS is configured
// on another listener, the redirect listener wraps its handler with the
// autocert HTTPHandler so /.well-known/acme-challenge/* gets served. The
// non-challenge path still returns the redirect.
func TestRedirectListener_AlsoServesACMEChallenge(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Config{
		Listeners: Listeners{
			HTTP(":0").RedirectTo("https"),
			HTTPS(":0",
				AutoTLS("test.example.com").Email("ops@example.com").Storage(dir),
				HTTP2(),
			),
		},
		Upstreams: Upstreams{
			"a": Pool{Backends: []Backend{{Address: "127.0.0.1:1"}}},
		},
		Routes: Routes{Match("/*").ProxyTo("a")},
	}
	r := mustResolve(t, cfg)
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		for _, ph := range srv.pools {
			ph.shutdown()
		}
	})

	// Redirect listener handler: hit /.well-known/acme-challenge/<token>
	// — autocert's HTTPHandler returns 404 because the token is unknown,
	// but the request did not get redirected to https://, confirming the
	// handler wrapping happened. A normal path still redirects.
	redirectHS := srv.listeners[0]
	rec := runRequest(t, redirectHS.Handler, httptest.NewRequest("GET", "http://example.com/.well-known/acme-challenge/x", nil))
	if rec.Code == http.StatusMovedPermanently {
		t.Errorf("acme challenge path got redirected; autocert wrap not applied")
	}

	rec = runRequest(t, redirectHS.Handler, httptest.NewRequest("GET", "http://example.com/other", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("non-acme path: got %d, want 301", rec.Code)
	}
}

// TestStripPrefix verifies static-route prefix stripping.
func TestStripPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern, path, wantUpstream string
	}{
		{"/static/*", "/static/css/app.css", "/css/app.css"},
		{"/static/*", "/static/", "/"},
		{"/api/*", "/api/v1/x", "/v1/x"},
		{"/*", "/anything", "/anything"},
		// Exact patterns name a file, not a directory prefix: the path
		// reaches the FileServer whole so it can find the file.
		{"/robots.txt", "/robots.txt", "/robots.txt"},
		{"/well-known/security.txt", "/well-known/security.txt", "/well-known/security.txt"},
	}
	for _, c := range cases {
		inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if r.URL.Path != c.wantUpstream {
				t.Errorf("pattern %q path %q: inner saw %q, want %q", c.pattern, c.path, r.URL.Path, c.wantUpstream)
			}
		})
		h := stripPrefix(c.pattern, inner)
		req := httptest.NewRequest("GET", "http://x"+c.path, nil)
		runRequest(t, h, req)
	}
}

// TestStaticRouteServesFiles — an exact static route serves the file that
// its pattern names, while a trailing-wildcard route keeps serving the
// directory with its prefix stripped.
func TestStaticRouteServesFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "robots.txt", "User-agent: *\nDisallow:\n")
	writeFile(t, dir, "index.html", "<h1>root</h1>")
	if err := os.Mkdir(filepath.Join(dir, "css"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, dir, filepath.Join("css", "app.css"), "body{}")

	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/robots.txt").Host("foo.example.com").Serve(dir),
			Match("/static/*").Serve(dir),
		},
	})
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	h := srv.buildRouter()

	cases := []struct {
		name, host, path string
		wantStatus       int
		wantBody         string
	}{
		{"exact route serves its file", "foo.example.com", "/robots.txt", http.StatusOK, "User-agent: *\nDisallow:\n"},
		{"exact route is host scoped", "bar.example.com", "/robots.txt", http.StatusNotFound, ""},
		{"wildcard route strips its prefix", "foo.example.com", "/static/css/app.css", http.StatusOK, "body{}"},
		{"wildcard route serves the directory index", "foo.example.com", "/static/", http.StatusOK, "<h1>root</h1>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://"+c.host+c.path, nil)
			rec := runRequest(t, h, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, c.wantStatus)
			}
			if c.wantBody == "" {
				return
			}
			if got := rec.Body.String(); got != c.wantBody {
				t.Errorf("body: got %q, want %q", got, c.wantBody)
			}
		})
	}
}

// TestBuildMetricsServer — a metrics-enabled server exposes /metrics and
// /debug/pprof/ on its listener.
func TestBuildMetricsServer(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"a": Pool{Backends: []Backend{{Address: "127.0.0.1:1"}}},
		},
		Routes: Routes{Match("/*").ProxyTo("a")},
		Observability: Observability{
			Metrics: Prometheus(":0", "/metrics"),
		},
	})
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		for _, ph := range srv.pools {
			ph.shutdown()
		}
	})

	// Hit /metrics
	rec := runRequest(t, srv.metricsServer.Handler, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/metrics status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "statute_requests_total") {
		t.Errorf("/metrics missing canonical counter; body=%s", rec.Body.String())
	}

	// Hit /debug/pprof/
	rec = runRequest(t, srv.metricsServer.Handler, httptest.NewRequest("GET", "/debug/pprof/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/debug/pprof/ status: %d", rec.Code)
	}
}

// TestServerShutdownWithoutStart — calling Shutdown on a server that was
// never started must not panic or hang.
func TestServerShutdownWithoutStart(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"a": Pool{Backends: []Backend{{Address: "127.0.0.1:1"}}},
		},
		Routes:   Routes{Match("/*").ProxyTo("a")},
		Shutdown: Shutdown{GracePeriod: "100ms"},
	})
	srv, err := newServer(r)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Shutdown did not return within 1s")
	}
}

// waitForListen polls addr until a TCP Dial succeeds, for up to two
// seconds: the serve goroutines launch just before Start returns, so a
// first request can beat the accept loop.
func waitForListen(t *testing.T, addr string) {
	t.Helper()
	const deadline = 2 * time.Second
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listener at %s never accepted within %s", addr, deadline)
}
