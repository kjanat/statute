package statute

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjanat/statute/resolved"
)

func TestCacheHandler_HitMiss(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Source", "upstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})

	h := cacheHandler(resolved.Middleware{Type: resolved.MWCache, CacheTTL: time.Hour}, inner)

	// First call: miss → upstream is hit.
	rec := runRequest(t, h, httptest.NewRequest("GET", "/foo", nil))
	if rec.Body.String() != "hello" {
		t.Errorf("body: %q", rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Errorf("calls after miss: %d", calls.Load())
	}

	// Second call: hit → upstream not invoked.
	rec = runRequest(t, h, httptest.NewRequest("GET", "/foo", nil))
	if rec.Body.String() != "hello" {
		t.Errorf("cached body: %q", rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Errorf("calls after hit: %d (should still be 1)", calls.Load())
	}
}

func TestCacheHandler_OnlyCachesGetAndHead(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := cacheHandler(resolved.Middleware{Type: resolved.MWCache, CacheTTL: time.Hour}, inner)

	runRequest(t, h, httptest.NewRequest("POST", "/x", nil))
	runRequest(t, h, httptest.NewRequest("POST", "/x", nil))
	if calls.Load() != 2 {
		t.Errorf("POST should never cache; calls=%d", calls.Load())
	}
}

func TestCacheHandler_DoesNotCacheErrors(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	h := cacheHandler(resolved.Middleware{Type: resolved.MWCache, CacheTTL: time.Hour}, inner)

	runRequest(t, h, httptest.NewRequest("GET", "/x", nil))
	runRequest(t, h, httptest.NewRequest("GET", "/x", nil))
	if calls.Load() != 2 {
		t.Errorf("5xx response should not be cached; calls=%d", calls.Load())
	}
}

func TestCacheHandler_TTLExpiry(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := cacheHandler(resolved.Middleware{Type: resolved.MWCache, CacheTTL: 50 * time.Millisecond}, inner)

	runRequest(t, h, httptest.NewRequest("GET", "/x", nil))
	time.Sleep(80 * time.Millisecond)
	runRequest(t, h, httptest.NewRequest("GET", "/x", nil))
	if calls.Load() != 2 {
		t.Errorf("expired entry should refetch; calls=%d", calls.Load())
	}
}

func TestCacheHandler_ZeroTTLDisables(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := cacheHandler(resolved.Middleware{Type: resolved.MWCache, CacheTTL: 0}, inner)

	runRequest(t, h, httptest.NewRequest("GET", "/x", nil))
	runRequest(t, h, httptest.NewRequest("GET", "/x", nil))
	if calls.Load() != 2 {
		t.Errorf("zero TTL should disable caching; calls=%d", calls.Load())
	}
}
