package statute

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCacheMetadataAdmissionLimits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		request  func(*http.Request)
		response func(http.Header)
	}{
		{"large target", func(r *http.Request) { r.URL.Path = "/" + strings.Repeat("x", 17<<10) }, nil},
		{"escaped target", func(r *http.Request) { r.URL.Path = "/" + strings.Repeat("é", 3000) }, nil},
		{"large original target", func(r *http.Request) { r.RequestURI = "/" + strings.Repeat("x", 17<<10) }, nil},
		{"large request header", func(r *http.Request) { r.Header.Set("X-Large", strings.Repeat("x", 65<<10)) }, nil},
		{"many request names", func(r *http.Request) { fillCacheHeaderNames(r.Header) }, nil},
		{"many request values", func(r *http.Request) { r.Header["X-Many"] = make([]string, 1025) }, nil},
		{"large response header", nil, func(h http.Header) { h.Set("X-Large", strings.Repeat("x", 65<<10)) }},
		{"many response names", nil, fillCacheHeaderNames},
		{"many response values", nil, func(h http.Header) { h["X-Many"] = make([]string, 1025) }},
		{"duplicate vary tokens", nil, func(h http.Header) { h.Set("Vary", strings.Repeat("X-One,", 257)) }},
		{"duplicate trailer tokens", nil, func(h http.Header) { h.Set("Trailer", strings.Repeat("X-One,", 257)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if tc.response != nil {
					tc.response(w.Header())
				}
				_, _ = io.WriteString(w, "complete")
			}), Cache("1h"))
			for range 2 {
				r := httptest.NewRequest("GET", "/", nil)
				if tc.request != nil {
					tc.request(r)
				}
				rec := runRequest(t, h, r)
				if rec.Code != 200 || rec.Body.String() != "complete" {
					t.Fatalf("response=%d %q", rec.Code, rec.Body.String())
				}
			}
			if calls != 2 {
				t.Fatalf("oversized metadata retained: calls=%d", calls)
			}
		})
	}
}

func fillCacheHeaderNames(h http.Header) {
	for i := range 257 {
		h.Set(fmt.Sprintf("X-Name-%d", i), "value")
	}
}

func TestCacheProjectionAdmissionLimits(t *testing.T) {
	t.Parallel()
	for _, mws := range [][]Middleware{
		{SetResponseHeader("X-Large", strings.Repeat("x", 65<<10))},
		{SetResponseHeader("Vary", strings.Repeat("X-One,", 257))},
	} {
		calls := 0
		mws = append(mws, Cache("1h"))
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; _, _ = io.WriteString(w, "complete") }), mws...)
		for range 2 {
			rec := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
			if rec.Code != 200 || rec.Body.String() != "complete" {
				t.Fatal(rec.Code, rec.Body.String())
			}
		}
		if calls != 2 {
			t.Fatalf("oversized projection retained: calls=%d", calls)
		}
	}
}
