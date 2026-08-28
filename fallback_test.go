package statute

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

// countingFallback answers 418 and counts its invocations, so a test can
// assert both that the fallback ran and that it did not.
func countingFallback(calls *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	})
}

// fallbackRouter resolves cfg and returns the router, closing any pools.
func fallbackRouter(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		for _, ph := range srv.pools {
			ph.transport.CloseIdleConnections()
		}
	})
	return srv.buildRouter()
}

// backendHostPort starts an echo-less backend and returns its host and port
// split, the form the fake Docker daemon reports container endpoints in.
func backendHostPort(t *testing.T, ts *httptest.Server) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("split backend address: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse backend port: %v", err)
	}
	return host, port
}

// An unset fallback leaves Fallback and HasFallback zero.
func TestResolveFallback(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Fallback:  countingFallback(&calls),
	})
	if r.Fallback == nil {
		t.Error("resolved config has no Fallback")
	}
	if !r.HasFallback {
		t.Error("resolved config does not carry the HasFallback marker")
	}

	bare := mustResolve(t, Config{Listeners: Listeners{HTTP(":0")}})
	if bare.Fallback != nil || bare.HasFallback {
		t.Errorf("unset fallback resolved to %v/%v", bare.Fallback, bare.HasFallback)
	}
}

// A typed-nil Fallback fails at resolve. Handle treats a typed-nil the same.
func TestResolveFallbackTypedNil(t *testing.T) {
	t.Parallel()
	cases := map[string]http.Handler{
		"typed-nil ServeMux pointer": (*http.ServeMux)(nil),
		"typed-nil HandlerFunc":      http.HandlerFunc(nil),
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(Config{
				Listeners: Listeners{HTTP(":0")},
				Fallback:  h,
			})
			if err == nil {
				t.Fatal("want an error for a typed-nil fallback")
			}
			if !strings.Contains(err.Error(), "fallback: handler is nil") {
				t.Errorf("got %q, want substring %q", err, "fallback: handler is nil")
			}
		})
	}
}

