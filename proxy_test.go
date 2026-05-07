package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kjanat/statute/resolved"
)

// TestEndToEndProxy spins up a tiny upstream and exercises the resolved
// pipeline end-to-end: pool handler picks the backend, the reverse proxy
// forwards the request, and X-Forwarded headers reach the upstream.
func TestEndToEndProxy(t *testing.T) {
	got := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r
		w.Header().Set("X-Upstream", "ok")
		_, _ = io.WriteString(w, "hello")
	}))
	defer upstream.Close()

	cfg := Config{
		Listeners: Listeners{HTTP(":0")}, // not actually started; we drive the handler directly
		Upstreams: Upstreams{
			"echo": Pool{
				Backends: []Backend{{Address: strings.TrimPrefix(upstream.URL, "http://")}},
				Strategy: RoundRobin,
			},
		},
		Routes: Routes{Match("/*").ProxyTo("echo")},
	}
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	srv, err := newServer(r)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer func() {
		for _, ph := range srv.pools {
			ph.shutdown()
		}
	}()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/anything", nil)
	req.RemoteAddr = "203.0.113.7:55555"
	srv.buildRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello" {
		t.Errorf("body: got %q", rec.Body.String())
	}
	if got := rec.Header().Get("X-Upstream"); got != "ok" {
		t.Errorf("upstream header missing: %q", got)
	}

	select {
	case ur := <-got:
		if ur.URL.Path != "/anything" {
			t.Errorf("upstream saw path %q", ur.URL.Path)
		}
		if !strings.Contains(ur.Header.Get("X-Forwarded-For"), "203.0.113.7") {
			t.Errorf("XFF not propagated: %q", ur.Header.Get("X-Forwarded-For"))
		}
	case <-time.After(time.Second):
		t.Fatal("upstream never received the request")
	}
}

func mkStates(addrs ...string) []*backendState {
	bs := make([]*backendState, len(addrs))
	for i, a := range addrs {
		bs[i] = &backendState{backend: &resolved.Backend{Address: a, Weight: 1}}
		bs[i].markHealthy(true)
	}
	return bs
}

// TestStrategyRoundRobin verifies the picker rotates through healthy backends.
func TestStrategyRoundRobin(t *testing.T) {
	bs := mkStates("a", "b", "c")
	p := newPicker(resolved.RoundRobin)
	counts := map[string]int{}
	for i := 0; i < 30; i++ {
		s := p.pick(bs, "")
		counts[s.backend.Address]++
	}
	for k, v := range counts {
		if v != 10 {
			t.Errorf("backend %s saw %d picks, want 10", k, v)
		}
	}
}

// TestLeastConnections verifies the picker steers traffic away from busy backends.
func TestLeastConnections(t *testing.T) {
	bs := mkStates("a", "b", "c")
	bs[0].inFlight.Add(5)
	bs[1].inFlight.Add(2)

	p := &leastConnPicker{}
	got := p.pick(bs, "")
	if got != bs[2] {
		t.Errorf("least-conn picked %q, want c", got.backend.Address)
	}
}

func TestIPHashStable(t *testing.T) {
	bs := mkStates("a", "b", "c")
	p := &ipHashPicker{}
	first := p.pick(bs, "203.0.113.5")
	for i := 0; i < 100; i++ {
		if p.pick(bs, "203.0.113.5") != first {
			t.Fatal("ip-hash unstable for same key")
		}
	}
}
