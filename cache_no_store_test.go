package statute

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestCacheNoStoreResponse(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"GET", "HEAD"} {
		for _, values := range [][]string{{"no-store"}, {"public", "max-age=60, No-StOrE"}, {`ext="a,b", no-store`}, {`ext="broken`}} {
			t.Run(method+fmt.Sprint(values), func(t *testing.T) {
				t.Parallel()
				calls := 0
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.Header()["Cache-Control"] = slices.Clone(values)
					w.Header().Set("X-Call", fmt.Sprint(calls))
					w.WriteHeader(http.StatusOK)
				}), Cache("1h"))
				for i := 1; i <= 2; i++ {
					rec := runRequest(t, h, httptest.NewRequest(method, "/", nil))
					if rec.Code != 200 || rec.Header().Get("X-Call") != fmt.Sprint(i) || !slices.Equal(rec.Header().Values("Cache-Control"), values) {
						t.Fatalf("response %d: %d %v", i, rec.Code, rec.Header())
					}
				}
				if calls != 2 {
					t.Fatalf("stored prohibited response; calls=%d", calls)
				}
			})
		}
	}
}

func TestCacheNoStoreRequest(t *testing.T) {
	t.Parallel()
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("X-Call", fmt.Sprint(calls))
	}), Cache("1h"))
	for i, noStore := range []bool{true, true, false, true, false} {
		req := httptest.NewRequest("GET", "/", nil)
		if noStore {
			req.Header.Add("Cache-Control", "max-age=60")
			req.Header.Add("Cache-Control", "No-Store")
		}
		rec := runRequest(t, h, req)
		// Request no-store forbids new storage; it does not invalidate an
		// existing entry or prohibit serving that entry (RFC 9111 5.2.1.5).
		want := min(i+1, 3)
		if calls != want || rec.Header().Get("X-Call") != fmt.Sprint(want) {
			t.Fatalf("request %d: calls=%d, headers=%v", i, calls, rec.Header())
		}
	}
}

func TestCacheNoStoreMiddlewareInteractions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		mws        []Middleware
		originDeny bool
		wantCalls  int
	}{
		{"route add", []Middleware{AddResponseHeader("Cache-Control", "no-store")}, false, 4},
		{"route set", []Middleware{SetResponseHeader("Cache-Control", "no-store")}, false, 4},
		{"add then remove", []Middleware{AddResponseHeader("Cache-Control", "no-store"), RemoveResponseHeader("Cache-Control")}, false, 2},
		{"remove then add", []Middleware{RemoveResponseHeader("Cache-Control"), AddResponseHeader("Cache-Control", "no-store")}, false, 4},
		{"origin deny survives remove", []Middleware{RemoveResponseHeader("Cache-Control")}, true, 4},
		{"origin deny survives replace", []Middleware{SetResponseHeader("Cache-Control", "public")}, true, 4},
	} {
		for _, retryOutside := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retryOutside=%v", tc.name, retryOutside), func(t *testing.T) {
				t.Parallel()
				calls := 0
				base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					if tc.originDeny {
						w.Header().Set("Cache-Control", "no-store")
					}
					if calls%2 == 1 {
						w.WriteHeader(503)
						return
					}
					_, _ = io.WriteString(w, "successful response")
				})
				order := []Middleware{Cache("1h"), Retry(2, OnStatus(503))}
				if retryOutside {
					order[0], order[1] = order[1], order[0]
				}
				mws := append([]Middleware{Compress(Gzip), AddResponseHeader("X-Once", "yes")}, order...)
				mws = append(mws, tc.mws...)
				h := chain(t, base, mws...)
				for range 2 {
					req := httptest.NewRequest("GET", "/", nil)
					req.Header.Set("Accept-Encoding", "gzip")
					rec := runRequest(t, h, req)
					assertCacheGzipResponse(t, rec)
				}
				if calls != tc.wantCalls {
					t.Fatalf("origin calls=%d, want %d", calls, tc.wantCalls)
				}
			})
		}
	}
}

func assertCacheGzipResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != 200 || !slices.Equal(rec.Header().Values("X-Once"), []string{"yes"}) {
		t.Fatalf("status/hoisted headers: %d %v", rec.Code, rec.Header())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil || string(body) != "successful response" {
		t.Fatalf("body=%q, err=%v", body, err)
	}
}

func TestCacheNoStoreRouteIsolation(t *testing.T) {
	t.Parallel()
	calls := 0
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("X-Call", fmt.Sprint(calls))
	})
	open := chain(t, base, Cache("1h"))
	denied := chain(t, base, Cache("1h"), SetResponseHeader("Cache-Control", "no-store"))
	for i, h := range []http.Handler{open, denied, open, denied, open} {
		rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
		if i%2 == 0 && rec.Header().Get("X-Call") != "1" {
			t.Fatal("sibling route lost its cache")
		}
	}
	if calls != 3 {
		t.Fatalf("route policy leaked: %d origin calls", calls)
	}
}
