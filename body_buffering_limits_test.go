package statute

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"statute.kjanat.dev/internal/docker"
	"statute.kjanat.dev/resolved"
)

func TestBodyBufferResolveLimits(t *testing.T) {
	assertBodyBufferLimits(t, mustResolveMW(t, BufferBodies(BodyBufferPolicy{})), BodyBufferPolicy{MaxRequestBytes: 8 << 20, MaxResponseBytes: 8 << 20, MaxInFlight: 128})
	explicit := BodyBufferPolicy{MaxRequestBytes: 1, MaxResponseBytes: 2, MaxInFlight: 3}
	assertBodyBufferLimits(t, mustResolveMW(t, BufferBodies(explicit)), explicit)
	for _, p := range []BodyBufferPolicy{{MaxInFlight: -1}, {MaxRequestBytes: -1}, {MaxResponseBytes: -1}} {
		if _, err := resolveMiddleware(BufferBodies(p)); err == nil {
			t.Fatalf("invalid limits accepted: %+v", p)
		}
	}
}

func TestBodyBufferExportAndDockerLimits(t *testing.T) {
	mw := BufferBodies(BodyBufferPolicy{MaxInFlight: 3})
	cfg := Config{Listeners: Listeners{HTTP(":8080")}, Routes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(mw)}, FallbackRoutes: Routes{Match("/*").Handle(http.NotFoundHandler()).With(mw)}}
	var encoded bytes.Buffer
	if err := Export(cfg, &encoded); err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Routes, FallbackRoutes []struct{ Middleware []resolved.Middleware }
	}
	if err := json.Unmarshal(encoded.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	dc, err := resolveDocker(Docker().Middleware("bounded", mw))
	if err != nil {
		t.Fatal(err)
	}
	p := &dockerProvider{cfg: dc}
	chain, warning := p.routeMiddleware(&docker.Service{Name: "app"}, docker.Matcher{Middlewares: []string{"bounded"}}, nil)
	if warning != "" || len(chain) != 1 {
		t.Fatalf("chain=%+v warning=%q", chain, warning)
	}
	for _, m := range []resolved.Middleware{exported.Routes[0].Middleware[0], exported.FallbackRoutes[0].Middleware[0], chain[0]} {
		assertBodyBufferLimits(t, m, BodyBufferPolicy{MaxRequestBytes: 8 << 20, MaxResponseBytes: 8 << 20, MaxInFlight: 3})
	}
}

func assertBodyBufferLimits(t *testing.T, m resolved.Middleware, want BodyBufferPolicy) {
	t.Helper()
	if b := m.BodyBuffers; b == nil || b.MaxRequestBytes != want.MaxRequestBytes || b.MaxResponseBytes != want.MaxResponseBytes || b.MaxInFlight != want.MaxInFlight {
		t.Fatalf("limits=%+v want=%+v", b, want)
	}
}

func TestBodyBufferDefaultRequestLimit(t *testing.T) {
	calls := 0
	h := chain(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }), BufferBodies(BodyBufferPolicy{}))
	r := httptest.NewRequest(http.MethodPost, "/", io.LimitReader(zeroReader{}, 8<<20+1))
	r.ContentLength = 8<<20 + 1
	if w := runRequest(t, h, r); w.Code != http.StatusRequestEntityTooLarge || calls != 0 {
		t.Fatalf("status=%d calls=%d", w.Code, calls)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestBodyBufferAdmissionReleasesSlots(t *testing.T) {
	m := mustResolveMW(t, BufferBodies(BodyBufferPolicy{MaxInFlight: 1}))
	entered, release := make(chan struct{}), make(chan struct{})
	h := bufferBodiesHandler(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/block":
			entered <- struct{}{}
			<-release
		case "/panic":
			panic(http.ErrAbortHandler)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	var wg sync.WaitGroup
	wg.Go(func() { runRequest(t, h, httptest.NewRequest(http.MethodGet, "/block", nil)) })
	<-entered
	rejected := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
	if rejected.Code != http.StatusServiceUnavailable || rejected.Header().Get("Cache-Control") != "no-store" || rejected.Body.Len() != 0 {
		t.Fatalf("exhausted admission: %d %v %q", rejected.Code, rejected.Header(), rejected.Body.String())
	}
	close(release)
	wg.Wait()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic did not propagate")
			}
		}()
		runRequest(t, h, httptest.NewRequest(http.MethodGet, "/panic", nil))
	}()
	if w := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil)); w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("slot not released: %d %q", w.Code, w.Body.String())
	}
}

func TestBodyBufferAdmissionIsPerInstanceAndBuffered(t *testing.T) {
	for _, tc := range []struct {
		policy BodyBufferPolicy
		shared bool
	}{
		{BodyBufferPolicy{MaxInFlight: 1}, false},
		{BodyBufferPolicy{MaxInFlight: 1, DisableRequest: true, DisableResponse: true}, true},
	} {
		m := mustResolveMW(t, BufferBodies(tc.policy))
		entered, release := make(chan struct{}), make(chan struct{})
		block := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/block" {
				entered <- struct{}{}
				<-release
			}
			_, _ = io.WriteString(w, "ok")
		})
		first, second := bufferBodiesHandler(m, block), bufferBodiesHandler(m, block)
		var wg sync.WaitGroup
		wg.Go(func() { runRequest(t, first, httptest.NewRequest(http.MethodGet, "/block", nil)) })
		<-entered
		if w := runRequest(t, second, httptest.NewRequest(http.MethodGet, "/", nil)); w.Code != http.StatusOK {
			t.Fatalf("separate instance shared admission: %d", w.Code)
		}
		w := runRequest(t, first, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body")))
		if (w.Code == http.StatusOK) != tc.shared {
			t.Fatalf("policy=%+v same-instance status=%d", tc.policy, w.Code)
		}
		close(release)
		wg.Wait()
	}
}
