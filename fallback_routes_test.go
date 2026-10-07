package statute

import (
	"bytes"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"statute.kjanat.dev/resolved"
)

func fallbackRoutesConfig(routes Routes) Config {
	return Config{
		Listeners:      Listeners{HTTP(":0")},
		Upstreams:      Upstreams{"shared": Pool{Backends: []Backend{{Address: "http://127.0.0.1:1"}}}},
		Routes:         Routes{Match("/ordinary").ProxyTo("shared")},
		FallbackRoutes: routes,
	}
}

func TestFallbackRoutesResolution(t *testing.T) {
	t.Parallel()
	r := mustResolve(t, fallbackRoutesConfig(Routes{
		Match("/*").Hosts("a.example", "b.example").ProxyTo("shared").With(SetRequestHeader("X-Route", "terminal")),
		Match("/*").Handle(noContentHandler),
	}))
	if len(r.FallbackRoutes) != 3 || r.HasFallback || r.Fallback != nil {
		t.Fatalf("terminal expansion or handler marker: %+v", r)
	}
	if got := []string{r.FallbackRoutes[0].Host, r.FallbackRoutes[1].Host, r.FallbackRoutes[2].Host}; !slices.Equal(got, []string{"a.example", "b.example", ""}) {
		t.Fatalf("terminal declaration order: %v", got)
	}
	for _, route := range r.FallbackRoutes[:2] {
		if route.Upstream != r.Routes[0].Upstream || route.Upstream != r.Upstreams["shared"] {
			t.Fatal("terminal route did not retain canonical shared pool")
		}
	}
	if &r.FallbackRoutes[0].Middleware[0] == &r.FallbackRoutes[1].Middleware[0] {
		t.Fatal("expanded routes share middleware storage")
	}
}

func TestFallbackRoutesResolutionErrors(t *testing.T) {
	t.Parallel()
	for name, route := range map[string]*Route{
		"nil":                nil,
		"missing pool":       Match("/*").ProxyTo("docker-only"),
		"no action":          Match("/*"),
		"multiple actions":   Match("/*").Serve(".").ProxyTo("shared"),
		"typed nil":          Match("/*").Handle(http.HandlerFunc(nil)),
		"invalid client":     Match("/*").ClientIPs("invalid").Handle(noContentHandler),
		"invalid middleware": Match("/*").Handle(noContentHandler).With(SetRequestHeader("Bad Header", "x")),
		"ambiguous hosts":    Match("/*").Host("a").Hosts("b").Handle(noContentHandler),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(fallbackRoutesConfig(Routes{Match("/ok").Hosts("a", "b").Handle(noContentHandler), route}))
			if err == nil || !strings.Contains(err.Error(), "fallback_routes[1]") {
				t.Fatalf("want original terminal declaration diagnostic, got %v", err)
			}
		})
	}
	cfg := fallbackRoutesConfig(Routes{Match("/*").Handle(noContentHandler)})
	cfg.Routes = nil
	cfg.Listeners = Listeners{HTTP(":0").RedirectTo("https")}
	if _, err := Resolve(cfg); err == nil || !strings.Contains(err.Error(), "no listener serves content") {
		t.Fatalf("unreachable terminal routes accepted: %v", err)
	}
}

func TestFallbackRoutesDiagnosticCompatibility(t *testing.T) {
	t.Parallel()
	cfg := fallbackRoutesConfig(Routes{nil})
	cfg.Routes = Routes{nil}
	if _, err := Resolve(cfg); err == nil || err.Error() != "route[0]: nil route" {
		t.Fatalf("ordinary diagnostic changed: %v", err)
	}
	cfg.Routes = nil
	if _, err := Resolve(cfg); err == nil || err.Error() != "fallback_routes[0]: nil route" {
		t.Fatalf("terminal diagnostic changed: %v", err)
	}
}

func TestFallbackRoutesActionsAndOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "asset.txt"), []byte("asset"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, html.EscapeString(r.URL.Path))
	}))
	t.Cleanup(backend.Close)
	var legacy atomic.Int64
	router := fallbackRouter(t, Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{"shared": Pool{Backends: []Backend{{Address: backend.URL}}}},
		Routes:    Routes{Match("/static").Handle(noContentHandler)},
		FallbackRoutes: Routes{
			Match("/static").Handle(countingFallback(&legacy)),
			Match("/assets/*").Serve(dir),
			Match("/redirect").RedirectTo("/destination", http.StatusTemporaryRedirect),
			Match("/proxy/*").Hosts("a.example", "b.example").ProxyTo("shared").With(StripPrefix("/proxy")),
			Match("/private").ClientIPs("192.0.2.0/24").Handle(noContentHandler),
			Match("/denied").Handle(noContentHandler).With(AllowIPs("10.0.0.0/8")),
			Match("/error").Handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })),
			Match("/error").Handle(countingFallback(&legacy)),
		},
		Fallback: countingFallback(&legacy),
	})
	for _, tc := range []struct {
		url, peer string
		status    int
		body      string
	}{
		{"http://a.example/static", "192.0.2.1:1", 204, ""},
		{"http://a.example/assets/asset.txt", "192.0.2.1:1", 200, "asset"},
		{"http://a.example/assets/missing", "192.0.2.1:1", 404, ""},
		{"http://a.example/redirect", "192.0.2.1:1", 307, ""},
		{"http://b.example/proxy/value", "192.0.2.1:1", 200, "/value"},
		{"http://a.example/private", "192.0.2.1:1", 204, ""},
		{"http://a.example/denied", "192.0.2.1:1", 403, ""},
		{"http://a.example/error", "192.0.2.1:1", 403, ""},
	} {
		assertFallbackAction(t, router, tc.url, tc.peer, tc.status, tc.body)
	}
	if legacy.Load() != 0 {
		t.Fatal("matched action fell through")
	}
	for _, url := range []string{"http://unknown.example/proxy/value", "http://a.example/missing"} {
		if rec := runRequest(t, router, httptest.NewRequest(http.MethodGet, url, nil)); rec.Code != http.StatusTeapot {
			t.Fatal(rec.Code)
		}
	}
	if legacy.Load() != 2 {
		t.Fatal("terminal misses did not reach legacy handler")
	}
	bare := fallbackRouter(t, Config{Listeners: Listeners{HTTP(":0")}, FallbackRoutes: Routes{Match("/known").Handle(noContentHandler)}})
	if rec := runRequest(t, bare, httptest.NewRequest(http.MethodGet, "http://x/miss", nil)); rec.Code != http.StatusNotFound {
		t.Fatal(rec.Code)
	}
}

func assertFallbackAction(t *testing.T, router http.Handler, url, peer string, status int, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.RemoteAddr = peer
	rec := runRequest(t, router, req)
	if rec.Code != status || (body != "" && rec.Body.String() != body) {
		t.Errorf("%s: %d %q", url, rec.Code, rec.Body.String())
	}
	if status == http.StatusTemporaryRedirect && rec.Header().Get("Location") != "/destination" {
		t.Errorf("redirect location: %v", rec.Header())
	}
}

func TestFallbackRoutesIndependentCacheState(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, html.EscapeString(r.Header.Get("X-Table")))
	}))
	t.Cleanup(backend.Close)
	router := fallbackRouter(t, Config{
		Listeners:      Listeners{HTTP(":0")},
		Upstreams:      Upstreams{"shared": Pool{Backends: []Backend{{Address: backend.URL}}}},
		Routes:         Routes{Match("/*").ClientIPs("192.0.2.0/24").ProxyTo("shared").With(Cache("1m"), SetRequestHeader("X-Table", "ordinary"))},
		FallbackRoutes: Routes{Match("/*").ProxyTo("shared").With(Cache("1m"), SetRequestHeader("X-Table", "terminal"))},
	})
	// The cache key (method, host and URL) is identical. Only route-local
	// cache ownership may separate these responses over the shared pool.
	for range 2 {
		for _, tc := range []struct{ peer, body string }{{"192.0.2.1:1", "ordinary"}, {"198.51.100.1:1", "terminal"}} {
			req := httptest.NewRequest(http.MethodGet, "http://same.example/same", nil)
			req.RemoteAddr = tc.peer
			rec := runRequest(t, router, req)
			if rec.Code != http.StatusOK || rec.Body.String() != tc.body {
				t.Fatalf("cache policy leak: %d %q want %q", rec.Code, rec.Body.String(), tc.body)
			}
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("backend hits: %d want two independent cache fills", hits.Load())
	}
}

func TestFallbackRoutesMatchedErrorsAreFinal(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable} {
		for _, terminal := range []bool{false, true} {
			var native, legacy atomic.Int64
			matched := Match("/*").Handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			cfg := Config{Listeners: Listeners{HTTP(":0")},
				FallbackRoutes: Routes{Match("/*").Handle(countingFallback(&native))},
				Fallback:       countingFallback(&legacy),
			}
			if terminal {
				cfg.FallbackRoutes = append(Routes{matched}, cfg.FallbackRoutes...)
			} else {
				cfg.Routes = Routes{matched}
			}
			rec := runRequest(t, fallbackRouter(t, cfg), httptest.NewRequest(http.MethodGet, "http://x/", nil))
			if rec.Code != status || native.Load() != 0 || legacy.Load() != 0 {
				t.Fatalf("terminal=%v status=%d got=%d fallback=%d/%d", terminal, status, rec.Code, native.Load(), legacy.Load())
			}
		}
	}
}