// With no fallback configured the terminal branch is still net/http's 404.
func TestFallbackUnsetKeeps404(t *testing.T) {
	t.Parallel()
	router := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes:    Routes{Match("/healthz").Handle(noContentHandler)},
	})
	rec := runRequest(t, router, httptest.NewRequest("GET", "http://x/nothing", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

// With no Docker provider, an unmatched request reaches the fallback.
func TestFallbackWithoutDockerTable(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	router := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes:    Routes{Match("/healthz").Handle(noContentHandler)},
		Fallback:  countingFallback(&calls),
	})

	rec := runRequest(t, router, httptest.NewRequest("GET", "http://x/nothing", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("unmatched: got %d, want 418", rec.Code)
	}
	rec = runRequest(t, router, httptest.NewRequest("GET", "http://x/healthz", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("matched route: got %d, want 204", rec.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("fallback calls: got %d, want 1", got)
	}
}

func TestStaticHostDoesNotUseTraefikDotFolding(t *testing.T) {
	var calls atomic.Int64
	router := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/*").Host("static.example.com.").Handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "static")
			})),
		},
		Fallback: countingFallback(&calls),
	})

	dotted := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://static.example.com./", nil))
	if dotted.Code != http.StatusOK || dotted.Body.String() != "static" {
		t.Fatalf("dotted static host: code=%d body=%q", dotted.Code, dotted.Body.String())
	}
	undotted := runRequest(t, router, httptest.NewRequest(http.MethodGet, "http://static.example.com/", nil))
	if undotted.Code != http.StatusTeapot || calls.Load() != 1 {
		t.Fatalf("undotted static host: code=%d calls=%d, want fallback", undotted.Code, calls.Load())
	}
}

// A matched route's own 404 is not rewritten into the fallback.
func TestFallbackDoesNotReplaceMatchedRoute404(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	router := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/api/*").Handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "no such object", http.StatusNotFound)
			})),
		},
		Fallback: countingFallback(&calls),
	})

	rec := runRequest(t, router, httptest.NewRequest("GET", "http://x/api/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("matched route: got %d, want its own 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no such object") {
		t.Errorf("matched route body: got %q, want the route's own", rec.Body.String())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("fallback replaced a matched route's 404 (%d calls)", got)
	}

	// The same status from an unmatched path is the fallback's to answer.
	rec = runRequest(t, router, httptest.NewRequest("GET", "http://x/missing", nil))
	if rec.Code != http.StatusTeapot || calls.Load() != 1 {
		t.Errorf("unmatched path: code=%d calls=%d, want 418 from the fallback", rec.Code, calls.Load())
	}
}

// Route middleware does not decorate the fallback response.
func TestFallbackCarriesNoRouteMiddleware(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	router := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/healthz").Handle(noContentHandler).
				With(SetResponseHeader("X-Route", "yes")),
		},
		Fallback: countingFallback(&calls),
	})

	rec := runRequest(t, router, httptest.NewRequest("GET", "http://x/healthz", nil))
	assertHeader(t, rec.Header(), "X-Route", "yes")
	rec = runRequest(t, router, httptest.NewRequest("GET", "http://x/nothing", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("fallback status: got %d, want 418", rec.Code)
	}
	assertNoHeader(t, rec.Header(), "X-Route")
}

// Static wins its path, Docker wins the paths beneath, unmatched hits Fallback.
func TestFallbackAfterStaticAndDockerRoutes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("from docker"))
	}))
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)

	p, srv, _ := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{{
		name: "web-1", ip: host, port: port,
		labels: map[string]string{"statute.enable": "true", "statute.host": "app.example.com"},
	}})
	var calls atomic.Int64
	srv.cfg = mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes:    Routes{Match("/static").Host("app.example.com").Handle(noContentHandler)},
		Fallback:  countingFallback(&calls),
	})
	mustSync(t, p)
	router := srv.buildRouter()

	rec := runRequest(t, router, httptest.NewRequest("GET", "http://app.example.com/static", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("static route: got %d, want 204 (a Docker route shadowed it)", rec.Code)
	}
	rec = runRequest(t, router, httptest.NewRequest("GET", "http://app.example.com/x", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "from docker" {
		t.Errorf("docker route: got %d %q (the fallback shadowed it)", rec.Code, rec.Body.String())
	}
	rec = runRequest(t, router, httptest.NewRequest("GET", "http://other.example.com/x", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("unmatched: got %d, want 418", rec.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("fallback calls: got %d, want 1", got)
	}
}

// The fallback is consulted against the current generation.
func TestFallbackAfterDockerGenerationSwap(t *testing.T) {
	p, srv, swap := newFakeProvider(t, &resolved.Docker{}, []fakeDaemonContainer{{
		name: "web-1", ip: "127.0.0.1", port: 1,
		labels: map[string]string{"statute.enable": "true", "statute.host": "old.example.com"},
	}})
	var calls atomic.Int64
	srv.cfg = mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Fallback:  countingFallback(&calls),
	})
	mustSync(t, p)
	router := srv.buildRouter()

	// A 502 from the refused backend is still the discovered route.
	rec := runRequest(t, router, httptest.NewRequest("GET", "http://old.example.com/x", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("first generation: got %d, want 502 from the discovered pool", rec.Code)
	}

	swap([]fakeDaemonContainer{{
		name: "web-2", ip: "127.0.0.1", port: 1,
		labels: map[string]string{"statute.enable": "true", "statute.host": "new.example.com"},
	}})
	mustSync(t, p)

	rec = runRequest(t, router, httptest.NewRequest("GET", "http://new.example.com/x", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("replacement generation: got %d, want 502 from the discovered pool", rec.Code)
	}
	rec = runRequest(t, router, httptest.NewRequest("GET", "http://old.example.com/x", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("retired route: got %d, want 418 from the fallback", rec.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("fallback calls: got %d, want 1", got)
	}
}

// A pending HTTP-01 token is answered before the fallback sees the path.
func TestFallbackAndHTTP01ChallengePaths(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	cfg := Config{
		Listeners: Listeners{
			HTTP(":80"),
			HTTPS(":443", AutoTLS("foo.example.com").HTTP01().Email("x@x").Storage(t.TempDir())),
		},
		Fallback: countingFallback(&calls),
	}
	r := mustResolve(t, cfg)
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	src := r.Listeners[1].AutoTLSSources[0]
	solver, ok := srv.acmeManagers[src].solver.(*http01Solver)
	if !ok {
		t.Fatalf("solver: got %T", srv.acmeManagers[src].solver)
	}
	path := "/.well-known/acme-challenge/tok-73"
	solver.mu.Lock()
	solver.tokens[path] = "tok-73.keyauth"
	solver.mu.Unlock()

	h := srv.buildListenerHandler(r.Listeners[0], srv.buildRouter(), nil)
	rec := runRequest(t, h, httptest.NewRequest("GET", "http://foo.example.com"+path, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "tok-73.keyauth" {
		t.Errorf("challenge: code=%d body=%q, want the key authorization", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("fallback ran for the challenge path (%d calls)", got)
	}

	rec = runRequest(t, h, httptest.NewRequest("GET", "http://foo.example.com/other", nil))
	if rec.Code != http.StatusTeapot || calls.Load() != 1 {
		t.Fatalf("ordinary path: code=%d calls=%d, want 418 from the fallback", rec.Code, calls.Load())
	}

	// A pinned solver claims only its pending tokens; everything else under
	// the prefix falls through to the router, and so to the fallback.
	unknown := "/.well-known/acme-challenge/no-such-token"
	rec = runRequest(t, h, httptest.NewRequest("GET", "http://foo.example.com"+unknown, nil))
	if rec.Code != http.StatusTeapot || calls.Load() != 2 {
		t.Errorf("unknown token: code=%d calls=%d, want 418 from the fallback", rec.Code, calls.Load())
	}
}

// Autocert answers unknown tokens with its own 404. The fallback sits under
// the router.
func TestFallbackNotConsultedForAutocertChallenge(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	cfg := Config{
		Listeners: Listeners{
			HTTP(":80"),
			HTTPS(":443", AutoTLS("foo.example.com").Email("ops@example.com").Storage(t.TempDir())),
		},
		Fallback: countingFallback(&calls),
	}
	r := mustResolve(t, cfg)
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	h := srv.buildListenerHandler(r.Listeners[0], srv.buildRouter(), nil)
	rec := runRequest(t, h, httptest.NewRequest("GET", "http://foo.example.com/.well-known/acme-challenge/x", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown challenge token: got %d, want autocert's 404", rec.Code)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("fallback ran for the challenge path (%d calls)", got)
	}

	rec = runRequest(t, h, httptest.NewRequest("GET", "http://foo.example.com/other", nil))
	if rec.Code != http.StatusTeapot || calls.Load() != 1 {
		t.Errorf("ordinary path: code=%d calls=%d, want 418 from the fallback", rec.Code, calls.Load())
	}
}

// A redirect-only listener never reaches the content router.
func TestFallbackNotReachedOnRedirectListener(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	cfg := Config{
		Listeners: Listeners{
			HTTP(":80").RedirectTo("https"),
			HTTPS(":443", AutoTLS("foo.example.com").Email("ops@example.com").Storage(t.TempDir())),
		},
		Fallback: countingFallback(&calls),
	}
	r := mustResolve(t, cfg)
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	h := srv.buildListenerHandler(r.Listeners[0], srv.buildRouter(), nil)
	rec := runRequest(t, h, httptest.NewRequest("GET", "http://foo.example.com/anything", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("redirect listener: got %d, want 301", rec.Code)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("redirect listener reached the fallback (%d calls)", got)
	}
}

// Listener observability records the fallback's own final status.
func TestFallbackObservedByListenerObservability(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var mu sync.Mutex
	var calls atomic.Int64
	cfg := Config{
		Listeners:     Listeners{HTTP(":80")},
		Fallback:      countingFallback(&calls),
		Observability: Observability{AccessLog: JSONLog(LogWriter{w: &mu_writer{Mutex: &mu, w: &buf}, name: "test"})},
	}
	r := mustResolve(t, cfg)
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	h := srv.buildListenerHandler(r.Listeners[0], srv.buildRouter(), nil)
	rec := runRequest(t, h, httptest.NewRequest("GET", "http://x/nothing", nil))
	if rec.Code != http.StatusTeapot || calls.Load() != 1 {
		t.Fatalf("fallback: code=%d calls=%d", rec.Code, calls.Load())
	}

	mu.Lock()
	line := buf.String()
	mu.Unlock()
	var v map[string]any
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		t.Fatalf("access log line %q: %v", line, err)
	}
	if got, _ := v["status"].(float64); int(got) != http.StatusTeapot {
		t.Errorf("logged status: got %v, want 418", v["status"])
	}

	var metrics bytes.Buffer
	srv.stats.WritePrometheus(&metrics)
	if want := `statute_requests_by_status_total{status="418"} 1`; !strings.Contains(metrics.String(), want) {
		t.Errorf("metrics missing %q:\n%s", want, metrics.String())
	}
}

// JSON export carries HasFallback and omits the handler.
func TestExport_Fallback(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	cfg := Config{
		Listeners: Listeners{HTTP(":80")},
		Fallback:  countingFallback(&calls),
	}
	var buf bytes.Buffer
	if err := Export(cfg, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if v, ok := out["HasFallback"].(bool); !ok || !v {
		t.Errorf("export missing the HasFallback marker: %v", out["HasFallback"])
	}
	if _, ok := out["Fallback"]; ok {
		t.Errorf("export serialized the handler itself: %v", out["Fallback"])
	}

	buf.Reset()
	if err := Export(Config{Listeners: Listeners{HTTP(":80")}}, &buf); err != nil {
		t.Fatalf("Export without fallback: %v", err)
	}
	out = nil
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if v, ok := out["HasFallback"]; ok && v != false {
		t.Errorf("HasFallback without a fallback: %v", v)
	}
}

// The graph renders the fallback from content listeners only.
func TestGraphDOT_Fallback(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	cfg := Config{
		Listeners: Listeners{
			HTTP(":80").RedirectTo("https"),
			HTTPS(":443", StaticTLS("/etc/cert.pem", "/etc/key.pem")),
		},
		Fallback: countingFallback(&calls),
	}
	var buf bytes.Buffer
	if err := GraphDOT(cfg, &buf); err != nil {
		t.Fatalf("GraphDOT: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `label="fallback"`) {
		t.Errorf("graph missing the fallback node:\n%s", out)
	}
	if !strings.Contains(out, "L1 -> F") {
		t.Errorf("graph missing the content listener edge:\n%s", out)
	}
	if strings.Contains(out, "L0 -> F") {
		t.Errorf("redirect-only listener grew a fallback edge:\n%s", out)
	}

	buf.Reset()
	cfg.Fallback = nil
	if err := GraphDOT(cfg, &buf); err != nil {
		t.Fatalf("GraphDOT without fallback: %v", err)
	}
	if strings.Contains(buf.String(), "fallback") {
		t.Errorf("graph renders a fallback that is not configured:\n%s", buf.String())
	}
}

// A fail-closed Docker router is refused with 404. The operator's fallback
// is often a catch-all proxy to the same container.
func TestFallbackNotReachedByFailClosedDockerRoute(t *testing.T) {
	p, srv, _ := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{{
		name: "app-1", ip: "10.0.0.9", port: 3000,
		labels: map[string]string{
			"traefik.enable":                       "true",
			"traefik.http.routers.app.rule":        "Host(`app.example.com`)",
			"traefik.http.routers.app.middlewares": "ghost@file",
		},
	}})
	var calls atomic.Int64
	srv.cfg = mustResolve(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Fallback:  countingFallback(&calls),
	})
	mustSync(t, p)

	tab := srv.dynamic.Load()
	if len(tab.routes) != 0 || len(tab.pools) != 0 {
		t.Fatalf("fail-closed router kept state: routes=%+v pools=%+v", tab.routes, tab.pools)
	}
	if len(tab.tombstones) != 1 {
		t.Fatalf("fail-closed router left %d tombstones, want 1", len(tab.tombstones))
	}
	rec := runRequest(t, srv.buildRouter(), httptest.NewRequest("GET", "http://app.example.com/", nil))
	if rec.Code != http.StatusNotFound || calls.Load() != 0 {
		t.Errorf("dropped route: code=%d calls=%d, want a 404 refusal with the fallback untouched", rec.Code, calls.Load())
	}
}

// fallbackDrainBody is long enough that a truncated write is visible.
const fallbackDrainBody = "fallback finished after shutdown began"

// The request parks in Config.Fallback; Shutdown must still deliver the
// whole body. Release is gated on the content listener closing.
func TestFallbackDrainsThroughShutdown(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP("127.0.0.1:0")},
		Routes: Routes{
			// A route that does not match, so the request provably
			// arrives through the terminal stage and not this handler.
			Match("/known").Handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})),
		},
		Fallback: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(entered)
			<-release
			w.WriteHeader(http.StatusTeapot)
			_, _ = io.WriteString(w, fallbackDrainBody)
		}),
		Defaults: Defaults{ReadHeaderTimeout: "1s"},
		Shutdown: Shutdown{GracePeriod: "5s"},
	})
	addr := reserveAddr(t)
	r.Listeners[0].Addr = addr
	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForListen(t, addr)

	type result struct {
		status int
		body   string
		err    error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/unmatched")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		got <- result{status: resp.StatusCode, body: string(body), err: err}
	}()
	<-entered

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- srv.Shutdown() }()
	// Load-bearing: released before the drain begins, the response proves
	// nothing. A closed listener is that moment; the readiness flag is not.
	waitForRefused(t, addr, shutdownDone)
	close(release)

	res := <-got
	if res.err != nil {
		t.Fatalf("in-flight fallback request failed across Shutdown: %v", res.err)
	}
	if res.status != http.StatusTeapot {
		t.Errorf("in-flight fallback status: got %d, want %d", res.status, http.StatusTeapot)
	}
	if res.body != fallbackDrainBody {
		t.Errorf("in-flight fallback body: got %q, want %q", res.body, fallbackDrainBody)
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the fallback drained")
	}
}
