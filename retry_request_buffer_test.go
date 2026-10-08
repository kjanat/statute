package statute

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func retryRequestBudgetUsage(b *responseBufferBudget) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func TestRetryRequestBufferInitialAdmission(t *testing.T) {
	for _, size := range []int64{1, 511, 512, 1024} {
		budget := newResponseBufferBudget(size)
		b := newRetryRequestBuffer(budget)
		if b == nil {
			t.Fatalf("size %d refused first buffer", size)
		}
		want := min(size, initialRetryRequestBufferBytes)
		if got := retryRequestBudgetUsage(budget); got != want {
			t.Fatalf("charged %d, want %d", got, want)
		}
		b.release()
		if got := retryRequestBudgetUsage(budget); got != 0 {
			t.Fatalf("released charge=%d", got)
		}
	}
}

func TestRetryRequestBufferGrowthOverlap(t *testing.T) {
	budget := newResponseBufferBudget(1535)
	b := newRetryRequestBuffer(budget)
	defer b.release()
	r := strings.NewReader(strings.Repeat("x", 513))
	complete, err := b.readFrom(r)
	if complete || err != nil || len(b.body) != 512 || r.Len() != 1 {
		t.Fatalf("growth fallback complete=%v err=%v prefix=%d remaining=%d", complete, err, len(b.body), r.Len())
	}
	if got := retryRequestBudgetUsage(budget); got != 512 {
		t.Fatalf("failed growth charge=%d", got)
	}
}

func TestRetryRequestBufferGrowthSuccess(t *testing.T) {
	budget := newResponseBufferBudget(1536)
	b := newRetryRequestBuffer(budget)
	defer b.release()
	payload := strings.Repeat("x", 513)
	complete, err := b.readFrom(strings.NewReader(payload))
	if !complete || err != nil || string(b.body) != payload {
		t.Fatalf("complete=%v err=%v size=%d", complete, err, len(b.body))
	}
	if got := retryRequestBudgetUsage(budget); got != 1024 {
		t.Fatalf("capacity charge=%d", got)
	}
}

func TestRetryRequestBufferSentinel(t *testing.T) {
	for _, size := range []int{0, maxRetryBufferBytes, maxRetryBufferBytes + 1, maxRetryBufferBytes + 4096} {
		budget := newResponseBufferBudget(4 << 20)
		b := newRetryRequestBuffer(budget)
		r := strings.NewReader(strings.Repeat("x", size))
		complete, err := b.readFrom(r)
		if err != nil || complete != (size <= maxRetryBufferBytes) {
			t.Fatalf("size=%d complete=%v err=%v", size, complete, err)
		}
		want := min(size, maxRetryBufferBytes+1)
		if len(b.body) != want || r.Len() != size-want {
			t.Fatalf("size=%d prefix=%d remaining=%d", size, len(b.body), r.Len())
		}
		if cap(b.body) > maxRetryBufferBytes+1 {
			t.Fatalf("oversized capacity=%d", cap(b.body))
		}
		b.release()
		if got := retryRequestBudgetUsage(budget); got != 0 {
			t.Fatalf("remaining charge=%d", got)
		}
	}
}

type retryRequestTestBody struct {
	reader io.Reader
	closes int
}

func (b *retryRequestTestBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (b *retryRequestTestBody) Close() error               { b.closes++; return nil }

type retryRequestErrorReader struct{ err error }

func (r retryRequestErrorReader) Read(p []byte) (int, error) { return copy(p, "partial"), r.err }

func TestRetryRequestBufferReadErrorAndClose(t *testing.T) {
	wantErr := errors.New("read failure")
	original := &retryRequestTestBody{reader: retryRequestErrorReader{wantErr}}
	r := httptest.NewRequest(http.MethodPut, "/", nil)
	r.Body = original
	b := newRetryRequestBuffer(newResponseBufferBudget(1024))
	defer b.release()
	rec := httptest.NewRecorder()
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("producer called after read error") })
	if _, ok := bufferRetryBody(rec, r, next, b); ok {
		t.Fatal("read error allowed retry")
	}
	if rec.Code != http.StatusBadRequest || original.closes != 1 || !strings.Contains(rec.Body.String(), wantErr.Error()) {
		t.Fatalf("status=%d closes=%d body=%q", rec.Code, original.closes, rec.Body.String())
	}
}

func TestRetryRequestBufferFullBodyClosesOriginal(t *testing.T) {
	original := &retryRequestTestBody{reader: strings.NewReader("body")}
	r := httptest.NewRequest(http.MethodPut, "/", nil)
	r.Body = original
	b := newRetryRequestBuffer(newResponseBufferBudget(1024))
	defer b.release()
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("single attempt used for complete body") })
	buffered, ok := bufferRetryBody(httptest.NewRecorder(), r, next, b)
	if !ok || original.closes != 1 || string(b.body) != "body" {
		t.Fatalf("closes=%d body=%q", original.closes, b.body)
	}
	if buffered == r || r.Body != original {
		t.Fatal("caller request body ownership changed")
	}
}

