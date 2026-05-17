package statute

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestRateLimit_AllowsBurstThenBlocks(t *testing.T) {
	t.Parallel()
	// 10 rps → burst = 20. A burst of 20 in rapid succession should all pass.
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	m := resolved.Middleware{Type: resolved.MWRateLimit, RateLimitPerSecond: 10, RateLimitKey: resolved.KeyClientIP}
	h := rateLimitHandler(m, inner)

	var blocked atomic.Int64
	for range 30 {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		rec := runRequest(t, h, req)
		if rec.Code == http.StatusTooManyRequests {
			blocked.Add(1)
		}
	}
	if blocked.Load() == 0 {
		t.Errorf("no requests blocked at 30 requests through a 20-burst bucket")
	}
}

func TestRateLimit_KeyIsolatesClients(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	m := resolved.Middleware{Type: resolved.MWRateLimit, RateLimitPerSecond: 1, RateLimitKey: resolved.KeyClientIP}
	h := rateLimitHandler(m, inner)

	// Client A exhausts its bucket.
	for range 5 {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "1.2.3.4:1"
		runRequest(t, h, req)
	}

	// Client B with a fresh bucket gets its first request through.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "5.6.7.8:1"
	rec := runRequest(t, h, req)
	if rec.Code != http.StatusOK {
		t.Errorf("isolated client got %d, want 200", rec.Code)
	}
}

func TestRateLimit_ZeroRateIsNoOp(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	m := resolved.Middleware{Type: resolved.MWRateLimit, RateLimitPerSecond: 0}
	h := rateLimitHandler(m, inner)
	for range 100 {
		rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatal("zero rate must pass everything")
		}
	}
}
