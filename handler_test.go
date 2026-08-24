package statute

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noContentHandler answers every request with 204.
var noContentHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
})

// handlerConfig is a minimal config whose only route serves h in-process.
func handlerConfig(h http.Handler) Config {
	return Config{
		Listeners: Listeners{HTTP(":0")},
		Routes:    Routes{Match("/*").Handle(h)},
	}
}

// handlerRouter resolves cfg and returns the router, shutting down any pools.
func handlerRouter(t *testing.T, cfg Config) http.Handler {
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

// TestResolveHandlerRoute — a handler route resolves with the handler
// carried through, the marker set, and no other action populated.
func TestResolveHandlerRoute(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, handlerConfig(noContentHandler))
	rt := r.Routes[0]
	if rt.Handler == nil {
		t.Fatal("resolved route has no Handler")
	}
	if !rt.HandlerRoute {
		t.Error("resolved route does not carry the HandlerRoute marker")
	}
	if rt.Upstream != nil || rt.StaticDir != "" || rt.Redirect != nil {
		t.Errorf("handler route carries another action: %+v", rt)
	}
}

// TestResolveHandlerRouteErrors — Handle is mutually exclusive with each of
// the other three actions, a route still needs exactly one action, and a
// nil handler fails on its own nilness rather than as an action-less route.
func TestResolveHandlerRouteErrors(t *testing.T) {
	t.Parallel()
	pooled := Upstreams{"api": Pool{Backends: []Backend{{Address: "127.0.0.1:9001"}}}}
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			"proxy plus handle",
			Config{
				Listeners: Listeners{HTTP(":0")},
				Upstreams: pooled,
				Routes:    Routes{Match("/*").ProxyTo("api").Handle(noContentHandler)},
			},
			"more than one of ProxyTo, Serve, RedirectTo, and Handle",
		},
		{
			"serve plus handle",
			Config{
				Listeners: Listeners{HTTP(":0")},
				Routes:    Routes{Match("/*").Serve("./public").Handle(noContentHandler)},
			},
			"more than one of ProxyTo, Serve, RedirectTo, and Handle",
		},
		{
			"redirect plus handle",
			Config{
				Listeners: Listeners{HTTP(":0")},
				Routes:    Routes{Match("/*").RedirectTo("/new", 301).Handle(noContentHandler)},
			},
			"more than one of ProxyTo, Serve, RedirectTo, and Handle",
		},
		{
			"all four actions",
			Config{
				Listeners: Listeners{HTTP(":0")},
				Upstreams: pooled,
				Routes: Routes{
					Match("/*").ProxyTo("api").Serve("./public").
						RedirectTo("/new", 301).Handle(noContentHandler),
				},
			},
			"more than one of ProxyTo, Serve, RedirectTo, and Handle",
		},
		{
			"no action at all",
			Config{
				Listeners: Listeners{HTTP(":0")},
				Routes:    Routes{Match("/*")},
			},
			"none of ProxyTo, Serve, RedirectTo, or Handle",
		},
		{
			// Handle(nil) declares the action — the call betrays it — so
			// it must fail on the nil handler, never resolve into a route
			// whose runtime base would be the 500 fallback.
			"nil handler",
			handlerConfig(nil),
			"handle: handler is nil",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(c.cfg)
			if err == nil {
				t.Fatalf("want error containing %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("got %q, want substring %q", err, c.wantErr)
			}
		})
	}
}

// TestHandlerRouteServes — the issue's example end to end: a handler route
// answers through a real listener, not just through buildRouter.
func TestHandlerRouteServes(t *testing.T) {
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP("127.0.0.1:0")},
		Routes: Routes{
			Match("/healthz").Host("foo.example.com").Handle(noContentHandler),
		},
		Defaults: Defaults{ReadHeaderTimeout: "1s"},
		Shutdown: Shutdown{GracePeriod: "2s"},
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
	defer func() {
		if err := srv.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()
	waitForListen(t, addr)

	req, err := http.NewRequest("GET", "http://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "foo.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status: got %d, want 204", resp.StatusCode)
	}
}

