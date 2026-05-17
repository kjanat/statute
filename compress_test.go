package statute

import (
	"bytes"
	"compress/gzip"
	"io"
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

// keep the bytes import used; this test uses it only via http/httptest internals
var _ = bytes.MinRead
