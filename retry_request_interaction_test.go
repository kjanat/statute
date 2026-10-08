package statute

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// retryRequestBody reports EOF together with its final bytes, making tiny
// budgets exercise allocation ownership without requiring a speculative grow.
type retryRequestBody struct {
	data          string
	reads, closes int
}

func (b *retryRequestBody) Read(p []byte) (int, error) {
	b.reads++
	n := copy(p, b.data)
	b.data = b.data[n:]
	if b.data == "" {
		return n, io.EOF
	}
	return n, nil
}

func (b *retryRequestBody) Close() error {
	b.closes++
	return nil
}

func retryBodyRequest(path string, body io.ReadCloser) *http.Request {
	r := httptest.NewRequest(http.MethodPut, path, body)
	r.ContentLength = -1
	return r
}

func TestRetryRequestBudgetRetainedDuringFinalReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probe := &retryRequestBody{data: "p"}
		probeCalls := 0
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/probe" {
				probeCalls++
				if r.Body != probe || probe.reads != 0 {
					t.Errorf("exhausted budget replaced/read original body: identity=%v reads=%d", r.Body == probe, probe.reads)
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, "response")
		}), Retry(2, OnStatus(503)).RequestBufferBudget("1B"))
		slow := &slowBudgetWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ServeHTTP(slow, retryBodyRequest("/held", &retryRequestBody{data: "h"}))
		}()
		<-slow.entered
		rec := runRequest(t, h, retryBodyRequest("/probe", probe))
		close(slow.release)
		<-done
		if rec.Code != http.StatusServiceUnavailable || probeCalls != 1 {
			t.Errorf("fallback status=%d calls=%d, want 503 and one call", rec.Code, probeCalls)
		}
		if slow.Code != http.StatusOK || slow.Body.String() != "response" {
			t.Errorf("held response changed: status=%d body=%q", slow.Code, slow.Body.String())
		}
		if got := runRequest(t, h, retryBodyRequest("/released", &retryRequestBody{data: "r"})); got.Code != http.StatusOK {
			t.Errorf("released request status=%d", got.Code)
		}
	})
}

func TestRetryRequestBudgetGrowthFallbackPreservesBodyAndClose(t *testing.T) {
	original := &retryRequestBody{data: strings.Repeat("body", 1024)}
	want := original.data
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assertRetryRequestFallbackBody(t, original, r.Body, want)
		w.Header().Set("X-Producer", "preserved")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "producer response")
	}), Retry(3, OnStatus(503)).RequestBufferBudget("1B"))
	rec := runRequest(t, h, retryBodyRequest("/", original))
	if calls != 1 || original.closes != 1 || rec.Code != http.StatusServiceUnavailable || rec.Header().Get("X-Producer") != "preserved" || rec.Body.String() != "producer response" {
		t.Fatalf("calls=%d closes=%d status=%d headers=%v body=%q", calls, original.closes, rec.Code, rec.Header(), rec.Body.String())
	}
}

func assertRetryRequestFallbackBody(t *testing.T, original *retryRequestBody, body io.ReadCloser, want string) {
	t.Helper()
	if original.reads == 0 || original.data == "" || original.closes != 0 {
		t.Errorf("expected retained prefix and unread original: reads=%d remaining=%d closes=%d", original.reads, len(original.data), original.closes)
	}
	got, err := io.ReadAll(body)
	if err != nil || string(got) != want {
		t.Errorf("fallback lost request bytes: length=%d err=%v", len(got), err)
	}
	if err := body.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

type retryBlockedEmptyBody struct {
	entered, release chan struct{}
	closes           int
}

func (b *retryBlockedEmptyBody) Read([]byte) (int, error) {
	close(b.entered)
	<-b.release
	return 0, io.EOF
}

func (b *retryBlockedEmptyBody) Close() error {
	b.closes++
	return nil
}

func TestRetryRequestBudgetReservesBeforeEmptyRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		empty := &retryBlockedEmptyBody{entered: make(chan struct{}), release: make(chan struct{})}
		probe := &retryRequestBody{data: "p"}
		calls := 0
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/probe" {
				calls++
				if r.Body != probe || probe.reads != 0 {
					t.Errorf("blocked empty reader failed to reserve capacity: identity=%v reads=%d", r.Body == probe, probe.reads)
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}), Retry(2, OnStatus(503)).RequestBufferBudget("1B"))
		done := make(chan struct{})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			defer close(done)
			h.ServeHTTP(httptest.NewRecorder(), retryBodyRequest("/empty", empty).WithContext(ctx))
		}()
		<-empty.entered
		cancel()
		synctest.Wait()
		rec := runRequest(t, h, retryBodyRequest("/probe", probe))
		close(empty.release)
		<-done
		if calls != 1 || rec.Code != http.StatusServiceUnavailable || empty.closes != 1 {
			t.Fatalf("probe calls=%d status=%d empty closes=%d", calls, rec.Code, empty.closes)
		}
	})
}

