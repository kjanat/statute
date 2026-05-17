package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestRetry_SuccessOnFirstAttempt(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := retryHandler(resolved.Middleware{Type: resolved.MWRetry, RetryMax: 3, RetryOnStatuses: []int{502, 503, 504}}, inner)
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls: got %d, want 1", got)
	}
}

func TestRetry_RetriesOnConfiguredStatus(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	h := retryHandler(resolved.Middleware{Type: resolved.MWRetry, RetryMax: 3, RetryOnStatuses: []int{502}}, inner)
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("final status: %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("final body: %q", rec.Body.String())
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls: got %d, want 3", got)
	}
}

func TestRetry_GivesUpAfterMax(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	})
	h := retryHandler(resolved.Middleware{Type: resolved.MWRetry, RetryMax: 3, RetryOnStatuses: []int{502}}, inner)
	rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status: %d", rec.Code)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls: got %d, want 3", got)
	}
}

func TestRetry_SkipsForPOST(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	})
	h := retryHandler(resolved.Middleware{Type: resolved.MWRetry, RetryMax: 3, RetryOnStatuses: []int{502}}, inner)
	rec := runRequest(t, h, httptest.NewRequest("POST", "/", strings.NewReader("body")))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status: %d", rec.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("POST retries forbidden; calls=%d", got)
	}
}

func TestRetry_SkipsForGRPC(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	})
	h := retryHandler(resolved.Middleware{Type: resolved.MWRetry, RetryMax: 3, RetryOnStatuses: []int{502}}, inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Content-Type", "application/grpc+proto")
	runRequest(t, h, req)

	if got := calls.Load(); got != 1 {
		t.Errorf("gRPC retries forbidden; calls=%d", got)
	}
}

func TestRetry_BodyReplayedAcrossAttempts(t *testing.T) {
	t.Parallel()
	var seenBodies []string
	var mu atomicMu
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.do(func() { seenBodies = append(seenBodies, string(b)) })
		if len(seenBodies) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := retryHandler(resolved.Middleware{Type: resolved.MWRetry, RetryMax: 2, RetryOnStatuses: []int{503}}, inner)
	req := httptest.NewRequest("PUT", "/", strings.NewReader("hello"))
	rec := runRequest(t, h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	mu.do(func() {
		if len(seenBodies) != 2 {
			t.Errorf("body seen %d times", len(seenBodies))
		}
		for i, b := range seenBodies {
			if b != "hello" {
				t.Errorf("attempt %d body: %q", i, b)
			}
		}
	})
}

// atomicMu is a tiny mutex helper to avoid the sync.Mutex test-syntax weight.
type atomicMu struct{ ch chan struct{} }

func (m *atomicMu) do(f func()) {
	if m.ch == nil {
		m.ch = make(chan struct{}, 1)
	}
	m.ch <- struct{}{}
	defer func() { <-m.ch }()
	f()
}
