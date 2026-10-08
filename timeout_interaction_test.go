package statute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"statute.kjanat.dev/resolved"
)

func TestTimeoutLimitMiddlewareCompositions(t *testing.T) {
	limited := Timeout("1s").MaxResponseBody("8B").BufferBudget("64B")
	for _, tc := range []struct {
		name     string
		chain    []Middleware
		attempts int32
	}{
		{"cache-timeout", []Middleware{Cache("1m"), limited}, 1},
		{"timeout-cache", []Middleware{limited, Cache("1m")}, 1},
		{"etag-timeout", []Middleware{ETag(), limited}, 1},
		{"timeout-etag", []Middleware{limited, ETag()}, 1},
		{"retry-timeout", []Middleware{Retry(2, OnStatus(502)), limited}, 2},
		{"timeout-retry", []Middleware{limited, Retry(2, OnStatus(502))}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.Header().Set("ETag", `"origin"`)
					w.Header().Set("Set-Cookie", "private=value")
					_, _ = io.WriteString(w, "123456789")
				}), tc.chain...)
				for range 2 {
					assertRenderFailure(t, runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil)), http.StatusBadGateway)
					synctest.Wait()
				}
				if got := calls.Load(); got != 2*tc.attempts {
					t.Fatalf("producer calls=%d want=%d; failure was cached or Retry policy changed", got, 2*tc.attempts)
				}
			})
		})
	}
}

func TestTimeoutSuccessfulMiddlewareCompositions(t *testing.T) {
	limited := Timeout("1s").MaxResponseBody("8B").BufferBudget("64B")
	for _, tc := range []struct {
		name  string
		chain []Middleware
		calls int32
	}{
		{"cache-timeout", []Middleware{Cache("1m"), limited}, 1},
		{"timeout-cache", []Middleware{limited, Cache("1m")}, 1},
		{"etag-timeout", []Middleware{ETag(), limited}, 2},
		{"timeout-etag", []Middleware{limited, ETag()}, 2},
		{"retry-timeout", []Middleware{Retry(2), limited}, 2},
		{"timeout-retry", []Middleware{limited, Retry(2)}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					_, _ = io.WriteString(w, "body")
				}), tc.chain...)
				for range 2 {
					got := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
					if got.Code != http.StatusOK || got.Body.String() != "body" {
						t.Fatalf("status=%d body=%q", got.Code, got.Body.String())
					}
				}
				if got := calls.Load(); got != tc.calls {
					t.Fatalf("producer calls=%d want=%d", got, tc.calls)
				}
			})
		})
	}
}

func TestTimeoutPreservesCacheFreshness(t *testing.T) {
	for _, outside := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout-outside=%t", outside), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				mws := []Middleware{Cache("3s"), Timeout("10s").MaxResponseBody("4B").BufferBudget("4B")}
				if outside {
					mws[0], mws[1] = mws[1], mws[0]
				}
				var calls atomic.Int32
				stamp := time.Now().UTC().Format(http.TimeFormat)
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.Header().Set("Age", "10")
					w.WriteHeader(http.StatusOK)
					time.Sleep(time.Second)
					_, _ = io.WriteString(w, "body")
				}), mws...)
				first := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
				if first.Code != http.StatusOK || first.Header().Get("Age") != "11" || first.Header().Get("Date") != stamp {
					t.Fatalf("first response=%d headers=%v", first.Code, first.Header())
				}
				time.Sleep(time.Second)
				second := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
				if calls.Load() != 1 || second.Header().Get("Age") != "12" {
					t.Fatalf("fresh response not reused correctly: calls=%d headers=%v", calls.Load(), second.Header())
				}
				time.Sleep(time.Second)
				runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
				if calls.Load() != 2 {
					t.Fatal("hidden Timeout buffering restarted cache TTL")
				}
			})
		})
	}
}

func TestTimeoutRequestIDAndRouteHeaderOwnership(t *testing.T) {
	for _, outside := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout-outside=%t", outside), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				mws := make([]Middleware, 0, 5)
				mws = append(mws, RequestID().From("X-Input"), Timeout("1s"))
				if outside {
					mws[0], mws[1] = mws[1], mws[0]
				}
				mws = append(mws, ETag(), Cache("1m"), SetResponseHeader("X-Route", "route"))
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set(defaultRequestIDHeader, "origin")
					w.Header().Set("X-Route", "origin")
					_, _ = io.WriteString(w, "body")
					w.Header()["x-request-id"] = []string{"late-origin"}
				}), mws...)
				for _, id := range []string{"first", "second"} {
					r := httptest.NewRequest(http.MethodGet, "/", nil)
					r.Header.Set("X-Input", id)
					rec := runRequest(t, h, r)
					assertOnlyRequestID(t, rec.Result().Header, defaultRequestIDHeader, id)
					if rec.Code != http.StatusOK || rec.Body.String() != "body" || rec.Result().Header.Get("X-Route") != "route" {
						t.Fatalf("response=%d headers=%v body=%q", rec.Code, rec.Result().Header, rec.Body.String())
					}
				}
			})
		})
	}
}