func TestRetryRequestBudgetRetainedThroughLaterAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		probe := &retryRequestBody{data: "p"}
		heldCalls, probeCalls := 0, 0
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/probe" {
				probeCalls++
				if r.Body != probe || probe.reads != 0 {
					t.Errorf("later attempt released request allocation: identity=%v reads=%d", r.Body == probe, probe.reads)
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			heldCalls++
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "h" {
				t.Errorf("attempt %d body=%q err=%v", heldCalls, body, err)
			}
			if heldCalls == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			close(entered)
			<-release
		}), Retry(2, OnStatus(503)).RequestBufferBudget("1B"))
		go func() {
			defer close(done)
			h.ServeHTTP(httptest.NewRecorder(), retryBodyRequest("/held", &retryRequestBody{data: "h"}))
		}()
		<-entered
		rec := runRequest(t, h, retryBodyRequest("/probe", probe))
		close(release)
		<-done
		if heldCalls != 2 || probeCalls != 1 || rec.Code != http.StatusServiceUnavailable {
			t.Errorf("held calls=%d probe calls=%d status=%d", heldCalls, probeCalls, rec.Code)
		}
	})
}

func TestRetryRequestBudgetRetainedByTimedOutProducers(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		mws                        []Middleware
		attempts, releasedAttempts int
		cancelled                  bool
	}{
		{"single", []Middleware{Retry(1).RequestBufferBudget("1B"), Timeout("1ms")}, 1, 1, false},
		{"multiple-attempts", []Middleware{Retry(2, OnStatus(503)).RequestBufferBudget("1B"), Timeout("1ms")}, 2, 2, false},
		// The inner Retry retains its allocation after the first two timeouts,
		// so the second outer attempt must use its one-shot fallback.
		{"nested-retry", []Middleware{Retry(2, OnStatus(503)).RequestBufferBudget("1B"), Retry(2, OnStatus(503)).RequestBufferBudget("2B"), Timeout("1ms")}, 3, 4, false},
		{"nested-timeout", []Middleware{Retry(1).RequestBufferBudget("1B"), Timeout("2ms"), Retry(1).RequestBufferBudget("2B"), Timeout("1ms")}, 1, 1, false},
		{"already-cancelled", []Middleware{Retry(1).RequestBufferBudget("1B"), Timeout("1ms")}, 1, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				releases := make([]chan struct{}, tc.attempts)
				for i := range releases {
					releases[i] = make(chan struct{})
				}
				defer releaseRetryProducers(releases)
				var started, finished, probeCalls atomic.Int32
				var probe *retryRequestBody
				expectFallback := true
				producer := retryLateProducer(t, releases, &started, &finished, &probeCalls, func() (*retryRequestBody, bool) {
					return probe, expectFallback
				})
				h := chain(t, producer, tc.mws...)
				r := retryBodyRequest("/held", &retryRequestBody{data: "h"})
				if tc.cancelled {
					ctx, cancel := context.WithCancel(r.Context())
					cancel()
					r = r.WithContext(ctx)
				}
				rec := runRequest(t, h, r)
				synctest.Wait()
				assertRetryProducerCounts(t, rec.Code, int(started.Load()), int(finished.Load()), tc.attempts, 0)
				// Retire later outer attempts first: in the nested case the
				// original inner allocation must remain occupied for each probe.
				for i := range slices.Backward(releases) {
					probe = &retryRequestBody{data: "p"}
					probeCalls.Store(0)
					rec = runRequest(t, h, retryBodyRequest("/probe", probe))
					if rec.Code != http.StatusServiceUnavailable || probeCalls.Load() != 1 {
						t.Errorf("%d producers still live: probe status=%d calls=%d", i+1, rec.Code, probeCalls.Load())
					}
					close(releases[i])
					synctest.Wait()
				}
				expectFallback = false
				probe = &retryRequestBody{data: "p"}
				probeCalls.Store(0)
				rec = runRequest(t, h, retryBodyRequest("/released", probe))
				assertRetryProducerCounts(t, rec.Code, int(probeCalls.Load()), int(finished.Load()), tc.releasedAttempts, tc.attempts)
			})
		})
	}
}

func assertRetryProducerCounts(t *testing.T, status, calls, finished, wantCalls, wantFinished int) {
	t.Helper()
	if status != http.StatusServiceUnavailable || calls != wantCalls || finished != wantFinished {
		t.Fatalf("status=%d calls=%d (want %d) finished=%d (want %d)", status, calls, wantCalls, finished, wantFinished)
	}
}

func retryLateProducer(t *testing.T, releases []chan struct{}, started, finished, probeCalls *atomic.Int32, probeState func() (*retryRequestBody, bool)) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/held" {
			probeCalls.Add(1)
			probe, fallback := probeState()
			assertRetryProbeBody(t, r.Body, probe, fallback)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		i := int(started.Add(1)) - 1
		defer finished.Add(1)
		if i >= len(releases) {
			t.Errorf("unexpected extra producer %d", i)
			return
		}
		<-releases[i] // Deliberately ignore the Timeout's cancellation.
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "h" {
			t.Errorf("late producer %d body=%q err=%v", i, body, err)
		}
	})
}

