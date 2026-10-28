package statute

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestBufferSharedAcrossRoutingGenerations(t *testing.T) {
	var retained io.ReadCloser
	hold := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		retained, err = r.GetBody()
		if err != nil {
			t.Fatal(err)
		}
	})
	cfg := Config{
		Listeners:      Listeners{HTTP(":0")},
		RequestBuffers: RequestBufferResources{MemoryBudget: "3B", Storage: t.TempDir(), DiskBudget: "5B"},
		Routes:         Routes{Match("/static").Handle(hold).With(RequestBuffer("5B").MemoryThreshold("2B"))},
		FallbackRoutes: Routes{Match("/*").Handle(hold).With(RequestBuffer("5B").MemoryThreshold("2B"))},
	}
	s, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.requestBuffers.cancel)
	router := s.buildRouter()
	dynamicConfig := mustResolve(t, Config{Listeners: Listeners{HTTP(":0")}, Routes: Routes{Match("/dynamic").Handle(hold).With(RequestBuffer("5B").MemoryThreshold("2B"))}})
	for _, first := range []string{"/static", "/fallback", "/dynamic"} {
		s.dynamic.Store(&dynamicTable{routes: s.compileRoutes(dynamicConfig.Routes)})
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", first, strings.NewReader("abc")))
		// A replacement borrows capacity still owned by the retired generation.
		s.dynamic.Store(&dynamicTable{routes: s.compileRoutes(dynamicConfig.Routes)})
		for _, path := range []string{"/static", "/fallback", "/dynamic"} {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader("abc")))
			if rec.Code != 503 {
				t.Fatalf("holder=%s sibling=%s status=%d", first, path, rec.Code)
			}
		}
		if retained == nil {
			t.Fatal("no retained reader")
		}
		_ = retained.Close()
		retained = nil
		assertBufferReleased(t, s.requestBuffers)
	}
}

func TestRequestBufferWireCancellation(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmtBufferCase(h2, 0, false), func(t *testing.T) {
			m := bufferTestManager(t, true, 16, 32)
			h := bufferTestHandler(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("incomplete request reached downstream") }), Timeout("30ms"), RequestBuffer("32B").MemoryThreshold("2B"))
			srv := httptest.NewUnstartedServer(h)
			srv.EnableHTTP2 = h2
			srv.StartTLS()
			defer srv.Close()
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			req, err := http.NewRequestWithContext(t.Context(), "POST", srv.URL, reader)
			if err != nil {
				t.Fatal(err)
			}
			sent := make(chan struct{})
			go func() { defer close(sent); _, _ = writer.Write([]byte("abc")) }()
			response, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != 503 {
				t.Fatalf("status=%d", response.StatusCode)
			}
			_ = writer.Close()
			<-sent
			awaitBufferRetirement(t, m)
		})
	}
}

func awaitBufferRetirement(t *testing.T, m *requestBufferManager) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		active := m.active
		m.mu.Unlock()
		if active == 0 {
			assertBufferReleased(t, m)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("buffer did not retire")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRequestBufferShutdownRetainsReader(t *testing.T) {
	readers := make(chan io.ReadCloser, 1)
	cfg := Config{
		Listeners:      Listeners{HTTP("127.0.0.1:0")},
		RequestBuffers: RequestBufferResources{MemoryBudget: "3B", Storage: t.TempDir(), DiskBudget: "5B"},
		Routes: Routes{Match("/*").Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := r.GetBody()
			readers <- body
			w.WriteHeader(204)
		})).With(RequestBuffer("5B").MemoryThreshold("2B"))},
		Shutdown: Shutdown{GracePeriod: "1s", DrainListeners: true},
	}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown() })
	addr := srv.run.listeners.http[0].listener.Addr().String()
	req, err := http.NewRequestWithContext(t.Context(), "POST", "http://"+addr, strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("status=%d", response.StatusCode)
	}
	if err := srv.Shutdown(); err != nil {
		t.Fatal(err)
	}
	retained := <-readers
	data, err := io.ReadAll(retained)
	if err != nil || string(data) != "abc" {
		t.Fatalf("retired read=%q error=%v", data, err)
	}
	_ = retained.Close()
	assertBufferReleased(t, srv.requestBuffers)
}

func TestRequestBufferFailedStartRetryServes(t *testing.T) {
	cfg := Config{Listeners: Listeners{HTTP("127.0.0.1:0")}, Routes: Routes{Match("/*").Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, r.Body) })).With(RequestBuffer("5B"))}}
	srv, err := newServer(mustResolve(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	srv.listenTCP = func(context.Context, string, string) (net.Listener, error) {
		return nil, errors.New("bind unavailable")
	}
	if err := srv.Start(); err == nil {
		t.Fatal("startup should fail")
	}
	if err := srv.Shutdown(); err != nil {
		t.Fatal(err)
	}
	srv.listenTCP = nil
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := srv.Shutdown(); err != nil {
			t.Error(err)
		}
	}()
	assertRequestBufferServerEcho(t, srv)
}

func assertRequestBufferServerEcho(t *testing.T, srv *server) {
	t.Helper()
	addr := srv.run.listeners.http[0].listener.Addr().String()
	req, err := http.NewRequestWithContext(t.Context(), "POST", "http://"+addr, strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(data) != "abc" {
		t.Fatalf("retry serving: status=%d body=%q err=%v", response.StatusCode, data, err)
	}
}
