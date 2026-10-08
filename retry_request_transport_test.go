package statute

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"statute.kjanat.dev/resolved"
)

type retryLeaseRoundTrip func(*http.Request) (*http.Response, error)

func (f retryLeaseRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRetryRequestTransportRetainsEarlyResponseBody(t *testing.T) {
	var transportBody io.ReadCloser
	var budget *responseBufferBudget
	base := retryLeaseRoundTrip(func(r *http.Request) (*http.Response, error) {
		transportBody = r.Body
		budget = retryRequestBuffers(r.Context()).buffer.budget
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})
	proxy := &httputil.ReverseProxy{
		Rewrite:   func(*httputil.ProxyRequest) {},
		Transport: retryRequestLeaseTransport(base),
	}
	h := retryHandler(resolved.Middleware{RetryMax: 1, RequestBufferBudgetBytes: 512}, proxy)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/", bytes.NewBufferString("body")))
	if transportBody == nil || budget == nil {
		t.Fatal("transport did not receive request body")
	}
	defer transportBody.Close()
	if got := retryRequestBudgetUsage(budget); got != initialRetryRequestBufferBytes {
		t.Fatalf("transport body released at ServeHTTP return: charged=%d want=%d", got, initialRetryRequestBufferBytes)
	}
	data, err := io.ReadAll(transportBody)
	if err != nil || string(data) != "body" {
		t.Fatalf("late read=%q,%v", data, err)
	}
	if err := transportBody.Close(); err != nil {
		t.Fatal(err)
	}
	if got := retryRequestBudgetUsage(budget); got != 0 {
		t.Fatalf("transport close leaked charge=%d", got)
	}
}

type retryTransportTestLease struct {
	buffer *retryRequestBuffer
	budget *responseBufferBudget
	once   sync.Once
}

func newRetryTransportTestLease(t *testing.T, body io.ReadCloser) (*http.Request, *retryTransportTestLease) {
	t.Helper()
	budget := newResponseBufferBudget(1024)
	lease := &retryTransportTestLease{budget: budget, buffer: newRetryRequestBuffer(budget)}
	t.Cleanup(lease.release)
	r := httptest.NewRequest(http.MethodPut, "/", nil)
	r.Body = body
	return r.WithContext(withRetryRequestBuffer(r.Context(), lease.buffer)), lease
}

func (l *retryTransportTestLease) release() { l.once.Do(l.buffer.release) }

func assertRetryTransportCharge(t *testing.T, l *retryTransportTestLease, want int64) {
	t.Helper()
	if got := retryRequestBudgetUsage(l.budget); got != want {
		t.Fatalf("charge=%d want=%d", got, want)
	}
}

type retryTransportTestBody struct {
	read          func([]byte) (int, error)
	close         func() error
	reads, closes atomic.Int32
}

func (b *retryTransportTestBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	if b.read != nil {
		return b.read(p)
	}
	return 0, io.EOF
}

func (b *retryTransportTestBody) Close() error {
	b.closes.Add(1)
	if b.close != nil {
		return b.close()
	}
	return nil
}

func captureRetryTransportBody(t *testing.T, r *http.Request) io.ReadCloser {
	t.Helper()
	var captured io.ReadCloser
	transport := retryRequestLeaseTransport(retryLeaseRoundTrip(func(forwarded *http.Request) (*http.Response, error) {
		if forwarded == r || forwarded.Body == r.Body {
			t.Error("transport did not privately wrap request body")
		}
		captured = forwarded.Body
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	}))
	if _, err := transport.RoundTrip(r); err != nil {
		t.Fatal(err)
	}
	if _, bypass := captured.(io.WriterTo); bypass {
		t.Fatal("WriterTo bypasses active Read accounting")
	}
	return captured
}

func TestRetryRequestTransportCloseWaitsForActiveRead(t *testing.T) {
	entered, release, readDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	body := &retryTransportTestBody{read: func([]byte) (int, error) { close(entered); <-release; return 0, io.EOF }}
	r, lease := newRetryTransportTestLease(t, body)
	wrapped := captureRetryTransportBody(t, r)
	lease.release()
	go func() { _, _ = wrapped.Read(make([]byte, 1)); close(readDone) }()
	t.Cleanup(func() { unblock.Do(func() { close(release) }); <-readDone; _ = wrapped.Close() })
	<-entered
	closeDone := make(chan struct{})
	go func() { _ = wrapped.Close(); close(closeDone) }()
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked behind Read")
	}
	assertRetryTransportCharge(t, lease, 512)
	assertRetryTransportClosed(t, wrapped, body)
	unblock.Do(func() { close(release) })
	<-readDone
	assertRetryTransportCharge(t, lease, 0)
}

