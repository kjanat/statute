package statute

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"statute.kjanat.dev/resolved"
)

func TestTimeoutResponseBodyBoundary(t *testing.T) {
	for _, size := range []int{512, 1024} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			h := buildTimeout(resolved.Middleware{Timeout: time.Second, MaxResponseBodyBytes: 512, ResponseBufferBudgetBytes: 512}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Set-Cookie", "private=value")
				_, _ = w.Write(bytes.Repeat([]byte("x"), size))
			}))
			rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
			if size > 512 {
				assertRenderFailure(t, rec, http.StatusBadGateway)
			} else if rec.Code != http.StatusOK || rec.Body.Len() != size {
				t.Fatalf("exact boundary status=%d body=%d", rec.Code, rec.Body.Len())
			}
		})
	}
}

func timeoutTestHandler(limit, budget int64, count int, next http.Handler) *timeoutHandler {
	return newTimeoutHandler(resolved.Middleware{Timeout: time.Millisecond, MaxResponseBodyBytes: limit, ResponseBufferBudgetBytes: budget, TimeoutMaxInFlight: count}, next)
}

func TestTimeoutLateProducerRetainsResources(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		late := make(chan error, 1)
		h := timeoutTestHandler(512, 512, 2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), 512))
			if r.URL.Path == "/hold" {
				<-release
				w.Header().Set("Set-Cookie", "late=private")
				_, err := w.Write([]byte("late"))
				late <- err
			}
		}))
		rec := runRequest(t, h, httptest.NewRequest("GET", "/hold", nil))
		if rec.Code != 503 || rec.Body.String() != "request timed out" {
			t.Fatalf("timeout=%d %q", rec.Code, rec.Body.String())
		}
		synctest.Wait()
		if h.active.Load() != 1 || h.budget.used != 512 {
			t.Fatalf("late ownership active=%d budget=%d", h.active.Load(), h.budget.used)
		}
		assertRenderFailure(t, runRequest(t, h, httptest.NewRequest("GET", "/other", nil)), 503)
		close(release)
		synctest.Wait()
		if err := <-late; !errors.Is(err, http.ErrHandlerTimeout) {
			t.Fatalf("late write=%v", err)
		}
		if h.active.Load() != 0 || h.budget.used != 0 {
			t.Fatalf("leaked active=%d budget=%d", h.active.Load(), h.budget.used)
		}
		if rec := runRequest(t, h, httptest.NewRequest("GET", "/after", nil)); rec.Code != 200 {
			t.Fatalf("retired capacity not reusable: %d", rec.Code)
		}
	})
}

func TestTimeoutGrowthAccountsOverlappingAllocations(t *testing.T) {
	h := timeoutTestHandler(600, 600, 1, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 400))
		_, _ = w.Write(bytes.Repeat([]byte("x"), 200))
	}))
	h.duration = time.Second
	assertRenderFailure(t, runRequest(t, h, httptest.NewRequest("GET", "/", nil)), 503)
}

type timeoutSlowWriter struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
}

func (w *timeoutSlowWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseRecorder.Write(p)
}

func TestTimeoutBudgetRetainedDuringReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := timeoutTestHandler(512, 512, 1, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), 512))
		}))
		writer := &timeoutSlowWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ServeHTTP(writer, httptest.NewRequest("GET", "/", nil))
		}()
		<-writer.entered
		synctest.Wait()
		if h.active.Load() != 0 || h.budget.used != 512 {
			t.Fatalf("replay ownership active=%d body=%d", h.active.Load(), h.budget.used)
		}
		assertRenderFailure(t, runRequest(t, h, httptest.NewRequest("GET", "/", nil)), 503)
		close(writer.release)
		<-done
		synctest.Wait()
		if h.budget.used != 0 {
			t.Fatalf("replay budget leaked: %d", h.budget.used)
		}
	})
}

func TestTimeoutCancellationAndCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		entered, release := make(chan struct{}), make(chan struct{})
		late := make(chan error, 1)
		h := timeoutTestHandler(512, 512, 1, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(entered)
			<-release
			_, err := w.Write([]byte("late"))
			late <- err
		}))
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- runRequest(t, h, httptest.NewRequest("GET", "/", nil).WithContext(ctx)) }()
		<-entered
		cancel()
		rec := <-done
		if rec.Code != 503 || rec.Body.Len() != 0 {
			t.Fatalf("cancel response=%d %q", rec.Code, rec.Body.String())
		}
		assertRenderFailure(t, runRequest(t, h, httptest.NewRequest("GET", "/", nil)), 503)
		close(release)
		synctest.Wait()
		if err := <-late; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled write=%v", err)
		}
		if h.active.Load() != 0 {
			t.Fatalf("producer slot leaked: %d", h.active.Load())
		}
	})
}

func TestTimeoutDefaultProducerCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var entered atomic.Int32
		h := buildTimeout(resolved.Middleware{Timeout: time.Millisecond}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			entered.Add(1)
			<-release
		}))
		for range 129 {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}
		synctest.Wait()
		got := entered.Load()
		close(release)
		synctest.Wait()
		if got != 128 {
			t.Fatalf("live producers=%d want=128", got)
		}
	})
}
