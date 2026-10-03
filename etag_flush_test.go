package statute

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
)

func TestETagCompressionIgnoresFlushBoundaries(t *testing.T) {
	for name, algo := range map[string]CompressAlgo{"gzip": Gzip, "br": Brotli} {
		t.Run(name, func(t *testing.T) {
			mode := 0
			origin := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				if mode > 0 {
					w.(http.Flusher).Flush()
				}
				for _, part := range []string{"repres", "entation"} {
					_, _ = io.WriteString(w, part)
					if mode > 1 {
						w.(http.Flusher).Flush()
					}
				}
			})
			for name, mws := range map[string][]Middleware{
				"direct":         {ETag(), Compress(algo)},
				"outer cache":    {Cache("1h"), ETag(), Compress(algo)},
				"encoded cache":  {ETag(), Cache("1h"), Compress(algo)},
				"identity cache": {ETag(), Compress(algo), Cache("1h")},
				"retry re-entry": {ETag(), Retry(2, OnStatus(503)), Compress(algo)},
			} {
				t.Run(name, func(t *testing.T) {
					mode = 0
					h := chain(t, origin, mws...)
					req := httptest.NewRequest("GET", "/", nil)
					req.Header.Set("Accept-Encoding", "gzip, br")
					get := runRequest(t, h, req)
					for mode = 1; mode <= 2; mode++ {
						assertETagFlushRender(t, h, req, get)
					}
				})
			}
		})
	}
}

func assertETagFlushRender(t *testing.T, h http.Handler, request *http.Request, baseline *httptest.ResponseRecorder) {
	t.Helper()
	for _, method := range []string{"HEAD", "GET"} {
		for _, conditional := range []bool{false, true} {
			req := request.Clone(request.Context())
			req.Header.Set("Cache-Control", "no-cache")
			req.Method = method
			status := http.StatusOK
			if conditional {
				req.Header.Set("If-None-Match", baseline.Header().Get("ETag"))
				status = http.StatusNotModified
			}
			res := runRequest(t, h, req)
			assertETagFlushResponse(t, res, baseline, method, status)
		}
	}
}

func assertETagFlushResponse(t *testing.T, res, baseline *httptest.ResponseRecorder, method string, status int) {
	t.Helper()
	if res.Code != status || res.Header().Get("ETag") != baseline.Header().Get("ETag") {
		t.Fatalf("flush changed %s: status=%d tag=%s; want status=%d tag=%s",
			method, res.Code, res.Header().Get("ETag"), status, baseline.Header().Get("ETag"))
	}
	if status == http.StatusNotModified || method == "HEAD" {
		if res.Body.Len() != 0 {
			t.Fatal("HEAD/304 emitted buffered bytes")
		}
	} else if !bytes.Equal(res.Body.Bytes(), baseline.Body.Bytes()) {
		t.Fatal("flush changed compressed representation bytes")
	}
	if status == http.StatusOK && res.Header().Get("Content-Length") != baseline.Header().Get("Content-Length") {
		t.Fatal("flush changed representation length")
	}
}

func TestCompressionWithoutETagStillFlushes(t *testing.T) {
	for name, algo := range map[string]CompressAlgo{"gzip": Gzip, "br": Brotli} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			origin := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "first")
				w.(http.Flusher).Flush()
				if !rec.Flushed {
					t.Fatal("stream flush did not reach the client")
				}
				var reader io.Reader = brotli.NewReader(bytes.NewReader(rec.Body.Bytes()))
				if algo == Gzip {
					gz, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
					if err != nil {
						t.Fatal(err)
					}
					defer gz.Close()
					reader = gz
				}
				prefix := make([]byte, len("first"))
				if _, err := io.ReadFull(reader, prefix); err != nil || string(prefix) != "first" {
					t.Fatalf("prefix unavailable before handler return: %q, %v", prefix, err)
				}
				_, _ = io.WriteString(w, "last")
			})
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Accept-Encoding", "gzip, br")
			chain(t, origin, Compress(algo)).ServeHTTP(rec, req)
		})
	}
}