func assertRetryTransportClosed(t *testing.T, wrapped io.ReadCloser, original *retryTransportTestBody) {
	t.Helper()
	reads := original.reads.Load()
	if _, err := wrapped.Read(make([]byte, 1)); !errors.Is(err, http.ErrBodyReadAfterClose) {
		t.Errorf("closed Read error=%v", err)
	}
	if err := wrapped.Close(); err != nil {
		t.Error(err)
	}
	if original.closes.Load() != 1 || original.reads.Load() != reads {
		t.Errorf("underlying closes=%d reads=%d want=%d", original.closes.Load(), original.reads.Load(), reads)
	}
}

func TestRetryRequestTransportRetainsUntilCloseFinishes(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	body := &retryTransportTestBody{close: func() error { close(entered); <-release; return nil }}
	r, lease := newRetryTransportTestLease(t, body)
	wrapped := captureRetryTransportBody(t, r)
	lease.release()
	go func() { _ = wrapped.Close(); close(done) }()
	t.Cleanup(func() { unblock.Do(func() { close(release) }); <-done })
	<-entered
	assertRetryTransportCharge(t, lease, 512)
	assertRetryTransportClosed(t, wrapped, body)
	unblock.Do(func() { close(release) })
	<-done
	assertRetryTransportCharge(t, lease, 0)
	if wrapped.(*retryTransportBody).reader != nil {
		t.Fatal("closed wrapper retains original reader")
	}
}

func TestRetryRequestTransportErrorAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		body := &retryTransportTestBody{}
		r, lease := newRetryTransportTestLease(t, body)
		ctx, cancel := context.WithCancel(r.Context())
		r = r.WithContext(ctx)
		want := errors.New("transport failed")
		var captured io.ReadCloser
		transport := retryRequestLeaseTransport(retryLeaseRoundTrip(func(r *http.Request) (*http.Response, error) {
			captured = r.Body
			if cancelled {
				cancel()
				return nil, ctx.Err()
			}
			return nil, want
		}))
		_, err := transport.RoundTrip(r)
		cancel()
		assertRetryTransportError(t, err, want, cancelled)
		lease.release()
		assertRetryTransportCharge(t, lease, 512)
		if err := captured.Close(); err != nil {
			t.Fatal(err)
		}
		assertRetryTransportCharge(t, lease, 0)
	}
}

func assertRetryTransportError(t *testing.T, got, want error, cancelled bool) {
	t.Helper()
	if cancelled {
		want = context.Canceled
	}
	if !errors.Is(got, want) {
		t.Fatalf("error=%v want=%v", got, want)
	}
}

func TestRetryRequestTransportGetBodyRewind(t *testing.T) {
	original, replacement := &retryTransportTestBody{}, &retryTransportTestBody{}
	r, lease := newRetryTransportTestLease(t, original)
	r.GetBody = func() (io.ReadCloser, error) {
		// Remove the Retry caller's lease during the gap between readers.
		lease.release()
		assertRetryTransportCharge(t, lease, 512)
		return replacement, nil
	}
	var captured io.ReadCloser
	transport := retryRequestLeaseTransport(retryLeaseRoundTrip(func(r *http.Request) (*http.Response, error) {
		if err := r.Body.Close(); err != nil {
			return nil, err
		}
		var err error
		captured, err = r.GetBody()
		return nil, err
	}))
	if _, err := transport.RoundTrip(r); err != nil {
		t.Fatal(err)
	}
	assertRetryTransportCharge(t, lease, 512)
	if err := captured.Close(); err != nil {
		t.Fatal(err)
	}
	assertRetryTransportCharge(t, lease, 0)
	if original.closes.Load() != 1 || replacement.closes.Load() != 1 {
		t.Fatal("rewind reader Close ownership changed")
	}
}

func TestRetryRequestTransportGetBodyError(t *testing.T) {
	body := &retryTransportTestBody{}
	r, lease := newRetryTransportTestLease(t, body)
	want := errors.New("factory failed")
	r.GetBody = func() (io.ReadCloser, error) { return nil, want }
	transport := retryRequestLeaseTransport(retryLeaseRoundTrip(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		result, err := r.GetBody()
		if result != nil {
			t.Error("factory failure acquired a body")
		}
		return nil, err
	}))
	if _, err := transport.RoundTrip(r); !errors.Is(err, want) {
		t.Fatalf("factory error=%v", err)
	}
	lease.release()
	assertRetryTransportCharge(t, lease, 0)
}

func TestRetryRequestTransportPanicCleanup(t *testing.T) {
	for _, kind := range []string{"roundtrip", "read", "close", "getbody", "cleanup-close"} {
		t.Run(kind, func(t *testing.T) { testRetryTransportPanic(t, kind) })
	}
}