// TestHandlerRouteWithMiddleware — a handler route sits behind the same
// middleware chain as any other action.
func TestHandlerRouteWithMiddleware(t *testing.T) {
	t.Parallel()
	router := handlerRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/*").Handle(noContentHandler).With(
				SetResponseHeader("Cache-Control", "no-store"),
			),
		},
	})
	rec := runRequest(t, router, httptest.NewRequest("GET", "http://x/healthz", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", rec.Code)
	}
	assertHeader(t, rec.Header(), "Cache-Control", "no-store")
}

// TestHandlerRouteRetryReentry — under Retry the handler is re-entered once
// per attempt for an idempotent method; that is the intended contract. A
// non-idempotent method is never re-entered, as Retry already enforces.
func TestHandlerRouteRetryReentry(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	router := handlerRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/*").Handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			})).With(Retry(3, OnStatus(http.StatusBadGateway))),
		},
	})

	rec := runRequest(t, router, httptest.NewRequest("GET", "http://x/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("GET status: got %d, want 502", rec.Code)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("GET attempts: got %d, want 3", got)
	}

	calls.Store(0)
	rec = runRequest(t, router, httptest.NewRequest("POST", "http://x/", strings.NewReader("body")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("POST status: got %d, want 502", rec.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("POST retries forbidden; calls=%d", got)
	}
}

// TestHandlerRoutePathRewrite — the hoisted rewrite contract holds for a
// handler route: matching observes the original path while the handler sees
// the rewritten URL.
func TestHandlerRoutePathRewrite(t *testing.T) {
	t.Parallel()
	var seen string
	router := handlerRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/api/*").Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.URL.Path
				w.WriteHeader(http.StatusNoContent)
			})).With(StripPrefix("/api")),
		},
	})
	// The route matched at all only because matching ran on the original
	// "/api/users"; the handler must then see the rewritten "/users".
	rec := runRequest(t, router, httptest.NewRequest("GET", "http://x/api/users", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", rec.Code)
	}
	if seen != "/users" {
		t.Errorf("handler saw path %q, want %q (rewritten)", seen, "/users")
	}
}

// TestHandlerRouteUnstrippedPath — Serve's wildcard prefix stripping is
// Serve-specific: a wildcard handler route receives the request path whole.
func TestHandlerRouteUnstrippedPath(t *testing.T) {
	t.Parallel()
	var seen string
	router := handlerRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/api/*").Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.URL.Path
				w.WriteHeader(http.StatusNoContent)
			})),
		},
	})
	rec := runRequest(t, router, httptest.NewRequest("GET", "http://x/api/users", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", rec.Code)
	}
	if seen != "/api/users" {
		t.Errorf("handler saw path %q, want %q (unstripped)", seen, "/api/users")
	}
}

// TestHandlerRouteDeclarationOrder — a handler route participates in
// declaration-order matching like any other static route: it wins the
// requests it matches first and does not shadow later routes it misses.
func TestHandlerRouteDeclarationOrder(t *testing.T) {
	t.Parallel()
	backend := newEchoBackend(t)
	router := handlerRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"api": Pool{Backends: []Backend{{Address: strings.TrimPrefix(backend.URL, "http://")}}},
		},
		Routes: Routes{
			Match("/healthz").Handle(noContentHandler),
			Match("/*").ProxyTo("api"),
		},
	})

	rec := runRequest(t, router, httptest.NewRequest("GET", "http://x/healthz", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("handler route: got %d, want 204", rec.Code)
	}
	rec = runRequest(t, router, httptest.NewRequest("GET", "http://x/other", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("proxy route beneath: got %d, want 200", rec.Code)
	}
}

// TestExport_HandlerRoute — a config with a handler route exports without a
// serialization error; the JSON carries the HandlerRoute marker and never
// the handler itself.
func TestExport_HandlerRoute(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Export(handlerConfig(noContentHandler), &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var out struct {
		Routes []map[string]any
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if len(out.Routes) != 1 {
		t.Fatalf("routes: got %d, want 1", len(out.Routes))
	}
	if v, ok := out.Routes[0]["HandlerRoute"].(bool); !ok || !v {
		t.Errorf("export missing HandlerRoute marker: %v", out.Routes[0])
	}
	if _, ok := out.Routes[0]["Handler"]; ok {
		t.Errorf("export serialized the handler itself: %v", out.Routes[0])
	}
}

// TestGraphDOT_HandlerRoute — the graph renders a handler route as an
// edge-less route node, like redirect and static routes.
func TestGraphDOT_HandlerRoute(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := handlerConfig(noContentHandler)
	cfg.Routes[0].Host("h.example.com")
	if err := GraphDOT(cfg, &buf); err != nil {
		t.Fatalf("GraphDOT: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "h.example.com /*") {
		t.Errorf("graph missing the handler route node:\n%s", out)
	}
	if strings.Contains(out, "R0 -> P_") {
		t.Errorf("handler route grew a route->pool edge:\n%s", out)
	}
}

// TestLint_HandlerRoute — a handler route passes Lint without error
// findings.
func TestLint_HandlerRoute(t *testing.T) {
	t.Parallel()
	findings, err := Lint(handlerConfig(noContentHandler))
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	for _, f := range findings {
		if f.Severity == SeverityError {
			t.Errorf("unexpected error finding on handler route config: %s", f)
		}
	}
}

// TestHandlerRouteDrainsThroughShutdown — an in-flight handler request
// completes during Shutdown's grace period, like a proxied one.
func TestHandlerRouteDrainsThroughShutdown(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTP("127.0.0.1:0")},
		Routes: Routes{
			Match("/*").Handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(entered)
				<-release
				w.WriteHeader(http.StatusNoContent)
			})),
		},
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
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow")
		got <- result{resp, err}
	}()
	<-entered

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- srv.Shutdown() }()
	// Let Shutdown begin draining before the handler is allowed to finish,
	// so the request provably completes during shutdown rather than before.
	time.Sleep(100 * time.Millisecond)
	close(release)

	res := <-got
	if res.err != nil {
		t.Fatalf("in-flight GET failed across Shutdown: %v", res.err)
	}
	t.Cleanup(func() { _ = res.resp.Body.Close() })
	if res.resp.StatusCode != http.StatusNoContent {
		t.Errorf("in-flight status: got %d, want 204", res.resp.StatusCode)
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the drain")
	}
}
