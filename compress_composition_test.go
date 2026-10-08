package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompressionNegotiationCacheVariants(t *testing.T) {
	for _, cacheFirst := range []bool{true, false} {
		calls := 0
		base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Vary", "Accept-Encoding")
			_, _ = io.WriteString(w, "body")
		})
		mws := []Middleware{Cache("1h"), Compress(Gzip, Brotli)}
		if !cacheFirst {
			mws[0], mws[1] = mws[1], mws[0]
		}
		h := chain(t, base, mws...)
		for range 2 {
			for _, coding := range []string{"gzip", "br", "identity"} {
				req := httptest.NewRequest("GET", "/", nil)
				req.Header.Set("Accept-Encoding", coding)
				res := runRequest(t, h, req)
				wantCoding := coding
				if coding == "identity" {
					wantCoding = ""
				}
				if res.Code != 200 || res.Header().Get("Content-Encoding") != wantCoding {
					t.Fatalf("cacheFirst=%v mixed variants: %d %v", cacheFirst, res.Code, res.Header())
				}
				assertNegotiatedBody(t, res, wantCoding, false)
			}
		}
		if calls != 3 {
			t.Fatalf("cacheFirst=%v did not reuse selected variants: calls=%d", cacheFirst, calls)
		}
	}
}

func TestCompressionRejectionPrecedesETagConditions(t *testing.T) {
	for _, mws := range [][]Middleware{
		{ETag(), Compress(Gzip)},
		{Compress(Gzip), ETag()},
		{Cache("1h"), ETag(), Compress(Gzip)},
		{Cache("1h"), Compress(Gzip), ETag()},
		{ETag(), Cache("1h"), Compress(Gzip)},
		{Compress(Gzip), Cache("1h"), ETag()},
		{ETag(), Compress(Gzip), Cache("1h")},
		{Compress(Gzip), ETag(), Cache("1h")},
	} {
		base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "body")
		})
		h := chain(t, base, mws...)
		// Prime the cache with an acceptable variant before rejecting another.
		first := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
		if first.Code != 200 || first.Body.String() != "body" {
			t.Fatal("identity render failed")
		}
		for _, method := range []string{"GET", "HEAD"} {
			for _, condition := range []string{"", "*", first.Header().Get("ETag")} {
				req := httptest.NewRequest(method, "/", nil)
				req.Header.Set("Accept-Encoding", "zstd,identity;q=0")
				if condition != "" {
					req.Header.Set("If-None-Match", condition)
				}
				res := runRequest(t, h, req)
				if res.Code != 406 || res.Body.Len() != 0 || res.Header().Get("ETag") != "" {
					t.Fatalf("%s condition=%q bypassed negotiation: %d %v %q", method, condition, res.Code, res.Header(), res.Body.String())
				}
			}
		}
	}
}

func TestCompressionNegotiationPreservesRetry(t *testing.T) {
	for _, compressFirst := range []bool{false, true} {
		calls := 0
		base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			if calls == 1 {
				w.WriteHeader(503)
				_, _ = io.WriteString(w, "retryable but unacceptable error body")
				return
			}
			w.Header().Set("Content-Encoding", "zstd")
			_, _ = io.WriteString(w, "origin-coded bytes")
		})
		mws := []Middleware{Retry(2, OnStatus(503)), Compress(Gzip)}
		if compressFirst {
			mws[0], mws[1] = mws[1], mws[0]
		}
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Accept-Encoding", "zstd,identity;q=0")
		res := runRequest(t, chain(t, base, mws...), req)
		if calls != 2 || res.Code != 200 || res.Body.String() != "origin-coded bytes" || res.Header().Get("Content-Encoding") != "zstd" {
			t.Fatalf("compressFirst=%v hid retryable error or changed origin encoding: calls=%d status=%d headers=%v", compressFirst, calls, res.Code, res.Header())
		}
	}
}
