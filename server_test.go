package statute

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	r := mustResolve(t, cfg)
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	// Discover a free port, close, and patch the resolved listener's Addr
	// so Start() binds to a known port we can dial. The race between close
	// and bind is fine in practice for hermetic tests.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.listeners[0].Addr = ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
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
	addr := srv.listeners[0].Addr
	waitForListen(t, addr, 2*time.Second)

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
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	firstAddr := first.Addr().String()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	srv.listeners[0].Addr = firstAddr
	srv.listeners[1].Addr = busy.Addr().String()

	if err := srv.Start(); err == nil {
		t.Fatal("Start succeeded despite a conflicting listener address")
	}
	waitForRelease(t, firstAddr, 2*time.Second)

	// A failed Start must be retryable: free the contested port and the
	// retry must succeed with listeners that actually serve — not
	// http.Servers poisoned by an unwind Close reporting success over
	// dead sockets.
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
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	content, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	contentAddr := content.Addr().String()
	if err := content.Close(); err != nil {
		t.Fatal(err)
	}
	srv.listeners[0].Addr = contentAddr

	if err := srv.Start(); err == nil {
		t.Fatal("Start succeeded despite a conflicting metrics address")
	}
	waitForRelease(t, contentAddr, 2*time.Second)

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

// mustServeMetrics asserts the metrics endpoint answers 200 on addr.
func mustServeMetrics(t *testing.T, addr string) {
	t.Helper()
	waitForListen(t, addr, 2*time.Second)
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
	waitForListen(t, addr, 2*time.Second)
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

// waitForRelease asserts addr becomes bindable again within deadline —
// that whatever held it has been closed. The serve goroutine may close
// its listener a moment after Start returns, so this polls.
func waitForRelease(t *testing.T, addr string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if ln, err := net.Listen("tcp", addr); err == nil {
			_ = ln.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s still bound after failed Start", addr)
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

// waitForListen polls addr until a TCP Dial succeeds or the deadline fires.
func waitForListen(t *testing.T, addr string, deadline time.Duration) {
	t.Helper()
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
