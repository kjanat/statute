package statute

import (
	"bytes"
	"compress/gzip"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"

	"statute.kjanat.dev/resolved"
)

func TestCompress_GzipNegotiation(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("the quick brown fox\n", 64)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	})
	h := compressHandler([]resolved.CompressAlgo{resolved.Gzip}, inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := runRequest(t, h, req)

	assertHeader(t, rec.Header(), "Content-Encoding", "gzip")
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != payload {
		t.Errorf("decoded payload mismatch")
	}
}

func TestCompress_BrotliPreferredOverGzip(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("hello brotli\n", 32)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	})
	h := compressHandler([]resolved.CompressAlgo{resolved.Gzip, resolved.Brotli}, inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	rec := runRequest(t, h, req)

	assertHeader(t, rec.Header(), "Content-Encoding", "br")
	br := brotli.NewReader(rec.Body)
	decoded, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != payload {
		t.Errorf("brotli decoded mismatch")
	}
}

func TestCompress_IdentityWhenNotAccepted(t *testing.T) {
	t.Parallel()
	payload := "raw"
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	})
	h := compressHandler([]resolved.CompressAlgo{resolved.Gzip}, inner)

	req := httptest.NewRequest("GET", "/", nil)
	// no Accept-Encoding
	rec := runRequest(t, h, req)

	assertNoHeader(t, rec.Header(), "Content-Encoding")
	if rec.Body.String() != payload {
		t.Errorf("identity body: %q", rec.Body.String())
	}
}

func TestCompress_NoAlgosConfiguredIsNoOp(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "x")
	})
	h := compressHandler(nil, inner)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	rec := runRequest(t, h, req)
	assertNoHeader(t, rec.Header(), "Content-Encoding")
}

func TestCompress_VaryHeaderSet(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "x")
	})
	h := compressHandler([]resolved.CompressAlgo{resolved.Gzip}, inner)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := runRequest(t, h, req)
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary: got %q, want to contain Accept-Encoding", got)
	}
}

func TestCompressIdentityVaryCommit(t *testing.T) {
	for _, commit := range []string{"empty", "write", "header", "flush", "copy"} {
		t.Run(commit, func(t *testing.T) {
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Vary", "Accept-Language")
				switch commit {
				case "write":
					_, _ = io.WriteString(w, "body")
				case "header":
					w.WriteHeader(200)
				case "flush":
					w.(http.Flusher).Flush()
				case "copy":
					_, _ = io.Copy(w, plainReader{strings.NewReader("body")})
				}
			})
			rec := runRequest(t, compressHandler([]resolved.CompressAlgo{resolved.Gzip}, inner), httptest.NewRequest("GET", "/", nil))
			for _, name := range []string{"Accept-Language", "Accept-Encoding"} {
				if !strings.Contains(strings.Join(rec.Result().Header.Values("Vary"), ","), name) {
					t.Fatalf("committed Vary lost %s: %v", name, rec.Result().Header)
				}
			}
		})
	}
}

func TestCompressBodyless(t *testing.T) {
	for _, coding := range []struct {
		name string
		algo resolved.CompressAlgo
	}{{"gzip", resolved.Gzip}, {"br", resolved.Brotli}} {
		for _, status := range []int{200, 204, 205, 304} {
			method := "GET"
			if status == 200 {
				method = "HEAD"
			}
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
			req := httptest.NewRequest(method, "/", nil)
			req.Header.Set("Accept-Encoding", coding.name)
			res := runRequest(t, compressHandler([]resolved.CompressAlgo{coding.algo}, inner), req)
			if res.Code != status || res.Body.Len() != 0 {
				t.Fatalf("%s %s/%d emitted compressor bytes: %x", coding.name, method, status, res.Body.Bytes())
			}
		}
	}
}

func TestCompressPreservesSelectedRepresentation(t *testing.T) {
	for _, tc := range []struct {
		name              string
		status            int
		request, response http.Header
	}{
		{name: "encoded", response: http.Header{"content-encoding": {"gzip"}}},
		{name: "unknown coding", request: http.Header{"Accept-Encoding": {"gzip, unknown"}}, response: http.Header{"Content-Encoding": {"unknown"}}},
		{name: "response no-transform", response: http.Header{"Cache-Control": {"no-transform"}}},
		{name: "request no-transform", request: http.Header{"Cache-Control": {"no-transform"}}},
		{name: "invalid policy", response: http.Header{"Cache-Control": {`extension="unclosed`}}},
		{name: "partial status", status: 206},
		{name: "partial field", response: http.Header{"content-range": {"bytes 0-3/10"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				maps.Copy(w.Header(), tc.response)
				w.Header().Set("ETag", `"original"`)
				w.Header().Set("Content-Length", "4")
				status := tc.status
				if status == 0 {
					status = 200
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "body")
			})
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			maps.Copy(req.Header, tc.request)
			got := runRequest(t, compressHandler([]resolved.CompressAlgo{resolved.Gzip}, base), req)
			if got.Body.String() != "body" || got.Header().Get("ETag") != `"original"` || got.Header().Get("Content-Length") != "4" {
				t.Fatalf("changed selected representation: %v %q", got.Header(), got.Body.String())
			}
			if original, present := cacheHeaderValues(tc.response, "Content-Encoding"); present {
				value, _ := cacheHeaderValues(got.Header(), "Content-Encoding")
				if strings.Join(value, ",") != strings.Join(original, ",") {
					t.Fatal("replaced origin coding")
				}
			} else if got.Header().Get("Content-Encoding") != "" {
				t.Fatal("introduced a forbidden coding")
			}
		})
	}
}

func TestCompressAbortAndReuse(t *testing.T) {
	for _, algo := range []resolved.CompressAlgo{resolved.Gzip, resolved.Brotli} {
		for _, prefix := range []bool{false, true} {
			var committedSize int
			rec := httptest.NewRecorder()
			base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if prefix {
					_, _ = io.WriteString(w, "incomplete")
					w.(http.Flusher).Flush()
				}
				committedSize = rec.Body.Len()
				panic(http.ErrAbortHandler)
			})
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Accept-Encoding", "gzip, br")
			var got any
			func() {
				defer func() { got = recover() }()
				compressHandler([]resolved.CompressAlgo{algo}, base).ServeHTTP(rec, req)
			}()
			var want any = http.ErrAbortHandler
			if got != want || rec.Body.Len() != committedSize {
				t.Fatalf("abort changed or emitted a footer: %v, %d -> %d bytes", got, committedSize, rec.Body.Len())
			}
			ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "complete") })
			res := runRequest(t, compressHandler([]resolved.CompressAlgo{algo}, ok), req)
			var reader io.Reader = brotli.NewReader(res.Body)
			if algo == resolved.Gzip {
				gz, err := gzip.NewReader(res.Body)
				if err != nil {
					t.Fatal(err)
				}
				defer gz.Close()
				reader = gz
			}
			body, err := io.ReadAll(reader)
			if err != nil || string(body) != "complete" {
				t.Fatalf("codec reuse: %q %v", body, err)
			}
		}
	}
}

// keep the bytes import used; this test uses it only via http/httptest internals
var _ = bytes.MinRead
