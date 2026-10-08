package statute

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func TestDockerRoutePolicyEffectiveChains(t *testing.T) {
	for _, tc := range []struct {
		name     string
		first    []Middleware
		second   []Middleware
		defaults []Middleware
		unknown  bool
		want     int
	}{
		{name: "registry aliases", first: []Middleware{Timeout("1m")}, second: []Middleware{Timeout("60s")}, want: 1},
		{name: "compression set", first: []Middleware{Compress(Brotli, Gzip, Gzip)}, second: []Middleware{Compress(Gzip, Brotli)}, want: 1},
		{name: "order matters", first: []Middleware{Timeout("1m"), RateLimit("1/h")}, second: []Middleware{RateLimit("1/h"), Timeout("1m")}},
		{name: "multiplicity matters", first: []Middleware{Timeout("1m")}, second: []Middleware{Timeout("1m"), Timeout("1m")}},
		{name: "unknown reference", unknown: true, want: 1},
		{name: "invalid cache ordering", defaults: []Middleware{Cache("1m")}, first: []Middleware{AllowIPs("127.0.0.1/32")}, second: []Middleware{AllowIPs("127.0.0.1/32")}},
		{name: "invalid compression metadata", defaults: []Middleware{SetResponseHeader("Content-Encoding", "gzip")}, first: []Middleware{Compress(Gzip)}, second: []Middleware{Compress(Gzip)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolve := func(mws []Middleware) []resolved.Middleware {
				out, err := resolveMiddlewares(mws)
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			p := &dockerProvider{cfg: &resolved.Docker{
				DefaultMiddleware: resolve(tc.defaults),
				Middleware: map[string][]resolved.Middleware{
					"first": resolve(tc.first), "second": resolve(tc.second),
				},
			}, warned: map[string]bool{}}
			if tc.unknown {
				delete(p.cfg.Middleware, "second")
			}
			first := docker.CompileNative("app.example.com", "/admin/*")
			second := first
			first.Middlewares, second.Middlewares = []string{"first"}, []string{"second"}
			tab := &dynamicTable{}
			chains, _ := p.routeChains(&docker.Service{Name: "shared", Routes: []docker.Matcher{first, second}}, tab)
			if len(chains) != tc.want {
				t.Fatalf("chains=%d want=%d", len(chains), tc.want)
			}
			refused := tc.want == 0 || tc.unknown
			if (len(tab.rejections) > 0) != refused {
				t.Fatalf("refusals=%d want refusal=%v", len(tab.rejections), refused)
			}
		})
	}
}

func TestDockerRoutePolicyRevisionUsesEffectiveSemantics(t *testing.T) {
	m := docker.CompileNative("app.example.com", "/admin/*")
	m.Middlewares = []string{"first"}
	m.Hints = docker.MiddlewareHints{Timeout: "1m", Compress: "br,gzip,gzip"}
	mws := []resolved.Middleware{
		{Type: resolved.MWTimeout, Timeout: time.Minute},
		{Type: resolved.MWCompress, CompressAlgos: []resolved.CompressAlgo{resolved.Brotli, resolved.Gzip, resolved.Gzip}},
	}
	original := []routeChain{{m: m, mws: mws}}
	before := fingerprintWorkloadRoutes(original)
	m.Middlewares = []string{"alias"}
	m.Hints = docker.MiddlewareHints{Timeout: "60s", Compress: "true"}
	equivalent := []routeChain{{m: m, mws: canonicalDockerMiddleware(mws)}}
	if got := fingerprintWorkloadRoutes(equivalent); got != before {
		t.Fatal("equivalent policy spelling changed the workload revision")
	}
	if !reflect.DeepEqual(mws[1].CompressAlgos, []resolved.CompressAlgo{resolved.Brotli, resolved.Gzip, resolved.Gzip}) {
		t.Fatal("canonicalization mutated the registered chain")
	}
	equivalent[0].mws[0].Timeout = 2 * time.Minute
	if fingerprintWorkloadRoutes(equivalent) == before {
		t.Fatal("effective policy change retained the workload revision")
	}
}

func TestDockerNativeHintRevisionFencesQueuedRequest(t *testing.T) {
	backend := httptest.NewServer(noContentHandler)
	t.Cleanup(backend.Close)
	host, port := backendHostPort(t, backend)
	labels := func(timeout string) map[string]string {
		return map[string]string{"statute.enable": "true", "statute.host": "wl.example.com", "statute.timeout": timeout}
	}
	policy := testWorkloadPolicy()
	policy.Readiness.Mode = resolved.ReadinessDockerHealth
	p, srv, daemon := newFakeProviderDaemon(t, &resolved.Docker{Workloads: map[string]resolved.Workload{"wl": policy}}, []fakeDaemonContainer{
		{name: "wl", ip: host, port: port, stopped: true, health: "starting", labels: labels("1m")},
	})
	run, err := p.start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.stop)
	old := srv.dynamic.Load()
	req := httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)
	handler := findHandler(old.routes, req.Host, req)
	if handler == nil {
		t.Fatal("missing dormant route")
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		done <- rec
	}()
	waitWorkloadServicePhase(t, p, "wl", workloadStarting)
	for _, timeout := range []string{"60s", "2m"} {
		daemon.mu.Lock()
		c := daemon.containers[0]
		c.labels = labels(timeout)
		daemon.mu.Unlock()
		daemon.swap([]fakeDaemonContainer{c})
		mustSync(t, p)
		next := srv.dynamic.Load()
		if old.workloadBindings["wl"] != next.workloadBindings["wl"] {
			t.Fatal("hint change replaced immutable workload binding")
		}
		if same := old.workloadRevisions["wl"] == next.workloadRevisions["wl"]; same != (timeout == "60s") {
			t.Fatalf("timeout=%s revision unchanged=%v", timeout, same)
		}
	}
	daemon.mu.Lock()
	daemon.find("wl").health = "healthy"
	daemon.mu.Unlock()
	select {
	case rec := <-done:
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("stale queued request=%d want=503", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued request did not finish")
	}
	waitWorkloadServicePhase(t, p, "wl", workloadReady)
	if got := runRequest(t, srv.buildRouter(), httptest.NewRequest(http.MethodGet, "http://wl.example.com/", nil)); got.Code != http.StatusNoContent {
		t.Fatalf("current policy request=%d", got.Code)
	}
}
