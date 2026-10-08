package statute

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestTrailerIngressPublishesOnlyAtEOF(t *testing.T) {
	source := httptest.NewRequest(http.MethodPut, "http://example.com/", nil)
	source.Trailer = http.Header{"X-Announced": nil}
	body := &requestTrailerPhasedBody{source: source}
	source.Body = body
	source.ContentLength = -1
	h := requestTrailerIngressHandler(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if body.reads != 0 {
			t.Fatal("ingress read the body early")
		}
		if r == source {
			t.Fatal("transport request was not isolated")
		}
		live := r.Trailer
		clone := r.WithContext(r.Context())
		assertRequestTrailerPartialReads(t, clone, live)
		assertRequestTrailerFinalRead(t, clone, live)
		body.source.Trailer = http.Header{"X-Subsequent": {"must-not-publish"}}
		body.reads = 4
		if _, err := clone.Body.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("repeated EOF=%v", err)
		}
		if live.Get("X-Final") != "complete" || len(live) != 1 {
			t.Fatalf("repeated EOF republished: %v", live)
		}
		if err := clone.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), source)
	if body.closes != 1 || source.Body != body {
		t.Fatalf("close=%d transport body replaced=%v", body.closes, source.Body != body)
	}
}

func assertRequestTrailerPartialReads(t *testing.T, r *http.Request, live http.Header) {
	t.Helper()
	for range 2 {
		if _, err := r.Body.Read(make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		if len(live) != 1 || live["X-Announced"] != nil {
			t.Fatalf("published before EOF: %v", live)
		}
		if _, ok := live["X-Announced"]; !ok {
			t.Fatalf("lost declaration: %v", live)
		}
	}
}

func assertRequestTrailerFinalRead(t *testing.T, r *http.Request, live http.Header) {
	t.Helper()
	if n, err := r.Body.Read(make([]byte, 1)); n != 1 || !errors.Is(err, io.EOF) {
		t.Fatalf("final read=(%d,%v)", n, err)
	}
	if live.Get("X-Final") != "complete" || r.Trailer.Get("X-Final") != "complete" || len(live) != 1 {
		t.Fatalf("EOF trailers=%v clone=%v", live, r.Trailer)
	}
}

func TestRequestTrailerIngressReadErrorDoesNotPublish(t *testing.T) {
	source := httptest.NewRequest(http.MethodPut, "http://example.com/", nil)
	source.Trailer = http.Header{"X-Announced": nil}
	body := &requestTrailerPhasedBody{source: source, fail: true}
	source.Body = body
	h := requestTrailerIngressHandler(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("read error=%v", err)
		}
		if r.Trailer.Get("X-Partial") != "" || r.Trailer.Get("X-Final") != "" {
			t.Fatalf("published failed stream: %v", r.Trailer)
		}
		body.fail = false
		body.reads = 2
		if _, err := r.Body.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("subsequent EOF=%v", err)
		}
		if r.Trailer.Get("X-Final") != "" {
			t.Fatalf("published failed stream on later EOF: %v", r.Trailer)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), source)
}

func requestTrailerIngressHandler(t *testing.T, handler http.Handler) http.Handler {
	t.Helper()
	srv, err := newServer(mustResolve(t, Config{Listeners: Listeners{HTTP("127.0.0.1:0")}, Routes: Routes{Match("/*").Handle(handler)}}))
	if err != nil {
		t.Fatal(err)
	}
	return srv.listeners[0].Handler
}

type requestTrailerPhasedBody struct {
	source        *http.Request
	reads, closes int
	fail          bool
}

func (b *requestTrailerPhasedBody) Read(p []byte) (int, error) {
	b.reads++
	if b.reads > 3 {
		return 0, io.EOF
	}
	if b.reads < 3 {
		b.source.Trailer = http.Header{"X-Partial": {"uncommitted"}}
		if b.fail {
			return 0, io.ErrUnexpectedEOF
		}
		if b.reads == 2 {
			return 0, nil
		}
		p[0] = 'a'
		return 1, nil
	}
	b.source.Trailer = http.Header{"X-Final": {"complete"}}
	p[0] = 'b'
	return 1, io.EOF
}

func (b *requestTrailerPhasedBody) Close() error { b.closes++; return nil }