func testRetryTransportPanic(t *testing.T, kind string) {
	t.Helper()
	const want = "original panic"
	body := &retryTransportTestBody{}
	if kind == "read" {
		body.read = func([]byte) (int, error) { panic(want) }
	}
	if kind == "close" {
		body.close = func() error { panic(want) }
	}
	if kind == "cleanup-close" {
		body.close = func() error { panic("cleanup panic") }
	}
	r, lease := newRetryTransportTestLease(t, body)
	r.GetBody = func() (io.ReadCloser, error) { panic(want) }
	transport := retryRequestLeaseTransport(retryLeaseRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch kind {
		case "read":
			_, _ = r.Body.Read(make([]byte, 1))
		case "close":
			_ = r.Body.Close()
		case "getbody":
			_, _ = r.GetBody()
		}
		panic(want)
	}))
	got := captureRetryTransportPanic(func() { _, _ = transport.RoundTrip(r) })
	if got != want {
		t.Fatalf("panic=%v want=%s", got, want)
	}
	lease.release()
	assertRetryTransportCharge(t, lease, 0)
	if body.closes.Load() != 1 {
		t.Fatalf("panic cleanup closes=%d", body.closes.Load())
	}
}

func captureRetryTransportPanic(fn func()) (value any) {
	defer func() { value = recover() }()
	fn()
	return nil
}

func TestRetryRequestTransportPanicClosesGetBodyReaders(t *testing.T) {
	original := &retryTransportTestBody{}
	r, lease := newRetryTransportTestLease(t, original)
	var readers []*retryTransportTestBody
	r.GetBody = func() (io.ReadCloser, error) {
		b := &retryTransportTestBody{}
		readers = append(readers, b)
		return b, nil
	}
	transport := retryRequestLeaseTransport(retryLeaseRoundTrip(func(r *http.Request) (*http.Response, error) {
		_, _ = r.GetBody()
		_, _ = r.GetBody()
		panic("transport panic")
	}))
	if got := captureRetryTransportPanic(func() { _, _ = transport.RoundTrip(r) }); got != "transport panic" {
		t.Fatal(got)
	}
	lease.release()
	assertRetryTransportCharge(t, lease, 0)
	readers = append(readers, original)
	for _, b := range readers {
		if b.closes.Load() != 1 {
			t.Fatalf("reader closes=%d", b.closes.Load())
		}
	}
}

func TestRetryRequestTransportPassThrough(t *testing.T) {
	for _, kind := range []string{"no-lease", "nil-body", "no-body"} {
		r := httptest.NewRequest(http.MethodPut, "/", bytes.NewBufferString("body"))
		if kind != "no-lease" {
			var lease *retryTransportTestLease
			r, lease = newRetryTransportTestLease(t, nil)
			defer lease.release()
			if kind == "no-body" {
				r.Body = http.NoBody
			}
		}
		transport := retryRequestLeaseTransport(retryLeaseRoundTrip(func(got *http.Request) (*http.Response, error) {
			if got != r {
				t.Error("pass-through request was cloned")
			}
			return nil, nil
		}))
		if _, err := transport.RoundTrip(r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetryRequestTransportPreservesNilGetBody(t *testing.T) {
	body := &retryTransportTestBody{}
	r, lease := newRetryTransportTestLease(t, body)
	transport := retryRequestLeaseTransport(retryLeaseRoundTrip(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		if r.GetBody != nil {
			t.Error("transport synthesized GetBody")
		}
		return nil, nil
	}))
	if _, err := transport.RoundTrip(r); err != nil {
		t.Fatal(err)
	}
	lease.release()
	assertRetryTransportCharge(t, lease, 0)
}

func TestRetryRequestTransportPreservesBodyErrors(t *testing.T) {
	readErr, closeErr := errors.New("read failed"), errors.New("close failed")
	body := &retryTransportTestBody{
		read:  func(p []byte) (int, error) { return copy(p, "x"), readErr },
		close: func() error { return closeErr },
	}
	r, lease := newRetryTransportTestLease(t, body)
	wrapped := captureRetryTransportBody(t, r)
	lease.release()
	var data [1]byte
	if n, err := wrapped.Read(data[:]); n != 1 || !errors.Is(err, readErr) {
		t.Fatalf("Read=%d,%v", n, err)
	}
	assertRetryTransportCharge(t, lease, 512)
	for range 2 {
		if err := wrapped.Close(); !errors.Is(err, closeErr) {
			t.Fatalf("Close=%v", err)
		}
	}
	if body.closes.Load() != 1 {
		t.Fatalf("underlying closes=%d", body.closes.Load())
	}
	assertRetryTransportCharge(t, lease, 0)
}
