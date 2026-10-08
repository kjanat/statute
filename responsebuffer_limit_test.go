package statute

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

func limitedRenderMiddlewares(size, budget string) map[string]Middleware {
	return map[string]Middleware{
		"etag":  ETag().MaxResponseBody(size).BufferBudget(budget),
		"retry": Retry(3, OnStatus(502, 503)).MaxResponseBody(size).BufferBudget(budget),
	}
}

func TestResponseRenderLimitBoundary(t *testing.T) {
	for name, mw := range limitedRenderMiddlewares("8B", "64B") {
		for _, chunks := range [][]string{{"12345678"}, {"1234", "5678"}, {"123456789"}, {"1234", "5678", "9"}} {
			t.Run(name+"/"+strings.Join(chunks, "-"), func(t *testing.T) {
				calls, returned := 0, false
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					for _, chunk := range chunks {
						_, _ = io.WriteString(w, chunk)
					}
					returned = true
				}), mw)
				rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
				if len(strings.Join(chunks, "")) > 8 {
					assertRenderFailure(t, rec, http.StatusBadGateway)
					if calls != 1 {
						t.Fatalf("overflow retried: calls=%d", calls)
					}
					return
				}
				if rec.Code != http.StatusOK || rec.Body.String() != "12345678" || !returned {
					t.Fatalf("boundary: %d %q returned=%v", rec.Code, rec.Body.String(), returned)
				}
			})
		}
	}
}

func assertRenderFailure(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status || rec.Body.Len() != 0 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("failure: status=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
	for _, name := range []string{"ETag", "Trailer", "X-Finished", "Content-Encoding", "Content-Length", "Set-Cookie"} {
		if rec.Header().Get(name) != "" {
			t.Fatalf("producer header leaked: %s=%q", name, rec.Header().Get(name))
		}
	}
}

func TestResponseRenderLimitHeadAndConditions(t *testing.T) {
	for name, mw := range limitedRenderMiddlewares("8B", "64B") {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			for _, condition := range []string{"", "If-None-Match", "If-Match"} {
				t.Run(name+"/"+method+"/"+condition, func(t *testing.T) {
					h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("ETag", `"origin"`)
						w.Header().Set("Trailer", "X-Finished")
						w.Header().Set("Set-Cookie", "secret=value")
						_, _ = io.WriteString(w, "123456789")
					}), mw)
					r := httptest.NewRequest(method, "/", nil)
					if condition != "" {
						r.Header.Set(condition, `"origin"`)
					}
					assertRenderFailure(t, runRequest(t, h, r), http.StatusBadGateway)
				})
			}
		}
	}
}

func TestResponseRenderBudgetConcurrent(t *testing.T) {
	for name, mw := range limitedRenderMiddlewares("4B", "4B") {
		t.Run(name, func(t *testing.T) {
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan *httptest.ResponseRecorder, 1)
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "body")
				if r.URL.Path == "/hold" {
					close(entered)
					select {
					case <-release:
					case <-t.Context().Done():
					}
				}
			}), mw)
			go func() {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hold", nil))
				done <- rec
			}()
			<-entered
			rec := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/other", nil))
			close(release)
			first := <-done
			assertRenderFailure(t, rec, http.StatusServiceUnavailable)
			if first.Code != http.StatusOK || first.Body.String() != "body" {
				t.Fatalf("first response=%d %q", first.Code, first.Body.String())
			}
			last := runRequest(t, h, httptest.NewRequest(http.MethodGet, "/after", nil))
			if last.Code != http.StatusOK || last.Body.String() != "body" {
				t.Fatalf("budget not released: %d %q", last.Code, last.Body.String())
			}
		})
	}
}

func TestResponseRenderLimitProxyWire(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"origin"`)
		_, _ = io.WriteString(w, strings.Repeat("x", 1024))
	}))
	defer origin.Close()
	target, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	for name, mw := range limitedRenderMiddlewares("8B", "64B") {
		for _, h2 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/h2=%v", name, h2), func(t *testing.T) {
				srv := httptest.NewUnstartedServer(chain(t, httputil.NewSingleHostReverseProxy(target), mw))
				srv.EnableHTTP2 = h2
				srv.StartTLS()
				defer srv.Close()
				resp, err := srv.Client().Get(srv.URL)
				if err != nil {
					t.Fatalf("overflow aborted connection: %v", err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || len(body) != 0 || resp.StatusCode != http.StatusBadGateway || resp.Header.Get("ETag") != "" {
					t.Fatalf("status=%d body=%q headers=%v err=%v", resp.StatusCode, body, resp.Header, err)
				}
			})
		}
	}
}

func TestResponseBufferLimitPanicOwnership(t *testing.T) {
	for _, value := range []any{http.ErrAbortHandler, "application panic"} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			b := newLimitedResponseBuffer(4)
			b.budget = newResponseBufferBudget(4)
			defer func() {
				if got := recover(); got != value {
					t.Errorf("panic changed: got %v want %v", got, value)
				}
				b.release()
				if b.budget.used != 0 {
					t.Error("panic retained budget")
				}
			}()
			b.render(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "body")
				panic(value)
			}), httptest.NewRequest(http.MethodGet, "/", nil))
		})
	}
}

func TestResponseBufferBudgetGrowth(t *testing.T) {
	b := newLimitedResponseBuffer(8)
	b.budget = newResponseBufferBudget(8)
	defer b.release()
	if !b.render(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.Copy(w, bytes.NewReader([]byte("12345678")))
	}), httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("exact budget rejected")
	}
	if b.body.Cap() != 8 || b.budget.used != 8 {
		t.Fatalf("capacity=%d charge=%d", b.body.Cap(), b.budget.used)
	}
}

func TestResponseBufferBudgetGrowthOverlap(t *testing.T) {
	type outcome struct {
		n, status, length, capacity int
		failed                      bool
		used                        int64
	}
	for _, budget := range []int64{1024, 1536} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			b := newLimitedResponseBuffer(1024)
			b.budget = newResponseBufferBudget(budget)
			defer b.release()
			if _, err := b.Write(bytes.Repeat([]byte("x"), 512)); err != nil {
				t.Fatal(err)
			}
			n, err := b.Write([]byte("y"))
			want := outcome{n: 1, length: 513, capacity: 1024, used: 1024}
			if budget == 1024 {
				want = outcome{status: http.StatusServiceUnavailable, length: 512, capacity: 512, used: 512, failed: true}
			}
			got := outcome{n: n, status: b.failureStatus, length: b.body.Len(), capacity: b.body.Cap(), used: b.budget.used, failed: err != nil}
			if got != want {
				t.Fatalf("growth accounting: got %+v want %+v", got, want)
			}
			b.release()
			if b.budget.used != 0 {
				t.Fatalf("release retained %d bytes", b.budget.used)
			}
		})
	}
}
