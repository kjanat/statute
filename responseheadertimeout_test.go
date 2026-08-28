package statute

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func responseHeaderTimeoutConfig(backendURL, timeout string) Config {
	return Config{
		Listeners: Listeners{HTTP(":0")},
		Upstreams: Upstreams{
			"api": Pool{
				Backends:  []Backend{{Address: backendURL}},
				Transport: Transport{ResponseHeaderTimeout: timeout},
			},
		},
		Routes: Routes{Match("/*").ProxyTo("api")},
	}
}

// TestResponseHeaderTimeoutBoundsOnlyHeaderWait proves the transport rejects
// a backend that does not begin responding in time, while a response whose
// headers arrive promptly may stream its body beyond the same duration.
func TestResponseHeaderTimeoutBoundsOnlyHeaderWait(t *testing.T) {
	t.Run("delayed headers time out", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(150 * time.Millisecond)
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(backend.Close)

		rec := proxyThrough(t, responseHeaderTimeoutConfig(backend.URL, "40ms"))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status: got %d, want 502", rec.Code)
		}
	})

	t.Run("delayed body remains allowed", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write([]byte("complete"))
		}))
		t.Cleanup(backend.Close)

		rec := proxyThrough(t, responseHeaderTimeoutConfig(backend.URL, "40ms"))
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "complete" {
			t.Fatalf("response: got %d %q, want 200 complete", rec.Code, rec.Body.String())
		}
	})

	t.Run("retry gets a fresh header wait", func(t *testing.T) {
		var calls atomic.Int64
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				time.Sleep(150 * time.Millisecond)
			}
			_, _ = w.Write([]byte("ready"))
		}))
		t.Cleanup(backend.Close)

		cfg := responseHeaderTimeoutConfig(backend.URL, "40ms")
		cfg.Routes = Routes{
			Match("/*").ProxyTo("api").With(Retry(2, OnStatus(http.StatusBadGateway))),
		}
		rec := proxyThrough(t, cfg)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ready" {
			t.Fatalf("response: got %d %q, want 200 ready", rec.Code, rec.Body.String())
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("backend calls: got %d, want 2", got)
		}
	})
}