func TestRetryRequestBufferFallbackClose(t *testing.T) {
	payload := strings.Repeat("x", 513)
	original := &retryRequestTestBody{reader: strings.NewReader(payload)}
	r := httptest.NewRequest(http.MethodPut, "/", nil)
	r.Body = original
	b := newRetryRequestBuffer(newResponseBufferBudget(512))
	defer b.release()
	calls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		got, err := io.ReadAll(r.Body)
		if err != nil || string(got) != payload {
			t.Errorf("body length=%d err=%v", len(got), err)
		}
		if original.closes != 0 {
			t.Error("original closed before producer")
		}
		if err := r.Body.Close(); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusTeapot)
	})
	rec := httptest.NewRecorder()
	_, ok := bufferRetryBody(rec, r, next, b)
	if ok || calls != 1 || original.closes != 1 || rec.Code != http.StatusTeapot {
		t.Fatalf("calls=%d closes=%d status=%d", calls, original.closes, rec.Code)
	}
	if r.Body != original {
		t.Fatal("fallback replaced caller body")
	}
}

func TestRetryRequestBufferConcurrentAdmission(t *testing.T) {
	budget := newResponseBufferBudget(4 * initialRetryRequestBufferBytes)
	var wg sync.WaitGroup
	admitted := make(chan *retryRequestBuffer, 32)
	for range 32 {
		wg.Go(func() { admitted <- newRetryRequestBuffer(budget) })
	}
	wg.Wait()
	close(admitted)
	if got := retryRequestBudgetUsage(budget); got != budget.limit {
		t.Fatalf("charged=%d", got)
	}
	count := 0
	for b := range admitted {
		if b != nil {
			count++
			b.release()
		}
	}
	if count != 4 || retryRequestBudgetUsage(budget) != 0 {
		t.Fatalf("admitted=%d charge=%d", count, retryRequestBudgetUsage(budget))
	}
}

func TestRetryRequestBufferAncestorLeases(t *testing.T) {
	budget := newResponseBufferBudget(2048)
	a, b := newRetryRequestBuffer(budget), newRetryRequestBuffer(budget)
	ctx := withRetryRequestBuffer(withRetryRequestBuffer(context.Background(), a), b)
	chain := retryRequestBuffers(ctx)
	chain.retain()
	a.release()
	b.release()
	if got := retryRequestBudgetUsage(budget); got != 1024 {
		t.Fatalf("ancestor charge released early: %d", got)
	}
	chain.release()
	if got := retryRequestBudgetUsage(budget); got != 0 {
		t.Fatalf("ancestor charge leaked: %d", got)
	}
	if a.body != nil || b.body != nil {
		t.Fatal("released owners retain allocation")
	}
}

func TestRetryRequestBufferGrowthPreservesPrefix(t *testing.T) {
	budget := newResponseBufferBudget(4096)
	b := newRetryRequestBuffer(budget)
	defer b.release()
	b.body = append(b.body, bytes.Repeat([]byte("x"), 512)...)
	if !b.grow() || len(b.body) != 512 || cap(b.body) != 1024 || !bytes.Equal(b.body, bytes.Repeat([]byte("x"), 512)) {
		t.Fatal("growth changed prefix")
	}
}

type retryRequestBlockedReader struct {
	entered chan struct{}
	release chan struct{}
}

func (r retryRequestBlockedReader) Read([]byte) (int, error) {
	close(r.entered)
	<-r.release
	return 0, io.EOF
}

func TestRetryRequestBufferBlockedEmptyReader(t *testing.T) {
	budget := newResponseBufferBudget(512)
	b := newRetryRequestBuffer(budget)
	r := retryRequestBlockedReader{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan bool, 1)
	go func() { complete, err := b.readFrom(r); done <- complete && err == nil }()
	<-r.entered
	if other := newRetryRequestBuffer(budget); other != nil {
		other.release()
		t.Error("blocked empty reader did not retain admission")
	}
	if got := retryRequestBudgetUsage(budget); got != 512 {
		t.Errorf("blocked charge=%d", got)
	}
	close(r.release)
	if !<-done {
		t.Error("empty read did not complete")
	}
	b.release()
	if got := retryRequestBudgetUsage(budget); got != 0 {
		t.Errorf("empty reader charge leaked: %d", got)
	}
}