func TestFallbackRoutesDockerRefusalsAndReplacement(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)
	container := func(policy string) fakeDaemonContainer {
		return fakeDaemonContainer{name: "app", ip: host, port: port, labels: map[string]string{
			"traefik.enable": "true", "traefik.http.routers.app.rule": "Host(`app.example`)", "traefik.http.routers.app.middlewares": policy,
		}}
	}
	p, srv, swap := newFakeProvider(t, &resolved.Docker{TraefikLabels: true}, []fakeDaemonContainer{container("missing@file")})
	var native, legacy atomic.Int64
	srv.cfg = mustResolve(t, Config{
		Listeners:      Listeners{HTTP(":0")},
		Routes:         Routes{Match("/static").Handle(noContentHandler)},
		FallbackRoutes: Routes{Match("/*").Handle(countingFallback(&native))},
		Fallback:       countingFallback(&legacy),
	})
	mustSync(t, p)
	router := srv.buildRouter()
	assertRouteResponse(t, router, "http://app.example/static", http.StatusNoContent, "")
	assertRouteResponse(t, router, "http://app.example/", http.StatusNotFound, "404 page not found\n")
	if native.Load() != 0 || legacy.Load() != 0 {
		t.Fatal("refusal reached fallback")
	}
	swap([]fakeDaemonContainer{container("")})
	mustSync(t, p)
	assertRouteResponse(t, router, "http://app.example/", http.StatusBadGateway, "")
	if native.Load() != 0 || legacy.Load() != 0 {
		t.Fatal("matched backend error reached fallback")
	}
	swap(nil)
	mustSync(t, p)
	assertRouteResponse(t, router, "http://app.example/", http.StatusTeapot, "")
	if native.Load() != 1 || legacy.Load() != 0 {
		t.Fatal("withdrawal did not reach native terminal route")
	}
	swap([]fakeDaemonContainer{container("missing@file")})
	mustSync(t, p)
	assertRouteResponse(t, router, "http://app.example/", http.StatusNotFound, "404 page not found\n")
	if native.Load() != 1 {
		t.Fatal("replacement refusal reached fallback")
	}
}

func TestFallbackRoutesQuarantineSurvivesRetirement(t *testing.T) {
	backend := httptest.NewServer(noContentHandler)
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)
	p, srv, daemon := newFakeProviderDaemon(t, &resolved.Docker{
		Workloads: map[string]resolved.Workload{"a": testWorkloadPolicy()},
	}, []fakeDaemonContainer{{name: "container-c", ip: host, port: port,
		labels: map[string]string{"statute.enable": "true", "statute.service": "a", "statute.host": "a.example"},
	}})
	var native, legacy atomic.Int64
	srv.cfg = mustResolve(t, Config{Listeners: Listeners{HTTP(":0")},
		FallbackRoutes: Routes{Match("/*").Handle(countingFallback(&native))}, Fallback: countingFallback(&legacy)})
	daemon.stopStarted = make(chan struct{})
	daemon.stopRelease = make(chan struct{})
	started, release := daemon.stopStarted, daemon.stopRelease
	var once sync.Once
	releaseStop := func() { once.Do(func() { close(release) }) }
	defer releaseStop()
	run, err := p.start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.stop)
	w := p.workloadFor("a")
	waitSignal(t, started, "idle stop not issued")
	router := srv.buildRouter()
	daemon.swap([]fakeDaemonContainer{{name: "container-c", stopped: true,
		labels: map[string]string{"statute.enable": "true", "statute.service": "b", "statute.host": "b.example"},
	}})
	mustSync(t, p)
	assertRouteResponse(t, router, "http://b.example/", http.StatusServiceUnavailable, "")
	if native.Load() != 0 || legacy.Load() != 0 {
		t.Fatal("unsettled retired mutation escaped quarantine")
	}
	releaseStop()
	waitRetiredWorkloadDormant(t, w)
	waitPublishedQuarantineRemoval(t, p)
	assertRouteResponse(t, router, "http://b.example/", http.StatusTeapot, "")
	if native.Load() != 1 || legacy.Load() != 0 {
		t.Fatal("terminal evidence did not reopen terminal routing")
	}
}

