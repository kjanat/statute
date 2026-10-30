package statute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