type timeoutBlockingPushWriter struct {
	*httptest.ResponseRecorder
	calls   atomic.Int32
	release chan struct{}
}

func (w *timeoutBlockingPushWriter) Push(string, *http.PushOptions) error {
	w.calls.Add(1)
	<-w.release
	return nil
}

func TestTimeoutPushCannotReachDownstreamWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &timeoutBlockingPushWriter{ResponseRecorder: httptest.NewRecorder(), release: make(chan struct{})}
		result := make(chan error, 1)
		h := buildTimeout(resolved.Middleware{Timeout: time.Second}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			pusher, ok := w.(http.Pusher)
			if !ok {
				result <- errors.New("Timeout writer lost Pusher interface")
				return
			}
			result <- pusher.Push("/asset", nil)
			_, _ = io.WriteString(w, "body")
		}))
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		close(w.release)
		synctest.Wait()
		if w.calls.Load() != 0 {
			t.Fatalf("Push reached downstream writer %d times", w.calls.Load())
		}
		if err := <-result; !errors.Is(err, http.ErrNotSupported) {
			t.Fatalf("Push error=%v want ErrNotSupported", err)
		}
		if w.Code != http.StatusOK || w.Body.String() != "body" {
			t.Fatalf("rejected Push changed response: %d %q", w.Code, w.Body.String())
		}
	})
}

func TestTimeoutPropagatesProducerPanics(t *testing.T) {
	for _, value := range []any{"application panic", http.ErrAbortHandler} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := buildTimeout(resolved.Middleware{Timeout: time.Second}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Set-Cookie", "private=value")
					_, _ = io.WriteString(w, "partial")
					panic(value)
				}))
				rec := httptest.NewRecorder()
				var got any
				func() {
					defer func() { got = recover() }()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
				}()
				if got != value {
					t.Fatalf("panic=%v want=%v", got, value)
				}
				if rec.Body.Len() != 0 || rec.Header().Get("Set-Cookie") != "" {
					t.Fatalf("aborted producer leaked response: headers=%v body=%q", rec.Header(), rec.Body.String())
				}
			})
		})
	}
}

func TestTimeoutLimitConvertsOnlyRefusedProxyAbort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := &limitTestBody{Reader: strings.NewReader("123456789")}
		proxy := &httputil.ReverseProxy{
			Rewrite: func(*httputil.ProxyRequest) {},
			Transport: limitTestTransport(func(r *http.Request) (*http.Response, error) {
				body.ctx = r.Context()
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, ContentLength: 9}, nil
			}),
		}
		h := chain(t, proxy, Timeout("1s").MaxResponseBody("8B").BufferBudget("64B"))
		ctx := context.WithValue(t.Context(), http.ServerContextKey, &http.Server{ReadHeaderTimeout: time.Second})
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://origin/", nil)
		assertRenderFailure(t, runRequest(t, h, r), http.StatusBadGateway)
		synctest.Wait()
		if !body.closed || !body.cancelledAtClose || ctx.Err() != nil {
			t.Fatalf("closed=%v cancelled-at-close=%v parent-error=%v", body.closed, body.cancelledAtClose, ctx.Err())
		}
	})
}

func TestTimeoutSuccessfulTrailersAcrossBuffers(t *testing.T) {
	for _, mws := range [][]Middleware{
		{Timeout("1s")},
		{Timeout("1s"), ETag()},
		{ETag(), Timeout("1s")},
		{Timeout("1s"), Retry(2)},
		{Retry(2), Timeout("1s")},
	} {
		t.Run(fmt.Sprint(mws), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Trailer", "X-Finished")
					w.WriteHeader(http.StatusEarlyHints)
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "body")
					w.Header().Set("X-Finished", "done")
					w.Header().Set(http.TrailerPrefix+"X-Late", "late")
				}), mws...)
				rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
				resp := rec.Result()
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK || rec.Body.String() != "body" {
					t.Fatalf("response=%d body=%q", resp.StatusCode, rec.Body.String())
				}
				if resp.Header.Get("X-Finished") != "" || resp.Header.Get("X-Late") != "" || resp.Trailer.Get("X-Finished") != "done" || resp.Trailer.Get("X-Late") != "late" {
					t.Fatalf("trailers promoted or dropped: headers=%v trailers=%v", resp.Header, resp.Trailer)
				}
			})
		})
	}
}
