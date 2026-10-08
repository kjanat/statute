package statute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func interactionCache(size string) Middleware {
	return Cache("1h").MaxResponseBody(size).BufferBudget("2MiB").MaxEntries(4)
}

func TestCacheLimitStreamingFallback(t *testing.T) {
	for _, mode := range []string{"write", "reader-from", "flush", "budget"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			mw := interactionCache("16B")
			if mode == "flush" {
				mw = interactionCache("64B")
			}
			if mode == "budget" {
				mw = Cache("1h").MaxResponseBody("1B").BufferBudget("1B")
			}
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("X-Origin", "retained")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "prefix-")
				writeCacheInteractionTail(t, w, mode)
			}), mw)
			for range 2 {
				rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
				if rec.Code != http.StatusOK || rec.Body.String() != "prefix-0123456789-tail" || rec.Header().Get("X-Origin") != "retained" {
					t.Fatalf("stream status=%d body=%q headers=%v", rec.Code, rec.Body.String(), rec.Header())
				}
			}
			wantCalls := 2
			if mode == "flush" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("producer calls=%d want=%d", calls, wantCalls)
			}
		})
	}
}

func writeCacheInteractionTail(t *testing.T, w http.ResponseWriter, mode string) {
	t.Helper()
	if mode == "flush" {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}
	if mode == "reader-from" {
		rf, ok := w.(io.ReaderFrom)
		if !ok {
			t.Fatal("Cache writer lost ReaderFrom")
		}
		n, err := rf.ReadFrom(strings.NewReader("0123456789-tail"))
		if err != nil || n != 15 {
			t.Fatalf("ReadFrom=%d, %v", n, err)
		}
		return
	}
	_, _ = io.WriteString(w, "0123456789-tail")
}

func TestCacheLimitOverflowFlushProgress(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "head")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "tail")
	}), interactionCache("1B"))
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Flush failed to deliver headers while producer was blocked: %v", err)
	}
	defer resp.Body.Close()
	var prefix [4]byte
	if _, err := io.ReadFull(resp.Body, prefix[:]); err != nil || string(prefix[:]) != "head" {
		t.Fatalf("progress before producer completion: %q, %v", prefix, err)
	}
	unblock()
	tail, err := io.ReadAll(resp.Body)
	if err != nil || string(tail) != "tail" {
		t.Fatalf("tail=%q error=%v", tail, err)
	}
}

func TestCacheLimitTrailersOnWire(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		for _, late := range []bool{false, true} {
			t.Run(fmt.Sprintf("h2=%t/late=%t", h2, late), func(t *testing.T) {
				testCacheLimitTrailersOnWire(t, h2, late)
			})
		}
	}
}

func testCacheLimitTrailersOnWire(t *testing.T, h2, late bool) {
	t.Helper()
	var calls atomic.Int32
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		header := w.Header()
		header.Set("Content-Type", "text/plain")
		if !late {
			header.Set("Trailer", "X-Checksum")
		}
		_, _ = io.WriteString(w, "first")
		_, _ = io.WriteString(w, "second")
		name := "X-Checksum"
		if late {
			name = http.TrailerPrefix + name
		}
		header.Set(name, "complete")
	}), interactionCache("8B"))
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = h2
	srv.StartTLS()
	defer srv.Close()
	for range 2 {
		resp, err := srv.Client().Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		assertCacheLimitWireTrailers(t, resp, h2)
	}
	if calls.Load() != 2 {
		t.Fatalf("overflow response was stored: calls=%d", calls.Load())
	}
}

func assertCacheLimitWireTrailers(t *testing.T, resp *http.Response, h2 bool) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "firstsecond" {
		t.Fatalf("status=%d body=%q err=%v", resp.StatusCode, body, err)
	}
	if resp.Trailer.Get("X-Checksum") != "complete" || resp.Header.Get("X-Checksum") != "" {
		t.Fatalf("trailer lost or became an ordinary header: headers=%v trailers=%v", resp.Header, resp.Trailer)
	}
	if (resp.ProtoMajor == 2) != h2 {
		t.Fatalf("wrong test protocol: %s", resp.Proto)
	}
}

