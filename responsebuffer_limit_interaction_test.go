package statute

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"
	"time"
)

type limitTestTransport func(*http.Request) (*http.Response, error)

func (f limitTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type limitTestBody struct {
	io.Reader
	ctx                      context.Context
	closed, cancelledAtClose bool
}

func (b *limitTestBody) Close() error {
	b.closed = true
	b.cancelledAtClose = errors.Is(b.ctx.Err(), context.Canceled)
	return nil
}

func TestResponseLimitClosesProxyBody(t *testing.T) {
	for name, mw := range limitedRenderMiddlewares("8B", "64B") {
		t.Run(name, func(t *testing.T) {
			body := &limitTestBody{Reader: strings.NewReader("123456789")}
			proxy := &httputil.ReverseProxy{Rewrite: func(*httputil.ProxyRequest) {}, Transport: limitTestTransport(func(r *http.Request) (*http.Response, error) {
				body.ctx = r.Context()
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, ContentLength: 9}, nil
			})}
			ctx := context.WithValue(t.Context(), http.ServerContextKey, &http.Server{ReadHeaderTimeout: time.Second})
			r := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://origin/", nil)
			assertRenderFailure(t, runRequest(t, chain(t, proxy, mw), r), http.StatusBadGateway)
			if !body.closed || !body.cancelledAtClose || ctx.Err() != nil {
				t.Fatalf("closed=%v cancelled-at-close=%v parent-error=%v", body.closed, body.cancelledAtClose, ctx.Err())
			}
		})
	}
}

func TestResponseLimitCompositions(t *testing.T) {
	for name, mws := range map[string][]Middleware{
		"cache-etag":  {Cache("1m"), ETag().MaxResponseBody("8B")},
		"etag-cache":  {ETag().MaxResponseBody("8B"), Cache("1m")},
		"cache-retry": {Cache("1m"), Retry(2).MaxResponseBody("8B")},
		"retry-cache": {Retry(2).MaxResponseBody("8B"), Cache("1m")},
		"etag-retry":  {ETag().MaxResponseBody("8B"), Retry(2)},
		"retry-etag":  {Retry(2).MaxResponseBody("8B"), ETag()},
	} {
		t.Run(name, func(t *testing.T) {
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "123456789")
			}), mws...)
			for range 2 {
				assertRenderFailure(t, runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil)), http.StatusBadGateway)
			}
		})
	}
}

func TestResponseLimitCompressionPosition(t *testing.T) {
	for name, mw := range limitedRenderMiddlewares("128B", "1KiB") {
		for _, compressedInside := range []bool{false, true} {
			t.Run(name+"/"+map[bool]string{true: "encoded", false: "identity"}[compressedInside], func(t *testing.T) {
				mws := []Middleware{Compress(Gzip), mw}
				if compressedInside {
					mws[0], mws[1] = mws[1], mws[0]
				}
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/plain")
					_, _ = io.WriteString(w, strings.Repeat("a", 1024))
				}), mws...)
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Header.Set("Accept-Encoding", "gzip")
				rec := runRequest(t, h, r)
				wantStatus, wantBody := http.StatusBadGateway, ""
				if compressedInside {
					wantStatus, wantBody = http.StatusOK, strings.Repeat("a", 1024)
				}
				if rec.Code != wantStatus || decodedLimitBody(t, rec) != wantBody {
					t.Fatalf("status=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
				}
			})
		}
	}
}

func decodedLimitBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Header().Get("Content-Encoding") == "" {
		return rec.Body.String()
	}
	r, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestResponseLimitOuterRetryPolicy(t *testing.T) {
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, "123456789")
	}), Retry(2, OnStatus(502)), ETag().MaxResponseBody("8B"))
	assertRenderFailure(t, runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil)), http.StatusBadGateway)
	if calls != 2 {
		t.Fatalf("outer Retry policy changed: calls=%d", calls)
	}
}

func TestResponseBudgetReleasedBetweenRetryAttempts(t *testing.T) {
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = io.WriteString(w, "body")
	}), Retry(2, OnStatus(503)).MaxResponseBody("4B").BufferBudget("4B"))
	rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "body" || calls != 2 {
		t.Fatalf("status=%d body=%q calls=%d", rec.Code, rec.Body.String(), calls)
	}
}

type slowBudgetWriter struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
}

func (w *slowBudgetWriter) Write(b []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseRecorder.Write(b)
}

func TestResponseBudgetRetainedDuringReplay(t *testing.T) {
	for name, mw := range limitedRenderMiddlewares("4B", "4B") {
		t.Run(name, func(t *testing.T) {
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "body")
			}), mw)
			slow := &slowBudgetWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.ServeHTTP(slow, httptest.NewRequest(http.MethodGet, "/", nil))
			}()
			<-slow.entered
			rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
			close(slow.release)
			<-done
			assertRenderFailure(t, rec, http.StatusServiceUnavailable)
			if slow.Body.String() != "body" {
				t.Fatalf("slow replay changed: %q", slow.Body.String())
			}
		})
	}
}

func TestResponseBudgetReleaseOnApplicationPanic(t *testing.T) {
	for name, mw := range limitedRenderMiddlewares("4B", "4B") {
		t.Run(name, func(t *testing.T) {
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "body")
				if r.URL.Path == "/panic" {
					panic("application")
				}
			}), mw)
			func() {
				defer func() {
					if value := recover(); value != "application" {
						t.Errorf("panic changed: %v", value)
					}
				}()
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
			}()
			rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != http.StatusOK || rec.Body.String() != "body" {
				t.Fatalf("panic leaked budget: %d %q", rec.Code, rec.Body.String())
			}
		})
	}
}
