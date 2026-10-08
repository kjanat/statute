package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

type retryClosingTrailerBody struct {
	trailers http.Header
	closed   atomic.Bool
	started  chan struct{}
	stop     chan struct{}
	done     chan struct{}
}

func (b *retryClosingTrailerBody) Read([]byte) (int, error) {
	return 0, http.ErrBodyReadAfterClose
}

func (b *retryClosingTrailerBody) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	defer close(b.done)
	close(b.started)
	for {
		select {
		case <-b.stop:
			return nil
		default:
			b.trailers.Set("X-Draining", "value")
			b.trailers.Del("X-Draining")
			runtime.Gosched()
		}
	}
}

func TestRetryRequestTrailersAfterOuterTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		enter := make(chan struct{})
		finish := make(chan struct{})
		retry := chain(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("closed body reached producer")
		}), Retry(2))
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(finish)
			<-enter
			retry.ServeHTTP(w, r)
		}), Timeout("1ms"))
		r := httptest.NewRequest(http.MethodPut, "/", nil)
		r.Trailer = http.Header{"X-Announced": nil}
		body := &retryClosingTrailerBody{trailers: r.Trailer, started: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
		r.Body = body
		response := runRequest(t, h, r)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("timeout status=%d", response.Code)
		}
		go func() { _ = body.Close() }()
		<-body.started
		close(enter)
		<-finish
		close(body.stop)
		<-body.done
	})
}

func TestRetryTrailerSnapshotEmptyAndCompleted(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/", nil)
	r = r.WithContext(retryTrailerContext(r.Context(), r.Trailer))
	r.Trailer = http.Header{"X-Late": {"final"}}
	if trailers := announcedRetryTrailers(retryTrailerContext(r.Context(), r.Trailer)); len(trailers) != 0 {
		t.Fatalf("empty snapshot re-read transport map: %v", trailers)
	}
	r.Body = &retryRequestBody{data: "p"}
	b := newRetryRequestBuffer(newResponseBufferBudget(1))
	defer b.release()
	forwarded, ok := bufferRetryBody(httptest.NewRecorder(), r, http.NotFoundHandler(), b)
	if !ok {
		t.Fatal("complete body was not buffered")
	}
	names := announcedRetryTrailers(retryTrailerContext(forwarded.Context(), forwarded.Trailer))
	if _, ok := names["X-Late"]; !ok || forwarded.Trailer.Get("X-Late") != "final" {
		t.Fatalf("EOF snapshot=%v values=%v", names, forwarded.Trailer)
	}
	if _, err := forwarded.Body.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("original completed reader error=%v", err)
	}
}