func TestCacheLimitRetryETagOrders(t *testing.T) {
	c, e, r := interactionCache("16B"), ETag(), Retry(2, OnStatus(503))
	for index, order := range [][]Middleware{{c, e, r}, {c, r, e}, {e, c, r}, {e, r, c}, {r, e, c}, {r, c, e}} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			calls := 0
			payload := strings.Repeat("x", 32)
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls%2 == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
				_, _ = io.WriteString(w, payload)
			}), order...)
			var etag string
			for range 2 {
				rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
				assertCacheLimitETagBody(t, rec, payload)
				if etag != "" && etag != rec.Header().Get("ETag") {
					t.Fatal("streaming fallback changed representation validator")
				}
				etag = rec.Header().Get("ETag")
			}
			if calls != 4 {
				t.Fatalf("overflow was cached or changed retry attempts: calls=%d", calls)
			}
		})
	}
}

func assertCacheLimitETagBody(t *testing.T, rec *httptest.ResponseRecorder, payload string) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Body.String() != payload || rec.Header().Get("ETag") == "" {
		t.Fatalf("status=%d body=%q headers=%v", rec.Code, rec.Body.String(), rec.Header())
	}
}

func TestCacheLimitRequestIDAndPrivacyProjection(t *testing.T) {
	for _, noCache := range []bool{false, true} {
		t.Run(fmt.Sprint(noCache), func(t *testing.T) {
			calls := 0
			mws := []Middleware{RequestID().From("X-Client-ID"), interactionCache("16B")}
			if noCache {
				mws = append(mws, SetResponseHeader("Cache-Control", "no-cache"))
			}
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("X-Request-ID", "origin")
				_, _ = io.WriteString(w, "ok")
			}), mws...)
			for _, id := range []string{"first", "second"} {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Header.Set("X-Client-ID", id)
				rec := runRequest(t, h, r)
				if rec.Body.String() != "ok" || rec.Header().Get("X-Request-ID") != id {
					t.Fatalf("id=%s body=%q headers=%v", id, rec.Body.String(), rec.Header())
				}
			}
			want := 1
			if noCache {
				want = 2
			}
			if calls != want {
				t.Fatalf("origin calls=%d want=%d", calls, want)
			}
		})
	}
}

func TestCacheLimitCORSVariants(t *testing.T) {
	for _, cacheOutside := range []bool{false, true} {
		t.Run(fmt.Sprint(cacheOutside), func(t *testing.T) {
			calls := 0
			mws := []Middleware{CORS().Origins("https://a.example", "https://b.example"), interactionCache("16B")}
			if cacheOutside {
				mws[0], mws[1] = mws[1], mws[0]
			}
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				_, _ = io.WriteString(w, "ok")
			}), mws...)
			for _, origin := range []string{"https://a.example", "https://b.example", "https://a.example"} {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Header.Set("Origin", origin)
				rec := runRequest(t, h, r)
				if rec.Header().Get("Access-Control-Allow-Origin") != origin || rec.Body.String() != "ok" {
					t.Fatalf("origin=%s body=%q headers=%v", origin, rec.Body.String(), rec.Header())
				}
			}
			if calls != 2 {
				t.Fatalf("variant calls=%d want=2", calls)
			}
		})
	}
}

func TestCacheLimitRoutePoolIsolation(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Length", "32")
				_, _ = io.WriteString(w, strings.Repeat("x", 32))
			}))
			t.Cleanup(origin.Close)
			routes := Routes{
				Match("/tight").ProxyTo("shared").With(interactionCache("16B")),
				Match("/roomy").ProxyTo("shared").With(interactionCache("64B")),
			}
			cfg := Config{Listeners: Listeners{HTTP(":0")}, Upstreams: Upstreams{"shared": Pool{Backends: []Backend{{Address: origin.URL}}}}, Routes: routes}
			if fallback {
				cfg.Routes, cfg.FallbackRoutes = nil, routes
			}
			h := fallbackRouter(t, cfg)
			assertCacheLimitRouteResponses(t, h, "", &calls, 3)
		})
	}
}