func TestFallbackRoutesListenerPolicyAndObservability(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	var mu sync.Mutex
	cert, key := writeSelfSignedCert(t, "x.example")
	r := mustResolve(t, Config{
		Listeners: Listeners{HTTPS(":443", StaticTLS(cert, key), HTTP3(":443/udp"), TrustedProxy("203.0.113.0/24").ClientIPHeader("X-Client-IP"))},
		FallbackRoutes: Routes{Match("/original").ClientIPs("192.0.2.0/24").Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/rewritten" {
				t.Errorf("handler path: %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusForbidden)
		})).With(ReplacePath("/rewritten"))},
		Observability: Observability{AccessLog: JSONLog(LogWriter{w: &muWriter{Mutex: &mu, w: &logs}, name: "test"})},
	})
	srv, err := newServer(r)
	if err != nil {
		t.Fatal(err)
	}
	var alive atomic.Bool
	alive.Store(true)
	h := srv.buildListenerHandler(r.Listeners[0], srv.buildRouter(), &alive)
	// Exercise policy directly with explicit peers; localhost is untrusted.
	for _, tc := range []struct {
		peer   string
		status int
	}{{"203.0.113.1:1", 403}, {"198.51.100.1:1", 404}} {
		req := httptest.NewRequest(http.MethodGet, "http://x/original", nil)
		req.RemoteAddr = tc.peer
		req.Header.Set("X-Client-IP", "192.0.2.1")
		rec := runRequest(t, h, req)
		if rec.Code != tc.status {
			t.Fatalf("peer %s: %d", tc.peer, rec.Code)
		}
		if rec.Header().Get("Alt-Svc") == "" {
			t.Fatal("terminal response lost Alt-Svc")
		}
	}
	mu.Lock()
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	mu.Unlock()
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["status"] != float64(403) || entry["path"] != "/original" {
		t.Fatalf("final status/original path: %v", entry)
	}
	var metrics bytes.Buffer
	srv.stats.WritePrometheus(&metrics)
	if !strings.Contains(metrics.String(), `statute_requests_by_status_total{status="403"} 1`) {
		t.Fatal(metrics.String())
	}
}

func TestFallbackRoutesChallengeAndRedirectPrecedence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode                            string
		challengeStatus, ordinaryStatus int
		calls                           int64
	}{
		{"pinned", http.StatusOK, http.StatusTeapot, 1},
		{"automatic", http.StatusNotFound, http.StatusTeapot, 1},
		{"redirect", http.StatusOK, http.StatusMovedPermanently, 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			var native, legacy atomic.Int64
			path := "/.well-known/acme-challenge/pending"
			h := fallbackChallengeHandler(t, tc.mode, path, &native, &legacy)
			if rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "http://x.example"+path, nil)); rec.Code != tc.challengeStatus {
				t.Fatalf("challenge: %d want %d", rec.Code, tc.challengeStatus)
			}
			if native.Load() != 0 || legacy.Load() != 0 {
				t.Fatal("challenge reached terminal routing")
			}
			if rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "http://x.example/ordinary", nil)); rec.Code != tc.ordinaryStatus {
				t.Fatalf("ordinary: %d want %d", rec.Code, tc.ordinaryStatus)
			}
			if native.Load() != tc.calls || legacy.Load() != 0 {
				t.Fatalf("terminal calls = %d/%d, want %d/0", native.Load(), legacy.Load(), tc.calls)
			}
		})
	}
}

func fallbackChallengeHandler(t *testing.T, mode, path string, native, legacy *atomic.Int64) http.Handler {
	t.Helper()
	source := AutoTLS("x.example").Email("ops@example.com").Storage(t.TempDir())
	if mode != "automatic" {
		source = source.HTTP01()
	}
	plain := HTTP(":80")
	if mode == "redirect" {
		plain = plain.RedirectTo("https")
	}
	r := mustResolve(t, Config{
		Listeners:      Listeners{plain, HTTPS(":443", source)},
		FallbackRoutes: Routes{Match("/*").Handle(countingFallback(native))},
		Fallback:       countingFallback(legacy),
	})
	srv, err := newServer(r)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "automatic" {
		solver := srv.acmeManagers[r.Listeners[1].AutoTLSSources[0]].solver.(*http01Solver)
		solver.mu.Lock()
		solver.tokens[path] = "proof"
		solver.mu.Unlock()
	}
	return srv.buildListenerHandler(r.Listeners[0], srv.buildRouter(), nil)
}