func TestRetryRequestBufferConcurrentGrowth(t *testing.T) {
	budget := newResponseBufferBudget(3072)
	var buffers [3]*retryRequestBuffer
	for i := range buffers {
		buffers[i] = newRetryRequestBuffer(budget)
	}
	var wg sync.WaitGroup
	for _, b := range buffers {
		wg.Go(func() { b.grow() })
	}
	wg.Wait()
	var total int64
	grown := 0
	for _, b := range buffers {
		total += int64(cap(b.body))
		if cap(b.body) == 1024 {
			grown++
		}
	}
	if grown < 1 || grown > 2 || retryRequestBudgetUsage(budget) != total {
		t.Errorf("grown=%d charged=%d capacities=%d", grown, retryRequestBudgetUsage(budget), total)
	}
	for _, b := range buffers {
		b.release()
	}
	if got := retryRequestBudgetUsage(budget); got != 0 {
		t.Errorf("growth charge leaked: %d", got)
	}
}

type retryTrailerReplacingBody struct {
	request       *http.Request
	full          bool
	reads, closes int
}

func (b *retryTrailerReplacingBody) Read(p []byte) (int, error) {
	b.reads++
	if b.full {
		b.request.Trailer = http.Header{"X-Final": {"complete"}}
		return copy(p, "p"), io.EOF
	}
	switch b.reads {
	case 1:
		return copy(p, "p"), nil
	case 2:
		b.request.Trailer = http.Header{"X-Progress": {"before-eof"}}
		return 0, nil
	default:
		b.request.Trailer = http.Header{"X-Final": {"complete"}}
		return copy(p, "t"), io.EOF
	}
}

func (b *retryTrailerReplacingBody) Close() error { b.closes++; return nil }

func TestRetryRequestBufferFullBodyReplacedTrailers(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/", nil)
	original := &retryTrailerReplacingBody{request: r, full: true}
	r.Body = original
	b := newRetryRequestBuffer(newResponseBufferBudget(1))
	defer b.release()
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected fallback") })
	forwarded, ok := bufferRetryBody(httptest.NewRecorder(), r, next, b)
	if !ok || forwarded == nil {
		t.Fatal("complete body was not replayable")
	}
	if forwarded.Trailer.Get("X-Final") != "complete" || original.closes != 1 || r.Body != original {
		t.Fatalf("trailers=%v closes=%d original body preserved=%v", forwarded.Trailer, original.closes, r.Body == original)
	}
}

func TestRetryRequestBufferFallbackPublishesTrailersAtEOF(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/", nil)
	r.Trailer = http.Header{"X-Announced": {"transport-owned"}}
	original := &retryTrailerReplacingBody{request: r}
	r.Body = original
	b := newRetryRequestBuffer(newResponseBufferBudget(1))
	defer b.release()
	next := http.HandlerFunc(func(_ http.ResponseWriter, forwarded *http.Request) {
		assertRetryTrailerFallback(t, forwarded, r)
	})
	if _, ok := bufferRetryBody(httptest.NewRecorder(), r, next, b); ok {
		t.Fatal("growth pressure should serve once")
	}
	if r.Body != original || original.closes != 1 {
		t.Fatalf("body preserved=%v closes=%d", r.Body == original, original.closes)
	}
}

func assertRetryTrailerFallback(t *testing.T, forwarded, original *http.Request) {
	t.Helper()
	if forwarded == original || forwarded.Trailer == nil {
		t.Fatal("fallback needs independent request and stable trailer map")
	}
	// Timeout makes this same shallow context clone before its producer.
	clone := forwarded.WithContext(forwarded.Context())
	stable := clone.Trailer
	assertRetryAnnouncedTrailer(t, stable)
	assertRetryTrailerRead(t, clone.Body, 1, nil)
	assertRetryTrailerRead(t, clone.Body, 0, nil)
	if stable.Get("X-Progress") != "" {
		t.Fatalf("zero read published trailers before EOF: %v", stable)
	}
	assertRetryAnnouncedTrailer(t, stable)
	stable.Set("X-Independent", "clone")
	if original.Trailer.Get("X-Independent") != "" {
		t.Fatal("fallback aliases original trailer map")
	}
	assertRetryTrailerRead(t, clone.Body, 1, io.EOF)
	if stable.Get("X-Final") != "complete" || stable.Get("X-Progress") != "" {
		t.Fatalf("EOF trailers=%v", stable)
	}
	if err := clone.Body.Close(); err != nil {
		t.Error(err)
	}
}

func assertRetryAnnouncedTrailer(t *testing.T, h http.Header) {
	t.Helper()
	values, ok := h["X-Announced"]
	if !ok || values != nil {
		t.Fatalf("announced key snapshot=%v", h)
	}
}

func assertRetryTrailerRead(t *testing.T, r io.Reader, want int, wantErr error) {
	t.Helper()
	var data [1]byte
	n, err := r.Read(data[:])
	if n != want || !errors.Is(err, wantErr) {
		t.Fatalf("Read=%d,%v want %d,%v", n, err, want, wantErr)
	}
}
