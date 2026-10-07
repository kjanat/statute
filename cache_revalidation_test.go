package statute

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestCacheResponseNoCache(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"GET", "HEAD"} {
		for _, tc := range []struct {
			values    []string
			cacheable bool
		}{
			{[]string{"no-cache"}, false},
			{[]string{"public", "No-CaChE"}, false},
			{[]string{`no-cache="X-Secret, X-Other"`}, false},
			{[]string{`ext="a,b\"c", no-cache`}, false},
			{[]string{`no-cache="broken`}, false},
			{[]string{"x-no-cache"}, true},
			{[]string{"ext=no-cache"}, true},
			{[]string{`ext="public, no-cache"`}, true},
		} {
			t.Run(method+fmt.Sprint(tc.values), func(t *testing.T) {
				t.Parallel()
				calls := 0
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.Header()["cAcHe-CoNtRoL"] = slices.Clone(tc.values)
					w.Header().Set("X-Call", fmt.Sprint(calls))
					_, _ = io.WriteString(w, "representation")
				}), Cache("1h"))
				for i := 1; i <= 2; i++ {
					want := i
					if tc.cacheable {
						want = 1
					}
					rec := runRequest(t, h, httptest.NewRequest(method, "/", nil))
					if rec.Code != 200 || rec.Header().Get("X-Call") != fmt.Sprint(want) || rec.Body.String() != "representation" {
						t.Fatalf("response %d: %d %v %q", i, rec.Code, rec.Header(), rec.Body.String())
					}
					values, _ := cacheHeaderValues(rec.Header(), "Cache-Control")
					if !slices.Equal(values, tc.values) {
						t.Fatalf("policy changed: %v", values)
					}
				}
			})
		}
	}
}

func TestCacheNoCacheProjectionRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		origin bool
		ops    []Middleware
		calls  int
	}{
		{"origin removed", true, []Middleware{RemoveResponseHeader("Cache-Control")}, 4},
		{"origin replaced", true, []Middleware{SetResponseHeader("Cache-Control", "public")}, 4},
		{"route set", false, []Middleware{SetResponseHeader("Cache-Control", `no-cache="X-Secret"`)}, 4},
		{"route add", false, []Middleware{AddResponseHeader("Cache-Control", "no-cache")}, 4},
		{"route add remove", false, []Middleware{AddResponseHeader("Cache-Control", "no-cache"), RemoveResponseHeader("Cache-Control")}, 2},
		{"route remove add", false, []Middleware{RemoveResponseHeader("Cache-Control"), AddResponseHeader("Cache-Control", "no-cache")}, 4},
	} {
		for _, outside := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/retryOutside=%v", tc.name, outside), func(t *testing.T) {
				t.Parallel()
				calls := 0
				order := []Middleware{Cache("1h"), Retry(2, OnStatus(503))}
				if outside {
					order[0], order[1] = order[1], order[0]
				}
				mws := append([]Middleware{Compress(Gzip), AddResponseHeader("X-Once", "yes")}, order...)
				mws = append(mws, tc.ops...)
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					if tc.origin {
						w.Header().Set("Cache-Control", "no-cache")
					}
					if calls%2 == 1 {
						w.WriteHeader(503)
						return
					}
					_, _ = io.WriteString(w, "successful response")
				}), mws...)
				for range 2 {
					r := httptest.NewRequest("GET", "/", nil)
					r.Header.Set("Accept-Encoding", "gzip")
					assertCacheGzipResponse(t, runRequest(t, h, r))
				}
				if calls != tc.calls {
					t.Fatalf("calls=%d want=%d", calls, tc.calls)
				}
			})
		}
	}
}

func TestCacheNoCacheThenPublic(t *testing.T) {
	t.Parallel()
	calls := 0
	h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("X-Call", fmt.Sprint(calls))
		if calls == 1 {
			w.Header().Set("Cache-Control", "no-cache")
		}
	}), Cache("1h"))
	for _, want := range []string{"1", "2", "2"} {
		rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
		if rec.Header().Get("X-Call") != want {
			t.Fatalf("headers=%v want=%s", rec.Header(), want)
		}
	}
}
