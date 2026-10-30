package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestResponseEncodingPreservesVaryThroughBuffers(t *testing.T) {
	for _, buffered := range []Middleware{InOrder(), ETag(), Retry(2), InOrder(ETag(), Retry(2))} {
		calls := 0
		h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Vary", "Accept-Language")
			// #nosec G705 -- Plain-text fixture identifies the requested language.
			_, _ = io.WriteString(w, r.Header.Get("Accept-Language")+" body")
		}), Cache("1h"), EncodeResponse(EncodingPolicy{Encodings: []string{"br", "zstd"}, MinimumBytes: 1}), buffered)
		for range 2 {
			for _, language := range []string{"en", "nl"} {
				for _, coding := range []string{"br", "identity", "zstd"} {
					assertEncodingCacheResponse(t, h, coding, language)
				}
			}
		}
		if calls != 6 {
			t.Fatalf("cache did not reuse six independent variants: calls=%d", calls)
		}
	}
}

func assertEncodingCacheResponse(t *testing.T, h http.Handler, coding, language string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", coding)
	r.Header.Set("Accept-Language", language)
	w := runRequest(t, h, r)
	body, err := io.ReadAll(decodeEncodedResponse(t, w.Header().Get("Content-Encoding"), w.Body))
	if err != nil || string(body) != language+" body" {
		t.Fatalf("coding=%s language=%s body=%q err=%v", coding, language, body, err)
	}
	want := coding
	if want == "identity" {
		want = ""
	}
	if w.Header().Get("Content-Encoding") != want || !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "Accept-Encoding") {
		t.Fatalf("coding=%s headers=%v", coding, w.Header())
	}
}

func TestResponseEncodingPreservesCommittedHeaders(t *testing.T) {
	for _, coding := range []string{"gzip", "br", "zstd"} {
		t.Run(coding, func(t *testing.T) {
			calls := 0
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Cache-Control", "private, no-store")
				_, _ = io.WriteString(w, "private body")
				w.Header().Del("Cache-Control")
				w.Header().Set("X-Late", "ignored")
			}), Cache("1h"), EncodeResponse(EncodingPolicy{Encodings: []string{coding}}))
			for range 2 {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				r.Header.Set("Accept-Encoding", coding)
				w := runRequest(t, h, r)
				if w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("X-Late") != "" || w.Body.String() != "private body" {
					t.Fatalf("committed headers changed: %v %q", w.Header(), w.Body.String())
				}
			}
			if calls != 2 {
				t.Fatalf("private response was cached: calls=%d", calls)
			}
		})
	}
}

func TestResponseEncodingReportsLogicalCommitToCache(t *testing.T) {
	for _, coding := range []string{"gzip", "br", "zstd"} {
		t.Run(coding, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.Header().Set("Content-Type", "text/plain")
					w.WriteHeader(http.StatusOK)
					time.Sleep(2 * time.Second)
					_, _ = io.WriteString(w, "body")
				}), Cache("1s"), EncodeResponse(EncodingPolicy{Encodings: []string{coding}}))
				for range 2 {
					r := httptest.NewRequest(http.MethodGet, "/", nil)
					r.Header.Set("Accept-Encoding", coding)
					runRequest(t, h, r)
				}
				if calls != 2 {
					t.Fatalf("encoder buffering restarted TTL: calls=%d", calls)
				}
			})
		})
	}
}

func TestResponseEncodingPublishesUncommittedHeadersAndLateTrailers(t *testing.T) {
	for _, coding := range []string{"gzip", "br", "zstd"} {
		t.Run(coding, func(t *testing.T) {
			h := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Returned", "yes")
				if r.URL.Path == "/empty" {
					return
				}
				_, _ = io.WriteString(w, "body")
				w.Header().Set(http.TrailerPrefix+"X-Finished", "yes")
			}), EncodeResponse(EncodingPolicy{Encodings: []string{coding}}))
			for _, path := range []string{"/empty", "/body"} {
				r := httptest.NewRequest(http.MethodGet, path, nil)
				r.Header.Set("Accept-Encoding", coding)
				res := runRequest(t, h, r).Result()
				_ = res.Body.Close()
				if res.Header.Get("X-Returned") != "yes" || (path == "/body") != (res.Trailer.Get("X-Finished") == "yes") {
					t.Fatalf("%s headers=%v trailers=%v", path, res.Header, res.Trailer)
				}
			}
		})
	}
}
