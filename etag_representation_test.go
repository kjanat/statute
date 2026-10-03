package statute

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestETagHEADRendersGET(t *testing.T) {
	for _, cacheFirst := range []bool{false, true} {
		calls := 0
		base := etagRenderOrigin(t, &calls)
		mws := []Middleware{ETag(), Cache("1h")}
		if cacheFirst {
			mws[0], mws[1] = mws[1], mws[0]
		}
		h := chain(t, base, mws...)
		get := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
		req := httptest.NewRequest("HEAD", "/", nil)
		req.Header.Set("Range", "bytes=0-1")
		req.Header.Set("If-Match", "*")
		original := req.Clone(req.Context())
		head := runRequest(t, h, req)
		assertOriginalHEAD(t, req, original)
		if head.Body.Len() != 0 || head.Header().Get("ETag") != get.Header().Get("ETag") || head.Header().Get("Content-Length") != "23" {
			t.Fatalf("HEAD differs from GET: %v, %q", head.Header(), head.Body.String())
		}
		req.Header.Set("If-None-Match", `"other", W/`+get.Header().Get("ETag"))
		notModified := runRequest(t, h, req)
		if notModified.Code != 304 || notModified.Body.Len() != 0 {
			t.Fatalf("original HEAD condition not evaluated: %d %q", notModified.Code, notModified.Body.String())
		}
		if calls < 1 {
			t.Fatal("GET representation never rendered")
		}
	}
}

func assertOriginalHEAD(t *testing.T, req, original *http.Request) {
	t.Helper()
	if req.Method != original.Method || !reflect.DeepEqual(req.Header, original.Header) {
		t.Fatal("render mutated the original HEAD request")
	}
}

func etagRenderOrigin(t *testing.T, calls *int) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Method != http.MethodGet || r.Header.Get("If-None-Match") != "" || r.Header.Get("Range") != "" || r.Header.Get("If-Match") != "" {
			t.Errorf("conditional render short-circuited: %s %v", r.Method, r.Header)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "rendered representation")
	})
}

func TestETagCompressionPosition(t *testing.T) {
	for _, tc := range []struct {
		outerETag bool
		algo      CompressAlgo
	}{{false, Gzip}, {true, Gzip}, {false, Brotli}, {true, Brotli}} {
		outerETag := tc.outerETag
		base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "representation")
		})
		mws := []Middleware{Compress(tc.algo), ETag()}
		if outerETag {
			mws[0], mws[1] = mws[1], mws[0]
		}
		h := chain(t, base, mws...)
		getReq := httptest.NewRequest("GET", "/", nil)
		getReq.Header.Set("Accept-Encoding", "gzip, br")
		get := runRequest(t, h, getReq)
		headReq := getReq.Clone(getReq.Context())
		headReq.Method = "HEAD"
		head := runRequest(t, h, headReq)
		want := assertETagDigest(t, get, head, outerETag)
		headReq.Header.Set("If-None-Match", want)
		if got := runRequest(t, h, headReq); got.Code != 304 || got.Body.Len() != 0 {
			t.Fatal("encoded HEAD conditional mismatch")
		}
		getReq.Header.Set("If-None-Match", want)
		if got := runRequest(t, h, getReq); got.Code != 304 || got.Body.Len() != 0 {
			t.Fatal("encoded GET conditional emitted body bytes")
		}
	}
}

func assertETagDigest(t *testing.T, get, head *httptest.ResponseRecorder, outerETag bool) string {
	t.Helper()
	content := []byte("representation")
	prefix := "W/"
	if outerETag {
		content, prefix = get.Body.Bytes(), ""
	}
	sum := sha256.Sum256(content)
	want := prefix + `"` + hex.EncodeToString(sum[:16]) + `"`
	if get.Header().Get("ETag") != want || head.Header().Get("ETag") != want || head.Body.Len() != 0 {
		t.Fatalf("wrong representation validator: get=%v head=%v body=%q", get.Header(), head.Header(), head.Body.String())
	}
	if !outerETag && head.Header().Get("Content-Length") != "" {
		t.Fatal("identity length leaked into encoded HEAD")
	}
	return want
}

func TestETagHEADIsOneObservation(t *testing.T) {
	var log bytes.Buffer
	stats := newStats()
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "body") })
	h := metricsMiddleware(stats, accessLogMiddleware(resolved.AccessLog{Enabled: true, Writer: &log, SampleRate: 1}, etagHandler(base)))
	runRequest(t, h, httptest.NewRequest("HEAD", "/", nil))
	if stats.requests.Load() != 1 || strings.Count(log.String(), `"method":"HEAD"`) != 1 || strings.Contains(log.String(), `"method":"GET"`) {
		t.Fatalf("internal render became another external request: %s", log.String())
	}
}

func TestETagInformationalAndAbort(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "body")
	})
	if got := runRequest(t, etagHandler(base), httptest.NewRequest("GET", "/", nil)); got.Code != 200 || got.Header().Get("ETag") == "" {
		t.Fatal("informational status hid the selected representation")
	}
	bad := etagHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "incomplete")
		panic(http.ErrAbortHandler)
	}))
	w := httptest.NewRecorder()
	var want any = http.ErrAbortHandler
	if got := handlerPanic(bad, w); got != want || w.Body.Len() != 0 || w.Header().Get("ETag") != "" {
		t.Fatal("failed render published a partial body or validator")
	}
}

func TestRepresentationMiddlewareUpgradePassthrough(t *testing.T) {
	for _, mws := range [][]Middleware{{ETag()}, {Cache("1h")}, {Compress(Gzip)}, {ETag(), Cache("1h"), Compress(Gzip)}} {
		w := httptest.NewRecorder()
		base := http.HandlerFunc(func(got http.ResponseWriter, r *http.Request) {
			if got != w || r.Method != "GET" || r.Header.Get("If-Match") != "*" {
				t.Fatal("upgrade entered buffered representation rendering")
			}
		})
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("If-Match", "*")
		chain(t, base, mws...).ServeHTTP(w, req)
	}
}

func TestETagReplacesNoncanonicalMetadata(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["etag"] = []string{`"stale"`}
		w.Header()["content-length"] = []string{"999"}
		w.Header()["content-digest"] = []string{"stale"}
		_, _ = io.WriteString(w, "body")
	})
	h := etagHandler(base)
	get := runRequest(t, h, httptest.NewRequest("GET", "/", nil))
	if values, _ := cacheHeaderValues(get.Header(), "ETag"); len(values) != 1 || values[0] == `"stale"` {
		t.Fatal("stale validator survived rendering")
	}
	if values, _ := cacheHeaderValues(get.Header(), "Content-Length"); len(values) != 1 || values[0] != "4" {
		t.Fatal("stale length survived rendering")
	}
	for _, condition := range []string{get.Header().Get("ETag"), "invalid"} {
		req := httptest.NewRequest("HEAD", "/", nil)
		req.Header.Set("If-None-Match", condition)
		res := runRequest(t, h, req)
		for _, name := range []string{"Content-Length", "Content-Digest"} {
			if _, present := cacheHeaderValues(res.Header(), name); present {
				t.Fatalf("bodyless conditional result retained %s", name)
			}
		}
	}
}

func TestETagHEADInferredContentType(t *testing.T) {
	h := etagHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<!doctype html><html><body>page</body></html>")
	}))
	server := httptest.NewServer(h)
	defer server.Close()
	for _, method := range []string{"GET", "HEAD"} {
		req, err := http.NewRequestWithContext(t.Context(), method, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatalf("%s lost inferred representation type: %v", method, res.Header)
		}
	}
}