func TestRequestTrailerIngressCloseUnblocksRead(t *testing.T) {
	source := httptest.NewRequest(http.MethodPut, "http://example.com/", nil)
	source.Trailer = http.Header{"X-Announced": nil}
	body := &requestTrailerCloseBody{source: source, entered: make(chan struct{}), release: make(chan struct{})}
	source.Body = body
	h := requestTrailerIngressHandler(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		readDone, closeDone := make(chan error, 1), make(chan error, 1)
		go func() { _, err := r.Body.Read(make([]byte, 1)); readDone <- err }()
		<-body.entered
		go func() { closeDone <- r.Body.Close() }()
		select {
		case err := <-closeDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			body.once.Do(func() { close(body.release) })
			<-readDone
			<-closeDone
			t.Fatal("Close blocked behind body Read")
		}
		if err := <-readDone; !errors.Is(err, io.EOF) {
			t.Fatalf("read=%v", err)
		}
		if r.Trailer.Get("X-Drained") != "" {
			t.Fatalf("Close published its drained trailers: %v", r.Trailer)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), source)
}

type requestTrailerCloseBody struct {
	source           *http.Request
	entered, release chan struct{}
	once             sync.Once
}

func (b *requestTrailerCloseBody) Read([]byte) (int, error) {
	close(b.entered)
	<-b.release
	return 0, io.EOF
}

func (b *requestTrailerCloseBody) Close() error {
	b.source.Trailer = http.Header{"X-Drained": {"closed"}}
	b.once.Do(func() { close(b.release) })
	return nil
}

func TestRequestTrailerNativeDeepCloneWithoutRetry(t *testing.T) {
	source := httptest.NewRequest(http.MethodPut, "http://example.com/", nil)
	source.Trailer = http.Header{"X-Announced": nil}
	source.Body = &requestTrailerPhasedBody{source: source}
	transport := retryRequestLeaseTransport(retryLeaseRoundTrip(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "ab" || r.Trailer.Get("X-Final") != "complete" {
			t.Errorf("native clone body=%q trailers=%v error=%v", body, r.Trailer, err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}))
	h := requestTrailerIngressHandler(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if retryRequestBuffers(r.Context()) != nil {
			t.Fatal("unexpected Retry lease")
		}
		res, err := transport.RoundTrip(r.Clone(r.Context()))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
	}))
	h.ServeHTTP(httptest.NewRecorder(), source)
}

func TestRequestTrailerReplacementBodyAndGetBody(t *testing.T) {
	source := httptest.NewRequest(http.MethodPut, "http://example.com/", nil)
	source.Trailer = http.Header{"X-Announced": nil}
	original := &requestTrailerPhasedBody{source: source}
	source.Body = original
	h := requestTrailerIngressHandler(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		replacement := io.NopCloser(strings.NewReader("replacement"))
		r.Body, r.ContentLength = replacement, 11
		r.Trailer = http.Header{"X-Replacement": {"independent"}}
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("rewind")), nil }
		out := r.Clone(r.Context())
		forwardRequestTrailers(&httputil.ProxyRequest{In: r, Out: out})
		if out.Body != replacement || out.ContentLength != 11 || len(out.TransferEncoding) != 0 {
			t.Fatalf("replacement framing changed: body=%T length=%d encoding=%v", out.Body, out.ContentLength, out.TransferEncoding)
		}
		transport := retryRequestLeaseTransport(retryLeaseRoundTrip(func(forwarded *http.Request) (*http.Response, error) {
			defer forwarded.Body.Close()
			if forwarded.Trailer.Get("X-Replacement") != "independent" || len(forwarded.Trailer) != 1 {
				t.Errorf("replacement trailers=%v", forwarded.Trailer)
			}
			assertRequestTrailerIndependentRewind(t, forwarded)
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}))
		res, err := transport.RoundTrip(out)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
	}))
	h.ServeHTTP(httptest.NewRecorder(), source)
	if original.reads != 0 || original.closes != 0 {
		t.Fatalf("unrelated source consumed: reads=%d closes=%d", original.reads, original.closes)
	}
}

func assertRequestTrailerIndependentRewind(t *testing.T, r *http.Request) {
	t.Helper()
	rewind, err := r.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer rewind.Close()
	if _, associated := rewind.(retryTrailerBody); associated {
		t.Error("GetBody inherited unrelated provenance")
	}
	data, err := io.ReadAll(rewind)
	if err != nil || string(data) != "rewind" {
		t.Errorf("GetBody=%q error=%v", data, err)
	}
}
