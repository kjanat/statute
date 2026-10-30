package statute

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestBufferBodiesBudgetResolvesAndValidates(t *testing.T) {
	c, err := resolveRequestBufferResources(RequestBufferResources{})
	if err != nil || c.BufferBodiesBudgetBytes != 1<<30 {
		t.Fatalf("default=%+v err=%v", c, err)
	}
	c, err = resolveRequestBufferResources(RequestBufferResources{BufferBodiesBudget: "32MiB"})
	if err != nil || c.BufferBodiesBudgetBytes != 32<<20 {
		t.Fatalf("explicit=%+v err=%v", c, err)
	}
	for _, budget := range []string{"0", "-1", "bad"} {
		if _, err := Resolve(Config{RequestBuffers: RequestBufferResources{BufferBodiesBudget: budget}}); err == nil {
			t.Errorf("accepted budget %q", budget)
		}
	}
	if got := bodyBufferReservation(BodyBufferPolicy{MaxRequestBytes: math.MaxInt64, MaxResponseBytes: math.MaxInt64}); got != math.MaxInt64 {
		t.Fatalf("reservation overflowed: %d", got)
	}
	if got := bodyBufferReservation(BodyBufferPolicy{MaxRequestBytes: 3, MaxResponseBytes: 5, DisableResponse: true}); got != 3 {
		t.Fatalf("disabled response reserved: %d", got)
	}
}

func TestBufferBodiesBudgetTooling(t *testing.T) {
	large := BufferBodies(BodyBufferPolicy{MaxRequestBytes: 2 << 20, MaxResponseBytes: 2 << 20})
	small := BufferBodies(BodyBufferPolicy{MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20})
	cfg := Config{
		Listeners:      Listeners{HTTP(":8080")},
		RequestBuffers: RequestBufferResources{BufferBodiesBudget: "3MiB"},
		Routes:         Routes{Match("/large").Handle(http.NotFoundHandler()).With(large), Match("/small").Handle(http.NotFoundHandler()).With(small)},
		FallbackRoutes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(large)},
		Docker:         Docker().Middleware("large", large).Middleware("small", small),
	}
	var out bytes.Buffer
	if err := Export(cfg, &out); err != nil {
		t.Fatal(err)
	}
	var r resolved.Config
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.RequestBuffers.BufferBodiesBudgetBytes != 3<<20 {
		t.Fatalf("exported resources=%+v err=%v", r.RequestBuffers, err)
	}
	out.Reset()
	if err := GraphDOT(cfg, &out); err != nil || !strings.Contains(out.String(), "buffer_bodies=3145728") {
		t.Fatalf("graph omitted budget: %v", err)
	}
	findings, err := Lint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range findings {
		if f.Code == "RBF002" {
			paths = append(paths, f.Path)
		}
	}
	if strings.Join(paths, ",") != `routes[0].middleware[0],fallback_routes[0].middleware[0],docker.middleware["large"].middleware[0]` {
		t.Fatalf("RBF002 paths=%v", paths)
	}
}

func TestBufferBodiesBudgetSharedAcrossRoutes(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	hold := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Hold") != "" {
			entered <- struct{}{}
			<-release
		}
		_, _ = w.Write([]byte("ok"))
	})
	mw := BufferBodies(BodyBufferPolicy{MaxRequestBytes: 8, MaxResponseBytes: 8})
	cfg := Config{
		Listeners:      Listeners{HTTP(":0")},
		RequestBuffers: RequestBufferResources{BufferBodiesBudget: "16B"},
		Routes:         Routes{Match("/static").Handle(hold).With(mw)},
		FallbackRoutes: Routes{Match("/*").Handle(hold).With(mw)},
	}
	s, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.requestBuffers.cancel)
	router := s.buildRouter()
	dynamic := mustResolve(t, Config{Listeners: Listeners{HTTP(":0")}, Routes: Routes{Match("/dynamic").Handle(hold).With(mw)}})
	s.dynamic.Store(&dynamicTable{routes: s.compileRoutes(dynamic.Routes)})
	for _, holder := range []string{"/static", "/fallback", "/dynamic"} {
		finished := assertBufferBodiesBudgetHeld(t, router, holder, entered)
		release <- struct{}{}
		<-finished
		assertBufferBodiesReleased(t, s.requestBuffers)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, holder, strings.NewReader("abc")))
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("budget not released after %s: %d", holder, rec.Code)
		}
	}
	s.requestBuffers.stopAdmission()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/static", strings.NewReader("abc")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("shutdown admitted buffered request: %d", rec.Code)
	}
	assertBufferBodiesReleased(t, s.requestBuffers)
}

func assertBufferBodiesBudgetHeld(t *testing.T, router http.Handler, holder string, entered <-chan struct{}) <-chan struct{} {
	t.Helper()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		r := httptest.NewRequest(http.MethodPost, holder, strings.NewReader("abc"))
		r.Header.Set("X-Hold", "1")
		router.ServeHTTP(httptest.NewRecorder(), r)
	}()
	<-entered
	for _, path := range []string{"/static", "/fallback", "/dynamic"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("abc")))
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" || rec.Body.Len() != 0 {
			t.Fatalf("holder=%s sibling=%s status=%d", holder, path, rec.Code)
		}
	}
	return finished
}

func assertBufferBodiesReleased(t *testing.T, m *requestBufferManager) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bodies != 0 {
		t.Fatalf("BufferBodies reservation leaked: %d", m.bodies)
	}
}