func assertCacheLimitRouteResponses(t *testing.T, h http.Handler, base string, calls *atomic.Int32, wantCalls int32) {
	t.Helper()
	for _, path := range []string{"/tight", "/roomy", "/tight", "/roomy"} {
		rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, base+path, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != strings.Repeat("x", 32) {
			t.Fatalf("path=%s status=%d body=%q", path, rec.Code, rec.Body.String())
		}
	}
	if calls.Load() != wantCalls {
		t.Fatalf("origin calls=%d want=%d", calls.Load(), wantCalls)
	}
}

func TestCacheLimitDockerGenerationIsolation(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Length", "32")
		_, _ = io.WriteString(w, strings.Repeat("x", 32))
	}))
	t.Cleanup(origin.Close)
	host, port := backendHostPort(t, origin)
	cfg, err := resolveDocker(Docker().TraefikLabels().Middleware("tight", interactionCache("16B")).Middleware("roomy", interactionCache("64B")))
	if err != nil {
		t.Fatal(err)
	}
	c := fakeDaemonContainer{name: "app", ip: host, port: port, labels: map[string]string{
		"traefik.enable":                                        "true",
		"traefik.http.routers.tight.rule":                       "Host(`app.example`) && Path(`/tight`)",
		"traefik.http.routers.tight.service":                    "shared",
		"traefik.http.routers.tight.middlewares":                "tight",
		"traefik.http.routers.roomy.rule":                       "Host(`app.example`) && Path(`/roomy`)",
		"traefik.http.routers.roomy.service":                    "shared",
		"traefik.http.routers.roomy.middlewares":                "roomy",
		"traefik.http.services.shared.loadbalancer.server.port": fmt.Sprint(port),
	}}
	p, srv, replace := newFakeProvider(t, cfg, []fakeDaemonContainer{c})
	mustSync(t, p)
	first := srv.dynamic.Load()
	if len(first.pools) != 1 {
		t.Fatalf("expected one shared pool, got %d", len(first.pools))
	}
	h := srv.buildRouter()
	assertCacheLimitRouteResponses(t, h, "http://app.example", &calls, 3)
	c.labels = maps.Clone(c.labels)
	c.labels["traefik.http.routers.roomy.middlewares"] = "tight"
	replace([]fakeDaemonContainer{c})
	mustSync(t, p)
	for name, pool := range first.pools {
		if srv.dynamic.Load().pools[name] != pool {
			t.Fatalf("changing route limits replaced shared pool %s", name)
		}
	}
	assertCacheLimitRouteResponses(t, h, "http://app.example", &calls, 7)
}

type cacheInteractionFailWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w *cacheInteractionFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestCacheLimitDownstreamError(t *testing.T) {
	wantErr := errors.New("downstream closed")
	var writeErr error
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, "first")
		_, writeErr = io.WriteString(w, "second")
	}), interactionCache("8B"))
	w := &cacheInteractionFailWriter{ResponseRecorder: httptest.NewRecorder(), err: wantErr}
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if !errors.Is(writeErr, wantErr) {
		t.Fatalf("producer write error=%v want=%v", writeErr, wantErr)
	}
	rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Body.String() != "firstsecond" || calls != 2 {
		t.Fatalf("failed stream was stored: body=%q calls=%d", rec.Body.String(), calls)
	}
}

func TestCacheLimitPanicDoesNotPopulate(t *testing.T) {
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, "partial")
		if calls == 1 {
			panic(http.ErrAbortHandler)
		}
	}), Cache("1h").MaxResponseBody("16B").BufferBudget("2MiB").MaxEntries(1))
	func() {
		defer func() {
			if got, ok := recover().(error); !ok || !errors.Is(got, http.ErrAbortHandler) {
				t.Fatalf("producer panic changed: %v", got)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	for range 2 {
		rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Body.String() != "partial" {
			t.Fatalf("body=%q", rec.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("panic retained admission or poisoned entry: calls=%d", calls)
	}
}