func releaseRetryProducers(releases []chan struct{}) {
	for _, release := range releases {
		select {
		case <-release:
		default:
			close(release)
		}
	}
	synctest.Wait()
}

func assertRetryProbeBody(t *testing.T, body io.ReadCloser, probe *retryRequestBody, fallback bool) {
	t.Helper()
	if fallback {
		if body != probe || probe.reads != 0 {
			t.Errorf("live timed-out producer lost allocation: identity=%v reads=%d", body == probe, probe.reads)
		}
		return
	}
	if body == probe || probe.reads == 0 {
		t.Errorf("completed producers leaked capacity: identity=%v reads=%d", body == probe, probe.reads)
	}
	got, err := io.ReadAll(body)
	if err != nil || string(got) != "p" {
		t.Errorf("released probe body=%q err=%v", got, err)
	}
}

func TestRetryRequestBudgetPreservesResponseLimitPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, limit, budget string
		wantStatus          int
	}{
		{"independent-capacity", "4B", "4B", http.StatusOK},
		{"response-limit", "3B", "4B", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "r" {
					t.Errorf("request body=%q err=%v", body, err)
				}
				_, _ = io.WriteString(w, "body")
			}), Retry(3, OnStatus(502, 503)).RequestBufferBudget("1B").MaxResponseBody(tc.limit).BufferBudget(tc.budget))
			for range 2 {
				rec := runRequest(t, h, retryBodyRequest("/", &retryRequestBody{data: "r"}))
				if rec.Code != tc.wantStatus {
					t.Errorf("status=%d want=%d", rec.Code, tc.wantStatus)
				}
				if tc.wantStatus == http.StatusOK && rec.Body.String() != "body" {
					t.Errorf("body=%q", rec.Body.String())
				}
			}
			if calls != 2 {
				t.Errorf("own response failure retried: calls=%d", calls)
			}
		})
	}
}

func TestRetryRequestBudgetCompositions(t *testing.T) {
	for name, mws := range map[string][]Middleware{
		"cache-retry": {RequestID(), Cache("1m"), Retry(2, OnStatus(503)).RequestBufferBudget("1B")},
		"retry-cache": {RequestID(), Retry(2, OnStatus(503)).RequestBufferBudget("1B"), Cache("1m")},
		"etag-retry":  {RequestID(), ETag(), Retry(2, OnStatus(503)).RequestBufferBudget("1B")},
		"retry-etag":  {RequestID(), Retry(2, OnStatus(503)).RequestBufferBudget("1B"), ETag()},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			requestID := ""
			h := chain(t, retryCompositionProducer(t, &calls, &requestID), mws...)
			for range 2 {
				r := retryBodyRequest("/", &retryRequestBody{data: "r"})
				r.Method = http.MethodGet
				r.Header.Set("X-Request-ID", "request-buffer-test")
				rec := runRequest(t, h, r)
				if rec.Code != http.StatusOK || rec.Body.String() != "body" || rec.Header().Get("X-Request-ID") != requestID {
					t.Errorf("status=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
				}
			}
			if calls != 4 {
				t.Errorf("body-bearing requests were cached or not retried: calls=%d", calls)
			}
		})
	}
}

func retryCompositionProducer(t *testing.T, calls *int, requestID *string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "r" {
			t.Errorf("attempt=%d body=%q err=%v", *calls, body, err)
		}
		if *calls%2 == 1 {
			*requestID = r.Header.Get("X-Request-ID")
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		if *requestID == "" || r.Header.Get("X-Request-ID") != *requestID {
			t.Errorf("request ID changed across re-entry: %q", r.Header.Get("X-Request-ID"))
		}
		_, _ = io.WriteString(w, "body")
	})
}

func TestRetryRequestBudgetReleasedAfterPanic(t *testing.T) {
	for name, mws := range map[string][]Middleware{
		"direct":  {Retry(1).RequestBufferBudget("1B")},
		"timeout": {Retry(1).RequestBufferBudget("1B"), Timeout("1s")},
	} {
		t.Run(name, func(t *testing.T) { assertRetryBudgetReleasedAfterPanic(t, mws) })
	}
}

func assertRetryBudgetReleasedAfterPanic(t *testing.T, mws []Middleware) {
	t.Helper()
	h := chain(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panic" {
			panic("producer panic")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "r" {
			t.Errorf("body=%q err=%v", body, err)
		}
	}), mws...)
	func() {
		defer func() {
			if value := recover(); value != "producer panic" {
				t.Errorf("panic changed: %v", value)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), retryBodyRequest("/panic", &retryRequestBody{data: "p"}))
	}()
	original := &retryRequestBody{data: "r"}
	rec := runRequest(t, h, retryBodyRequest("/released", original))
	if rec.Code != http.StatusOK || original.closes != 1 {
		t.Errorf("panic leaked request budget: status=%d original closes=%d", rec.Code, original.closes)
	}
}
